package tools

// "احفظ إن..." - remembering something because he said so.
//
// The after-the-call pass decides most of what is kept, but an explicit instruction must not have
// to wait for it and must not be second-guessed by it. This is a Write, so it is repeated back
// before it lands: a misheard fact that becomes permanent is worse than one that is not saved.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Memory is the small part of the profile this package needs, so the tools package does not depend
// on the whole of it.
type Memory interface {
	Remember(fact string) error
	Facts() []string
}

// RememberThis saves one fact he asked for by name.
func RememberThis(m Memory) *Tool {
	return &Tool{
		Name: "remember",
		Description: "Remember something about him for next time. Answers 'تذكر إني', " +
			"'احفظ عندك', 'لا تنسى', 'remember that I', 'from now on'. Only for things that will " +
			"still matter later, never for a passing question.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"fact": map[string]any{
					"type":        "string",
					"description": "one short sentence, in his own words, written about him",
				},
			},
			"required": []string{"fact"},
		},
		Access:  Write,
		Timeout: 3 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			fact := strings.TrimSpace(str(args["fact"]))
			if fact == "" {
				return "", fmt.Errorf("there is nothing to remember")
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if err := m.Remember(fact); err != nil {
				return "", fmt.Errorf("it could not be saved: %w", err)
			}
			// Said back on purpose: this one is permanent, so a mishearing has to be audible now.
			return "Saved: " + oneLine(fact, 200), nil
		},
	}
}

// WhatYouKnow lets him ask what is being remembered, which is the other half of a memory he is
// allowed to disagree with.
func WhatYouKnow(m Memory) *Tool {
	return &Tool{
		Name: "what_you_remember",
		Description: "List what is remembered about him. Answers 'وش تعرف عني', 'وش تتذكر', " +
			"'what do you know about me', 'what do you remember'.",
		Access:  Read,
		Timeout: 2 * time.Second,
		Run: func(ctx context.Context, _ map[string]any) (string, error) {
			f := m.Facts()
			total := len(f)
			if len(f) == 0 {
				return "Nothing is saved about him yet.", nil
			}
			if len(f) > 12 {
				f = f[len(f)-12:]
			}
			return fmt.Sprintf("%d things are saved. The most recent: %s",
				total, strings.Join(f, "; ")), nil
		},
	}
}
