package tools

// In-call kitchen timer for cooking and household timing.
// Timers ring aloud via the bridge's Sideband callback while the call is open, and are cancelled
// automatically when the call ends so nothing ever rings into a different session.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type activeTimer struct {
	label string
	ends  time.Time
	t     *time.Timer
}

// TimerManager tracks active in-call kitchen timers.
type TimerManager struct {
	mu     sync.Mutex
	onRing func(label string)
	timers map[string]*activeTimer
	seq    int
}

func NewTimerManager() *TimerManager {
	return &TimerManager{timers: map[string]*activeTimer{}}
}

// SetOnRing installs the callback invoked when a timer fires during the current call.
func (tm *TimerManager) SetOnRing(fn func(label string)) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.onRing = fn
}

// CancelAll stops every running timer (called when a call ends).
func (tm *TimerManager) CancelAll() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	for k, at := range tm.timers {
		at.t.Stop()
		delete(tm.timers, k)
	}
	tm.onRing = nil
}

// KitchenTimer returns the kitchen_timer tool bound to tm.
func KitchenTimer(tm *TimerManager) *Tool {
	return &Tool{
		Name: "kitchen_timer",
		Description: "Set, check, or cancel a cooking/kitchen timer during the current call. " +
			"Rings out loud through the speaker when time is up (active only while the call stays open). " +
			"Answers 'مؤقت 10 دقائق للرز', 'حطي مؤقت 5 دقايق', 'كم باقي على المؤقت', 'الغي المؤقت'.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type": "string",
					"enum": []string{"start", "status", "cancel"},
				},
				"minutes": map[string]any{
					"type":        "number",
					"description": "duration in minutes (can be fractional like 0.5)",
				},
				"seconds": map[string]any{
					"type":        "number",
					"description": "additional duration in seconds",
				},
				"label": map[string]any{
					"type":        "string",
					"description": "optional name for the timer (e.g. 'الرز', 'الفرن', 'الشاي')",
				},
			},
			"required": []string{"action"},
		},
		Access:       Read, // immediate start/check/cancel without confirmation friction
		DirectReturn: true,
		Timeout:      3 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			action := strings.ToLower(strings.TrimSpace(str(args["action"])))
			label := strings.TrimSpace(str(args["label"]))

			switch action {
			case "start":
				mins := floatArg(args["minutes"])
				secs := floatArg(args["seconds"])
				dur := time.Duration(mins*float64(time.Minute)) + time.Duration(secs*float64(time.Second))
				if dur <= 0 {
					return "", fmt.Errorf("حددي مدة المؤقت بالدقائق أو الثواني")
				}
				if dur > 90*time.Minute {
					return "", fmt.Errorf("المؤقت داخل المكالمة حده 90 دقيقة؛ للمؤقت الطويل بعد إغلاق المكالمة قولي لأليكسا مباشرة Alexa set a timer")
				}
				if label == "" {
					label = "الطبخ"
				}
				tm.start(label, dur)
				return fmt.Sprintf("تم، شغّلت مؤقت %s لمدة %s (ينبّهك طول ما المكالمة مفتوحة).",
					label, arabicTimerDur(dur)), nil

			case "status":
				return tm.status(label), nil

			case "cancel":
				n := tm.cancel(label)
				if n == 0 {
					return "ما فيه مؤقت شغّال الحين.", nil
				}
				if label != "" {
					return "تم، ألغيت مؤقت " + label + ".", nil
				}
				return "تم، ألغيت المؤقت.", nil

			default:
				return "", fmt.Errorf("أمر غير معروف للمؤقت: %s", action)
			}
		},
	}
}

func (tm *TimerManager) start(label string, dur time.Duration) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	key := normToolArabic(label)
	if old, ok := tm.timers[key]; ok {
		old.t.Stop()
	}
	tm.seq++
	id := tm.seq
	at := &activeTimer{label: label, ends: time.Now().Add(dur)}
	at.t = time.AfterFunc(dur, func() {
		tm.mu.Lock()
		cur, ok := tm.timers[key]
		if !ok || cur != at {
			tm.mu.Unlock()
			return
		}
		delete(tm.timers, key)
		ring := tm.onRing
		tm.mu.Unlock()
		_ = id
		if ring != nil {
			ring(label)
		}
	})
	tm.timers[key] = at
}

func (tm *TimerManager) status(label string) string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if len(tm.timers) == 0 {
		return "ما فيه مؤقت شغّال الحين."
	}
	if label != "" {
		if at, ok := tm.timers[normToolArabic(label)]; ok {
			rem := time.Until(at.ends)
			if rem < 0 {
				rem = 0
			}
			return fmt.Sprintf("باقي على مؤقت %s: %s.", at.label, arabicTimerDur(rem))
		}
	}
	var list []*activeTimer
	for _, at := range tm.timers {
		list = append(list, at)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ends.Before(list[j].ends) })
	var parts []string
	for _, at := range list {
		rem := time.Until(at.ends)
		if rem < 0 {
			rem = 0
		}
		parts = append(parts, fmt.Sprintf("مؤقت %s باقي عليه %s", at.label, arabicTimerDur(rem)))
	}
	return strings.Join(parts, "، ") + "."
}

func (tm *TimerManager) cancel(label string) int {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if label != "" {
		key := normToolArabic(label)
		if at, ok := tm.timers[key]; ok {
			at.t.Stop()
			delete(tm.timers, key)
			return 1
		}
		return 0
	}
	n := len(tm.timers)
	for k, at := range tm.timers {
		at.t.Stop()
		delete(tm.timers, k)
	}
	return n
}

func floatArg(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	default:
		return 0
	}
}

func arabicTimerDur(d time.Duration) string {
	secs := int(d.Round(time.Second).Seconds())
	if secs <= 0 {
		return "لحظات"
	}
	m, s := secs/60, secs%60
	switch {
	case m == 0:
		return fmt.Sprintf("%d ثانية", s)
	case s == 0:
		if m == 1 {
			return "دقيقة"
		}
		if m == 2 {
			return "دقيقتين"
		}
		return fmt.Sprintf("%d دقائق", m)
	default:
		return fmt.Sprintf("%d دقيقة و%d ثانية", m, s)
	}
}
