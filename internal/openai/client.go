// Package openai speaks the Realtime API's server-side WebRTC contract:
//
//	POST https://api.openai.com/v1/realtime/calls?model=<model>
//	  Authorization: Bearer <standard API key>
//	  Content-Type: application/sdp
//	  body: our SDP offer
//	-> 201, body = SDP answer, Location: /v1/realtime/calls/rtc_<id>
//
//	wss://api.openai.com/v1/realtime?call_id=rtc_<id>   (sideband control, same API key)
//
// The call MUST be created with the standard API key, not an ephemeral client secret:
// ephemeral-key calls return 404 call_id_not_found on the sideband socket (reported
// 2026-09-09 on the OpenAI developer community). Because control runs over this sideband
// socket, the media PeerConnection needs no SCTP data channel.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	callsURL    = "https://api.openai.com/v1/realtime/calls"
	sidebandURL = "wss://api.openai.com/v1/realtime"
	// DefaultModel bills audio per token and does not charge for silence, unlike the
	// per-minute gpt-live-1 (which bills wall-clock including silence).
	DefaultModel = "gpt-realtime-2.1-mini"
)

var callIDRe = regexp.MustCompile(`rtc_[A-Za-z0-9_-]+`)

// Call is an established Realtime call: the SDP answer plus the id used for control.
type Call struct {
	ID     string
	Answer string
	Model  string
}

// Client creates and controls Realtime calls.
type Client struct {
	APIKey string
	Model  string
	HTTP   *http.Client
	// OnEvent receives every decoded server event from the sideband socket.
	OnEvent func(ev string, kv ...any)
}

// New returns a Client with sane defaults.
func New(apiKey, model string) *Client {
	if model == "" {
		model = DefaultModel
	}
	return &Client{APIKey: apiKey, Model: model, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) event(ev string, kv ...any) {
	if c.OnEvent != nil {
		c.OnEvent(ev, kv...)
	}
}

// CreateCall posts an SDP offer and returns the answer plus the call id.
func (c *Client) CreateCall(ctx context.Context, offerSDP string) (*Call, error) {
	if c.APIKey == "" {
		return nil, ErrNoAPIKey
	}
	endpoint := callsURL + "?model=" + url.QueryEscape(c.Model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(offerSDP))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/sdp")
	t0 := time.Now()
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create call: %w", err)
	}
	defer res.Body.Close()
	id := callIDRe.FindString(res.Header.Get("Location"))
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		if id != "" {
			hctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			return nil, errors.Join(fmt.Errorf("create call: %w", err), c.Hangup(hctx, id))
		}
		return nil, fmt.Errorf("create call: %w", err)
	}
	c.event("openai_create_call", "status", res.StatusCode, "ms", time.Since(t0).Milliseconds(),
		"location", res.Header.Get("Location"), "content_type", res.Header.Get("Content-Type"), "bytes", len(body))
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("create call: %w", &StatusError{Op: "create call", Status: res.StatusCode})
	}
	answer := string(body)
	if !strings.Contains(answer, "v=0") {
		c.event("openai_create_call_bad_sdp", "bytes", len(body))
		return nil, fmt.Errorf("create call: %w", ErrBadSDP)
	}
	if id == "" {
		// some responses carry the id only in the body of a JSON error; try the answer too
		id = callIDRe.FindString(answer)
	}
	if id == "" {
		return nil, ErrNoCallID
	}
	return &Call{ID: id, Answer: answer, Model: c.Model}, nil
}

// Sideband is the server-side control socket for one call.
type Sideband struct {
	conn *websocket.Conn
	c    *Client
}

// Attach opens the sideband control socket for an established call.
func (c *Client) Attach(ctx context.Context, callID string) (*Sideband, error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.APIKey)
	conn, res, err := websocket.Dial(ctx, sidebandURL+"?call_id="+callID, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		code := 0
		if res != nil {
			code = res.StatusCode
		}
		return nil, fmt.Errorf("attach sideband (HTTP %d): %w", code, err)
	}
	conn.SetReadLimit(4 << 20)
	c.event("openai_sideband_open", "call_id", callID)
	return &Sideband{conn: conn, c: c}, nil
}

// Send marshals and sends one client event.
func (s *Sideband) Send(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.conn.Write(ctx, websocket.MessageText, b)
}

// SessionUpdate applies a session configuration patch (instructions, voice, VAD, tools).
func (s *Sideband) SessionUpdate(ctx context.Context, session map[string]any) error {
	s.c.event("openai_session_update", "keys", keysOf(session))
	return s.Send(ctx, map[string]any{"type": "session.update", "session": session})
}

// Read pumps server events until the context ends or the socket closes. Every event is
// reported through Client.OnEvent; the payloads of audio deltas are summarised, not logged.
func (s *Sideband) Read(ctx context.Context) error {
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			s.c.event("openai_sideband_closed", "err", err.Error())
			return err
		}
		var ev map[string]any
		if err := json.Unmarshal(data, &ev); err != nil {
			s.c.event("openai_event_parse_error", "bytes", len(data))
			continue
		}
		t, _ := ev["type"].(string)
		switch {
		case strings.HasSuffix(t, ".delta"):
			s.c.event("openai_event", "type", t, "bytes", len(data))
		case t == "error":
			s.c.event("openai_error", "detail_chars", len(data))
		case strings.Contains(t, "transcript") || strings.Contains(t, "text") || strings.Contains(t, "delta"):
			s.c.event("openai_event", "type", t, "chars", len(data))
		default:
			s.c.event("openai_event", "type", t, "bytes", len(data))
		}
	}
}

// Close shuts the sideband socket.
func (s *Sideband) Close() error { return s.conn.Close(websocket.StatusNormalClosure, "bye") }

// Hangup ends the call server-side. Per the SDK, hangup works for calls initiated over
// either SIP or WebRTC (accept/reject/refer are SIP-only).
func (c *Client) Hangup(ctx context.Context, callID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callsURL+"/"+callID+"/hangup", bytes.NewReader(nil))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if err != nil {
		return fmt.Errorf("hangup: %w", err)
	}
	c.event("openai_hangup", "status", res.StatusCode, "body_chars", len(b))
	if res.StatusCode >= 300 {
		return fmt.Errorf("hangup: %w", &StatusError{Op: "hangup", Status: res.StatusCode})
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
