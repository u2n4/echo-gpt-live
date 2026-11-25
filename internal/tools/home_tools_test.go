package tools

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/u2n4/echo-gpt-live/internal/memory"
	"github.com/u2n4/echo-gpt-live/internal/tv"
)

type fakeTVClient struct {
	mu      sync.Mutex
	calls   []string
	args    []map[string]any
	respond func(action string, args map[string]any) (tv.Result, error)
}

func (f *fakeTVClient) Do(_ context.Context, action string, args map[string]any, _ time.Duration) (tv.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, action)
	f.args = append(f.args, args)
	fn := f.respond
	f.mu.Unlock()
	if fn != nil {
		return fn(action, args)
	}
	return tv.Result{OK: true, State: tv.State{Power: "on", App: "com.google.android.youtube.tv"}}, nil
}

func TestTVControlKeyRepeatSequenceAndMuteFallback(t *testing.T) {
	activated := 0
	fc := &fakeTVClient{
		respond: func(action string, _ map[string]any) (tv.Result, error) {
			if action == "set_mute" {
				return tv.Result{OK: false, Error: "unknown"}, nil
			}
			return tv.Result{OK: true, State: tv.State{Power: "on", App: "com.google.android.youtube.tv"}}, nil
		},
	}
	tool := TVControl(fc, func() { activated++ })

	// Press center button twice
	out, err := tool.Run(context.Background(), map[string]any{"action": "key", "key": "center", "count": float64(2)})
	if err != nil || !strings.Contains(out, "مرتين") || !strings.Contains(out, "النص") {
		t.Fatalf("center x2: out=%q err=%v", out, err)
	}
	// Play/pause twice
	out, err = tool.Run(context.Background(), map[string]any{"action": "play_pause", "count": float64(2)})
	if err != nil || !strings.Contains(out, "مرتين") {
		t.Fatalf("play_pause x2: out=%q err=%v", out, err)
	}
	// Multi-step sequence
	out, err = tool.Run(context.Background(), map[string]any{
		"action": "sequence",
		"steps": []any{
			map[string]any{"key": "down", "count": float64(2)},
			map[string]any{"key": "select", "count": float64(1)},
		},
	})
	if err != nil || !strings.Contains(out, "نفّذت") {
		t.Fatalf("sequence: out=%q err=%v", out, err)
	}
	// Mute falls back to toggle_mute when box reports unknown
	out, err = tool.Run(context.Background(), map[string]any{"action": "set_mute", "mute": true})
	if err != nil || !strings.Contains(out, "كتمت") {
		t.Fatalf("mute fallback: out=%q err=%v", out, err)
	}
	if strings.Join(fc.calls, ",") != "key,play_pause,sequence,set_mute,toggle_mute" {
		t.Fatalf("calls = %v", fc.calls)
	}
	if activated != 4 {
		t.Fatalf("expected onActive 4 times, got %d", activated)
	}
	// State query works and does not activate remote
	out, err = tool.Run(context.Background(), map[string]any{"action": "state"})
	if err != nil || !strings.Contains(out, "يوتيوب") {
		t.Fatalf("state: out=%q err=%v", out, err)
	}
}

func TestShoppingListImmediateAddAndConfirmedClear(t *testing.T) {
	reg := NewRegistry()
	reg.Add(ShoppingList(filepath.Join(t.TempDir(), "shopping.md")))

	out, err := reg.Call(context.Background(), "shopping_list", `{"action":"add","items":["حليب","خبز","ليمون"]}`, false)
	if err != nil || !strings.Contains(out, "حليب") {
		t.Fatalf("add should not ask confirmation: out=%q err=%v", out, err)
	}
	out, err = reg.Call(context.Background(), "shopping_list", `{"action":"list"}`, false)
	if err != nil || !strings.Contains(out, "3 أغراض") {
		t.Fatalf("list: out=%q err=%v", out, err)
	}
	out, err = reg.Call(context.Background(), "shopping_list", `{"action":"remove","item":"الحليب"}`, false)
	if err != nil || !strings.Contains(out, "حليب") {
		t.Fatalf("remove: out=%q err=%v", out, err)
	}
	// Clear must require confirmation with an Arabic prompt
	_, err = reg.Call(context.Background(), "shopping_list", `{"action":"clear"}`, false)
	var need *ErrNeedsConfirmation
	if !errors.As(err, &need) || !strings.Contains(need.What, "قائمة المقاضي") {
		t.Fatalf("clear must require Arabic confirmation, got %v", err)
	}
	out, err = reg.Call(context.Background(), "shopping_list", `{"action":"clear"}`, true)
	if err != nil || !strings.Contains(out, "مسحت") {
		t.Fatalf("confirmed clear: out=%q err=%v", out, err)
	}
}

func TestRemindersAndAmbientToday(t *testing.T) {
	store := NewReminderStore(filepath.Join(t.TempDir(), "reminders.json"))
	reg := NewRegistry()
	reg.Add(Reminders(store, time.UTC))
	reg.Add(TodayReminders(store, time.UTC))

	out, err := reg.Call(context.Background(), "reminders", `{"action":"add","title":"حبة الضغط","due_at":"20:00","repeat":"daily"}`, false)
	if err != nil || !strings.Contains(out, "حبة الضغط") {
		t.Fatalf("add reminder: out=%q err=%v", out, err)
	}
	amb := reg.Ambient(context.Background())
	if !strings.Contains(amb, "حبة الضغط") {
		t.Fatalf("today_reminders ambient missing daily reminder: %q", amb)
	}
	out, err = reg.Call(context.Background(), "reminders", `{"action":"done","title":"الضغط"}`, false)
	if err != nil || !strings.Contains(out, "شطبت") {
		t.Fatalf("done reminder: out=%q err=%v", out, err)
	}
	if amb2 := reg.Ambient(context.Background()); strings.Contains(amb2, "حبة الضغط") {
		t.Fatalf("completed reminder should not appear in ambient: %q", amb2)
	}
}

func TestKitchenTimerRingsAndCancels(t *testing.T) {
	tm := NewTimerManager()
	rung := make(chan string, 1)
	tm.SetOnRing(func(label string) { rung <- label })
	tool := KitchenTimer(tm)

	out, err := tool.Run(context.Background(), map[string]any{"action": "start", "seconds": float64(0.05), "label": "الرز"})
	if err != nil || !strings.Contains(out, "الرز") {
		t.Fatalf("start timer: out=%q err=%v", out, err)
	}
	select {
	case got := <-rung:
		if got != "الرز" {
			t.Fatalf("rung label = %q, want الرز", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timer did not ring")
	}

	// Start a longer timer and cancel it
	_, _ = tool.Run(context.Background(), map[string]any{"action": "start", "minutes": float64(5), "label": "الفرن"})
	st, err := tool.Run(context.Background(), map[string]any{"action": "status"})
	if err != nil || !strings.Contains(st, "الفرن") {
		t.Fatalf("status: %q %v", st, err)
	}
	tm.CancelAll()
	st, _ = tool.Run(context.Background(), map[string]any{"action": "status"})
	if !strings.Contains(st, "ما فيه مؤقت") {
		t.Fatalf("after CancelAll: %q", st)
	}
}

func TestQuranLookupAdhkarAndAyah(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"data":[{"text":"اللَّهُ لَا إِلَٰهَ إِلَّا هُوَ","numberInSurah":255,"surah":{"name":"سورة البقرة"}},{"text":"الله وحده لا معبود بحق سواه","numberInSurah":255,"surah":{"name":"سورة البقرة"}}]}`))
	}))
	defer srv.Close()

	old := quranBase
	quranBase = srv.URL
	defer func() { quranBase = old }()

	tool := QuranLookup(srv.Client())
	// Offline adhkar (tasbih al-zahra & faraj)
	out, err := tool.Run(context.Background(), map[string]any{"mode": "adhkar", "topic": "tasbih"})
	if err != nil || !strings.Contains(out, "الزهراء") {
		t.Fatalf("tasbih: %q %v", out, err)
	}
	out, err = tool.Run(context.Background(), map[string]any{"mode": "adhkar", "topic": "faraj"})
	if err != nil || !strings.Contains(out, "الحجة") {
		t.Fatalf("faraj: %q %v", out, err)
	}
	// Ayah + Tafsir
	out, err = tool.Run(context.Background(), map[string]any{"mode": "ayah", "reference": "2:255"})
	if err != nil || !strings.Contains(out, "سورة البقرة") || !strings.Contains(out, "التفسير الميسّر") {
		t.Fatalf("ayah: %q %v", out, err)
	}
}

func TestConvertKitchenAndCurrency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":"success","rates":{"SAR":1.0,"USD":0.266667,"AED":0.979333}}`))
	}))
	defer srv.Close()

	old := fxBase
	fxBase = srv.URL
	defer func() { fxBase = old }()

	tool := Convert(srv.Client())
	out, err := tool.Run(context.Background(), map[string]any{"amount": float64(350), "from": "فهرنهايت", "to": "مئوي"})
	if err != nil || !strings.Contains(out, "177") {
		t.Fatalf("350F -> C: %q %v", out, err)
	}
	out, err = tool.Run(context.Background(), map[string]any{"amount": float64(1), "from": "كوب", "to": "غرام", "ingredient": "طحين"})
	if err != nil || !strings.Contains(out, "125") {
		t.Fatalf("1 cup flour -> g: %q %v", out, err)
	}
	out, err = tool.Run(context.Background(), map[string]any{"amount": float64(100), "from": "دولار", "to": "ريال"})
	if err != nil || !strings.Contains(out, "375") {
		t.Fatalf("100 USD -> SAR: %q %v", out, err)
	}
}

func TestStrongFamilyMemoryTools(t *testing.T) {
	mem, err := memory.Open(filepath.Join(t.TempDir(), "memory.md"))
	if err != nil {
		t.Fatal(err)
	}
	refreshed := 0
	reg := NewRegistry()
	reg.Add(RememberThis(mem, func() { refreshed++ }))
	reg.Add(RecallMemory(mem))
	reg.Add(WhatYouKnow(mem))
	reg.Add(ForgetMemory(mem, func() { refreshed++ }))

	// Immediate save without confirmation
	out, err := reg.Call(context.Background(), "remember", `{"fact":"دكتور العيون اسمه الدكتور خالد في الرياض","category":"health"}`, false)
	if err != nil || !strings.Contains(out, "خالد") || refreshed != 1 {
		t.Fatalf("remember: out=%q err=%v refreshed=%d", out, err, refreshed)
	}
	// Recall with Arabic normalization
	out, err = reg.Call(context.Background(), "recall_memory", `{"query":"دكتور العيون"}`, false)
	if err != nil || !strings.Contains(out, "خالد") {
		t.Fatalf("recall: out=%q err=%v", out, err)
	}
	// Forget requires confirmation
	_, err = reg.Call(context.Background(), "forget_memory", `{"query":"خالد"}`, false)
	var need *ErrNeedsConfirmation
	if !errors.As(err, &need) {
		t.Fatalf("forget must require confirmation, got %v", err)
	}
	out, err = reg.Call(context.Background(), "forget_memory", `{"query":"خالد"}`, true)
	if err != nil || !strings.Contains(out, "مسحت") || refreshed != 2 {
		t.Fatalf("confirmed forget: out=%q err=%v refreshed=%d", out, err, refreshed)
	}
}
