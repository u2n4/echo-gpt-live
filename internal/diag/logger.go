// Package diag provides structured JSON-lines event logging and run directories
// for the Echo <-> bridge diagnostics. Every event carries a UTC wall-clock
// timestamp (for correlation with the Alexa Smart Home Debugger and Lambda logs)
// and a monotonic millisecond offset since process start (for intervals).
package diag

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Logger writes one JSON object per line to a file and mirrors it to stdout.
type Logger struct {
	mu      sync.Mutex
	f       *os.File
	start   time.Time
	RunDir  string
	session string
}

// NewLogger creates runs/<runID>/events.jsonl and returns the logger.
func NewLogger(runsDir, runID string) (*Logger, error) {
	dir := filepath.Join(runsDir, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Logger{f: f, start: time.Now(), RunDir: dir}, nil
}

// SetSession tags subsequent events with a session id (first 8 chars).
func (l *Logger) SetSession(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(id) > 8 {
		id = id[:8]
	}
	l.session = id
}

// Event writes an event with arbitrary key/value fields.
func (l *Logger) Event(ev string, kv ...any) {
	rec := map[string]any{
		"t":       time.Now().UTC().Format(time.RFC3339Nano),
		"mono_ms": time.Since(l.start).Milliseconds(),
		"ev":      ev,
	}
	l.mu.Lock()
	if l.session != "" {
		rec["sess"] = l.session
	}
	l.mu.Unlock()
	for i := 0; i+1 < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			k = fmt.Sprint(kv[i])
		}
		rec[k] = kv[i+1]
	}
	b, err := json.Marshal(rec)
	if err != nil {
		b = []byte(fmt.Sprintf(`{"ev":"log_marshal_error","err":%q}`, err.Error()))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.f.Write(append(b, '\n'))
	_, _ = os.Stdout.Write(append(b, '\n'))
}

// Since returns milliseconds since process start (monotonic).
func (l *Logger) Since() int64 { return time.Since(l.start).Milliseconds() }

// Close flushes and closes the file.
func (l *Logger) Close() error { return l.f.Close() }

// SaveSDP stores an SDP under the run dir with ICE credentials redacted.
func (l *Logger) SaveSDP(name, sdp string) {
	var out []string
	for _, ln := range strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n") {
		low := strings.ToLower(ln)
		if strings.HasPrefix(low, "a=ice-ufrag") || strings.HasPrefix(low, "a=ice-pwd") {
			ln = ln[:strings.Index(ln, ":")+1] + "REDACTED"
		}
		out = append(out, ln)
	}
	_ = os.WriteFile(filepath.Join(l.RunDir, name), []byte(strings.Join(out, "\n")), 0o600)
}
