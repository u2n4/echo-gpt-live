package tools

// Household shopping list backed by a plain markdown file (shopping.md).
// Adding, listing, and removing items run immediately without confirmation friction;
// clearing the entire list asks for spoken confirmation first.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ShoppingList manages the household grocery / shopping list at path.
func ShoppingList(path string) *Tool {
	var mu sync.Mutex
	return &Tool{
		Name: "shopping_list",
		Description: "Manage the family's shopping / grocery list (قائمة المقاضي). " +
			"Supports adding one or multiple items immediately, reading the list, checking off/removing an item, " +
			"or clearing the whole list (clear asks for spoken confirmation). " +
			"Answers 'ضيفي ... للمقاضي', 'سجلي في المقاضي', 'وش في قائمة المقاضي', 'شطبي ...', 'امسحي المقاضي'.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"description": "add, list, remove, or clear",
					"enum":        []string{"add", "list", "remove", "clear"},
				},
				"item": map[string]any{
					"type":        "string",
					"description": "one item to add or remove",
				},
				"items": map[string]any{
					"type":        "array",
					"description": "multiple items to add or remove in one call",
					"items":       map[string]any{"type": "string"},
				},
			},
			"required": []string{"action"},
		},
		Access:       Write,
		DirectReturn: true,
		ConfirmWhen: func(args map[string]any) bool {
			return strings.EqualFold(strings.TrimSpace(str(args["action"])), "clear")
		},
		Describe: func(args map[string]any) string {
			return "أمسح قائمة المقاضي كلها؟"
		},
		Timeout: 3 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			action := strings.ToLower(strings.TrimSpace(str(args["action"])))
			mu.Lock()
			defer mu.Unlock()

			items, err := readShoppingFile(path)
			if err != nil {
				return "", fmt.Errorf("ما قدرت أقرأ قائمة المقاضي: %w", err)
			}

			switch action {
			case "list":
				if len(items) == 0 {
					return "قائمة المقاضي فاضية الحين.", nil
				}
				return fmt.Sprintf("في قائمة المقاضي %d أغراض: %s.", len(items), strings.Join(items, "، ")), nil

			case "add":
				toAdd := collectStringArgs(args["item"], args["items"])
				if len(toAdd) == 0 {
					return "", fmt.Errorf("وش أضيف لقائمة المقاضي؟")
				}
				existing := map[string]bool{}
				for _, it := range items {
					existing[normToolArabic(it)] = true
				}
				var added []string
				for _, it := range toAdd {
					norm := normToolArabic(it)
					if norm == "" || existing[norm] {
						continue
					}
					existing[norm] = true
					items = append(items, it)
					added = append(added, it)
				}
				if len(added) == 0 {
					return "هذي الأغراض موجودة أصلاً في قائمة المقاضي.", nil
				}
				if err := writeShoppingFile(path, items); err != nil {
					return "", fmt.Errorf("ما قدرت أحفظ قائمة المقاضي: %w", err)
				}
				return "تم، أضفت لقائمة المقاضي: " + strings.Join(added, "، ") + ".", nil

			case "remove":
				toRemove := collectStringArgs(args["item"], args["items"])
				if len(toRemove) == 0 {
					return "", fmt.Errorf("وش أشطب من قائمة المقاضي؟")
				}
				var targets []string
				for _, t := range toRemove {
					if n := normToolArabic(t); n != "" {
						targets = append(targets, n)
					}
				}
				var kept, removed []string
				for _, it := range items {
					ni := normToolArabic(it)
					matched := false
					for _, t := range targets {
						if ni == t || strings.Contains(ni, t) || strings.Contains(t, ni) {
							matched = true
							break
						}
					}
					if matched {
						removed = append(removed, it)
					} else {
						kept = append(kept, it)
					}
				}
				if len(removed) == 0 {
					return "ما لقيت هالغرض في قائمة المقاضي.", nil
				}
				if err := writeShoppingFile(path, kept); err != nil {
					return "", fmt.Errorf("ما قدرت أحدث قائمة المقاضي: %w", err)
				}
				return "تم، شطبت من المقاضي: " + strings.Join(removed, "، ") + ".", nil

			case "clear":
				if len(items) == 0 {
					return "قائمة المقاضي فاضية أصلاً.", nil
				}
				if err := writeShoppingFile(path, nil); err != nil {
					return "", fmt.Errorf("ما قدرت أمسح قائمة المقاضي: %w", err)
				}
				return "تم، مسحت قائمة المقاضي كلها.", nil

			default:
				return "", fmt.Errorf("أمر غير معروف لقائمة المقاضي: %s", action)
			}
		},
	}
}

func readShoppingFile(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "- [ ] ")
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

func writeShoppingFile(path string, items []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# قائمة المقاضي\n")
	b.WriteString("# Last updated " + time.Now().Format(time.RFC3339) + "\n\n")
	for _, it := range items {
		b.WriteString("- " + it + "\n")
	}
	tmp := fmt.Sprintf("%s.tmp%d", path, os.Getpid())
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func collectStringArgs(single, list any) []string {
	var out []string
	if s := strings.TrimSpace(str(single)); s != "" {
		out = append(out, s)
	}
	if arr, ok := list.([]any); ok {
		for _, v := range arr {
			if s := strings.TrimSpace(str(v)); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func normToolArabic(s string) string {
	var b strings.Builder
	lastSpace := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 0x064B && r <= 0x065F, r == 0x0640, r == 0x0670:
			continue
		case r == 'أ' || r == 'إ' || r == 'آ' || r == 'ٱ':
			r = 'ا'
		case r == 'ة':
			r = 'ه'
		case r == 'ى' || r == 'ئ':
			r = 'ي'
		case r == 'ؤ':
			r = 'و'
		case unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r):
			if !lastSpace {
				b.WriteRune(' ')
				lastSpace = true
			}
			continue
		}
		b.WriteRune(r)
		lastSpace = false
	}
	return strings.TrimSpace(b.String())
}
