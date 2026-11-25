package tools

// Local household reminders & daily medicine/appointment schedule backed by reminders.json.
// Includes an Ambient companion tool (today_reminders) so "وش عندي اليوم؟" answers in one round.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type ReminderItem struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	DueAt     string `json:"due_at,omitempty"` // YYYY-MM-DD HH:MM or HH:MM (for daily)
	Repeat    string `json:"repeat,omitempty"` // none | daily
	Done      bool   `json:"done,omitempty"`
	CreatedAt string `json:"created_at"`
}

// ReminderStore guards concurrent access to reminders.json.
type ReminderStore struct {
	mu   sync.Mutex
	path string
}

func NewReminderStore(path string) *ReminderStore {
	return &ReminderStore{path: path}
}

func (s *ReminderStore) loadLocked() ([]ReminderItem, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var items []ReminderItem
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *ReminderStore) saveLocked(items []ReminderItem) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp%d", s.path, os.Getpid())
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Reminders manages household reminders, appointments, and daily medicines.
func Reminders(store *ReminderStore, tz *time.Location) *Tool {
	if tz == nil {
		tz = time.Local
	}
	return &Tool{
		Name: "reminders",
		Description: "Manage family reminders, appointments, and daily medicine schedules. " +
			"Supports adding immediately, listing today's or all reminders, marking one done, or clearing all (asks confirmation). " +
			"Answers 'ذكريني بـ...', 'عندي موعد...', 'وش مواعيدي اليوم', 'وش عندي اليوم', 'خلصت التذكير'.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type": "string",
					"enum": []string{"add", "list", "done", "clear"},
				},
				"title": map[string]any{
					"type":        "string",
					"description": "what to remember (e.g. 'موعد المستشفى' or 'حبة الضغط')",
				},
				"due_at": map[string]any{
					"type":        "string",
					"description": "date and/or time: 'YYYY-MM-DD HH:MM' for a dated appointment, or 'HH:MM' for today/daily",
				},
				"repeat": map[string]any{
					"type":        "string",
					"description": "none (default) or daily (for everyday medicines/routines)",
					"enum":        []string{"none", "daily"},
				},
				"scope": map[string]any{
					"type":        "string",
					"description": "for list: today or all (default all)",
					"enum":        []string{"today", "all"},
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
			return "أمسح كل التذكيرات والمواعيد المحفوظة؟"
		},
		Timeout: 3 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			action := strings.ToLower(strings.TrimSpace(str(args["action"])))
			now := time.Now().In(tz)

			store.mu.Lock()
			defer store.mu.Unlock()

			items, err := store.loadLocked()
			if err != nil {
				return "", fmt.Errorf("ما قدرت أقرأ التذكيرات: %w", err)
			}

			switch action {
			case "add":
				title := strings.TrimSpace(str(args["title"]))
				if title == "" {
					return "", fmt.Errorf("بوش أذكّرك؟")
				}
				rep := strings.ToLower(strings.TrimSpace(str(args["repeat"])))
				if rep != "daily" {
					rep = "none"
				}
				due := normaliseDueAt(strings.TrimSpace(str(args["due_at"])), rep, now)
				item := ReminderItem{
					ID:        fmt.Sprintf("r-%d", now.UnixNano()),
					Title:     oneLine(title, 140),
					DueAt:     due,
					Repeat:    rep,
					CreatedAt: now.Format(time.RFC3339),
				}
				items = append(items, item)
				if err := store.saveLocked(items); err != nil {
					return "", fmt.Errorf("ما قدرت أحفظ التذكير: %w", err)
				}
				return "تم، سجّلت التذكير: " + formatReminderArabic(item) + ".", nil

			case "list":
				scope := strings.ToLower(strings.TrimSpace(str(args["scope"])))
				today := now.Format("2006-01-02")
				var shown []string
				for _, it := range items {
					if it.Done {
						continue
					}
					if scope == "today" && !isDueToday(it, today) {
						continue
					}
					shown = append(shown, formatReminderArabic(it))
				}
				if len(shown) == 0 {
					if scope == "today" {
						return "ما فيه مواعيد أو تذكيرات مسجلة لليوم.", nil
					}
					return "ما فيه تذكيرات مسجلة الحين.", nil
				}
				return "التذكيرات المسجلة: " + strings.Join(shown, "؛ ") + ".", nil

			case "done":
				title := normToolArabic(str(args["title"]))
				if title == "" {
					return "", fmt.Errorf("أي تذكير خلصتي منه؟")
				}
				matched := ""
				for i := range items {
					if items[i].Done {
						continue
					}
					nt := normToolArabic(items[i].Title)
					if nt == title || strings.Contains(nt, title) || strings.Contains(title, nt) {
						items[i].Done = true
						matched = items[i].Title
						break
					}
				}
				if matched == "" {
					return "ما لقيت تذكير بهالاسم.", nil
				}
				if err := store.saveLocked(items); err != nil {
					return "", fmt.Errorf("ما قدرت أحدث التذكيرات: %w", err)
				}
				return "تم، شطبت تذكير «" + matched + "».", nil

			case "clear":
				if len(items) == 0 {
					return "ما فيه تذكيرات أصلاً.", nil
				}
				if err := store.saveLocked(nil); err != nil {
					return "", fmt.Errorf("ما قدرت أمسح التذكيرات: %w", err)
				}
				return "تم، مسحت كل التذكيرات.", nil

			default:
				return "", fmt.Errorf("أمر غير معروف للتذكيرات: %s", action)
			}
		},
	}
}

// TodayReminders is an Ambient tool that surfaces today's reminders & daily routines in <1 ms.
func TodayReminders(store *ReminderStore, tz *time.Location) *Tool {
	if tz == nil {
		tz = time.Local
	}
	return &Tool{
		Name:        "today_reminders",
		Description: "Today's active household reminders and daily medicines.",
		Access:      Read,
		Ambient:     true,
		Timeout:     2 * time.Second,
		Run: func(ctx context.Context, _ map[string]any) (string, error) {
			today := time.Now().In(tz).Format("2006-01-02")
			store.mu.Lock()
			items, err := store.loadLocked()
			store.mu.Unlock()
			if err != nil || len(items) == 0 {
				return "", nil
			}
			var due []string
			for _, it := range items {
				if it.Done {
					continue
				}
				if isDueToday(it, today) {
					due = append(due, formatReminderArabic(it))
				}
			}
			if len(due) == 0 {
				return "", nil
			}
			return "مواعيد وتذكيرات اليوم: " + strings.Join(due, "؛ ") + ".", nil
		},
	}
}

func normaliseDueAt(raw, repeat string, now time.Time) string {
	if raw == "" {
		return ""
	}
	if repeat == "daily" {
		if len(raw) >= 16 && raw[10] == ' ' {
			return raw[11:16]
		}
		if len(raw) >= 16 && raw[10] == 'T' {
			return raw[11:16]
		}
		return raw
	}
	// If only HH:MM was passed for a one-off reminder, attach today's date.
	if len(raw) == 5 && raw[2] == ':' {
		return now.Format("2006-01-02") + " " + raw
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.In(now.Location()).Format("2006-01-02 15:04")
	}
	return strings.ReplaceAll(raw, "T", " ")
}

func isDueToday(it ReminderItem, today string) bool {
	if it.Repeat == "daily" {
		return true
	}
	if it.DueAt == "" {
		return true
	}
	return strings.HasPrefix(it.DueAt, today)
}

func formatReminderArabic(it ReminderItem) string {
	var parts []string
	parts = append(parts, it.Title)
	if it.Repeat == "daily" {
		if it.DueAt != "" {
			parts = append(parts, "كل يوم الساعة "+formatArabic12h(trimTime(it.DueAt)))
		} else {
			parts = append(parts, "يومياً")
		}
	} else if it.DueAt != "" {
		if len(it.DueAt) >= 16 {
			parts = append(parts, it.DueAt[:10]+" الساعة "+formatArabic12h(it.DueAt[11:16]))
		} else {
			parts = append(parts, it.DueAt)
		}
	}
	return strings.Join(parts, " — ")
}
