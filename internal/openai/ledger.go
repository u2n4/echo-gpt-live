package openai

// Money safety for GPT-Live, which bills wall clock: "Active session time includes time when the
// user speaks, the assistant speaks, both are silent, or the backend is working", and "Muting
// microphone input does not close the session."
//
// A clean shutdown path is not enough. These are the ways a session can keep billing after we
// stop caring about it, and what closes each one:
//
//	Alexa sends SessionDisconnected   -> the bridge hangs up at once (measured: same second)
//	nobody speaks                     -> IdleGate hangs up
//	the media transport dies          -> the bridge hangs up on ICE failed/disconnected
//	the session simply runs long      -> MaxLiveAge hangs up regardless of anything else
//	hangup itself fails (401, network)-> retried with backoff, and then again by the periodic
//	                                     sweep for as long as this process runs
//	the process is SIGKILLed or panics-> THIS FILE: the id is on disk, and the next start
//	                                     hangs up whatever was left open and charges its time
//
// The ledger is a plain text file, one session per line: the id, when it was opened, and the pid
// that opened it, tab-separated. It is deliberately dumb: anything that can read a file can
// recover the money. Lines with only an id (the first format) still read.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Ledger records GPT-Live sessions that are open and therefore billing.
type Ledger struct {
	path string
	mu   sync.Mutex
	ev   func(string, ...any)
}

// Entry is one open session as the ledger knows it.
type Entry struct {
	ID      string
	Started time.Time // zero for a line written by the first format
	PID     int       // 0 for a line written by the first format
}

// NewLedger opens (creating the directory if needed) the ledger at path.
func NewLedger(path string, ev func(string, ...any)) (*Ledger, error) {
	if ev == nil {
		ev = func(string, ...any) {}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &Ledger{path: path, ev: ev}
	l.cleanStaleTmps()
	return l, nil
}

// cleanStaleTmps removes the per-pid write files a crash left between Close and Rename. Only a
// dead owner's litter is touched: never this process's, never a live sibling's in-flight file.
// They were never read, so this is tidiness, not money.
func (l *Ledger) cleanStaleTmps() {
	matches, _ := filepath.Glob(l.path + ".*.tmp")
	for _, m := range matches {
		base := strings.TrimSuffix(strings.TrimPrefix(m, l.path+"."), ".tmp")
		pid, err := strconv.Atoi(base)
		if err != nil || pid == os.Getpid() || pidAlive(pid) {
			continue
		}
		_ = os.Remove(m)
	}
}

// Add records a session as open. Called immediately after creation, before anything else can fail.
// An error means it is NOT on disk, and a session that no restart could recover is one the caller
// must hang up now.
func (l *Ledger) Add(id string) error {
	if id == "" {
		return nil
	}
	return l.locked(func() error {
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			l.ev("ledger_add_failed", "id", id, "err", err.Error())
			return err
		}
		defer f.Close()
		line := fmt.Sprintf("%s\t%d\t%d\n", id, time.Now().Unix(), os.Getpid())
		if _, err := f.WriteString(line); err != nil {
			l.ev("ledger_add_failed", "id", id, "err", err.Error())
			return err
		}
		// fsync: a crash a millisecond from now must still leave the id recoverable.
		if err := f.Sync(); err != nil {
			l.ev("ledger_sync_failed", "id", id, "err", err.Error())
			return err
		}
		return nil
	})
}

// Remove drops a session from the ledger after it has been confirmed hung up.
func (l *Ledger) Remove(id string) {
	if err := l.locked(func() error {
		es, err := l.read()
		if err != nil {
			return err
		}
		var keep []Entry
		for _, e := range es {
			if e.ID != id {
				keep = append(keep, e)
			}
		}
		l.write(keep)
		return nil
	}); err != nil {
		l.ev("ledger_remove_failed", "id", id, "err", err.Error())
	}
}

// locked runs fn with the ledger held against every OTHER PROCESS as well as this one. The
// in-process mutex orders goroutines; the flock on a sidecar file orders the live bridge, a test
// bridge started by scripts/prove.sh with the default --ledger, and a sweep in either, which
// otherwise interleave Remove's read-rewrite with another's append and publish a file without the
// session that was just opened (found by the 2026-09-16 fleet; budget.go had the same fix from
// Codex two days earlier). A lock that cannot be taken is reported as an error, never skipped: a
// session recorded nowhere is money nobody will recover.
func (l *Ledger) locked(fn func() error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	lock, err := os.OpenFile(l.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		l.ev("ledger_lock_failed", "err", err.Error())
		return err
	}
	defer lock.Close()
	if err := lockFile(lock); err != nil {
		l.ev("ledger_lock_failed", "err", err.Error())
		return err
	}
	defer func() { _ = unlockFile(lock) }()
	return fn()
}

// Open lists the ids of the sessions the ledger believes are still billing.
func (l *Ledger) Open() []string {
	var ids []string
	for _, e := range l.Entries() {
		ids = append(ids, e.ID)
	}
	return ids
}

// Entries lists the sessions the ledger believes are still billing, with what it knows of each.
func (l *Ledger) Entries() []Entry {
	var es []Entry
	if err := l.locked(func() error {
		var err error
		es, err = l.read()
		return err
	}); err != nil {
		l.ev("ledger_entries_failed", "err", err.Error())
		return nil
	}
	return es
}

func (l *Ledger) read() ([]Entry, error) {
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Entry
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(strings.TrimSpace(sc.Text()), "\t")
		id := strings.TrimSpace(fields[0])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		e := Entry{ID: id}
		if len(fields) > 1 {
			if n, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64); err == nil && n > 0 {
				e.Started = time.Unix(n, 0)
			}
		}
		if len(fields) > 2 {
			// Old lines have no PID; a bad number means zero, documented here, not silent.
			if n, err := strconv.Atoi(strings.TrimSpace(fields[2])); err == nil {
				e.PID = n
			}
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

func (l *Ledger) write(es []Entry) {
	// per-pid temp name: two processes are serialised by the flock, but a crash between the
	// write and the rename must not leave a half-written file for the other one to rename
	tmp := fmt.Sprintf("%s.%d.tmp", l.path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		l.ev("ledger_write_failed", "err", err.Error())
		return
	}
	for _, e := range es {
		var started int64
		if !e.Started.IsZero() {
			started = e.Started.Unix()
		}
		if _, err := fmt.Fprintf(f, "%s\t%d\t%d\n", e.ID, started, e.PID); err != nil {
			l.ev("ledger_write_failed", "err", err.Error())
			_ = f.Close()
			_ = os.Remove(tmp)
			return
		}
	}
	if err := f.Sync(); err != nil {
		l.ev("ledger_write_failed", "err", err.Error())
		_ = f.Close()
		_ = os.Remove(tmp)
		return
	}
	if err := f.Close(); err != nil {
		l.ev("ledger_write_failed", "err", err.Error())
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, l.path); err != nil {
		l.ev("ledger_rename_failed", "err", err.Error())
	}
}

// SweepOptions says which entries a sweep may touch, and what to do when one is closed.
type SweepOptions struct {
	// Except names the session this process is using right now; it is never touched.
	Except func() string
	// MinAge leaves alone entries younger than this: they are being set up by a dial that has not
	// yet handed its leg to anyone. Zero sweeps regardless of age.
	MinAge time.Duration
	// MaxLive bounds the trust in a living owner pid. Pids are reused: a session left by a
	// crashed bridge whose pid a long-lived stranger inherited would otherwise be skipped by
	// every sweep, billing until OpenAI's own limit. No session outlives its owner's hard cap,
	// so past MaxLive plus a margin the pid is not believed. Zero keeps the old skip. Muse found it.
	MaxLive time.Duration
	// OnClosed is told about each session the sweep hung up: its id, when it was opened (zero if
	// unknown), and whether THIS process opened it. A dead process's session was billing from
	// Started until now and nobody charged it; one of our own was already charged when its leg
	// closed and only failed to hang up.
	OnClosed func(id string, started time.Time, ownPID bool)
}

// Sweep hangs up every session left in the ledger, with no exceptions. It is what a fresh start
// runs: anything listed belongs to a process that died without closing, and is still on the clock.
func (l *Ledger) Sweep(ctx context.Context, c *LiveClient) {
	l.SweepWith(ctx, c, SweepOptions{})
}

// SweepWith hangs up every session in the ledger that the options allow. A session another
// process on this machine is still running is left to it.
//
// A 404 (already gone) counts as success: the goal is that nothing bills, not that we were the
// one to end it.
func (l *Ledger) SweepWith(ctx context.Context, c *LiveClient, o SweepOptions) {
	es := l.Entries()
	if len(es) == 0 {
		return
	}
	except := ""
	if o.Except != nil {
		except = o.Except()
	}
	self := os.Getpid()
	var due []Entry
	for _, e := range es {
		switch {
		case e.ID == except:
			continue
		case o.MinAge > 0 && !e.Started.IsZero() && time.Since(e.Started) < o.MinAge:
			continue
		case e.PID != 0 && e.PID != self && pidAlive(e.PID) && !o.pidTrustExpired(e):
			// Not ours and its owner is still running: rtcdiag or another bridge. The first sweep
			// hung these up too, which is how a diagnostic's session could be ended by the
			// service starting next to it.
			l.ev("ledger_entry_owned_by_live_process", "id", e.ID, "pid", e.PID)
			continue
		}
		due = append(due, e)
	}
	if len(due) == 0 {
		return
	}
	l.ev("ledger_sweep_start", "orphans", len(due), "note", "sessions left open and still billing")
	for _, e := range due {
		err := c.HangupRetry(ctx, e.ID, 3)
		if status, ok := httpStatus(err); err == nil || (ok && status == 404) {
			l.Remove(e.ID)
			l.ev("ledger_orphan_closed", "id", e.ID, "err_if_any", errText(err),
				"open_for_s", openFor(e.Started).Seconds(), "own_pid", e.PID == self)
			if o.OnClosed != nil {
				o.OnClosed(e.ID, e.Started, e.PID == self)
			}
			continue
		}
		l.ev("ledger_orphan_close_failed", "id", e.ID, "err", err.Error(),
			"note", "left in the ledger for the next sweep")
	}
}

// pidTrustExpired reports whether an entry is too old for its living owner pid to be believed.
func (o SweepOptions) pidTrustExpired(e Entry) bool {
	if o.MaxLive <= 0 || e.Started.IsZero() {
		return false
	}
	return time.Since(e.Started) > o.MaxLive+time.Minute
}

func openFor(started time.Time) time.Duration {
	if started.IsZero() {
		return 0
	}
	return time.Since(started)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// HangupRetry hangs up a session, retrying transient failures. A failed hangup leaves money
// running, so it is worth more than one attempt — but an auth failure will not improve with
// retries, so it stops early on those.
func (c *LiveClient) HangupRetry(ctx context.Context, sessionID string, attempts int) error {
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				if last != nil {
					return errors.Join(fmt.Errorf("hangup %s", sessionID), ctx.Err(), last)
				}
				return fmt.Errorf("hangup %s: %w", sessionID, ctx.Err())
			case <-time.After(time.Duration(i) * 500 * time.Millisecond):
			}
		}
		err := c.Hangup(ctx, sessionID)
		if err == nil {
			return nil
		}
		last = err
		if status, ok := httpStatus(err); ok && status == 404 {
			return nil // already gone: there is no paid session left to close
		}
		// An invalid or revoked key will not become valid on retry; say so once and stop.
		if status, ok := httpStatus(err); ok && status == 401 {
			c.event("live_hangup_auth_failed", "id", sessionID,
				"note", "the session may keep billing until its own limit; fix the API key and sweep")
			return err
		}
	}
	return last
}
