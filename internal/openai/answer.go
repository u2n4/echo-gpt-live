package openai

// Answering a question the voice model cannot answer alone.
//
// gpt-live-1 knows what it was trained on and nothing about this house, this machine, today, or what
// he wrote down last Tuesday. When it needs one of those it raises a delegation, and this is what
// runs underneath it.
//
// Two deliberate choices:
//
//   - A SEPARATE, CHEAP model does this work. The voice model is expensive per minute and the heavy
//     model is expensive per token, and neither is needed to decide that "how much have I spent"
//     means reading one file. Web search is the exception that proves it: OpenAI charges $10 per
//     thousand searches, one cent each, on top of tokens. That is worth paying for a question about
//     the world and worth avoiding for a question about the machine in the next room.
//   - The answer is ONE OR TWO SENTENCES. It is going to be spoken, and the API that carries it back
//     caps it at 500 tokens anyway. A tool that returns a table has not finished its job, and neither
//     has a model that returns a paragraph.
//
// The assistant is silent while this runs and that silence is billed, so everything here is bounded:
// a token cap, a round cap, and a deadline.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/u2n4/echo-gpt-live/internal/tools"
)

// SearchModel is the cheapest model that can both call functions and run a web search. Cheap
// matters here: this runs on every delegation, and a delegation happens mid-conversation while the
// clock is running.
const SearchModel = "gpt-5-nano"

// answerRounds bounds the tool loop. Two is enough for "search, then answer" or "read, then answer";
// a third exists only so a failed first tool can be retried differently. Past that the assistant has
// been silent too long to be worth continuing.
const answerRounds = 3

// Answerer turns a spoken question into a spoken answer, using tools when it has to.
type Answerer struct {
	APIKey  string
	Model   string
	HTTP    *http.Client
	Tools   *tools.Registry
	OnEvent func(string, ...any)
	// WebSearch adds OpenAI's built-in search to the tool list. It costs a cent per call, so it is
	// a flag rather than an assumption.
	WebSearch bool
	// MaxOutputTokens keeps the spoken answer short.
	MaxOutputTokens int
	// Deadline bounds the whole thing, tools included.
	Deadline time.Duration
	// context is where and when he is, and what it remembers. Without it this model answered "how
	// hot is it here" in 3.3 seconds using no tools at all - it had no idea what "here" meant, so
	// it guessed instead of searching. It is rewritten mid-call by a "remember" that settled on
	// another goroutine while a delegation may be reading it, so it lives behind an atomic.
	context atomic.Pointer[string]

	// noReasoningField remembers that this model rejected the reasoning knob, so the retry happens
	// once per process rather than once per question.
	noReasoningField atomic.Bool
}

// SetContext replaces what the router is told about where, when and who; safe from any goroutine.
func (a *Answerer) SetContext(s string) { a.context.Store(&s) }

func (a *Answerer) event(ev string, kv ...any) {
	if a.OnEvent != nil {
		a.OnEvent(ev, kv...)
	}
}

func (a *Answerer) model() string {
	if a.Model != "" {
		return a.Model
	}
	return SearchModel
}

func (a *Answerer) client() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// answerSystem is written for a model that is about to be read aloud in a room.
const answerSystem = `You answer a question that was asked out loud, and your answer is spoken back by a voice assistant.

Rules:
- One or two sentences. Never a list, never a table, never markdown.
- Use a tool when the answer depends on this machine, this house, his notes, today, or anything after your training. Otherwise answer directly; reaching for a tool you do not need costs him money and makes him wait.
- Say numbers the way a person says them out loud.
- If a tool fails or you cannot find out, say so plainly in one sentence. Do not guess.
- Never ask whether he wants you to look something up, and never say you cannot know before you have tried: he cannot type a yes. When the answer is about something current or something that changes - a price, a score, the news, what happened recently, the latest version of something - call web_search now, in this turn, and then speak what it found. Something that has not changed since your training you simply answer.
- Answer in the language the question was asked in.`

type respTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type respOutput struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	CallID    string `json:"call_id"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type respBody struct {
	// Output is kept RAW as well as parsed. A continuation has to send the model's own output items
	// back verbatim — reasoning items included — and a reasoning model rejects a tool result that
	// arrives without them. Rebuilding the items from the fields we happened to care about drops
	// exactly the ones we do not understand, which is the set that matters.
	Output []json.RawMessage `json:"output"`
	Status string            `json:"status"`
	// IncompleteDetails is how the API says "this stopped early". Without reading it, a sentence cut
	// off mid-word is indistinguishable from a finished answer.
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

// RunTool runs ONE already-agreed-to call, with no model in the loop at all.
//
// This is what a spoken yes executes. The alternative — re-running the question with permission
// switched on — hands a blanket authorisation to a fresh model turn, which is free to produce a
// different write, several writes, or a retry of one that already landed.
func (a *Answerer) RunTool(ctx context.Context, name, args string) (string, error) {
	if a.Tools == nil {
		return "", errors.New("no tools")
	}
	deadline := a.Deadline
	if deadline <= 0 {
		deadline = 25 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	t0 := time.Now()
	out, err := a.Tools.Call(ctx, name, args, true)
	a.event("tool_confirmed_run", "tool", name, "ms", time.Since(t0).Milliseconds(),
		"ok", err == nil)
	return out, err
}

// Answer runs the question to a spoken sentence. A write tool always refuses here and says what it
// wanted to do, which is the sentence the assistant asks him; the yes is executed by RunTool, not
// by calling this again with permission on.
func (a *Answerer) Answer(ctx context.Context, question string) (string, error) {
	const confirmed = false
	if a.APIKey == "" {
		return "", errors.New("no OpenAI API key")
	}
	if strings.TrimSpace(question) == "" {
		return "", errors.New("no question")
	}
	deadline := a.Deadline
	if deadline <= 0 {
		deadline = 25 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	var toolList []respTool
	if a.Tools != nil {
		for _, s := range a.Tools.Schemas() {
			toolList = append(toolList, respTool{
				Type:        "function",
				Name:        s["name"].(string),
				Description: s["description"].(string),
				Parameters:  s["parameters"].(map[string]any),
			})
		}
	}
	if a.WebSearch {
		toolList = append(toolList, respTool{Type: "web_search"})
	}

	system := answerSystem
	if c := a.context.Load(); c != nil && *c != "" {
		system += "\n\n" + *c
	}
	// The cheap local tools run BEFORE the model is asked anything, in parallel, and their answers
	// go in here. The first model call used to exist only so the model could name a tool it wanted;
	// for the questions he actually asks - the weather, the spend, the machine - the answer is now
	// already in front of it and one whole round trip disappears. Measured: a model call is 1.5-2.5
	// seconds, and these three together are about 0.6.
	if a.Tools != nil {
		if amb := a.Tools.Ambient(ctx); amb != "" {
			system += "\n\n" + amb
			a.event("ambient_ready", "chars", len(amb))
		}
	}

	// The conversation, in the Responses API's input shape. It holds `any` rather than maps so the
	// model's own output items can go back in untouched.
	input := []any{
		map[string]any{"role": "system", "content": system},
		map[string]any{"role": "user", "content": question},
	}

	started := time.Now()
	var usedTools []string

	for round := 0; round < answerRounds; round++ {
		body := map[string]any{
			"model": a.model(),
			"input": input,
		}
		// The LAST round offers no tools at all. It used to offer them, run whatever came back, and
		// then return "gave up" regardless — so an append could land on disk and be reported as a
		// failure, and the retry would write it twice. With nothing to call, the model has to answer
		// with a sentence, which is the only thing that round can actually use.
		lastRound := round == answerRounds-1
		if len(toolList) > 0 && !lastRound {
			body["tools"] = toolList
		}
		if a.MaxOutputTokens > 0 {
			body["max_output_tokens"] = a.MaxOutputTokens
		}
		// Thinking is billed to the same budget as the answer and is spent before it, so this was
		// "minimal". Measured on 2026-09-14, minimal is also why she could not search: on "what is
		// the price of gold today" and "who won yesterday" the model answered "I cannot know that,
		// do you want me to search?" instead of calling the tool, and on "the latest iPhone" it
		// stated a stale answer as fact. At "low" it called web_search on three of three such
		// questions, still answered "how are you" and a date sum without a tool, and took 1.4-2.3
		// seconds against minimal's 1.2-4.0. The extra thinking is what decides to use a tool.
		if !a.noReasoningField.Load() {
			body["reasoning"] = map[string]any{"effort": "low"}
		}

		out, err := a.post(ctx, body)
		if err != nil && strings.Contains(err.Error(), "reasoning") && !a.noReasoningField.Load() {
			// A tuning parameter must never be the reason the assistant does not answer.
			a.noReasoningField.Store(true)
			a.event("answer_dropped_reasoning_field", "err", err.Error())
			delete(body, "reasoning")
			out, err = a.post(ctx, body)
		}
		if err != nil {
			return "", err
		}

		var calls []respOutput
		var text string
		for _, raw := range out.Output {
			var o respOutput
			if json.Unmarshal(raw, &o) != nil {
				continue
			}
			switch o.Type {
			case "function_call":
				calls = append(calls, o)
			case "message":
				for _, c := range o.Content {
					if c.Text != "" {
						text += c.Text
					}
				}
			}
		}

		if out.Status == "incomplete" {
			reason := "unknown"
			if out.IncompleteDetails != nil {
				reason = out.IncompleteDetails.Reason
			}
			// max_output_tokens covers REASONING as well as the sentence, so a cap chosen for the
			// length of a spoken answer can be spent before any speech exists. Half a sentence read
			// aloud is worse than admitting it.
			a.event("answer_incomplete", "reason", reason, "chars", len(text))
			return "", fmt.Errorf("the answer was cut off (%s)", reason)
		}

		if len(calls) == 0 {
			answer := strings.TrimSpace(text)
			if answer == "" {
				return "", errors.New("the answer came back empty")
			}
			a.event("answer_done", "rounds", round+1, "ms", time.Since(started).Milliseconds(),
				"tools", strings.Join(usedTools, ","), "chars", len(answer))
			return answer, nil
		}

		// Everything the model produced goes back verbatim, in its own order, BEFORE the results.
		for _, raw := range out.Output {
			input = append(input, raw)
		}
		for _, call := range calls {
			usedTools = append(usedTools, call.Name)
			result, cerr := a.Tools.Call(ctx, call.Name, call.Arguments, confirmed)
			if cerr != nil {
				var need *tools.ErrNeedsConfirmation
				if errors.As(cerr, &need) {
					// Not a failure. The assistant has to ask him first, and this is the sentence.
					a.event("answer_needs_confirmation", "tool", call.Name)
					return "", cerr
				}
				a.event("answer_tool_failed", "tool", call.Name, "err", cerr.Error())
				result = "That did not work: " + cerr.Error()
			}
			input = append(input, map[string]any{
				"type": "function_call_output", "call_id": call.CallID, "output": result,
			})
		}
	}
	return "", fmt.Errorf("gave up after %d rounds of tools", answerRounds)
}

func (a *Answerer) post(ctx context.Context, body map[string]any) (*respBody, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responsesURL, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.APIKey)
	req.Header.Set("Content-Type", "application/json")

	t0 := time.Now()
	res, err := a.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("asking the backend: %w", err)
	}
	defer res.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	var out respBody
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("backend HTTP %d, unreadable body: %s", res.StatusCode, truncate(string(payload), 200))
	}
	if res.StatusCode >= 400 {
		msg := truncate(string(payload), 300)
		if out.Error != nil {
			msg = out.Error.Code + ": " + out.Error.Message
		}
		return nil, fmt.Errorf("backend HTTP %d: %s", res.StatusCode, msg)
	}
	a.event("answer_call", "model", body["model"], "ms", time.Since(t0).Milliseconds(),
		"in_tokens", out.Usage.InputTokens, "out_tokens", out.Usage.OutputTokens)
	return &out, nil
}
