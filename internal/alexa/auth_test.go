package alexa

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testHandler struct{ offers, disconnected int }

func (h *testHandler) Offer(string, string) (string, error) { h.offers++; return "v=0", nil }
func (*testHandler) Connected(string)                       {}
func (h *testHandler) Disconnected(string)                  { h.disconnected++ }

func TestAuthCannotReplayAcrossPathsOrSignatureCase(t *testing.T) {
	key := []byte("test-key")
	h := &testHandler{}
	s := NewServer(key, h)
	body := []byte(`{"type":"offer","sessionId":"test_session","sdp":"v=0"}`)
	ts, sig := Sign(key, body)
	request := func(path, signature string) int {
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		r.Header.Set("X-ACV-Timestamp", ts)
		r.Header.Set("X-ACV-Signature", signature)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code
	}
	if got := request("/rtc/disconnected", sig); got != 400 {
		t.Fatalf("cross-path status=%d", got)
	}
	if got := request("/rtc/offer", sig); got != 200 {
		t.Fatalf("offer status=%d", got)
	}
	if got := request("/rtc/offer", strings.ToUpper(sig)); got != 409 {
		t.Fatalf("case replay status=%d", got)
	}
	if h.offers != 1 || h.disconnected != 0 {
		t.Fatalf("offers=%d disconnects=%d", h.offers, h.disconnected)
	}
}

func TestEmptySigningKeyIsRefused(t *testing.T) {
	body := []byte(`{}`)
	ts, sig := Sign(nil, body)
	if err := NewServer(nil, &testHandler{}).verify(ts, body, sig); err == nil {
		t.Fatal("empty key authenticated a request")
	}
}
