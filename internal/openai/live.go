package openai

// GPT-Live (`gpt-live-1`) server-side WebRTC contract. Verified against
// developers.openai.com/api/docs/guides/voice-webrtc and .../guides/live-delegation,
// retrieved 2026-09-12:
//
//	POST https://api.openai.com/v1/live/sessions
//	  Authorization: Bearer <project API key>
//	  Content-Type: application/json
//	  {"session": {...}, "transport": {"type": "webrtc", "sdp": "<offer>"}}
//	-> 201 {"session": {"id": "live_..."}, "transport": {"type":"webrtc","sdp":"<answer>"}}
//
// Differences from the Realtime contract in client.go that the docs call out explicitly:
//   - the body is JSON, not application/sdp, and there is no Location header: the id lives
//     at .session.id and is opaque.
//   - the data channel label is "oai-events"; "Do not send session.start on the data channel"
//     because the HTTP request already started the session, and application commands must wait
//     for the `session.started` event.
//   - "WebRTC negotiates the audio format through SDP, so omit audio.format"; do not send
//     session.input_audio.append or expect session.output_audio.delta on the data channel.
//   - creating the session "bills 15 seconds of voice duration during initialization", credited
//     against the running session rather than added to it.
//
// Cost shape to respect: GPT-Live bills wall clock, and "Active session time includes time when
// the user speaks, the assistant speaks, both are silent, or the backend is working." So the
// caller MUST close the session on inactivity — see IdleGate.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	liveURL = "https://api.openai.com/v1/live/sessions"
	// modelsURL is the cheapest thing on the API that answers over the same connection: free,
	// unmetered, and it changes nothing. Warm uses it.
	modelsURL = "https://api.openai.com/v1/models"
	// LiveDataChannelLabel is the label the docs require for the events channel.
	LiveDataChannelLabel = "oai-events"
	// LiveModel is the full-duplex voice model: "It can listen and speak at the same time."
	LiveModel = "gpt-live-1"
	// BackendModel is the delegation backend. Luna is the cost-sensitive tier OpenAI lists for
	// delegation; at $0.20/$1.20 per 1M a hard question costs about a cent.
	BackendModel = "gpt-5.6-luna"
	// BackendEffortDefault keeps latency usable. Measured time to first answer token
	// (Artificial Analysis): medium is seconds, xhigh 43.81 s, max 134.29 s — so xhigh is an
	// announced escalation and max never belongs in a voice path.
	BackendEffortDefault = "medium"
	// liveInitBilledSeconds is what session creation costs before a word is spoken.
	liveInitBilledSeconds = 15
)

// Delegation configures how GPT-Live offloads hard work.
//
// Mode "responses" lets OpenAI call the backend model itself, but the configuration is fixed at
// session creation ("Choose the mode when you create the session; to change modes, start a new
// session"), which pins reasoning effort for the whole session. Mode "client" hands each request
// to this application instead, so effort can be chosen per question — the reason it is the default
// here. See BackendEffortFor.
type Delegation struct {
	Type      string             `json:"type"` // "responses" | "client"
	Responses *DelegationBackend `json:"responses,omitempty"`
}

// DelegationBackend mirrors the documented `delegation.responses` object.
type DelegationBackend struct {
	Model             string           `json:"model"`
	Instructions      string           `json:"instructions,omitempty"`
	Tools             []map[string]any `json:"tools,omitempty"`
	ToolChoice        string           `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens   int              `json:"max_output_tokens,omitempty"` // minimum 16 when set
	ServiceTier       string           `json:"service_tier,omitempty"`
	Reasoning         *Reasoning       `json:"reasoning,omitempty"`
}

// Reasoning carries the effort level for the delegated model.
type Reasoning struct {
	Effort string `json:"effort"` // none|low|medium|high|xhigh|max
}

// ClientDelegation returns the delegation config that keeps effort selection in this bridge.
func ClientDelegation() *Delegation { return &Delegation{Type: "client"} }

// ResponsesDelegation returns OpenAI-side delegation with a fixed effort.
func ResponsesDelegation(model, effort, instructions string) *Delegation {
	if model == "" {
		model = BackendModel
	}
	if effort == "" {
		effort = BackendEffortDefault
	}
	return &Delegation{Type: "responses", Responses: &DelegationBackend{
		Model: model, Instructions: instructions, ToolChoice: "auto", Reasoning: &Reasoning{Effort: effort},
	}}
}

// BackendEffortFor picks a reasoning effort from how hard the request looks, instead of pinning
// one level. The cost of a high level is latency, not money: at $1.20 per 1M output tokens a
// long xhigh answer is about one cent, but it is ~44 s to the first answer token.
//
// hard  -> the caller has already decided this deserves a wait (and should say so out loud)
// tool  -> the answer comes from a tool, so thinking time is wasted
// plain -> ordinary reasoning
func BackendEffortFor(kind string) string {
	switch kind {
	case "tool":
		return "none"
	case "quick":
		return "low"
	case "hard":
		return "xhigh"
	default:
		return BackendEffortDefault
	}
}

// LiveSessionConfig is the `session` object of the create request.
type LiveSessionConfig struct {
	Model        string         `json:"model"`
	Instructions string         `json:"instructions,omitempty"`
	Delegation   *Delegation    `json:"delegation,omitempty"`
	Store        *bool          `json:"store,omitempty"` // required for fork; recordings expire in 30 days
	Audio        map[string]any `json:"audio,omitempty"` // omit `format` on WebRTC: SDP negotiates it
	Extra        map[string]any `json:"-"`
}

// LiveClient creates and controls GPT-Live sessions.
type LiveClient struct {
	APIKey  string
	Model   string
	HTTP    *http.Client
	OnEvent func(ev string, kv ...any)

	// storeDenied records that this project refuses stored sessions, so later calls skip the
	// doomed attempt and callers know not to try forking.
	storeDenied atomic.Bool
	// audioInputDenied records that this deployment rejects session.audio.input, so later calls
	// stop sending it instead of paying a failed round trip per session.
	audioInputDenied atomic.Bool
	// Budget is the day's spending ceiling, shared on disk with every other process on this
	// machine. Nil means no ceiling. It is checked BEFORE a session is created, because after the
	// session exists the money is already committed.
	Budget *Budget
	// Ledger is where a session that was created but could not be handed over goes: the counter
	// unwritable and the hangup failing too used to leave it billing with its id in one log line
	// and nowhere a sweep could find it. Nil means that line is all there is.
	Ledger *Ledger
}

// NewLive returns a LiveClient with defaults.
func NewLive(apiKey, model string) *LiveClient {
	if model == "" {
		model = LiveModel
	}
	return &LiveClient{APIKey: apiKey, Model: model, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Warm keeps the HTTPS connection to OpenAI open between calls, so the next session is not created
// over a new one. Measured on the 20:52 call of 2026-09-13: creating a session took 1570 ms on a
// cold connection and 758 ms on the same connection a second later, and his calls come minutes or
// hours apart, so every first attempt was the cold one. The request itself is free and changes
// nothing; it only has to travel through the pooled connection so the pool keeps it.
func (c *LiveClient) Warm(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		c.event("live_warm_drain_failed", "err", err.Error())
	}
	if res.StatusCode >= 400 {
		return fmt.Errorf("warm: %w", &StatusError{Op: "warm", Status: res.StatusCode})
	}
	return nil
}

func (c *LiveClient) event(ev string, kv ...any) {
	if c.OnEvent != nil {
		c.OnEvent(ev, kv...)
	}
}

// LiveSession is an established GPT-Live session.
type LiveSession struct {
	ID     string
	Answer string
	Model  string
}

type liveCreateResponse struct {
	Session struct {
		ID string `json:"id"`
	} `json:"session"`
	Transport struct {
		Type string `json:"type"`
		SDP  string `json:"sdp"`
	} `json:"transport"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Type    string `json:"type"`
	} `json:"error"`
}

// ErrSessionCreated marks a failed setup that already incurred a session charge.
// Callers must not fall back to creating another paid session, even after cleanup.
var ErrSessionCreated = errors.New("paid session was already created")

// recoveredLiveID reads the session object even when the rest of the JSON was cut
// short. Only a complete session object is trusted; arbitrary text is not an ID.
func recoveredLiveID(raw []byte) string {
	d := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return ""
		}
		if key == "session" {
			var session struct {
				ID string `json:"id"`
			}
			if d.Decode(&session) == nil {
				return session.ID
			}
			return ""
		}
		var skip json.RawMessage
		if d.Decode(&skip) != nil {
			return ""
		}
	}
	return ""
}

// closeRecoveredSession persists the ID before cleanup and counts the committed
// init charge. Failed cleanup leaves the ledger entry for the next sweep.
func (c *LiveClient) closeRecoveredSession(id string, cause error) error {
	var ledgerErr error
	if c.Ledger != nil {
		ledgerErr = c.Ledger.Add(id)
	}
	budgetErr := c.Budget.Opened()
	hctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hangupErr := c.HangupRetry(hctx, id, 3)
	if hangupErr == nil {
		if c.Ledger != nil {
			c.Ledger.Remove(id)
		}
	} else {
		c.event("live_recovered_cleanup_failed", "session_id", id, "err", hangupErr.Error())
	}
	return errors.Join(fmt.Errorf("session %s: %w", id, ErrSessionCreated), cause, ledgerErr, budgetErr, hangupErr)
}

// StorageDenied reports whether an error is the project refusing stored sessions. A project with
// data persistence disabled (or Zero Data Retention) rejects store:true, and fork depends on it.
func StorageDenied(err error) bool {
	var ae *apiError
	return err != nil && errors.As(err, &ae) && ae.Code == "session_storage_not_allowed"
}

// AudioInputDenied reports the API refusing session.audio.input.
//
// Measured against gpt-live-1 on 2026-09-12:
//
//	HTTP 400: unknown_parameter: Unknown parameter: 'session.audio.input'.
//
// The Realtime API documents audio.input.noise_reduction (near_field/far_field) and
// audio.input.turn_detection, and it is tempting to assume the Live API shares the shape. It does
// not: gpt-live-1 listens continuously rather than through an input buffer with turn detection, so
// there is no input buffer to filter or segment. Only audio.output is accepted.
func AudioInputDenied(err error) bool {
	var ae *apiError
	return err != nil && errors.As(err, &ae) && ae.Code == "unknown_parameter" &&
		strings.Contains(ae.Message, "session.audio.input")
}

// CreateSession posts the SDP offer and returns the answer plus the opaque session id.
//
// store:true is requested because it is what makes Fork possible, and Fork is how a conversation
// survives the Echo tearing its RTC session down. A project that forbids persistence rejects it
// outright, so that one error is retried once without store: a session with no resume is far
// better than no session, and the log says which one was obtained.
func (c *LiveClient) CreateSession(ctx context.Context, offerSDP string, cfg LiveSessionConfig) (*LiveSession, error) {
	if err := c.Budget.Check(); err != nil {
		return nil, err
	}
	sess, err := c.createOnce(ctx, offerSDP, cfg)
	// Each refusal is retried at most once, and the two can stack: the first attempt is refused
	// for audio.input, the second for store. Handling them as separate one-shot branches meant
	// the second refusal was returned as the error (found by the 2026-09-16 fleet).
	if err != nil && AudioInputDenied(err) {
		// An audio tuning parameter must never be the reason the assistant does not answer.
		c.event("live_audio_input_not_supported", "action", "retrying without session.audio.input",
			"note", "gpt-live-1 accepts audio.output only; input noise reduction is a Realtime API feature")
		c.audioInputDenied.Store(true)
		sess, err = c.createOnce(ctx, offerSDP, cfg)
	}
	if err != nil && StorageDenied(err) && cfg.Store != nil && *cfg.Store {
		c.event("live_storage_not_allowed", "action", "retrying without store; fork will be unavailable",
			"fix", "enable data persistence for this project if conversation resume is wanted")
		off := false
		cfg.Store = &off
		c.storeDenied.Store(true)
		sess, err = c.createOnce(ctx, offerSDP, cfg)
	}
	if err != nil && !errors.Is(err, ErrSessionCreated) {
		c.Budget.Abandon() // nothing was created; the admission goes back
	}
	return sess, err
}

// ForkAvailable reports whether the project allowed stored sessions, i.e. whether Fork can work.
func (c *LiveClient) ForkAvailable() bool { return !c.storeDenied.Load() }

func (c *LiveClient) createOnce(ctx context.Context, offerSDP string, cfg LiveSessionConfig) (*LiveSession, error) {
	if c.APIKey == "" {
		return nil, ErrNoAPIKey
	}
	if cfg.Model == "" {
		cfg.Model = c.Model
	}
	sessionObj := map[string]any{"model": cfg.Model}
	if cfg.Instructions != "" {
		sessionObj["instructions"] = cfg.Instructions
	}
	if cfg.Delegation != nil {
		sessionObj["delegation"] = cfg.Delegation
	}
	if cfg.Store != nil && *cfg.Store && c.storeDenied.Load() {
		// already known to be refused for this project
	} else if cfg.Store != nil {
		sessionObj["store"] = *cfg.Store
	}
	if len(cfg.Audio) > 0 {
		// Copy before pruning: the caller reuses this config for every session, so mutating it
		// here would make one session's failure silently rewrite the next one's request.
		audio := make(map[string]any, len(cfg.Audio))
		for k, v := range cfg.Audio {
			audio[k] = v
		}
		// guard the documented rules rather than trusting the caller
		delete(audio, "format")
		if c.audioInputDenied.Load() {
			delete(audio, "input")
		}
		if len(audio) > 0 {
			sessionObj["audio"] = audio
		}
	}
	for k, v := range cfg.Extra {
		sessionObj[k] = v
	}
	body, err := json.Marshal(map[string]any{
		"session":   sessionObj,
		"transport": map[string]any{"type": "webrtc", "sdp": offerSDP},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, liveURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	t0 := time.Now()
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create live session: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		if id := recoveredLiveID(raw); id != "" {
			return nil, c.closeRecoveredSession(id, fmt.Errorf("create live session: %w", err))
		}
		return nil, fmt.Errorf("create live session: %w", err)
	}
	c.event("live_create_session", "status", res.StatusCode, "ms", time.Since(t0).Milliseconds(),
		"billed_init_seconds", liveInitBilledSeconds, "bytes", len(raw))
	var out liveCreateResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("create live session: %w", &StatusError{Op: "create live session", Status: res.StatusCode})
	}
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		if out.Error != nil {
			return nil, fmt.Errorf("create live session: %w", &apiError{
				Code:    out.Error.Code,
				Message: out.Error.Message,
				Type:    out.Error.Type,
			})
		}
		return nil, fmt.Errorf("create live session: %w", &StatusError{Op: "create live session", Status: res.StatusCode})
	}
	if out.Session.ID == "" || out.Transport.SDP == "" {
		return nil, fmt.Errorf("create live session: %w", &StatusError{Op: "create live session", Status: res.StatusCode})
	}
	// Count the money the moment it is committed, not when the session closes: a process that dies
	// in between must still leave the cost on the record.
	if err := c.recordOpened(out.Session.ID); err != nil {
		return nil, err
	}
	return &LiveSession{ID: out.Session.ID, Answer: out.Transport.SDP, Model: cfg.Model}, nil
}

// recordOpened charges the session to the day's counter. If the counter cannot be written, the
// session is hung up on the spot: it exists and is billing, and a ceiling that keeps going when
// its own record fails is not a ceiling. Codex found the fail-open.
func (c *LiveClient) recordOpened(sessionID string) error {
	// The ledger line goes first: it is what a restart reads to hang up what a crash left open,
	// and it used to be written by the caller a few steps later, so a kill in between left a
	// session billed and unlisted (the 2026-09-16 gate). A ledger that cannot take the line is
	// treated like a counter that cannot: the session is hung up rather than left unrecoverable.
	err := c.Budget.Opened()
	if err == nil && c.Ledger != nil {
		if lerr := c.Ledger.Add(sessionID); lerr != nil {
			err = fmt.Errorf("ledger: %w", lerr)
		}
	}
	if err == nil {
		return nil
	}
	c.event("live_session_unrecorded", "err", err.Error(), "session_id", sessionID,
		"action", "hanging up the session just created")
	hctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if herr := c.HangupRetry(hctx, sessionID, 3); herr != nil {
		note := "created, unrecorded and not hung up; find it in the OpenAI dashboard"
		if c.Ledger != nil && c.Ledger.Add(sessionID) == nil {
			note = "created and not hung up; it is on the ledger, so the next sweep will try again"
		}
		c.event("live_hangup_failed", "err", herr.Error(), "session_id", sessionID, "note", note)
	} else if c.Ledger != nil {
		c.Ledger.Remove(sessionID) // hung up: whatever line was written is settled
	}
	return fmt.Errorf("the session could not be recorded: %w", err)
}

// LiveSideband is the trusted server-side control socket for one Live session. Route listed in
// the official Python SDK's generated API listing as
// wss://api.openai.com/v1/live/sessions/{session_id}/attach.
type LiveSideband struct {
	conn   *websocket.Conn
	c      *LiveClient
	id     string
	seq    int
	mu     sync.Mutex
	closed bool // protected by mu; an in-flight dial cannot resurrect a closed sideband
	// lastEvent is when the socket last delivered anything. While audio is flowing to the
	// session OpenAI announces every chunk of it here, several times a second, so a socket that
	// says nothing for ten seconds has died the way a TCP connection dies over WiFi: silently,
	// with Read blocked forever and no error to show for it.
	lastEvent atomic.Int64
	// attached counts the sockets this sideband has had, so a reader can tell a deliberate swap
	// from the end.
	attached atomic.Int64
}

// Attach opens the sideband control socket.
func (c *LiveClient) Attach(ctx context.Context, sessionID string) (*LiveSideband, error) {
	conn, err := c.dialSideband(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	s := &LiveSideband{conn: conn, c: c, id: sessionID}
	s.lastEvent.Store(time.Now().UnixNano())
	s.attached.Store(1)
	return s, nil
}

func (c *LiveClient) dialSideband(ctx context.Context, sessionID string) (*websocket.Conn, error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.APIKey)
	url := "wss://api.openai.com/v1/live/sessions/" + sessionID + "/attach"
	// Through the client's own HTTP client, so the socket shares its pool and, in tests, its
	// rewritten destination.
	conn, res, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h, HTTPClient: c.HTTP})
	if err != nil {
		code := 0
		if res != nil {
			code = res.StatusCode
		}
		return nil, fmt.Errorf("attach live sideband (HTTP %d): %w", code, err)
	}
	conn.SetReadLimit(4 << 20)
	c.event("live_sideband_open", "session_id", sessionID)
	return conn, nil
}

// Reattach opens a fresh socket to the same session and retires the old one. The session itself
// is untouched: OpenAI keeps it, and its billing, whoever is attached. Measured on the 17:23 call
// of 2026-09-14: the socket stopped delivering at 61 s with the media still flowing both ways, and
// because the transcript is what the idle timer listens to, the bridge hung up on him at 119 s
// while he was talking.
func (s *LiveSideband) Reattach(ctx context.Context) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return net.ErrClosed
	}
	conn, err := s.c.dialSideband(ctx, s.id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = conn.CloseNow()
		return net.ErrClosed
	}
	old := s.conn
	s.conn = conn
	s.attached.Add(1)
	s.mu.Unlock()
	s.lastEvent.Store(time.Now().UnixNano())
	// CloseNow, not Close: a polite close waits for the peer's close frame, and the peer that
	// earned this is the one that has stopped answering.
	_ = old.CloseNow()
	return nil
}

// Silent reports how long the socket has delivered nothing.
func (s *LiveSideband) Silent() time.Duration {
	return time.Since(time.Unix(0, s.lastEvent.Load()))
}

// Kick closes the current socket so a Read blocked on it returns. The sideband itself stays.
func (s *LiveSideband) Kick() {
	_ = s.current().CloseNow()
}

// Attached returns how many sockets this sideband has opened.
func (s *LiveSideband) Attached() int64 { return s.attached.Load() }

func (s *LiveSideband) current() *websocket.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn
}

func (s *LiveSideband) nextEventID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return fmt.Sprintf("bridge_%d_%d", time.Now().UnixMilli(), s.seq)
}

// Send marshals and sends one event.
func (s *LiveSideband) Send(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.current().Write(ctx, websocket.MessageText, b)
}

// Commentary appends text the model should SPEAK. delegationID ties it to a delegation request;
// pass "" for none. Content is limited to 500 tokens per append.
func (s *LiveSideband) Commentary(ctx context.Context, delegationID, content string) error {
	return s.appendEvent(ctx, "session.commentary.append", delegationID, content)
}

// Thinking appends context for the model to reason over WITHOUT speaking it.
func (s *LiveSideband) Thinking(ctx context.Context, delegationID, content string) error {
	return s.appendEvent(ctx, "session.thinking.append", delegationID, content)
}

// Instructions appends session-wide steering. The docs warn an acknowledgement "is not a signal
// to resume playback" and that corrective instructions "cannot retract audio the user has already
// heard", so use it to steer, not to undo.
func (s *LiveSideband) Instructions(ctx context.Context, content string) error {
	return s.appendEvent(ctx, "session.instructions.append", "", content)
}

func (s *LiveSideband) appendEvent(ctx context.Context, typ, delegationID, content string) error {
	ev := map[string]any{"type": typ, "event_id": s.nextEventID(), "content": content}
	// delegation_id is a required field; null means session-wide.
	if delegationID == "" {
		ev["delegation_id"] = nil
	} else {
		ev["delegation_id"] = delegationID
	}
	s.c.event("live_send", "type", typ, "delegation_id", delegationID, "content_chars", len(content))
	return s.Send(ctx, ev)
}

// Read pumps server events until the context ends or the socket closes. Returns the first
// `session.started` through started (closed once) so callers can wait before sending commands.
func (s *LiveSideband) Read(ctx context.Context, onEvent func(typ string, raw []byte)) error {
	conn := s.current()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			s.c.event("live_sideband_closed", "err", err.Error())
			return err
		}
		s.lastEvent.Store(time.Now().UnixNano())
		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			s.c.event("live_event_parse_error", "bytes", len(data))
			continue
		}
		if onEvent != nil {
			onEvent(env.Type, data)
		}
	}
}

// Close shuts the sideband socket.
func (s *LiveSideband) Close() error {
	s.mu.Lock()
	s.closed = true
	conn := s.conn
	s.mu.Unlock()
	return conn.Close(websocket.StatusNormalClosure, "bye")
}

// Hangup ends the session server-side and stops the per-minute clock.
func (c *LiveClient) Hangup(ctx context.Context, sessionID string) error {
	return c.post(ctx, "/"+sessionID+"/hangup", nil, "live_hangup")
}

// Fork starts a NEW session from the stored state of an old one, which is how a conversation
// survives the Echo tearing its RTC session down. Requires store:true on the source session; the
// fork gets a new id and does not reopen the original connection.
func (c *LiveClient) Fork(ctx context.Context, sourceID, offerSDP string) (*LiveSession, error) {
	// A resumed call costs exactly what a new one costs, so it passes the same gate. Codex
	// found that forking walked straight past the ceiling and then subtracted an
	// initialisation allowance that had never been added.
	if err := c.Budget.Check(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"transport": map[string]any{"type": "webrtc", "sdp": offerSDP}})
	if err != nil {
		c.Budget.Abandon()
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, liveURL+"/"+sourceID+"/fork", bytes.NewReader(body))
	if err != nil {
		c.Budget.Abandon()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		c.Budget.Abandon()
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		if id := recoveredLiveID(raw); id != "" {
			return nil, c.closeRecoveredSession(id, fmt.Errorf("fork: %w", err))
		}
		c.Budget.Abandon()
		return nil, fmt.Errorf("fork: %w", err)
	}
	c.event("live_fork", "status", res.StatusCode, "source", sourceID, "bytes", len(raw))
	var out liveCreateResponse
	// A 4xx with a well-formed error body parses fine and has no session id; a 2xx with no
	// answer SDP would be a session that exists and can never connect. Both are failures.
	if err := json.Unmarshal(raw, &out); err != nil || res.StatusCode >= 300 || out.Session.ID == "" || out.Transport.SDP == "" {
		c.Budget.Abandon()
		if out.Session.ID != "" {
			c.event("live_fork_unusable", "session_id", out.Session.ID, "status", res.StatusCode,
				"action", "hanging up: created without an answer")
			hctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if herr := c.HangupRetry(hctx, out.Session.ID, 3); herr != nil {
				c.event("live_fork_cleanup_failed", "session_id", out.Session.ID, "err", herr.Error())
			}
		}
		if out.Error != nil {
			return nil, fmt.Errorf("fork: %w", &apiError{
				Code:    out.Error.Code,
				Message: out.Error.Message,
				Type:    out.Error.Type,
			})
		}
		return nil, fmt.Errorf("fork: %w", &StatusError{Op: "fork", Status: res.StatusCode})
	}
	if err := c.recordOpened(out.Session.ID); err != nil {
		return nil, err
	}
	return &LiveSession{ID: out.Session.ID, Answer: out.Transport.SDP, Model: c.Model}, nil
}

func (c *LiveClient) post(ctx context.Context, path string, body []byte, ev string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, liveURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if err != nil {
		return fmt.Errorf("%s: %w", ev, err)
	}
	c.event(ev, "status", res.StatusCode, "body_chars", len(b))
	if res.StatusCode >= 300 {
		return fmt.Errorf("%s: %w", ev, &StatusError{Op: ev, Status: res.StatusCode})
	}
	return nil
}

// IdleGate closes a GPT-Live session after a period with no speech, because GPT-Live bills wall
// clock: "Active session time includes time when the user speaks, the assistant speaks, both are
// silent, or the backend is working", and "Closing saves $0.05 per minute of idle voice time".
// Muting the microphone does not help — the docs say it does not close the session.
//
// Call Touch() on every speech signal; onIdle fires once.
type IdleGate struct {
	mu     sync.Mutex
	last   time.Time
	after  time.Duration
	onIdle func(idleFor time.Duration)
	fired  bool
	stop   chan struct{}
}

// NewIdleGate starts the watchdog. after<=0 disables it.
func NewIdleGate(after time.Duration, onIdle func(idleFor time.Duration)) *IdleGate {
	g := &IdleGate{last: time.Now(), after: after, onIdle: onIdle, stop: make(chan struct{})}
	if after > 0 {
		go g.run()
	}
	return g
}

// Touch records activity.
func (g *IdleGate) Touch() {
	g.mu.Lock()
	g.last = time.Now()
	g.mu.Unlock()
}

// Stop ends the watchdog without firing.
func (g *IdleGate) Stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.fired {
		g.fired = true
		close(g.stop)
	}
}

func (g *IdleGate) run() {
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-tk.C:
		}
		g.mu.Lock()
		idle := time.Since(g.last)
		fire := !g.fired && idle >= g.after
		if fire {
			g.fired = true
			close(g.stop)
		}
		g.mu.Unlock()
		if fire {
			if g.onIdle != nil {
				g.onIdle(idle)
			}
			return
		}
	}
}

// EstimateLiveCostUSD returns the wall-clock cost of a GPT-Live session, including the 15 seconds
// billed at initialization (credited, so the floor is 15 s rather than the elapsed time).
func EstimateLiveCostUSD(elapsed time.Duration) float64 {
	sec := elapsed.Seconds()
	if sec < liveInitBilledSeconds {
		sec = liveInitBilledSeconds
	}
	return sec / 60.0 * 0.05
}
