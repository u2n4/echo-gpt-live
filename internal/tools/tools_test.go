package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The registry is an allowlist, and a name that is not in it does not exist. This is the whole
// safety model, so it is the first test.
func TestUnregisteredToolCannotBeCalled(t *testing.T) {
	r := NewRegistry()
	r.Add(&Tool{Name: "safe", Description: "d", Run: func(context.Context, map[string]any) (string, error) {
		return "ok", nil
	}})
	if _, err := r.Call(context.Background(), "rm_rf", "{}", true); err == nil {
		t.Fatal("an unregistered name must not run")
	}
	if out, err := r.Call(context.Background(), "safe", "", false); err != nil || out != "ok" {
		t.Fatalf("a registered read should run: %q %v", out, err)
	}
}

// A write must not happen because a sentence was spoken. It has to come back asking, and the
// question it comes back with is what the assistant says out loud.
func TestWriteNeedsConfirmationAndSaysWhat(t *testing.T) {
	var ran bool
	r := NewRegistry()
	r.Add(&Tool{
		Name: "notes_write", Description: "d", Access: Write,
		Run: func(context.Context, map[string]any) (string, error) { ran = true; return "done", nil },
	})

	_, err := r.Call(context.Background(), "notes_write", `{"text":"buy milk"}`, false)
	var need *ErrNeedsConfirmation
	if !errors.As(err, &need) {
		t.Fatalf("an unconfirmed write must ask, got %v", err)
	}
	if ran {
		t.Fatal("it ran anyway")
	}
	if !strings.Contains(need.What, "buy milk") {
		t.Fatalf("the question must say what it is about to do, got %q", need.What)
	}

	if out, err := r.Call(context.Background(), "notes_write", `{"text":"buy milk"}`, true); err != nil || out != "done" {
		t.Fatalf("a confirmed write should run: %q %v", out, err)
	}
}

// A tool that hangs must not hang the conversation, because the assistant is silent and billed while
// it waits.
func TestSlowToolIsAbandoned(t *testing.T) {
	r := NewRegistry()
	r.Add(&Tool{
		Name: "slow", Description: "d", Timeout: 50 * time.Millisecond,
		Run: func(ctx context.Context, _ map[string]any) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	})
	t0 := time.Now()
	_, err := r.Call(context.Background(), "slow", "", false)
	if err == nil {
		t.Fatal("a tool past its timeout must fail")
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("it waited %v; the timeout must bound it", d)
	}
}

// A tool that panics must not take the assistant with it.
func TestPanickingToolIsContained(t *testing.T) {
	r := NewRegistry()
	r.Add(&Tool{Name: "bad", Description: "d", Run: func(context.Context, map[string]any) (string, error) {
		panic("boom")
	}})
	if _, err := r.Call(context.Background(), "bad", "", false); err == nil {
		t.Fatal("a panic must come back as an error")
	}
}

// The spend tool is the one he asked for by name, and riyals are how he thinks about money.
func TestSpendSpeaksRiyals(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "spend.json")
	today := time.Now().UTC().Format("2006-01-02")
	// 12 minutes of talking at $0.05 a minute is $0.60, which is 2.25 riyals.
	body, _ := json.Marshal(map[string]any{"day": today, "seconds": 720.0, "sessions": 5})
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := Spend(p, 0.05, 2.0, nil).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2.25", "riyal", "0.60", "5 calls"} {
		if !strings.Contains(out, want) {
			t.Errorf("the answer should mention %q: %s", want, out)
		}
	}
	// $2.00 of limit is 7.50 riyals, so 5.25 should be left.
	if !strings.Contains(out, "5.25") {
		t.Errorf("it should say what is left: %s", out)
	}
}

// A counter from another day is not today's spending, and saying otherwise would be a lie about money.
func TestSpendIgnoresAnotherDay(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spend.json")
	body, _ := json.Marshal(map[string]any{"day": "2000-01-01", "seconds": 99999.0, "sessions": 42})
	_ = os.WriteFile(p, body, 0o600)
	out, err := Spend(p, 0.05, 2.0, nil).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Nothing has been spent today") {
		t.Fatalf("yesterday is not today: %s", out)
	}
	// 99999 seconds at five cents a minute is $83. None of it may appear as today's.
	if strings.Contains(out, "83") || strings.Contains(out, "312") {
		t.Fatalf("an old day's money leaked into today's answer: %s", out)
	}
	if !strings.Contains(out, "2000-01-01") {
		t.Errorf("it should say which day the record is from: %s", out)
	}
}

// The call that is running right now is the money he is asking about. The budget file records
// fifteen seconds when a call opens and the real duration only at close, so a tool that reads only
// the file understates the live call by however long it has been going.
func TestSpendCountsTheCallInProgress(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spend.json")
	body, _ := json.Marshal(map[string]any{
		"day": time.Now().UTC().Format("2006-01-02"), "seconds": 15.0, "sessions": 1,
	})
	_ = os.WriteFile(p, body, 0o600)

	// Ten minutes open at five cents a minute is $0.50, which is 1.88 riyals.
	out, err := Spend(p, 0.05, 2.0, func() time.Duration { return 10 * time.Minute }).
		Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1.88 riyals") || !strings.Contains(out, "0.50 dollars") {
		t.Errorf("the running call is missing from the total: %s", out)
	}
	if !strings.Contains(out, "still running") {
		t.Errorf("it should say the call has not been settled yet: %s", out)
	}
}

// The home-network tool must only ever reach a URL that was configured, never one that was spoken.
func TestHomeNetworkOnlyReachesConfiguredNames(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte(`{"summary":"the printer is idle"}`))
	}))
	defer srv.Close()

	tool := HomeNetwork([]Endpoint{{Name: "printer", URL: srv.URL, Says: JSONField("summary", "%v")}}, srv.Client())

	out, err := tool.Run(context.Background(), map[string]any{"what": "printer"})
	if err != nil || !strings.Contains(out, "idle") {
		t.Fatalf("a configured name should work: %q %v", out, err)
	}
	if !hit {
		t.Fatal("the endpoint was never called")
	}

	if _, err := tool.Run(context.Background(), map[string]any{"what": "http://evil.example/x"}); err == nil {
		t.Fatal("a name that is not configured must be refused, and a URL is never a name")
	}
}

// The schemas the model sees must name every tool and nothing else.
func TestSchemasMatchTheRegistry(t *testing.T) {
	r := NewRegistry()
	r.Add(&Tool{Name: "b", Description: "second", Run: func(context.Context, map[string]any) (string, error) { return "", nil }})
	r.Add(&Tool{Name: "a", Description: "first", Run: func(context.Context, map[string]any) (string, error) { return "", nil }})
	got := r.Schemas()
	if len(got) != 2 {
		t.Fatalf("schemas = %d, want 2", len(got))
	}
	if got[0]["name"] != "a" || got[1]["name"] != "b" {
		t.Fatal("schemas must be in a stable order")
	}
	for _, g := range got {
		if g["type"] != "function" || g["description"] == "" || g["parameters"] == nil {
			t.Fatalf("incomplete schema: %v", g)
		}
	}
}
