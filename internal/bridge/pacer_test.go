package bridge

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// atomicAdd keeps the concurrency assertion in the racing test readable.
func atomicAdd(p *int32, d int32) int32 { return atomic.AddInt32(p, d) }

// collector records what left the pacer and when.
type collector struct {
	mu  sync.Mutex
	seq []uint16
	at  []time.Time
}

func newCollector() *collector { return &collector{} }

func (c *collector) write(p *rtp.Packet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq = append(c.seq, p.SequenceNumber)
	c.at = append(c.at, time.Now())
}

func (c *collector) snapshot() ([]uint16, []time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]uint16(nil), c.seq...), append([]time.Time(nil), c.at...)
}

// closedEvents captures the pacer_closed report so the counters it publishes can be asserted.
func closedEvents(dst *map[string]any) Events {
	var mu sync.Mutex
	return func(ev string, kv ...any) {
		if ev != "pacer_closed" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		*dst = m
	}
}

func pkt(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: ts, SSRC: 7},
		Payload: []byte{0x78, 0x01, 0x02},
	}
}

func waitFor(c *collector, n int, d time.Duration) []time.Time {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if s, at := c.snapshot(); len(s) >= n {
			return at
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, at := c.snapshot()
	return at
}

// The point of the whole thing: a burst arriving all at once must leave 20 ms apart, because that is
// what its own timestamps say, and that is what stops the far jitter buffer accelerating.
func TestPacerSpreadsABurstBackToTwentyMilliseconds(t *testing.T) {
	c := newCollector()
	p := NewPacer(40*time.Millisecond, c.write, nil)
	if p == nil {
		t.Fatal("pacer should have been built")
	}
	defer p.Close()

	for i := 0; i < 10; i++ { // ten packets one frame apart, all handed over in the same instant
		p.Write(pkt(uint16(i), uint32(i)*960))
	}
	at := waitFor(c, 10, 3*time.Second)

	seq, _ := c.snapshot()
	if len(seq) != 10 {
		t.Fatalf("only %d of 10 packets came out", len(seq))
	}
	for i, s := range seq {
		if s != uint16(i) {
			t.Fatalf("order changed at %d: got seq %d", i, s)
		}
	}
	if span := at[9].Sub(at[0]); span < 150*time.Millisecond || span > 260*time.Millisecond {
		t.Fatalf("ten packets 20 ms apart left over %v; they were meant to be spread back out", span)
	}
	for i := 1; i < len(at); i++ {
		if g := at[i].Sub(at[i-1]); g < 8*time.Millisecond {
			t.Fatalf("packets %d and %d left %v apart; the burst was not smoothed", i-1, i, g)
		}
	}
}

// Silence between sentences must cost nothing. A gap in the timestamps is real elapsed time, so the
// packet after it is simply due later and must go straight out, not be held again.
func TestPacerFollowsATimestampGapWithoutStalling(t *testing.T) {
	c := newCollector()
	var closed map[string]any
	p := NewPacer(30*time.Millisecond, c.write, closedEvents(&closed))

	p.Write(pkt(0, 0))
	time.Sleep(260 * time.Millisecond)
	p.Write(pkt(1, 960*11)) // 220 ms later in stream time, after 260 ms of real time
	at := waitFor(c, 2, 2*time.Second)
	if len(at) != 2 {
		t.Fatalf("expected both packets out, got %d", len(at))
	}
	if gap := at[1].Sub(at[0]); gap > 400*time.Millisecond {
		t.Fatalf("the packet after the silence was held %v; it was already due", gap)
	}
	p.Close()
	if closed["timestamp_jumps"] != int64(0) {
		t.Fatalf("an ordinary silence was read as a jump: %v", closed["timestamp_jumps"])
	}
}

// A timestamp jump the schedule cannot explain must restart it rather than hold audio for most of a
// second. Holding would be worse than the unevenness this exists to fix.
func TestPacerRestartsOnAnImpossibleJump(t *testing.T) {
	c := newCollector()
	var closed map[string]any
	p := NewPacer(20*time.Millisecond, c.write, closedEvents(&closed))

	p.Write(pkt(0, 0))
	time.Sleep(50 * time.Millisecond)
	p.Write(pkt(1, 48000*5)) // five seconds ahead, arriving immediately
	at := waitFor(c, 2, 2*time.Second)
	if len(at) != 2 {
		t.Fatalf("expected both packets out, got %d", len(at))
	}
	if gap := at[1].Sub(at[0]); gap > 500*time.Millisecond {
		t.Fatalf("the second packet was held %v; a jump must restart the schedule, not stall", gap)
	}
	p.Close()
	if closed["timestamp_jumps"] != int64(1) {
		t.Fatalf("timestamp_jumps = %v, want 1", closed["timestamp_jumps"])
	}
}

// The measured failure that forced a relative schedule: a sender whose media clock runs slower than
// this machine's. An absolute schedule drifts until every packet is past its slot and the pacer
// stops pacing. What must survive is the OUTPUT spacing, which is the only thing the far jitter
// buffer sees.
func TestPacerKeepsSpacingWhileTheSenderClockDrifts(t *testing.T) {
	c := newCollector()
	p := NewPacer(40*time.Millisecond, c.write, nil)
	defer p.Close()

	const n = 60
	for i := 0; i < n; i++ {
		p.Write(pkt(uint16(i), uint32(i)*960)) // 20 ms apart in media time
		time.Sleep(22 * time.Millisecond)      // but 22 ms apart in real time
	}
	at := waitFor(c, n, 3*time.Second)
	if len(at) != n {
		t.Fatalf("only %d of %d packets came out", len(at), n)
	}

	// Nothing may be bunched, and the run must not have stretched: a schedule that drifted would
	// either emit in bursts or fall progressively further behind.
	var tight int
	for i := 1; i < len(at); i++ {
		if at[i].Sub(at[i-1]) < 8*time.Millisecond {
			tight++
		}
	}
	if tight > 2 {
		t.Fatalf("%d output gaps were under 8 ms; the spacing collapsed", tight)
	}
	span := at[n-1].Sub(at[0])
	if span < 1000*time.Millisecond || span > 1700*time.Millisecond {
		t.Fatalf("60 packets came out over %v; arrival spans ~1.3 s and output must track it", span)
	}
	if paced, _, _, spurts := p.Stats(); paced != n || spurts > 2 {
		t.Fatalf("paced=%d spurts=%d: steady drift is not a series of new talk spurts", paced, spurts)
	}
}

// Teardown must not swallow audio that was already accepted.
func TestPacerFlushesOnClose(t *testing.T) {
	c := newCollector()
	p := NewPacer(300*time.Millisecond, c.write, nil)
	for i := 0; i < 5; i++ {
		p.Write(pkt(uint16(i), uint32(i)*960))
	}
	p.Close()
	seq, _ := c.snapshot()
	if len(seq) != 5 {
		t.Fatalf("close dropped audio: %d of 5 packets came out", len(seq))
	}
}

// Pacing is optional, and a nil pacer must be safe everywhere so the caller can simply not build one.
func TestNilPacerIsSafe(t *testing.T) {
	var p *Pacer
	p.Write(pkt(1, 960))
	p.Close()
	if a, b, c2, d := p.Stats(); a|b|c2|d != 0 {
		t.Fatal("a nil pacer counts nothing")
	}
	if NewPacer(0, func(*rtp.Packet) {}, nil) != nil {
		t.Fatal("delay 0 means pacing is off")
	}
	if NewPacer(time.Second, nil, nil) != nil {
		t.Fatal("a pacer with nowhere to write is not a pacer")
	}
}

// Ordering is the one thing a relay must never break, and the first version did: with a channel
// queue and a default branch, a full buffer made the WRITER emit the newest packet while older ones
// were still queued. Codex found it by reading the code. This drives far more audio through than the
// backlog limit and asserts every packet came out exactly once, in order.
func TestPacerNeverReordersUnderABacklog(t *testing.T) {
	c := newCollector()
	p := NewPacer(50*time.Millisecond, c.write, nil)

	const n = 400 // four times the backlog limit, handed over as fast as possible
	for i := 0; i < n; i++ {
		p.Write(pkt(uint16(i), uint32(i)*960))
	}
	p.Close()

	seq, _ := c.snapshot()
	if len(seq) != n {
		t.Fatalf("%d of %d packets came out; none may be dropped", len(seq), n)
	}
	for i, s := range seq {
		if s != uint16(i) {
			t.Fatalf("packet %d came out at position %d: the queue reordered audio", s, i)
		}
	}
}

// Writes racing a Close must not corrupt the output or emit from two goroutines at once. Run this
// one under -race.
func TestPacerCloseRacesWithWrites(t *testing.T) {
	var mu sync.Mutex
	seen := map[uint16]int{}
	var inFlight int32
	p := NewPacer(20*time.Millisecond, func(pk *rtp.Packet) {
		if n := atomicAdd(&inFlight, 1); n != 1 {
			t.Errorf("two goroutines wrote to the track at once (%d)", n)
		}
		mu.Lock()
		seen[pk.SequenceNumber]++
		mu.Unlock()
		atomicAdd(&inFlight, -1)
	}, nil)

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(base uint16) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				p.Write(pkt(base+uint16(i), uint32(i)*960))
				time.Sleep(time.Millisecond)
			}
		}(uint16(w * 1000))
	}
	time.Sleep(30 * time.Millisecond)
	p.Close()
	wg.Wait()
	p.Close() // idempotent

	mu.Lock()
	defer mu.Unlock()
	for s, n := range seen {
		if n != 1 {
			t.Fatalf("packet %d was written %d times; every packet leaves exactly once", s, n)
		}
	}
	if len(seen) != 200 {
		t.Fatalf("%d distinct packets reached the track, want 200", len(seen))
	}
}

// The sender pauses without its timestamps covering the pause, so the packet after a silence claims
// to be 20 ms of media when hundreds of milliseconds have gone by. A receiver reads that as a LATE
// frame and conceals across the hole, which is heard as grit. RFC 3550's audio profile gives the
// marker bit the meaning "first packet of a talkspurt after a silent period", which is what tells a
// jitter buffer to reset and play silence instead. Only the header flag may change.
func TestPacerMarksTheStartOfATalkSpurt(t *testing.T) {
	var mu sync.Mutex
	var got []*rtp.Packet
	p := NewPacer(20*time.Millisecond, func(pk *rtp.Packet) {
		mu.Lock()
		got = append(got, pk)
		mu.Unlock()
	}, nil)

	original := pkt(0, 0)
	original.Marker = true // as they arrive on this path: every packet has it set
	p.Write(original)
	time.Sleep(60 * time.Millisecond)
	// 300 ms of wall clock with only 20 ms of media: the sender paused and said nothing about it
	time.Sleep(300 * time.Millisecond)
	p.Write(pkt(1, 960))
	time.Sleep(60 * time.Millisecond)
	cont := pkt(2, 1920) // ordinary continuation, also arriving with the bit set
	cont.Marker = true
	p.Write(cont)
	p.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("got %d packets, want 3", len(got))
	}
	if !got[1].Marker {
		t.Error("the packet after the pause was not marked as a talk-spurt start")
	}
	if got[2].Marker {
		t.Error("an ordinary continuation was marked")
	}
	if got[0].Marker != true {
		t.Error("the very first packet of a call is a talk-spurt start and must be marked")
	}
	if !original.Marker {
		t.Error("the caller's packet was mutated; the change must happen on a copy")
	}
	if !cont.Marker {
		t.Error("the caller's continuation packet was mutated")
	}
	for i, pk := range got {
		if pk.SequenceNumber != uint16(i) || pk.Timestamp != uint32(i)*960 {
			t.Errorf("packet %d had its sequence or timestamp changed: seq=%d ts=%d",
				i, pk.SequenceNumber, pk.Timestamp)
		}
		if len(pk.Payload) != 3 {
			t.Errorf("packet %d had its payload changed", i)
		}
	}
}

// The marking must be separable from the pacing, so the two can be told apart on a real call.
func TestPacerMarkingCanBeTurnedOff(t *testing.T) {
	var mu sync.Mutex
	var got []*rtp.Packet
	p := NewPacerWith(20*time.Millisecond, false, func(pk *rtp.Packet) {
		mu.Lock()
		got = append(got, pk)
		mu.Unlock()
	}, nil)
	a, b := pkt(0, 0), pkt(1, 960)
	a.Marker, b.Marker = true, true
	p.Write(a)
	time.Sleep(360 * time.Millisecond)
	p.Write(b)
	p.Close()

	mu.Lock()
	defer mu.Unlock()
	for i, pk := range got {
		if !pk.Marker {
			t.Fatalf("packet %d had its marker changed with marking turned off", i)
		}
	}
}
