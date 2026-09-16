// Package alexa implements the HTTP signaling contract between the existing
// AWS Lambda Smart Home adapter and the local bridge. The contract is kept
// byte-compatible with the July 2026 Python service (controller/signaling.py)
// so the deployed Lambda needs no change:
//
//	POST /rtc/offer        {"v":1,"type":"offer","sessionId":..,"sdp":..,"ts":..,"nonce":..}
//	POST /rtc/connected    {"v":1,"type":"connected","sessionId":..,"ts":..,"nonce":..}
//	POST /rtc/disconnected {"v":1,"type":"disconnected","sessionId":..,"ts":..,"nonce":..}
//	GET  /health
//
// Every POST carries X-ACV-Timestamp and X-ACV-Signature = hex(HMAC-SHA256(key, ts + "." + body)).
// Requests older than 30 s or with a replayed nonce are rejected.
package alexa

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	tsWindow = 30 * time.Second
	nonceTTL = 5 * time.Minute
	maxBody  = 64 * 1024
)

// Request is the decoded signaling body.
type Request struct {
	V         int     `json:"v"`
	Type      string  `json:"type"`
	SessionID string  `json:"sessionId"`
	SDP       string  `json:"sdp,omitempty"`
	TS        float64 `json:"ts"`
	Nonce     string  `json:"nonce"`
}

// Handler receives verified signaling requests.
type Handler interface {
	// Offer returns the SDP answer for the session or an error.
	Offer(sessionID, offerSDP string) (answerSDP string, err error)
	Connected(sessionID string)
	Disconnected(sessionID string)
}

// Server is the HMAC-verified HTTP front end.
type Server struct {
	key     []byte
	h       Handler
	mu      sync.Mutex
	nonces  map[string]time.Time
	OnEvent func(ev string, kv ...any)
}

// NewServer builds a server with the shared HMAC key.
func NewServer(key []byte, h Handler) *Server {
	return &Server{key: key, h: h, nonces: map[string]time.Time{}}
}

func (s *Server) event(ev string, kv ...any) {
	if s.OnEvent != nil {
		s.OnEvent(ev, kv...)
	}
}

func (s *Server) verify(ts string, body []byte, sig string) error {
	tsf, err := strconv.ParseFloat(ts, 64)
	if err != nil {
		return errors.New("bad_timestamp")
	}
	if d := time.Since(time.Unix(0, int64(tsf*1e9))); d > tsWindow || d < -tsWindow {
		return errors.New("stale")
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(sig))) {
		return errors.New("bad_signature")
	}
	return nil
}

func (s *Server) seenNonce(n string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, t := range s.nonces {
		if now.Sub(t) > nonceTTL {
			delete(s.nonces, k)
		}
	}
	if _, ok := s.nonces[n]; ok {
		return true
	}
	s.nonces[n] = now
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if r.Method == http.MethodGet && path == "/health" {
		writeJSON(w, 200, map[string]any{"ok": true, "service": "rtcdiag"})
		return
	}
	if r.Method != http.MethodPost || (path != "/rtc/offer" && path != "/rtc/connected" && path != "/rtc/disconnected") {
		writeJSON(w, 404, map[string]any{"error": "not_found"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		writeJSON(w, 413, map[string]any{"error": "too_large"})
		return
	}
	ts, sig := r.Header.Get("X-ACV-Timestamp"), r.Header.Get("X-ACV-Signature")
	if ts == "" || sig == "" {
		writeJSON(w, 401, map[string]any{"error": "missing_auth"})
		return
	}
	if err := s.verify(ts, body, sig); err != nil {
		s.event("signaling_auth_fail", "path", path, "reason", err.Error())
		code := 401
		if err.Error() == "bad_signature" {
			code = 403
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil || req.SessionID == "" {
		writeJSON(w, 400, map[string]any{"error": "bad_json"})
		return
	}
	// The session id names a run directory and a recording directory downstream. Alexa's ids
	// are UUIDs; anything with a path separator, a dot run or a control character is not one.
	if !plainID(req.SessionID) {
		writeJSON(w, 400, map[string]any{"error": "bad_session_id"})
		return
	}
	nonce := req.Nonce
	if nonce == "" {
		nonce = sig
	}
	if s.seenNonce(nonce) {
		writeJSON(w, 409, map[string]any{"error": "replay"})
		return
	}
	// Lambda-side send time vs our receive time: measures Lambda->Funnel->us latency.
	lambdaTS := time.Unix(0, int64(req.TS*1e9))
	s.event("signaling_rx", "path", path, "sessionId", req.SessionID, "lambda_to_us_ms", time.Since(lambdaTS).Milliseconds(), "body_bytes", len(body))

	switch path {
	case "/rtc/offer":
		if !strings.Contains(req.SDP, "v=0") {
			writeJSON(w, 400, map[string]any{"error": "bad_sdp"})
			return
		}
		t0 := time.Now()
		answer, err := s.h.Offer(req.SessionID, req.SDP)
		if err != nil {
			s.event("answer_failed", "sessionId", req.SessionID, "err", err.Error(), "ms", time.Since(t0).Milliseconds())
			writeJSON(w, 502, map[string]any{"v": 1, "type": "error", "sessionId": req.SessionID, "code": "ANSWER_FAILED", "message": err.Error()})
			return
		}
		s.event("answer_sent", "sessionId", req.SessionID, "answer_ms", time.Since(t0).Milliseconds(), "answer_bytes", len(answer))
		writeJSON(w, 200, map[string]any{"v": 1, "type": "answer", "sessionId": req.SessionID, "sdp": answer})
	case "/rtc/connected":
		s.h.Connected(req.SessionID)
		writeJSON(w, 200, map[string]any{"ok": true})
	case "/rtc/disconnected":
		s.h.Disconnected(req.SessionID)
		writeJSON(w, 200, map[string]any{"ok": true})
	}
}

// Sign produces the headers a client must send for body (used by the local simulator).
func Sign(key, body []byte) (ts, sig string) {
	ts = strconv.FormatFloat(float64(time.Now().UnixNano())/1e9, 'f', 3, 64)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return ts, hex.EncodeToString(mac.Sum(nil))
}

// plainID accepts the shape of an Alexa session id - letters, digits, '-' and '_', at most 128 of
// them - and nothing that could steer a file path or a log line.
func plainID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
