package openai

// Deciding what was worth remembering, after the call has ended.
//
// This is the half of memory that costs something, and the whole point of the design is WHEN it
// runs: after the hangup, after the billing has stopped, with nobody listening. He is never waiting
// on it. If it takes ten seconds, or fails, or the machine is rebooted in the middle, the only
// consequence is that one conversation did not teach it anything.
//
// It rewrites the WHOLE profile rather than appending to it. Appending is how a memory fills with
// "he asked about the weather" forty times; rewriting lets it merge, correct and drop, which is
// what a person's memory of someone actually does.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// reflectSystem is deliberately strict about what deserves to survive. Everything it keeps is
// injected into every future call, so a fact that is not worth reading a hundred times is not worth
// keeping once.
const reflectSystem = `You keep a small profile of one person, used by a voice assistant in his home.

You are given the profile as it stands and a transcript of the conversation that just ended.
Return the profile as it SHOULD stand now.

Keep a fact only if it will still matter in a month:
- who he is, what he does, who is in the house
- what he is working on, and what he has decided
- how he wants to be spoken to
- preferences, constraints, things he dislikes being asked twice

Never keep:
- anything he asked once out of passing curiosity (the weather, a score, a price)
- what the assistant said
- anything already implied by another fact
- anything about this single conversation that will not outlive it

Correct a fact that the conversation contradicts, and drop one it makes obsolete. Merge duplicates.
Write each fact as one short sentence in the language he speaks. Fewer is better; an empty-handed
conversation is normal and you should then return the profile unchanged.

Answer with JSON only: {"facts": ["...", "..."]}`

// Reflect reads a finished conversation and returns the profile as it should now stand. An error
// means nothing is changed, which is the right failure: a memory that rewrites itself badly is
// worse than one that did not update.
func (a *Answerer) Reflect(ctx context.Context, facts []string, transcript string) ([]string, error) {
	if strings.TrimSpace(transcript) == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	known := "(nothing yet)"
	if len(facts) > 0 {
		known = "- " + strings.Join(facts, "\n- ")
	}

	t0 := time.Now()
	body := map[string]any{
		"model": a.model(),
		"input": []any{
			map[string]any{"role": "system", "content": reflectSystem},
			map[string]any{"role": "user", "content": "The profile as it stands:\n" + known +
				"\n\nThe conversation that just ended:\n" + transcript},
		},
		// Reasoning is billed against this cap and spent before the answer. At the model's default
		// effort the first real call thought for 1,984 tokens of a 2,000 cap and returned no text at
		// all (reflect_unparsable chars=0). Nobody is waiting for this, so the cap is generous, and
		// the effort is low because deciding what to keep about one conversation is not hard.
		"max_output_tokens": 8000,
	}
	if !a.noReasoningField.Load() {
		body["reasoning"] = map[string]any{"effort": "low"}
	}
	out, err := a.post(ctx, body)
	if err != nil && strings.Contains(err.Error(), "reasoning") && !a.noReasoningField.Load() {
		a.noReasoningField.Store(true)
		delete(body, "reasoning")
		out, err = a.post(ctx, body)
	}
	if err != nil {
		return nil, err
	}
	if out.Status == "incomplete" {
		reason := "unknown"
		if out.IncompleteDetails != nil {
			reason = out.IncompleteDetails.Reason
		}
		a.event("reflect_incomplete", "reason", reason, "out_tokens", out.Usage.OutputTokens)
		return nil, fmt.Errorf("reflection was cut off (%s)", reason)
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
	// The model was asked for JSON and mostly obliges, but a fence or a sentence in front of it is
	// not a reason to throw the whole conversation away.
	if i := strings.Index(text, "{"); i >= 0 {
		if j := strings.LastIndex(text, "}"); j > i {
			text = text[i : j+1]
		}
	}
	var parsed struct {
		Facts []string `json:"facts"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		a.event("reflect_unparsable", "chars", len(text))
		return nil, err
	}
	a.event("reflect_done", "ms", time.Since(t0).Milliseconds(),
		"was", len(facts), "now", len(parsed.Facts),
		"in_tokens", out.Usage.InputTokens, "out_tokens", out.Usage.OutputTokens)
	return parsed.Facts, nil
}
