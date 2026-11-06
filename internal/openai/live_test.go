package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A short HTTP body can deliver all its JSON before reporting a framing error.
type failedResponseBody struct {
	io.Reader
	cancel context.CancelFunc
}

func (b failedResponseBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		b.cancel()
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func TestLiveBodyReadFailureClosesRecoveredSession(t *testing.T) {
	for _, fork := range []bool{false, true} {
		for _, raw := range []string{
			`{"session":{"id":"live_recovered"},"transport":{"sdp":"v=0\r\n"}}`,
			`{"session":{"id":"live_recovered"},"transport":`,
		} {
			for _, cleanupStatus := range []int{200, 401, 503} {
				t.Run(fmt.Sprintf("fork=%v/body=%s/cleanup=%d", fork, raw, cleanupStatus), func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					c := NewLive("fake-key", LiveModel)
					var err error
					c.Ledger, err = NewLedger(filepath.Join(t.TempDir(), "open"), nil)
					if err != nil {
						t.Fatal(err)
					}
					c.Budget, err = NewBudget(filepath.Join(t.TempDir(), "spend.json"), 1, nil)
					if err != nil {
						t.Fatal(err)
					}
					creates, hangups := 0, 0
					c.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						if strings.HasSuffix(r.URL.Path, "/live_recovered/hangup") {
							hangups++
							deadline, ok := r.Context().Deadline()
							if ctx.Err() == nil || r.Context().Err() != nil || !ok || time.Until(deadline) > 20*time.Second {
								t.Fatal("cleanup must have an independent, bounded context")
							}
							if ids := c.Ledger.Open(); len(ids) != 1 || ids[0] != "live_recovered" {
								t.Fatalf("session not persisted before cleanup: %v", ids)
							}
							return &http.Response{StatusCode: cleanupStatus, Body: io.NopCloser(strings.NewReader("{}"))}, nil
						}
						creates++
						return &http.Response{StatusCode: 201, Body: io.NopCloser(failedResponseBody{strings.NewReader(raw), cancel})}, nil
					})}
					if fork {
						_, err = c.Fork(ctx, "live_source", "offer")
					} else {
						_, err = c.CreateSession(ctx, "offer", LiveSessionConfig{})
					}
					wantHangups := 1
					if cleanupStatus == 503 {
						wantHangups = 3
					}
					if !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, ErrSessionCreated) || creates != 1 || hangups != wantHangups {
						t.Fatalf("err=%v creates=%d hangups=%d", err, creates, hangups)
					}
					wantOpen := 0
					if cleanupStatus >= 300 {
						wantOpen = 1
					}
					if ids := c.Ledger.Open(); len(ids) != wantOpen {
						t.Fatalf("ledger=%v want %d entries", ids, wantOpen)
					}
					spent, sessions := c.Budget.Report()
					if sessions != 1 || spent != EstimateLiveCostUSD(0) {
						t.Fatalf("spent=%v sessions=%d", spent, sessions)
					}
				})
			}
		}
	}
}

func TestRealtimeBodyReadFailureClosesLocation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New("fake-key", "fake-model")
	hangups := 0
	c.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/rtc_recovered/hangup") {
			hangups++
			deadline, ok := r.Context().Deadline()
			if ctx.Err() == nil || r.Context().Err() != nil || !ok || time.Until(deadline) > 20*time.Second {
				t.Fatal("cleanup must have an independent, bounded context")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		}
		return &http.Response{StatusCode: 201, Header: http.Header{"Location": {"/v1/realtime/calls/rtc_recovered"}},
			Body: io.NopCloser(failedResponseBody{strings.NewReader("v=0\r\n"), cancel})}, nil
	})}
	_, err := c.CreateCall(ctx, "offer")
	if !errors.Is(err, io.ErrUnexpectedEOF) || hangups != 1 {
		t.Fatalf("err=%v hangups=%d", err, hangups)
	}
}

// liveCreateServer answers the first request with the given error body and every later one with a
// valid session, recording the session object of every request so the retry can be inspected.
func liveCreateServer(t *testing.T, firstErr string) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var seen []map[string]any
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Session map[string]any `json:"session"`
		}
		_ = json.Unmarshal(raw, &body)
		seen = append(seen, body.Session)
		n++
		if n == 1 && firstErr != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(firstErr))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"session":{"id":"live_ok"},"transport":{"sdp":"v=0\r\n"}}`))
	}))
	return srv, &seen
}

func clientAt(srv *httptest.Server) *LiveClient {
	c := NewLive("test-key", LiveModel)
	c.HTTP = srv.Client()
	c.HTTP.Transport = rewriteHost{base: srv.Client().Transport, target: srv.URL}
	return c
}

// The measured refusal from gpt-live-1. An audio tuning parameter must degrade, never take the
// assistant down: the session has to come up on the second attempt without audio.input.
func TestCreateSessionDropsAudioInputWhenRejected(t *testing.T) {
	const rejected = `{"error":{"code":"unknown_parameter","message":"Unknown parameter: 'session.audio.input'."}}`
	srv, seen := liveCreateServer(t, rejected)
	defer srv.Close()
	c := clientAt(srv)

	cfg := LiveSessionConfig{Model: LiveModel, Audio: map[string]any{
		"output": map[string]any{"voice": "cedar"},
		"input":  map[string]any{"noise_reduction": map[string]any{"type": "far_field"}},
	}}
	sess, err := c.CreateSession(context.Background(), "v=0\r\n", cfg)
	if err != nil {
		t.Fatalf("the session must survive a rejected audio parameter: %v", err)
	}
	if sess.ID != "live_ok" {
		t.Fatalf("session id = %q", sess.ID)
	}
	if len(*seen) != 2 {
		t.Fatalf("attempts = %d, want 2", len(*seen))
	}
	first, _ := (*seen)[0]["audio"].(map[string]any)
	if _, ok := first["input"]; !ok {
		t.Fatal("the first attempt should have carried audio.input")
	}
	second, _ := (*seen)[1]["audio"].(map[string]any)
	if _, ok := second["input"]; ok {
		t.Fatal("the retry must not resend the parameter that was just refused")
	}
	if _, ok := second["output"]; !ok {
		t.Fatal("dropping audio.input must not take the voice with it")
	}

	// The caller reuses one config for every session, so the refusal must not have edited it.
	if _, ok := cfg.Audio["input"]; !ok {
		t.Fatal("the caller's config was mutated; one session's failure must not rewrite the next")
	}

	// A later session should skip the doomed parameter entirely rather than pay another round trip.
	if _, err := c.CreateSession(context.Background(), "v=0\r\n", cfg); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 3 {
		t.Fatalf("the second session should have taken one attempt, total = %d", len(*seen))
	}
	third, _ := (*seen)[2]["audio"].(map[string]any)
	if _, ok := third["input"]; ok {
		t.Fatal("audio.input was sent again after it was already refused")
	}
}

// An unrelated 400 must not be swallowed by the audio retry.
func TestCreateSessionDoesNotRetryUnrelatedErrors(t *testing.T) {
	const other = `{"error":{"code":"unknown_parameter","message":"Unknown parameter: 'session.nonsense'."}}`
	srv, seen := liveCreateServer(t, other)
	defer srv.Close()
	c := clientAt(srv)

	_, err := c.CreateSession(context.Background(), "v=0\r\n", LiveSessionConfig{Model: LiveModel})
	if err == nil {
		t.Fatal("an unrelated rejection must be reported, not retried away")
	}
	if len(*seen) != 1 {
		t.Fatalf("attempts = %d, want 1", len(*seen))
	}
}

// audio.format is negotiated by SDP on WebRTC and must never be sent, even if a caller sets it.
func TestCreateSessionStripsAudioFormat(t *testing.T) {
	srv, seen := liveCreateServer(t, "")
	defer srv.Close()
	c := clientAt(srv)

	cfg := LiveSessionConfig{Model: LiveModel, Audio: map[string]any{
		"format": map[string]any{"type": "audio/pcm"},
		"output": map[string]any{"voice": "cedar"},
	}}
	if _, err := c.CreateSession(context.Background(), "v=0\r\n", cfg); err != nil {
		t.Fatal(err)
	}
	sent, _ := (*seen)[0]["audio"].(map[string]any)
	if _, ok := sent["format"]; ok {
		t.Fatal("audio.format must be stripped: WebRTC negotiates it through SDP")
	}
	if _, ok := cfg.Audio["format"]; !ok {
		t.Fatal("stripping must happen on a copy, not on the caller's config")
	}
}

// A session that was created, could not be charged to the day's counter, and then could not be
// hung up either used to exist only in one log line. Now it goes on the ledger, where the next
// sweep finds it. The counter is made unwritable by taking the directory's write bit after the
// lock file exists, so the admission check still passes and only the write after creation fails.
func TestASessionTheCounterCannotRecordAndTheHangupCannotCloseGoesOnTheLedger(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a read-only directory does not make a write fail on Windows")
	}
	dir := t.TempDir()
	budgetDir := filepath.Join(dir, "budget")
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/hangup") {
			hits++
			w.WriteHeader(http.StatusUnauthorized) // not retried: the failure is immediate
			return
		}
		// The counter becomes unwritable BETWEEN the admission and the record: the admission
		// (Check) writes its hold, so the directory has to be writable until the session exists.
		if err := os.Chmod(budgetDir, 0o500); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"session":{"id":"live_lost"},"transport":{"sdp":"v=0\r\n"}}`))
	}))
	defer srv.Close()
	c := clientAt(srv)

	if err := os.MkdirAll(budgetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := NewBudget(filepath.Join(budgetDir, "spend.json"), 5.0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(budgetDir, 0o700)
	c.Budget = b
	l, err := NewLedger(filepath.Join(dir, "ledger", "open"), nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Ledger = l

	_, err = c.CreateSession(context.Background(), "v=0\r\n", LiveSessionConfig{Model: LiveModel})
	if err == nil {
		t.Fatal("a session the counter cannot record must be reported as a failure")
	}
	if hits == 0 {
		t.Fatal("the hangup was never attempted")
	}
	if got := l.Open(); len(got) != 1 || got[0] != "live_lost" {
		t.Fatalf("the lost session must be on the ledger for the sweep, got %v", got)
	}
}
