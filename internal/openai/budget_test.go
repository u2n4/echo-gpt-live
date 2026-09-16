package openai

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The ceiling has to hold, because it exists to stop spending nobody authorised.
func TestBudgetStopsNewSessionsOnceSpent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spend.json")
	b, err := NewBudget(p, 0.10, nil) // two minutes of voice
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Check(); err != nil {
		t.Fatalf("a fresh day must allow a session: %v", err)
	}
	b.Opened()
	b.Spent(90 * time.Second)
	if err := b.Check(); err != nil {
		t.Fatalf("$0.075 of $0.10 must still allow a session: %v", err)
	}
	b.Opened()
	b.Spent(60 * time.Second)
	err = b.Check()
	if !errors.Is(err, ErrBudgetSpent) {
		t.Fatalf("past the ceiling the answer must be ErrBudgetSpent, got %v", err)
	}
	spent, sessions := b.Report()
	if sessions != 2 {
		t.Fatalf("sessions = %d, want 2", sessions)
	}
	if spent < 0.10 {
		t.Fatalf("spent = %.3f, want at least the limit", spent)
	}
}

// The counter is shared by every process on the machine, which is the whole point: the money that
// surprised the owner was spent by diagnostics, not by the assistant.
func TestBudgetIsSharedAcrossProcesses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spend.json")
	first, err := NewBudget(p, 0.05, nil)
	if err != nil {
		t.Fatal(err)
	}
	first.Opened()
	first.Spent(70 * time.Second)

	second, err := NewBudget(p, 0.05, nil) // a different process, same machine
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Check(); !errors.Is(err, ErrBudgetSpent) {
		t.Fatalf("a second process must see the first one's spending, got %v", err)
	}
}

// The init charge is counted when the session is created, so a process killed before the session
// closes still leaves the cost on the record.
func TestBudgetCountsTheInitChargeImmediately(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spend.json")
	b, _ := NewBudget(p, 0, nil)
	b.Opened() // and then the process dies; Spent is never called
	spent, sessions := b.Report()
	if sessions != 1 {
		t.Fatalf("sessions = %d, want 1", sessions)
	}
	want := usd(liveInitBilledSeconds)
	if spent < want*0.99 || spent > want*1.01 {
		t.Fatalf("spent = %.4f, want about %.4f", spent, want)
	}
}

// A ceiling has to hold room for the session about to start, not only count what is gone: with a
// $1 limit and sessions capped at 10 minutes ($0.50), the last one may start at $0.40, not at $0.99.
func TestBudgetRefusesWhenTheNextSessionCouldPassTheLimit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spend.json")
	b, _ := NewBudget(p, 1.0, nil)
	b.Reserve = 10 * time.Minute
	if err := b.Opened(); err != nil {
		t.Fatal(err)
	}
	b.Spent(8 * time.Minute) // $0.40 on the record
	if err := b.Check(); err != nil {
		t.Fatalf("$0.40 spent, $0.50 worst case: should admit, got %v", err)
	}
	if err := b.Opened(); err != nil {
		t.Fatal(err)
	}
	b.Spent(4 * time.Minute) // $0.60 on the record
	if err := b.Check(); err == nil {
		t.Fatal("$0.60 spent plus a $0.50 session passes $1; must refuse")
	}
	b.Reserve = 0
	if err := b.Check(); err != nil {
		t.Fatalf("with no reserve the old check applies and $0.60 < $1: %v", err)
	}
}

// Two dials admitted together must not both pass a ceiling with room for one: the admission is
// held until the session is recorded (Opened) or given up (Abandon).
func TestBudgetCountsDialsInFlightAgainstTheCeiling(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spend.json")
	b, _ := NewBudget(p, 1.0, nil)
	b.Reserve = 12 * time.Minute // $0.60 each; the $1 ceiling holds one, not two
	if err := b.Check(); err != nil {
		t.Fatalf("the first dial must be admitted: %v", err)
	}
	if err := b.Check(); err == nil {
		t.Fatal("the second dial, admitted while the first is still being created, would pass the ceiling")
	}
	b.Abandon() // the first create failed; its admission goes back
	if err := b.Check(); err != nil {
		t.Fatalf("after Abandon the slot is free again: %v", err)
	}
	if err := b.Opened(); err != nil { // recorded: the admission is released, $0.0125 is on disk
		t.Fatal(err)
	}
	if err := b.Check(); err != nil {
		t.Fatalf("$0.0125 spent plus one $0.60 reserve is under $1: %v", err)
	}
	if err := b.Check(); err == nil {
		t.Fatal("a second reserve on top of the first passes $1")
	}
}

// A limit of zero means "count, do not stop", so the number is available even when nobody is being
// refused.
func TestBudgetWithNoLimitStillCounts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spend.json")
	b, _ := NewBudget(p, 0, nil)
	b.Opened()
	b.Spent(10 * time.Minute)
	if err := b.Check(); err != nil {
		t.Fatalf("a zero limit must never refuse: %v", err)
	}
	if spent, _ := b.Report(); spent < 0.4 {
		t.Fatalf("ten minutes should have been counted, got $%.3f", spent)
	}
}

// Yesterday's spending must not block today.
func TestBudgetResetsWithTheDay(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "spend.json")
	stale := `{"day":"2000-01-01","seconds":99999,"sessions":42}`
	if err := os.WriteFile(p, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := NewBudget(p, 0.10, nil)
	if err := b.Check(); err != nil {
		t.Fatalf("an old day must not count against today: %v", err)
	}
	if _, sessions := b.Report(); sessions != 0 {
		t.Fatalf("sessions = %d, want 0 for a new day", sessions)
	}
}

// A nil budget is how a caller opts out, so nothing may panic on one.
func TestNilBudgetIsSafe(t *testing.T) {
	var b *Budget
	if err := b.Check(); err != nil {
		t.Fatal(err)
	}
	b.Opened()
	b.Spent(time.Minute)
	if spent, sessions := b.Report(); spent != 0 || sessions != 0 {
		t.Fatal("a nil budget counts nothing")
	}
}

// A counter that cannot be read must REFUSE the session. Failing open is how a ceiling stops being
// one, and this whole file exists because money was spent while nothing was counting.
func TestBudgetRefusesWhenTheCounterIsUnreadable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spend.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &Budget{path: p, limit: 1.0, ev: func(string, ...any) {}}
	err := b.Check()
	if !errors.Is(err, ErrBudgetUnreadable) {
		t.Fatalf("corrupt state must refuse, got %v", err)
	}
}

// The lock has to hold between concurrent writers, which the earlier test did not exercise: it ran
// two objects in sequence and proved only that a file can be read twice.
func TestBudgetSurvivesConcurrentWriters(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spend.json")
	const writers, each = 8, 25
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := NewBudget(p, 0, nil) // a separate object, as a separate process would have
			if err != nil {
				t.Error(err)
				return
			}
			for j := 0; j < each; j++ {
				b.Spent(time.Duration(liveInitBilledSeconds+1) * time.Second)
			}
		}()
	}
	wg.Wait()

	b, _ := NewBudget(p, 0, nil)
	spent, _ := b.Report()
	want := usd(float64(writers * each)) // one second counted per call
	if spent < want*0.999 || spent > want*1.001 {
		t.Fatalf("spent $%.4f, want $%.4f: concurrent writers lost updates", spent, want)
	}
}
