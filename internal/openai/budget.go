package openai

// A hard ceiling on what this machine may spend on GPT-Live in a day.
//
// Written after the owner opened his usage page and found $0.633 he did not recognise. Nothing had
// leaked: every session had been closed and the ledger was empty. The money was 39 sessions
// totalling sixteen minutes, and 34 of those were automated test runs, not his calls. A guard that
// only stops runaway sessions does not stop that. Nothing was counting the total.
//
// So the total is counted, on disk, per UTC day, across every process that uses this package. Once
// the day's spend reaches the ceiling, creating a session fails with an error that says so instead
// of quietly costing more. Diagnostics get a small ceiling of their own and cannot eat the
// assistant's.
//
// The file is written with fsync on every change, because a budget that a crash can forget is not a
// budget.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LiveRatePerMinuteUSD is what gpt-live-1 charges for a connected session, silence included.
const LiveRatePerMinuteUSD = 0.05

// ErrBudgetSpent is returned when the day's ceiling has been reached.
var ErrBudgetSpent = errors.New("daily GPT-Live budget spent")

// ErrBudgetUnreadable is returned when the counter cannot be read, and it REFUSES the session
// rather than allowing it. A ceiling that fails open is not a ceiling, and this file exists
// because money was spent while nothing was counting.
var ErrBudgetUnreadable = errors.New("daily GPT-Live budget state unreadable")

// Budget tracks spend per UTC day in a file shared by every process on this machine.
type Budget struct {
	mu    sync.Mutex // orders this process; the file lock orders processes
	path  string
	limit float64
	ev    func(string, ...any)
	// Reserve is the most one session can cost (its hard time cap), and admission requires room
	// for all of it. Checked against what was already spent alone, a $2 ceiling let the last
	// session start at $1.99 and run to $2.49. Codex found it. Zero keeps the old check.
	Reserve time.Duration
}

// hold is one dial that passed Check and has not yet been recorded by Opened or released by
// Abandon. It lives in the FILE, under the same lock as the number it counts against: held in
// memory, two processes on one machine (the bridge and a diagnostic share the default file)
// each admitted a session with room for one. Muse found it.
type hold struct {
	PID int       `json:"pid"`
	At  time.Time `json:"at"`
}

// holdFor is how long a hold is believed. A dial takes seconds; a process that died between
// Check and Opened would otherwise block every later admission until tomorrow.
const holdFor = 2 * time.Minute

type budgetFile struct {
	Day      string  `json:"day"`      // UTC date, YYYY-MM-DD
	Seconds  float64 `json:"seconds"`  // connected seconds charged today
	Sessions int     `json:"sessions"` // sessions opened today
	Updated  string  `json:"updated"`
	Holds    []hold  `json:"holds,omitempty"` // dials admitted and not yet opened or abandoned
}

// live keeps the holds that still mean something: recent, and owned by a process that exists.
func (f *budgetFile) live(now time.Time) []hold {
	var keep []hold
	for _, h := range f.Holds {
		if now.Sub(h.At) < holdFor && (h.PID == 0 || pidAlive(h.PID)) {
			keep = append(keep, h)
		}
	}
	return keep
}

// release drops one hold of this process, the oldest.
func (f *budgetFile) release(pid int) {
	for i, h := range f.Holds {
		if h.PID == pid {
			f.Holds = append(f.Holds[:i], f.Holds[i+1:]...)
			return
		}
	}
}

// NewBudget opens (or creates) the shared counter. A limit of 0 disables enforcement but keeps
// counting, so the number is always available even when nobody is being stopped.
func NewBudget(path string, limitUSD float64, ev func(string, ...any)) (*Budget, error) {
	if ev == nil {
		ev = func(string, ...any) {}
	}
	if path == "" {
		return nil, errors.New("budget: no path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	b := &Budget{path: path, limit: limitUSD, ev: ev}
	spent, sessions := b.Report()
	ev("budget_open", "path", path, "limit_usd", limitUSD,
		"spent_today_usd", spent, "sessions_today", sessions)
	return b, nil
}

func usd(seconds float64) float64 { return seconds / 60 * LiveRatePerMinuteUSD }

func today() string { return time.Now().UTC().Format("2006-01-02") }

// withLock runs fn against the on-disk counter while holding an exclusive lock on it, so two
// processes cannot both read, add, and write back over each other. The first version did a plain
// read-modify-write behind an in-process mutex, which orders nothing between processes, and the
// test that claimed otherwise only ran two objects in sequence. Codex found both.
//
// Returning fn's bool says whether the record changed and needs writing back.
func (b *Budget) withLock(fn func(f *budgetFile) bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	lock, err := os.OpenFile(b.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBudgetUnreadable, err)
	}
	defer lock.Close()
	if err := lockFile(lock); err != nil {
		return fmt.Errorf("%w: %w", ErrBudgetUnreadable, err)
	}
	defer func() { _ = unlockFile(lock) }()

	var f budgetFile
	raw, rerr := os.ReadFile(b.path)
	switch {
	case rerr == nil:
		if json.Unmarshal(raw, &f) != nil {
			// Corrupt state is not the same as no spending, and guessing the cheaper of the two is
			// how a ceiling stops being one.
			return fmt.Errorf("%w: %s is not valid JSON", ErrBudgetUnreadable, b.path)
		}
	case os.IsNotExist(rerr):
		f = budgetFile{Day: today()}
	default:
		return fmt.Errorf("%w: %w", ErrBudgetUnreadable, rerr)
	}
	if f.Day != today() { // a new day starts at zero
		f = budgetFile{Day: today()}
	}

	if !fn(&f) {
		return nil
	}

	f.Day = today()
	f.Updated = time.Now().UTC().Format(time.RFC3339)
	out, err := json.Marshal(f)
	if err != nil {
		return err
	}
	// A per-process temporary name: one shared ".tmp" is two writers overwriting each other.
	tmp := fmt.Sprintf("%s.%d.tmp", b.path, os.Getpid())
	fh, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("budget write: %w", err)
	}
	if _, werr := fh.Write(out); werr != nil {
		_ = fh.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("budget write: %w", werr)
	}
	if serr := fh.Sync(); serr != nil {
		_ = fh.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("budget sync: %w", serr)
	}
	_ = fh.Close()
	if nerr := os.Rename(tmp, b.path); nerr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("budget rename: %w", nerr)
	}
	if err := syncDirectory(filepath.Dir(b.path)); err != nil {
		return fmt.Errorf("budget directory sync: %w", err)
	}
	return nil
}

// Check reports whether another session may be opened. A counter that cannot be read refuses.
func (b *Budget) Check() error {
	if b == nil {
		return nil
	}
	var spent float64
	var sessions, inFlight int
	// Dials admitted but not yet recorded count against the ceiling too. The check and the record
	// are two file transactions with a network round trip between them, so two dials admitted
	// together (a directive delivered twice, 1.17 s apart) both saw the same spend and both
	// passed. The admission is written into the file, in the same transaction that read the
	// spend, and released by Opened or Abandon.
	worst := usd(b.Reserve.Seconds())
	admitted := false
	if err := b.withLock(func(f *budgetFile) bool {
		f.Holds = f.live(time.Now())
		spent, sessions, inFlight = usd(f.Seconds), f.Sessions, len(f.Holds)
		if b.limit <= 0 {
			return false
		}
		if spent >= b.limit || spent+worst*float64(inFlight+1) > b.limit {
			return false
		}
		f.Holds = append(f.Holds, hold{PID: os.Getpid(), At: time.Now()})
		admitted = true
		return true
	}); err != nil {
		b.ev("budget_unreadable", "err", err.Error(),
			"note", "refusing to open a session while the spend counter cannot be read")
		return err
	}
	if b.limit <= 0 || admitted {
		return nil
	}
	b.ev("budget_exhausted", "spent_today_usd", spent, "limit_usd", b.limit,
		"sessions_today", sessions, "next_session_could_cost_usd", worst, "dials_in_flight", inFlight,
		"note", "no new GPT-Live session will be opened until tomorrow or the limit is raised")
	return fmt.Errorf("%w: $%.3f of $%.2f used today across %d sessions, and the next could cost $%.2f",
		ErrBudgetSpent, spent, b.limit, sessions, worst)
}

// Abandon releases an admission whose session was never created. Opened releases the others.
func (b *Budget) Abandon() {
	if b == nil || b.limit <= 0 {
		return
	}
	if err := b.withLock(func(f *budgetFile) bool {
		f.release(os.Getpid())
		return true
	}); err != nil {
		b.ev("budget_write_failed", "err", err.Error(), "note", "an admission could not be released; it expires on its own")
	}
}

// Opened records that a session was created. The init charge is counted immediately, so a process
// that dies before the session closes still leaves the cost on the record. An error means the
// record was NOT written: the caller has a billing session the counter does not know about, and
// the only honest thing to do with it is hang it up.
func (b *Budget) Opened() error {
	if b == nil {
		return nil
	}
	if err := b.withLock(func(f *budgetFile) bool {
		f.release(os.Getpid()) // an Opened without a Check releases nothing
		f.Sessions++
		f.Seconds += liveInitBilledSeconds
		return true
	}); err != nil {
		b.ev("budget_write_failed", "err", err.Error(),
			"note", "a session was opened that the counter does not know about")
		return err
	}
	return nil
}

// Spent records a finished session's connected time, less the init already counted.
func (b *Budget) Spent(d time.Duration) {
	if b == nil {
		return
	}
	var spent float64
	var sessions int
	if err := b.withLock(func(f *budgetFile) bool {
		if extra := d.Seconds() - liveInitBilledSeconds; extra > 0 {
			f.Seconds += extra
		}
		spent, sessions = usd(f.Seconds), f.Sessions
		return true
	}); err != nil {
		b.ev("budget_write_failed", "err", err.Error())
		return
	}
	b.ev("budget_spent", "session_s", d.Seconds(),
		"spent_today_usd", spent, "limit_usd", b.limit, "sessions_today", sessions)
}

// Report returns what has been spent today.
func (b *Budget) Report() (spentUSD float64, sessions int) {
	if b == nil {
		return 0, 0
	}
	if err := b.withLock(func(f *budgetFile) bool {
		spentUSD, sessions = usd(f.Seconds), f.Sessions
		return false
	}); err != nil {
		b.ev("budget_report_failed", "err", err.Error())
		return 0, 0
	}
	return spentUSD, sessions
}
