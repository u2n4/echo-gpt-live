package bridge

// Measuring how long a forward actually takes.
//
// The arrival meter marks a packet when the application reads it, not when it hits the wire. Codex
// pointed out what that hides: the reader forwards synchronously, so a slow write in one iteration
// delays the next read, and a stream that arrives perfectly evenly can still be MEASURED as bursty.
// A long gap followed by several short ones is exactly what a stalled write looks like, and it is
// also exactly what genuine burst delivery looks like.
//
// The two are told apart by timing the write itself. If forwards complete in tens of microseconds,
// the burstiness is real and belongs to the sender. If they take tens of milliseconds, the
// measurement was reporting this bridge back to itself.

import (
	"sort"
	"sync"
	"time"
)

// durMeter summarises a duration once per window.
type durMeter struct {
	mu     sync.Mutex
	ev     Events
	label  string
	from   time.Time
	all    []time.Duration
	slowN  int
	window time.Duration
	slow   time.Duration
}

func newDurMeter(ev Events, label string) *durMeter {
	if ev == nil {
		ev = func(string, ...any) {}
	}
	return &durMeter{
		ev:     ev,
		label:  label,
		all:    make([]time.Duration, 0, 512),
		window: paceWindow,
		slow:   5 * time.Millisecond, // a fifth of a frame; anything past this competes with the audio
	}
}

// observe records one completed forward.
func (d *durMeter) observe(took time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if d.from.IsZero() {
		d.from = now
	}
	d.all = append(d.all, took)
	if took >= d.slow {
		d.slowN++
	}
	if now.Sub(d.from) >= d.window {
		d.flush(now)
	}
}

func (d *durMeter) flush(now time.Time) {
	n := len(d.all)
	if n == 0 {
		d.from = now
		return
	}
	s := make([]time.Duration, n)
	copy(s, d.all)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	ms := func(x time.Duration) float64 { return float64(x.Microseconds()) / 1000.0 }
	d.ev("write_"+d.label,
		"n", n,
		"median_ms", ms(s[n/2]),
		"p95_ms", ms(s[(n*95+99)/100-1]),
		"max_ms", ms(s[n-1]),
		"slow_frac", float64(d.slowN)/float64(n),
		"note", "a forward slower than 5 ms delays the next read and shows up as a burst that is not one")
	d.all = d.all[:0]
	d.slowN = 0
	d.from = now
}

func (d *durMeter) done() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.all) > 0 {
		d.flush(time.Now())
	}
}

// WriteMeter is the exported handle: one per forwarding direction.
type WriteMeter struct{ m *durMeter }

var (
	meterMu  sync.Mutex
	allMeter []*durMeter
)

// NewWriteMeter creates a meter and registers it so a teardown can flush what it holds.
func NewWriteMeter(ev Events, label string) *WriteMeter {
	m := newDurMeter(ev, label)
	meterMu.Lock()
	allMeter = append(allMeter, m)
	meterMu.Unlock()
	return &WriteMeter{m: m}
}

// Observe records one completed forward. Safe on a nil meter.
func (w *WriteMeter) Observe(took time.Duration) {
	if w == nil || w.m == nil {
		return
	}
	w.m.observe(took)
}

// FlushWriteMeters reports whatever every meter still holds, so a short call still says how long
// its forwards took.
func FlushWriteMeters() {
	meterMu.Lock()
	ms := append([]*durMeter(nil), allMeter...)
	allMeter = nil
	meterMu.Unlock()
	for _, m := range ms {
		m.done()
	}
}
