package openai

// Client-mode delegation: this bridge calls the Responses API itself, so the reasoning effort can
// be chosen per question instead of being pinned for the whole Live session (the `delegation`
// object is fixed at session creation — "Choose the mode when you create the session; to change
// modes, start a new session").
//
// HOW THE REQUEST IS BOUNDED — and why not with a number per effort level.
//
// A deadline picked from the effort level is wrong in both directions: too small and it
// guillotines a legitimate long answer, which then looks exactly like a provider fault; too large
// and a genuinely dead connection hangs the voice session. Neither says anything about whether the
// model is still working.
//
// So the request is STREAMED (`stream: true`; every event carries a `type`) and bounded by
// PROGRESS instead:
//
//	FirstEvent  — how long we wait for the stream to say anything at all
//	Stall       — how long we tolerate NO event once the stream has started
//	MaxTotal    — a safety net only, so a wedged socket cannot leak a goroutine forever
//
// Reasoning summaries are requested (`reasoning: {effort, summary: "auto"}`) precisely so that the
// thinking phase emits events too. Without them a high-effort call is silent until the first
// answer token — measured at 43.81 s for gpt-5.6-luna at xhigh and 134.29 s at max (Artificial
// Analysis) — and any stall watchdog would fire on a healthy request. With them, "is it still
// working" becomes an observation rather than an assumption.
//
// Every run records its real first-event and inter-event timings, so these budgets stop being
// guesses after the first few calls.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const responsesURL = "https://api.openai.com/v1/responses"

// Progress budgets. These bound INACTIVITY, not the length of a correct answer.
const (
	// DefaultFirstEvent covers connect plus the model starting to emit anything, including a
	// reasoning summary. Generous because a cold high-effort call is legitimately slow.
	DefaultFirstEvent = 180 * time.Second
	// DefaultStall is how long a started stream may go quiet before it is considered dead.
	DefaultStall = 45 * time.Second
	// DefaultMaxTotal is a safety net, not a deadline. 0 disables it.
	DefaultMaxTotal = 15 * time.Minute
)

// Backend calls a text model for the heavy work behind the voice layer.
type Backend struct {
	APIKey string
	Model  string // default gpt-5.6-luna

	FirstEvent time.Duration
	Stall      time.Duration
	MaxTotal   time.Duration

	HTTP    *http.Client
	OnEvent func(ev string, kv ...any)
	// OnProgress fires as the answer develops, so the voice layer can keep the user informed
	// instead of going silent. phase is "reasoning" or "answer".
	OnProgress func(phase string, chars int, elapsed time.Duration)
}

// NewBackend returns a Backend with progress-based bounds and no fixed request timeout.
func NewBackend(apiKey, model string) *Backend {
	if model == "" {
		model = BackendModel
	}
	return &Backend{
		APIKey:     apiKey,
		Model:      model,
		FirstEvent: DefaultFirstEvent,
		Stall:      DefaultStall,
		MaxTotal:   DefaultMaxTotal,
		// No Timeout on the client: the stream is bounded by progress, above.
		HTTP: &http.Client{},
	}
}

func (b *Backend) event(ev string, kv ...any) {
	if b.OnEvent != nil {
		b.OnEvent(ev, kv...)
	}
}

// AnnounceWait reports whether this effort level is slow enough that the voice layer should tell
// the user to expect a pause rather than going silent.
func AnnounceWait(effort string) bool {
	return effort == "high" || effort == "xhigh" || effort == "max"
}

// AskResult carries the answer and what it actually cost in time, tokens and money.
type AskResult struct {
	Text            string
	Model           string
	Effort          string
	Elapsed         time.Duration
	HeaderWait      time.Duration
	FirstEventAfter time.Duration
	LongestGap      time.Duration
	Events          int
	ReasoningChars  int
	InputTokens     int
	OutputTokens    int
	ReasoningTokens int
	CachedTokens    int
	Truncated       bool
	CostUSD         float64
	Status          string
}

// streamEnvelope is the part of every streaming event this code relies on: the `type` field.
// Unknown event types are counted as progress and otherwise ignored, so new event names added by
// OpenAI cannot break the watchdog.
type streamEnvelope struct {
	Type     string          `json:"type"`
	Delta    string          `json:"delta"`
	Text     string          `json:"text"`
	Response *responseObject `json:"response"`
	Error    *apiError       `json:"error"`
}

type apiError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	Type    string `json:"type"`
}

type responseObject struct {
	Status            string   `json:"status"`
	OutputText        []string `json:"output_text"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage *struct {
		InputTokens       int `json:"input_tokens"`
		OutputTokens      int `json:"output_tokens"`
		InputTokenDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputTokenDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
	Error *apiError `json:"error"`
}

// Ask runs one backend request, streamed and bounded by progress. kind selects the effort via
// BackendEffortFor ("tool", "quick", "hard", or "" for the default).
func (b *Backend) Ask(ctx context.Context, kind, instructions, question string, maxOutput int) (*AskResult, error) {
	effort := BackendEffortFor(kind)

	payload := map[string]any{
		"model":  b.Model,
		"input":  question,
		"stream": true,
	}
	if instructions != "" {
		payload["instructions"] = instructions
	}
	if effort != "" {
		// summary:"auto" makes the thinking phase observable, which is what lets a single
		// stall watchdog be correct at every effort level.
		payload["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
	}
	if maxOutput > 0 {
		if maxOutput < 16 {
			maxOutput = 16 // documented minimum
		}
		payload["max_output_tokens"] = maxOutput
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	// The safety net is the only absolute bound, and it is deliberately far from any plausible
	// answer time so that it never decides the outcome of a healthy request.
	reqCtx := ctx
	var cancelTotal context.CancelFunc
	if b.MaxTotal > 0 {
		reqCtx, cancelTotal = context.WithTimeout(ctx, b.MaxTotal)
		defer cancelTotal()
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, responsesURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	t0 := time.Now()
	res, err := b.HTTP.Do(req)
	if err != nil {
		b.event("backend_error", "model", b.Model, "effort", effort, "ms", time.Since(t0).Milliseconds(), "err", err.Error())
		return nil, fmt.Errorf("backend %s (%s): %w", b.Model, effort, err)
	}
	defer res.Body.Close()

	if res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		msg := truncate(string(raw), 400)
		var e struct{ Error *apiError }
		if json.Unmarshal(raw, &e) == nil && e.Error != nil {
			msg = e.Error.Code + ": " + e.Error.Message
		}
		b.event("backend_error", "model", b.Model, "effort", effort, "status", res.StatusCode, "err", msg)
		return nil, fmt.Errorf("backend %s (%s): HTTP %d: %s", b.Model, effort, res.StatusCode, msg)
	}

	tHeaders := time.Now()
	b.event("backend_stream_open", "model", b.Model, "effort", effort,
		"header_wait_s", tHeaders.Sub(t0).Seconds())

	out := &AskResult{Model: b.Model, Effort: effort, HeaderWait: tHeaders.Sub(t0)}
	// The activity baseline is when the stream became readable, not when the request was sent.
	err = b.consume(reqCtx, res.Body, t0, tHeaders, out)
	out.Elapsed = time.Since(t0)
	if out.Usage() {
		out.CostUSD = lunaCostUSD(out.InputTokens, out.CachedTokens, out.OutputTokens)
	}
	// Truncation looks like success: a call can burn its whole budget on invisible reasoning and
	// stop mid-sentence. Detect it from the status and the token counts, not from text length.
	if maxOutput > 0 && out.OutputTokens >= maxOutput {
		out.Truncated = true
	}

	b.event("backend_answer", "model", b.Model, "effort", effort,
		"seconds", out.Elapsed.Seconds(), "first_event_s", out.FirstEventAfter.Seconds(),
		"longest_gap_s", out.LongestGap.Seconds(), "events", out.Events,
		"reasoning_chars", out.ReasoningChars, "chars", len(out.Text),
		"out_tokens", out.OutputTokens, "reasoning_tokens", out.ReasoningTokens,
		"cached_in", out.CachedTokens, "truncated", out.Truncated,
		"status", out.Status, "cost_usd", out.CostUSD)

	if err != nil {
		return out, err
	}
	if out.Text == "" {
		// An empty answer after reasoning tokens were spent means the budget went to thinking,
		// not that the endpoint is dead. Say so, so the caller raises the budget or lowers the
		// effort instead of blaming the provider.
		return out, fmt.Errorf("backend %s (%s) produced no text after %d reasoning tokens in %.1fs; raise max_output_tokens or lower the effort",
			b.Model, effort, out.ReasoningTokens, out.Elapsed.Seconds())
	}
	return out, nil
}

// Usage reports whether token counts were received.
func (r *AskResult) Usage() bool { return r.InputTokens > 0 || r.OutputTokens > 0 }

// consume reads the SSE stream, resetting the inactivity watchdog on every event. It accepts both
// the standard `data: {json}` framing and bare JSON lines, and ignores `event:` lines because the
// type is carried inside the JSON envelope as well.
func (b *Backend) consume(ctx context.Context, body io.Reader, t0, streamStart time.Time, out *AskResult) error {
	type line struct {
		s   string
		err error
	}
	lines := make(chan line, 64)
	// The reader runs in its own goroutine so the watchdog can act while it is blocked on Read.
	// The HTTP body is closed by the caller's defer, which unblocks this goroutine.
	go func() {
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
		for sc.Scan() {
			lines <- line{s: sc.Text()}
		}
		lines <- line{err: sc.Err()}
	}()

	var (
		answer    strings.Builder
		lastAt    = streamStart
		firstSeen bool
		mu        sync.Mutex
	)
	deadline := func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		if !firstSeen {
			return b.FirstEvent - time.Since(lastAt)
		}
		return b.Stall - time.Since(lastAt)
	}

	expired := func() error {
		mu.Lock()
		idle := time.Since(lastAt)
		seen := firstSeen
		mu.Unlock()
		if !seen && idle >= b.FirstEvent {
			return fmt.Errorf("backend %s: no stream activity at all within %s of the stream opening — treating as dead", b.Model, b.FirstEvent)
		}
		if seen && idle >= b.Stall {
			return fmt.Errorf("backend %s: stream stalled for %s after %d events — treating as dead", b.Model, b.Stall, out.Events)
		}
		return nil
	}

	for {
		// Decide on an already-expired budget BEFORE giving a pending line a chance: otherwise
		// select picks at random between a late event and a fired timer, and the watchdog
		// becomes nondeterministic.
		if err := expired(); err != nil {
			return err
		}
		wait := deadline()
		if wait <= 0 {
			wait = time.Millisecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("backend %s: %w after %.1fs (safety cap, not the model's deadline)", b.Model, ctx.Err(), time.Since(t0).Seconds())
		case <-timer.C:
			if err := expired(); err != nil {
				return err
			}
		case l := <-lines:
			timer.Stop()
			if l.err != nil {
				return fmt.Errorf("backend %s: stream read: %w", b.Model, l.err)
			}
			raw := strings.TrimSpace(l.s)
			if raw == "" {
				continue
			}
			now := time.Now()
			mu.Lock()
			if !firstSeen {
				firstSeen = true
				out.FirstEventAfter = now.Sub(streamStart)
			} else if gap := now.Sub(lastAt); gap > out.LongestGap {
				out.LongestGap = gap
			}
			lastAt = now
			mu.Unlock()

			if strings.HasPrefix(raw, "event:") || strings.HasPrefix(raw, ":") {
				continue // the type also lives in the JSON envelope
			}
			payload := raw
			if after, ok := strings.CutPrefix(raw, "data:"); ok {
				payload = strings.TrimSpace(after)
			}
			if payload == "" || payload == "[DONE]" {
				if payload == "[DONE]" {
					return nil
				}
				continue
			}
			if !strings.HasPrefix(payload, "{") {
				continue
			}
			out.Events++

			var env streamEnvelope
			if err := json.Unmarshal([]byte(payload), &env); err != nil {
				continue // an unparseable frame is still progress; do not fail the call on it
			}
			switch {
			case env.Type == "error" || env.Error != nil:
				msg := "unknown error"
				if env.Error != nil {
					msg = env.Error.Code + ": " + env.Error.Message
				}
				return errors.New("backend stream error: " + msg)

			case strings.HasSuffix(env.Type, "output_text.delta"):
				answer.WriteString(env.Delta)
				if b.OnProgress != nil {
					b.OnProgress("answer", answer.Len(), time.Since(t0))
				}

			case strings.Contains(env.Type, "reasoning") && strings.HasSuffix(env.Type, ".delta"):
				out.ReasoningChars += len(env.Delta)
				if b.OnProgress != nil {
					b.OnProgress("reasoning", out.ReasoningChars, time.Since(t0))
				}

			case env.Type == "response.completed" || env.Type == "response.incomplete" || env.Type == "response.failed":
				if env.Response != nil {
					applyResponse(out, env.Response)
				}
				out.Status = strings.TrimPrefix(env.Type, "response.")
				if out.Text == "" {
					out.Text = strings.TrimSpace(answer.String())
				}
				if env.Type == "response.failed" {
					msg := "response failed"
					if env.Response != nil && env.Response.Error != nil {
						msg = env.Response.Error.Code + ": " + env.Response.Error.Message
					}
					return errors.New("backend: " + msg)
				}
				return nil
			}
		}
	}
}

func applyResponse(out *AskResult, r *responseObject) {
	if t := collectText(r); t != "" {
		out.Text = t
	}
	if r.Status != "" {
		out.Status = r.Status
	}
	if r.IncompleteDetails != nil && r.IncompleteDetails.Reason != "" {
		out.Truncated = true
	}
	if r.Usage != nil {
		out.InputTokens = r.Usage.InputTokens
		out.OutputTokens = r.Usage.OutputTokens
		out.ReasoningTokens = r.Usage.OutputTokenDetails.ReasoningTokens
		out.CachedTokens = r.Usage.InputTokenDetails.CachedTokens
	}
}

func collectText(r *responseObject) string {
	var b strings.Builder
	for _, s := range r.OutputText {
		b.WriteString(s)
	}
	if b.Len() > 0 {
		return strings.TrimSpace(b.String())
	}
	for _, o := range r.Output {
		if o.Type == "reasoning" {
			continue // never let a reasoning summary leak into the spoken answer
		}
		for _, c := range o.Content {
			if c.Type == "output_text" || c.Text != "" {
				b.WriteString(c.Text)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// lunaCostUSD prices a call at the published gpt-5.6-luna rates:
// $0.20 / 1M input, $0.02 / 1M cached input, $1.20 / 1M output.
func lunaCostUSD(in, cached, out int) float64 {
	fresh := in - cached
	if fresh < 0 {
		fresh = 0
	}
	return float64(fresh)*0.20/1e6 + float64(cached)*0.02/1e6 + float64(out)*1.20/1e6
}
