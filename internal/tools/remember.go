package tools

// Household memory tools: immediate saving (no friction), full Arabic-normalized search across
// active memory + unlimited archive, listing, and confirmation-guarded forgetting.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Memory is the surface of internal/memory.Profile used by the tools package.
type Memory interface {
	Remember(fact string) error
	RememberCategorized(category, fact string) error
	Facts() []string
	Search(query string, limit int) []string
	ForgetMatching(query string) ([]string, error)
}

// RememberThis saves one household fact immediately (no confirmation friction) into both active
// memory and the unlimited long-term archive, then optionally refreshes the active call context.
func RememberThis(m Memory, onChange func()) *Tool {
	return &Tool{
		Name: "remember",
		Description: "Immediately remember a name, family relationship, preference, health/medicine note, " +
			"where an item is stored, or any household fact into active memory and the long-term archive. " +
			"Answers 'احفظي إن', 'تذكري إن', 'لا تنسين إن', 'اسم ...', 'حطيت ... في ...', 'remember that'. " +
			"Runs immediately and repeats back what was saved.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"fact": map[string]any{
					"type":        "string",
					"description": "one clear short sentence in Arabic (or the speaker's language) stating the fact, name, relation, or item location",
				},
				"category": map[string]any{
					"type":        "string",
					"description": "optional category: family_names, preferences, health, locations, dates, or general",
					"enum":        []string{"family_names", "preferences", "health", "locations", "dates", "general"},
				},
			},
			"required": []string{"fact"},
		},
		Access:       Read, // immediate save without "say yes" friction; repeats back what was saved
		DirectReturn: true,
		Timeout:      3 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			fact := strings.TrimSpace(str(args["fact"]))
			if fact == "" {
				return "", fmt.Errorf("ما فيه شي للحفظ")
			}
			cat := strings.TrimSpace(str(args["category"]))
			if err := m.RememberCategorized(cat, fact); err != nil {
				return "", fmt.Errorf("ما قدرت أحفظها: %w", err)
			}
			if onChange != nil {
				onChange()
			}
			return "تم، حفظت: " + oneLine(fact, 200), nil
		},
	}
}

// RecallMemory searches both active memory and the unlimited long-term archive.
func RecallMemory(m Memory) *Tool {
	return &Tool{
		Name: "recall_memory",
		Description: "Search the household's strong memory (both active facts and the unlimited archive) " +
			"for any person's name, family relation, preference, doctor, medicine, where an item was put, " +
			"or past household detail. Answers 'وش اسم...', 'وين حطيت...', 'وش تعرفين عن...', 'متى...', " +
			"'تتذكرين...'. Always check this before saying you don't remember.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "person name, item, topic, or keyword to look up",
				},
			},
			"required": []string{"query"},
		},
		Access:  Read,
		Timeout: 3 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			q := strings.TrimSpace(str(args["query"]))
			if q == "" {
				return "", fmt.Errorf("وش أدور عنه في الذاكرة؟")
			}
			hits := m.Search(q, 6)
			if len(hits) == 0 {
				return "ما لقيت شي محفوظ في الذاكرة عن «" + oneLine(q, 60) + "».", nil
			}
			return "اللي لقيته في الذاكرة: " + strings.Join(hits, "؛ "), nil
		},
	}
}

// WhatYouKnow lists the active facts currently held in household memory.
func WhatYouKnow(m Memory) *Tool {
	return &Tool{
		Name: "what_you_remember",
		Description: "List what is currently remembered in household memory. Answers 'وش تعرفين عننا', " +
			"'وش حافظة عندك', 'وش تتذكرين', 'what do you remember'.",
		Access:  Read,
		Timeout: 2 * time.Second,
		Run: func(ctx context.Context, _ map[string]any) (string, error) {
			all := m.Facts()
			if len(all) == 0 {
				return "ما فيه شي محفوظ في الذاكرة لين الحين.", nil
			}
			shown := all
			if len(shown) > 15 {
				shown = shown[len(shown)-15:]
			}
			return fmt.Sprintf("حافظة %d معلومة للبيت. أحدثها: %s",
				len(all), strings.Join(shown, "؛ ")), nil
		},
	}
}

// ForgetMemory deletes matching facts from active memory and archive, guarded by spoken confirmation.
func ForgetMemory(m Memory, onChange func()) *Tool {
	return &Tool{
		Name: "forget_memory",
		Description: "Delete a specific wrong or outdated fact from household memory. Answers 'امسحي من الذاكرة', " +
			"'انسي إن', 'احذفي معلومة'. Always asks for spoken confirmation first.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "keywords or name identifying the fact to remove",
				},
			},
			"required": []string{"query"},
		},
		Access: Write,
		Describe: func(args map[string]any) string {
			q := strings.TrimSpace(str(args["query"]))
			if q == "" {
				return "أمسح معلومة من الذاكرة؟"
			}
			return "أمسح من الذاكرة كل شي يخص «" + oneLine(q, 60) + "»؟"
		},
		Timeout: 3 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			q := strings.TrimSpace(str(args["query"]))
			if q == "" {
				return "", fmt.Errorf("حددي وش أمسح من الذاكرة")
			}
			removed, err := m.ForgetMatching(q)
			if err != nil {
				return "", err
			}
			if len(removed) == 0 {
				return "ما لقيت شي محفوظ يطابق «" + oneLine(q, 60) + "».", nil
			}
			if onChange != nil {
				onChange()
			}
			return fmt.Sprintf("تم، مسحت %d معلومة من الذاكرة.", len(removed)), nil
		},
	}
}
