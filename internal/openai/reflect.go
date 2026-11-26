package openai

// Learning from a call after it has ended, building a strong two-layer household memory:
//   - facts: up to 80 concise, categorized facts kept in active memory (injected every call)
//   - archive: extra specific details (names, item spots, anecdotes, dates) saved into the
//     unlimited searchable archive so nothing important is ever forgotten.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const reflectSystem = `You maintain a strong, well-organized two-layer family memory for a voice assistant in a family home.

You are given the current active memory list and a transcript of what the family said in the conversation that just ended.
Return the updated active memory ("facts", up to 80 items) AND any additional detail notes worth preserving in the long-term searchable archive ("archive").

Capture and keep EVERYTHING genuinely useful for the household:
- [أسماء وعائلة] Names and relationships of every family member, child, grandchild, sibling, relative, neighbor, friend, doctor, or teacher mentioned.
- [أماكن وأشياء] Where items, keys, documents, medicines, tools, or household things are kept ("مفتاح المخزن في الدرج الثاني").
- [تفضيلات] Favorite foods, drinks, dietary needs, allergies, favorite TV channels/shows/reciters/scholars, speaking style preferences.
- [صحة وأدوية] Medicines, health notes, recurring appointments, doctor names and clinics.
- [مواعيد ومناسبات] Birthdays, anniversaries, recurring schedules, important family events.
- [عام] Any other personal, practical, or household fact they mentioned wanting remembered or likely to ask about later.

Never keep:
- Passing one-off queries (today's weather, a one-time price check, random trivia, TV remote button clicks like "يمين / تحت / ميوت").
- What the assistant said.
- Exact duplicates.

Rules:
- Preserve existing valid facts! Only drop or update a fact if the new conversation clearly corrects or supersedes it.
- Prefix each active fact with its short Arabic category tag when clear: [أسماء وعائلة], [أماكن وأشياء], [تفضيلات], [صحة وأدوية], [مواعيد ومناسبات], or [عام].
- Write each fact as one clear, short sentence in Arabic (or the language used).
- If the conversation had nothing worth remembering (e.g., just weather or TV clicks), return the existing facts unchanged and an empty archive list.

Answer with JSON only: {"facts": ["...", "..."], "archive": ["..."]}`

// Reflect reads a finished conversation and returns the updated active facts.
func (a *Answerer) Reflect(ctx context.Context, facts []string, transcript string) ([]string, error) {
	core, _, err := a.ReflectFull(ctx, facts, transcript)
	return core, err
}

// ReflectFull returns both the updated active profile facts and any extra archive notes.
func (a *Answerer) ReflectFull(ctx context.Context, facts []string, transcript string) ([]string, []string, error) {
	if strings.TrimSpace(transcript) == "" {
		return nil, nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	known := "(nothing yet)"
	if len(facts) > 0 {
		known = "- " + strings.Join(facts, "\n- ")
	}

	t0 := time.Now()
	rmodel := strings.TrimSpace(a.ReflectModel)
	if rmodel == "" {
		rmodel = a.model()
	}
	body := map[string]any{
		"model": rmodel,
		"store": false,
		"input": []any{
			map[string]any{"role": "system", "content": reflectSystem},
			map[string]any{"role": "user", "content": "Current active household memory:\n" + known +
				"\n\nTranscript of the conversation that just ended:\n" + transcript},
		},
		"max_output_tokens": 8000,
	}
	reffort := strings.TrimSpace(a.ReflectEffort)
	if reffort == "" {
		if strings.HasPrefix(rmodel, "gpt-6.2-sol") {
			reffort = "high"
		} else {
			reffort = "low"
		}
	}
	if reffort != "off" && !strings.HasPrefix(rmodel, "gpt-4.1") && !strings.HasPrefix(rmodel, "gpt-4o") && !strings.Contains(rmodel, "nano") {
		body["reasoning"] = map[string]any{"effort": reffort}
	} else if strings.HasPrefix(rmodel, "gpt-5.4-nano") {
		body["reasoning"] = map[string]any{"effort": "none"}
	}
	out, err := a.post(ctx, body)
	var ae *apiError
	if err != nil && errors.As(err, &ae) && strings.Contains(ae.Message, "reasoning") && body["reasoning"] != nil {
		delete(body, "reasoning")
		out, err = a.post(ctx, body)
	}
	if err != nil {
		return nil, nil, err
	}
	if out.Status == "incomplete" {
		reason := "unknown"
		if out.IncompleteDetails != nil {
			reason = out.IncompleteDetails.Reason
		}
		a.event("reflect_incomplete", "reason", reason, "out_tokens", out.Usage.OutputTokens)
		return nil, nil, fmt.Errorf("reflection was cut off (%s)", reason)
	}

	var text string
	for _, raw := range out.Output {
		var o respOutput
		if json.Unmarshal(raw, &o) != nil || o.Type != "message" {
			continue
		}
		for _, c := range o.Content {
			text += c.Text
		}
	}
	if i := strings.Index(text, "{"); i >= 0 {
		if j := strings.LastIndex(text, "}"); j > i {
			text = text[i : j+1]
		}
	}
	var parsed struct {
		Facts   []string `json:"facts"`
		Archive []string `json:"archive"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		a.event("reflect_unparsable", "chars", len(text))
		return nil, nil, err
	}
	a.event("reflect_done", "ms", time.Since(t0).Milliseconds(),
		"was", len(facts), "now", len(parsed.Facts), "archived", len(parsed.Archive),
		"in_tokens", out.Usage.InputTokens, "out_tokens", out.Usage.OutputTokens)
	return parsed.Facts, parsed.Archive, nil
}
