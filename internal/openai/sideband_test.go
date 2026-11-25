package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A sideband server that delivers a few events on each socket and then says nothing, without
// closing: the shape of the 17:23 call of 2026-09-14, where the socket stayed open and silent from
// 61 s to the end while the session and its media carried on.
func silentAfterServer(t *testing.T, eventsPerSocket int, sockets *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		n := sockets.Add(1)
		ctx := r.Context()
		for i := 0; i < eventsPerSocket; i++ {
			msg := `{"type":"session.input_audio.append","socket":` + itoa(n) + `}`
			if err := conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
				return
			}
		}
		<-ctx.Done() // silent, open, forever
	}))
}

// Silence is measured, a kick makes a blocked Read return, and a reattach brings a fresh socket
// whose events flow to the same reader.
func TestSidebandKickAndReattachAfterSilence(t *testing.T) {
	var sockets atomic.Int64
	srv := silentAfterServer(t, 3, &sockets)
	defer srv.Close()
	c := liveClientAt(srv)

	sb, err := c.Attach(context.Background(), "live_t")
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	var got atomic.Int64
	readDone := make(chan error, 1)
	go func() { readDone <- sb.Read(context.Background(), func(string, []byte) { got.Add(1) }) }()

	// Three events, then silence.
	deadline := time.Now().Add(3 * time.Second)
	for got.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got.Load() != 3 {
		t.Fatalf("got %d events from the first socket, want 3", got.Load())
	}
	time.Sleep(150 * time.Millisecond)
	if sb.Silent() < 100*time.Millisecond {
		t.Fatalf("silence clock did not advance: %v", sb.Silent())
	}

	// Read is blocked on a socket that will never speak again. Kick it.
	sb.Kick()
	select {
	case <-readDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Read did not return after the socket was kicked")
	}

	if err := sb.Reattach(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for sockets.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond) // the server counts after its handshake returns
	}
	if sb.Attached() != 2 || sockets.Load() != 2 {
		t.Fatalf("attached=%d server sockets=%d, want 2 and 2", sb.Attached(), sockets.Load())
	}
	if sb.Silent() > 100*time.Millisecond {
		t.Fatalf("silence clock should reset on reattach: %v", sb.Silent())
	}
	go func() { readDone <- sb.Read(context.Background(), func(string, []byte) { got.Add(1) }) }()
	deadline = time.Now().Add(3 * time.Second)
	for got.Load() < 6 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got.Load() != 6 {
		t.Fatalf("got %d events in total, want 6: the second socket's events did not reach the reader", got.Load())
	}
}
