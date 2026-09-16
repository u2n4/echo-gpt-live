package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A session id must survive on disk the instant it is created, because the very next thing that
// happens might be a SIGKILL.
// Two Ledger objects over one file stand in for two processes (the live bridge and a test bridge
// on the default path): each has its own mutex, so only the flock orders them. Without it, a
// Remove that read the file before an Add and rewrote it after publishes a ledger missing the
// session that was just opened.
func TestLedgerSurvivesAnotherProcessRemovingWhileThisOneAdds(t *testing.T) {
	p := filepath.Join(t.TempDir(), "open-sessions")
	a, err := NewLedger(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewLedger(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	const n = 60
	for i := 0; i < n; i++ {
		if err := b.Add(fmt.Sprintf("live_b%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			b.Remove(fmt.Sprintf("live_b%02d", i))
		}
	}()
	for i := 0; i < n; i++ {
		if err := a.Add(fmt.Sprintf("live_a%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	<-done
	open := map[string]bool{}
	for _, id := range a.Open() {
		open[id] = true
	}
	for i := 0; i < n; i++ {
		if !open[fmt.Sprintf("live_a%02d", i)] {
			t.Fatalf("live_a%02d was added by one process and lost by the other's rewrite; open = %d ids", i, len(open))
		}
		if open[fmt.Sprintf("live_b%02d", i)] {
			t.Fatalf("live_b%02d was removed and came back", i)
		}
	}
}

func TestLedgerRecordsAndRemoves(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "open-sessions")
	l, err := NewLedger(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Add("live_a")
	l.Add("live_b")
	l.Add("live_a") // duplicates must not multiply
	if got := l.Open(); len(got) != 2 || got[0] != "live_a" || got[1] != "live_b" {
		t.Fatalf("open = %v, want [live_a live_b]", got)
	}
	// a fresh Ledger over the same file must see the same thing: this is the crash path
	l2, err := NewLedger(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := l2.Open(); len(got) != 2 {
		t.Fatalf("a new process saw %v; the record must survive the old one", got)
	}
	l.Remove("live_a")
	if got := l.Open(); len(got) != 1 || got[0] != "live_b" {
		t.Fatalf("after remove, open = %v, want [live_b]", got)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("ledger file should still exist: %v", err)
	}
}

// hangupServer counts hangup calls and answers with the given statuses in order.
func hangupServer(t *testing.T, statuses []int, hit *atomic.Int64, seen *[]string) *httptest.Server {
	t.Helper()
	var i atomic.Int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(i.Add(1)) - 1
		hit.Add(1)
		if seen != nil {
			*seen = append(*seen, r.URL.Path)
		}
		code := http.StatusOK
		if n < len(statuses) {
			code = statuses[n]
		}
		w.WriteHeader(code)
		switch code {
		case 401:
			_, _ = w.Write([]byte(`{"error":{"code":"token_invalidated","message":"Your API key has been invalidated."}}`))
		case 404:
			_, _ = w.Write([]byte(`{"error":{"code":"session_not_found","message":"gone"}}`))
		}
	}))
}

func liveClientAt(srv *httptest.Server) *LiveClient {
	c := NewLive("test-key", LiveModel)
	c.HTTP = srv.Client()
	c.HTTP.Transport = rewriteHost{base: srv.Client().Transport, target: srv.URL}
	return c
}

// A hangup that fails once must be retried, because a failed hangup leaves money running.
func TestHangupRetriesTransientFailure(t *testing.T) {
	var hits atomic.Int64
	srv := hangupServer(t, []int{500, 200}, &hits, nil)
	defer srv.Close()
	c := liveClientAt(srv)

	if err := c.HangupRetry(context.Background(), "live_x", 3); err != nil {
		t.Fatalf("retry should have succeeded on the second attempt: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", hits.Load())
	}
}

// An auth failure will not improve with retries, so it must stop after one attempt and say what
// the consequence is, rather than hammering the API while the clock runs.
func TestHangupDoesNotRetryAuthFailure(t *testing.T) {
	var hits atomic.Int64
	var events []string
	srv := hangupServer(t, []int{401, 401, 401}, &hits, nil)
	defer srv.Close()
	c := liveClientAt(srv)
	c.OnEvent = func(ev string, kv ...any) { events = append(events, ev) }

	err := c.HangupRetry(context.Background(), "live_x", 3)
	if err == nil {
		t.Fatal("a 401 hangup must be reported as a failure")
	}
	if hits.Load() != 1 {
		t.Fatalf("attempts = %d, want 1: a revoked key will not recover", hits.Load())
	}
	var warned bool
	for _, e := range events {
		if e == "live_hangup_auth_failed" {
			warned = true
		}
	}
	if !warned {
		t.Fatal("an auth failure must warn that the session may keep billing")
	}
}

// The whole point: a process that died without hanging up leaves ids on disk, and the next start
// closes them.
func TestSweepClosesOrphansFromAKilledProcess(t *testing.T) {
	var hits atomic.Int64
	var paths []string
	srv := hangupServer(t, []int{200, 200}, &hits, &paths)
	defer srv.Close()
	c := liveClientAt(srv)

	p := filepath.Join(t.TempDir(), "open-sessions")
	// simulate the crash: two sessions recorded, nothing removed
	dead, err := NewLedger(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	dead.Add("live_orphan_1")
	dead.Add("live_orphan_2")

	fresh, err := NewLedger(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Sweep(context.Background(), c)

	if hits.Load() != 2 {
		t.Fatalf("hangup calls = %d, want 2 (one per orphan)", hits.Load())
	}
	for _, want := range []string{"live_orphan_1", "live_orphan_2"} {
		var found bool
		for _, got := range paths {
			if strings.Contains(got, want) && strings.HasSuffix(got, "/hangup") {
				found = true
			}
		}
		if !found {
			t.Fatalf("no hangup was sent for %s; paths=%v", want, paths)
		}
	}
	if got := fresh.Open(); len(got) != 0 {
		t.Fatalf("ledger should be empty after a successful sweep, still has %v", got)
	}
}

// The first ledger format was a bare id per line. A file written by that version still reads.
func TestLedgerReadsTheFirstFormat(t *testing.T) {
	p := filepath.Join(t.TempDir(), "open-sessions")
	if err := os.WriteFile(p, []byte("live_old_1\nlive_old_2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, _ := NewLedger(p, nil)
	es := l.Entries()
	if len(es) != 2 || es[0].ID != "live_old_1" || !es[0].Started.IsZero() || es[0].PID != 0 {
		t.Fatalf("entries = %+v", es)
	}
}

// The periodic sweep runs beside a live call. It must not touch the session that call is using,
// nor one a dial opened seconds ago and has not handed over yet - and it must still close a session
// a dead process left, charging the time it was open.
func TestSweepWithLeavesTheCurrentAndTheYoungAlone(t *testing.T) {
	var hits atomic.Int64
	var paths []string
	srv := hangupServer(t, []int{200, 200, 200}, &hits, &paths)
	defer srv.Close()
	c := liveClientAt(srv)

	p := filepath.Join(t.TempDir(), "open-sessions")
	tenMinAgo := time.Now().Add(-10 * time.Minute).Unix()
	// live_current: old, ours in spirit but named as the exception; live_young: opened just now;
	// live_dead: opened ten minutes ago by a pid that no longer exists.
	content := "live_current\t" + itoa(tenMinAgo) + "\t0\n" +
		"live_young\t" + itoa(time.Now().Unix()) + "\t0\n" +
		"live_dead\t" + itoa(tenMinAgo) + "\t999999\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	l, _ := NewLedger(p, nil)

	var closed []string
	var openFor time.Duration
	l.SweepWith(context.Background(), c, SweepOptions{
		Except: func() string { return "live_current" },
		MinAge: time.Minute,
		OnClosed: func(id string, started time.Time, own bool) {
			closed = append(closed, id)
			openFor = time.Since(started)
			if own {
				t.Fatalf("%s was opened by pid 999999, not by this process", id)
			}
		},
	})
	if hits.Load() != 1 || len(closed) != 1 || closed[0] != "live_dead" {
		t.Fatalf("hangups=%d closed=%v; only the dead process's session should go", hits.Load(), closed)
	}
	if openFor < 9*time.Minute || openFor > 11*time.Minute {
		t.Fatalf("open_for = %v, want about ten minutes so it can be charged", openFor)
	}
	if got := l.Open(); len(got) != 2 {
		t.Fatalf("ledger should still hold the current and the young session, has %v", got)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// A session that is already gone counts as success: the goal is that nothing bills.
func TestSweepTreatsAlreadyGoneAsClosed(t *testing.T) {
	var hits atomic.Int64
	srv := hangupServer(t, []int{404}, &hits, nil)
	defer srv.Close()
	c := liveClientAt(srv)

	p := filepath.Join(t.TempDir(), "open-sessions")
	l, err := NewLedger(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Add("live_gone")
	l.Sweep(context.Background(), c)
	if got := l.Open(); len(got) != 0 {
		t.Fatalf("a 404 means it is not billing; ledger should be clear, has %v", got)
	}
}

// A sweep that cannot close a session must leave it recorded for the next attempt rather than
// quietly forgetting a session that is still costing money.
func TestSweepKeepsWhatItCouldNotClose(t *testing.T) {
	var hits atomic.Int64
	srv := hangupServer(t, []int{500, 500, 500}, &hits, nil)
	defer srv.Close()
	c := liveClientAt(srv)

	p := filepath.Join(t.TempDir(), "open-sessions")
	l, err := NewLedger(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Add("live_stubborn")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l.Sweep(ctx, c)
	if got := l.Open(); len(got) != 1 || got[0] != "live_stubborn" {
		t.Fatalf("an unclosed session must stay on the ledger, got %v", got)
	}
}
