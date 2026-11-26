package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseServer serves one streamed Responses call. frames are written in order; a frame with a
// non-zero pause sleeps BEFORE writing, which is how a stall is simulated.
type frame struct {
	pause time.Duration
	data  string
}

func sseServer(t *testing.T, frames []frame) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server cannot flush")
			return
		}
		// Flush the headers immediately: without this the client blocks in Do() and a frame
		// delay would be charged to the header wait instead of to the stream.
		fl.Flush()
		for _, f := range frames {
			if f.pause > 0 {
				time.Sleep(f.pause)
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", f.data); err != nil {
				return // client hung up, which is what a watchdog firing looks like
			}
			fl.Flush()
		}
		// hold the connection open so a stall test is a stall and not an EOF
		time.Sleep(2 * time.Second)
	}))
}

func backendFor(srv *httptest.Server, stall, first time.Duration) *Backend {
	b := NewBackend("test-key", BackendModel)
	b.Stall = stall
	b.FirstEvent = first
	b.MaxTotal = 30 * time.Second
	b.HTTP = srv.Client()
	return b
}

// askAt points the backend at the test server by overriding the transport's target host.
func askAt(ctx context.Context, b *Backend, srv *httptest.Server, kind, q string) (*AskResult, error) {
	orig := b.HTTP.Transport
	b.HTTP.Transport = rewriteHost{base: orig, target: srv.URL}
	defer func() { b.HTTP.Transport = orig }()
	return b.Ask(ctx, kind, "", q, 0)
}

type rewriteHost struct {
	base   http.RoundTripper
	target string
}

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL
	tgt := strings.TrimPrefix(r.target, "http://")
	u.Scheme, u.Host = "http", tgt
	base := r.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

const completedFrame = `{"type":"response.completed","response":{"status":"completed",` +
	`"output_text":["the answer"],` +
	`"usage":{"input_tokens":1000,"output_tokens":2000,` +
	`"input_tokens_details":{"cached_tokens":800},` +
	`"output_tokens_details":{"reasoning_tokens":1900}}}}`

// A long call that keeps reporting progress must NOT be killed. This is the property a
// deadline-per-effort-level gets wrong: total time is not evidence of failure.
func TestSlowButProgressingStreamSucceeds(t *testing.T) {
	var frames []frame
	frames = append(frames, frame{data: `{"type":"response.created"}`})
	// 20 reasoning frames, 100 ms apart: 2 s total, four times the 500 ms stall budget.
	for i := 0; i < 20; i++ {
		frames = append(frames, frame{pause: 100 * time.Millisecond,
			data: `{"type":"response.reasoning_summary_text.delta","delta":"thinking..."}`})
	}
	frames = append(frames, frame{pause: 100 * time.Millisecond,
		data: `{"type":"response.output_text.delta","delta":"the answer"}`})
	frames = append(frames, frame{data: completedFrame})

	srv := sseServer(t, frames)
	defer srv.Close()
	b := backendFor(srv, 500*time.Millisecond, 2*time.Second)

	r, err := askAt(context.Background(), b, srv, "", "q")
	if err != nil {
		t.Fatalf("a progressing stream was killed: %v", err)
	}
	if r.Elapsed < 2*time.Second {
		t.Fatalf("expected the call to really take >2s, got %v", r.Elapsed)
	}
	if r.Text != "the answer" {
		t.Fatalf("text = %q, want %q", r.Text, "the answer")
	}
	if r.ReasoningChars == 0 {
		t.Fatal("reasoning progress was not observed")
	}
	if r.Status != "completed" {
		t.Fatalf("status = %q, want completed", r.Status)
	}
	// usage from the terminal frame, and the Luna rate card
	if r.InputTokens != 1000 || r.OutputTokens != 2000 || r.CachedTokens != 800 || r.ReasoningTokens != 1900 {
		t.Fatalf("usage not applied: %+v", r)
	}
	want := 200*0.20/1e6 + 800*0.02/1e6 + 2000*1.20/1e6
	if r.CostUSD == nil {
		t.Fatal("known model should have a cost estimate")
	}
	if diff := *r.CostUSD - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("cost = %.9f, want %.9f", *r.CostUSD, want)
	}
}

// A stream that starts and then goes quiet is dead, and must be reported as a stall rather than
// as a provider error.
func TestStalledStreamIsDetected(t *testing.T) {
	frames := []frame{
		{data: `{"type":"response.created"}`},
		{data: `{"type":"response.output_text.delta","delta":"partial"}`},
		{pause: 3 * time.Second, data: completedFrame}, // arrives long after the stall budget
	}
	srv := sseServer(t, frames)
	defer srv.Close()
	b := backendFor(srv, 400*time.Millisecond, 2*time.Second)

	start := time.Now()
	_, err := askAt(context.Background(), b, srv, "", "q")
	if err == nil {
		t.Fatal("a stalled stream was accepted")
	}
	if !errors.Is(err, ErrStreamDead) {
		t.Fatalf("error should be ErrStreamDead, got: %v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("watchdog took %v to fire; it should act on the stall budget", el)
	}
}

// Silence from the very start is a different failure and gets its own budget and message.
func TestNoActivityAtAllIsDetected(t *testing.T) {
	frames := []frame{{pause: 3 * time.Second, data: `{"type":"response.created"}`}}
	srv := sseServer(t, frames)
	defer srv.Close()
	b := backendFor(srv, 5*time.Second, 300*time.Millisecond)

	_, err := askAt(context.Background(), b, srv, "", "q")
	if err == nil {
		t.Fatal("a silent stream was accepted")
	}
	if !errors.Is(err, ErrStreamDead) {
		t.Fatalf("error should be ErrStreamDead, got: %v", err)
	}
}

// Spending the whole budget on invisible reasoning and emitting no answer must be reported as a
// starved budget, not as a dead endpoint.
func TestEmptyAnswerAfterReasoningIsReportedAsStarved(t *testing.T) {
	frames := []frame{
		{data: `{"type":"response.created"}`},
		{data: `{"type":"response.reasoning_summary_text.delta","delta":"thinking hard"}`},
		{data: `{"type":"response.incomplete","response":{"status":"incomplete",` +
			`"incomplete_details":{"reason":"max_output_tokens"},` +
			`"usage":{"input_tokens":10,"output_tokens":2048,` +
			`"input_tokens_details":{"cached_tokens":0},` +
			`"output_tokens_details":{"reasoning_tokens":2048}}}}`},
	}
	srv := sseServer(t, frames)
	defer srv.Close()
	b := backendFor(srv, time.Second, 2*time.Second)

	r, err := askAt(context.Background(), b, srv, "", "q")
	if err == nil {
		t.Fatal("an empty answer was accepted as success")
	}
	if !errors.Is(err, ErrEmptyAnswer) {
		t.Fatalf("error should be ErrEmptyAnswer, got: %v", err)
	}
	if !r.Truncated {
		t.Fatal("incomplete_details should have marked the result truncated")
	}
	if r.ReasoningTokens != 2048 {
		t.Fatalf("reasoning tokens = %d, want 2048", r.ReasoningTokens)
	}
}

// A reasoning summary must never be spoken as the answer.
func TestReasoningOutputIsNotUsedAsAnswer(t *testing.T) {
	frames := []frame{
		{data: `{"type":"response.created"}`},
		{data: `{"type":"response.completed","response":{"status":"completed","output":[` +
			`{"type":"reasoning","content":[{"type":"summary_text","text":"internal thoughts"}]},` +
			`{"type":"message","content":[{"type":"output_text","text":"spoken answer"}]}]}}`},
	}
	srv := sseServer(t, frames)
	defer srv.Close()
	b := backendFor(srv, time.Second, 2*time.Second)

	r, err := askAt(context.Background(), b, srv, "", "q")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Text != "spoken answer" {
		t.Fatalf("text = %q; a reasoning summary must not leak into the answer", r.Text)
	}
}

// Effort selection is a function of the request kind, not a pinned constant.
func TestBackendEffortForKinds(t *testing.T) {
	for kind, want := range map[string]string{
		"tool": "none", "quick": "low", "hard": "xhigh", "": BackendEffortDefault, "unknown": BackendEffortDefault,
	} {
		if got := BackendEffortFor(kind); got != want {
			t.Errorf("BackendEffortFor(%q) = %q, want %q", kind, got, want)
		}
	}
	for _, e := range []string{"high", "xhigh", "max"} {
		if !AnnounceWait(e) {
			t.Errorf("AnnounceWait(%q) should be true: the user needs telling", e)
		}
	}
	for _, e := range []string{"none", "low", "medium"} {
		if AnnounceWait(e) {
			t.Errorf("AnnounceWait(%q) should be false", e)
		}
	}
}

// GPT-Live bills 15 s at initialization, credited rather than added, so the floor is 15 s.
func TestLiveCostFloorIsTheInitCharge(t *testing.T) {
	if got := EstimateLiveCostUSD(2 * time.Second); got != 15.0/60.0*0.05 {
		t.Fatalf("a 2s session should still bill the 15s floor, got %.6f", got)
	}
	if got, want := EstimateLiveCostUSD(60*time.Second), 0.05; got != want {
		t.Fatalf("a 60s session = %.6f, want %.6f", got, want)
	}
}

// The idle gate must fire once, and Touch must keep it from firing.
func TestIdleGate(t *testing.T) {
	fired := make(chan time.Duration, 4)
	g := NewIdleGate(1200*time.Millisecond, func(d time.Duration) { fired <- d })
	// keep it alive across two would-be firings
	for i := 0; i < 8; i++ {
		time.Sleep(200 * time.Millisecond)
		g.Touch()
	}
	select {
	case d := <-fired:
		t.Fatalf("gate fired while being touched (idle %v)", d)
	default:
	}
	select {
	case <-fired:
	case <-time.After(4 * time.Second):
		t.Fatal("gate never fired after activity stopped")
	}
	select {
	case <-fired:
		t.Fatal("gate fired more than once")
	case <-time.After(1500 * time.Millisecond):
	}
	g.Stop()
}
