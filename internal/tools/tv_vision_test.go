package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/u2n4/echo-gpt-live/internal/tv"
)

type mockTVVision struct {
	mu         sync.Mutex
	actions    []string
	args       []map[string]any
	app        string
	input      string
	unverified bool
}

func (m *mockTVVision) Do(_ context.Context, action string, args map[string]any, _ time.Duration) (tv.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.actions = append(m.actions, action)
	m.args = append(m.args, args)
	switch action {
	case "screenshot":
		return tv.Result{OK: true, Input: m.input, InputVerified: !m.unverified && m.input != "", ImageB64: "/9j/4AAQSkZJRg=="}, nil
	case "state":
		return tv.Result{OK: true, State: tv.State{Power: "on", Connected: true, App: m.app}}, nil
	default:
		return tv.Result{OK: true}, nil
	}
}

func TestTVVisionYouTubeFastDeepLink(t *testing.T) {
	yt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"videoRenderer":{"videoId":"abc123XYZ89","title":{"runs":[{"text":"سبايدر مان الجديد"}]}}}`))
	}))
	defer yt.Close()
	oldYT := ytSearchBase
	ytSearchBase = yt.URL
	defer func() { ytSearchBase = oldYT }()

	mock := &mockTVVision{}
	active := false
	tool := TVVision(TVVisionConfig{
		Client:   mock,
		HTTP:     yt.Client(),
		OnActive: func() { active = true },
	})
	out, err := tool.Run(context.Background(), map[string]any{
		"mode":  "search_play",
		"app":   "youtube",
		"goal":  "شغلي سبايدر مان في يوتيوب",
		"query": "سبايدر مان الجديد",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !active || !strings.Contains(out, "سبايدر مان") {
		t.Fatalf("got out=%q active=%v", out, active)
	}
	if len(mock.actions) != 1 || mock.actions[0] != "launch_url" || mock.args[0]["url"] != "https://www.youtube.com/watch?v=abc123XYZ89" {
		t.Fatalf("unexpected tv actions: %v %v", mock.actions, mock.args)
	}
}

func TestTVVisionComputerUseLoopTypesAndSelects(t *testing.T) {
	step := 0
	oai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		step++
		var reply string
		switch step {
		case 1:
			reply = `{"done":false,"action":"text","text":"Spider-Man","say":"كتبت سبايدر مان"}`
		default:
			reply = `{"done":true,"action":"done","say":"تم، شغّلت لك فيلم سبايدر مان في نتفلكس."}`
		}
		json.NewEncoder(w).Encode(map[string]any{
			"output": []any{
				map[string]any{
					"type": "message",
					"content": []any{
						map[string]any{"type": "output_text", "text": reply},
					},
				},
			},
		})
	}))
	defer oai.Close()
	oldURL := tvVisionURL
	tvVisionURL = oai.URL
	defer func() { tvVisionURL = oldURL }()

	mock := &mockTVVision{input: "hdmi3", app: "com.netflix.ninja"}
	tool := TVVision(TVVisionConfig{
		Client: mock,
		APIKey: "test-key",
		HTTP:   oai.Client(),
	})
	out, err := tool.Run(context.Background(), map[string]any{
		"mode":  "navigate",
		"app":   "netflix",
		"goal":  "دوري على فيلم سبايدر مان وشغليه",
		"query": "Spider-Man",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(out, "سبايدر مان") {
		t.Fatalf("unexpected out: %q", out)
	}
	// state -> screenshot -> backspace x2 -> text("Spider-Man") -> screenshot -> done
	want := "state,screenshot,key,key,text,screenshot"
	if got := strings.Join(mock.actions, ","); got != want {
		t.Fatalf("actions=%q want=%q", got, want)
	}
}

func TestTVControlSwitchInput(t *testing.T) {
	mock := &mockTVVision{}
	tool := TVControl(mock, nil)
	out, err := tool.Run(context.Background(), map[string]any{
		"action": "switch_input",
		"input":  "cameras",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "الكاميرات") {
		t.Fatalf("out=%q", out)
	}
	if len(mock.actions) != 1 || mock.actions[0] != "switch_input" {
		t.Fatalf("actions=%v", mock.actions)
	}
}

func TestTVVisionRejectsPrematurePermissionQuestion(t *testing.T) {
	step := 0
	oai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		step++
		var reply string
		switch step {
		case 1:
			reply = `{"done":true,"action":"done","say":"لقيت قناة بندريتا بس ما شغلت شي للحين، تبيني أشغل أحدث فيديو؟"}`
		default:
			reply = `{"done":true,"action":"sequence","steps":[{"key":"select","count":1}],"say":"تم، شغّلت لك أحدث فيديو لبندريتا."}`
		}
		json.NewEncoder(w).Encode(map[string]any{
			"output": []any{
				map[string]any{
					"type": "message",
					"content": []any{
						map[string]any{"type": "output_text", "text": reply},
					},
				},
			},
		})
	}))
	defer oai.Close()
	oldURL := tvVisionURL
	tvVisionURL = oai.URL
	defer func() { tvVisionURL = oldURL }()

	mock := &mockTVVision{input: "hdmi3", app: "com.google.android.youtube.tv"}
	tool := TVVision(TVVisionConfig{
		Client: mock,
		APIKey: "test-key",
		HTTP:   oai.Client(),
	})
	out, err := tool.Run(context.Background(), map[string]any{
		"mode": "navigate",
		"app":  "youtube",
		"goal": "شغلي أحدث فيديو لبندريتا",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if step != 2 {
		t.Fatalf("expected loop to reject premature question and continue to step 2, got step=%d out=%q", step, out)
	}
	if !strings.Contains(out, "شغّلت") {
		t.Fatalf("unexpected out: %q", out)
	}
}

func TestAppDescriptionRejectsUnconfirmedInput(t *testing.T) {
	for _, input := range []string{"hdmi2", "unknown", "", "hdmi3"} {
		mock := &mockTVVision{input: input, unverified: input == "hdmi3"}
		// A rejected frame must return before the HTTP client is used.
		_, err := describeTVScreen(context.Background(), mock, nil, "", "", "", "", "", "netflix")
		if err == nil {
			t.Fatalf("accepted input %q", input)
		}
	}
}
