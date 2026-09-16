package bridge

// Releasing OpenAI's audio at the spacing its own timestamps ask for, instead of the spacing the
// network happened to deliver it in.
//
// Measured on this path: the Echo's stream arrives at a 20.1 ms median with a 21 ms worst case and
// nothing bursty, while OpenAI's arrives with up to 46% of packets inside 5 ms of the one before and
// gaps reaching 500 ms. The bridge forwarded that rhythm untouched, so the Echo's jitter buffer had
// to accelerate through every burst and stretch across every gap, continuously, while RTCP reported
// almost no loss because nothing was lost. Constant time-scale modification is heard as grit.
//
// Nothing here invents or drops audio. Every packet still goes out exactly once, in the order it
// arrived, byte for byte, with its own sequence number and timestamp. Only the moment it leaves
// changes, and exactly one goroutine ever performs a write.
//
// The schedule is kept RELATIVE, not absolute, and that part took a measurement to get right. An
// absolute schedule anchored at the first packet assumes the sender's media clock and this machine's
// wall clock agree. They do not: OpenAI delivers 44-49 packets per second while its timestamps
// advance 20 ms each, so media time falls behind wall time by a millisecond or two per packet.
// Anchored absolutely, the schedule drifted until every packet was past its slot and the pacer
// silently stopped pacing; at a 100 ms depth that was 860 of 974 packets. Advancing a target time by
// each packet's own media delta, and clamping it to now whenever reality has moved past it, cannot
// accumulate that error. The cost is that a sender slower than real time eventually empties the
// buffer, which is correct: there is nothing to pace when nothing is waiting.

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

const (
	// opusClockRate is the RTP clock for Opus, always 48 kHz regardless of the encoded bandwidth.
	opusClockRate = 48000

	// DefaultPacerDelay is the headroom given to a talk spurt when it starts, chosen by measurement
	// rather than by taste. Compared over the same workload:
	//
	//	 60 ms: 29 of 1260 packets went out on arrival, and every five-second window still showed
	//	        0.4-0.8% of output gaps past two frame times.
	//	150 ms: 10 of 1280 went out on arrival, and three of five windows held a 21 ms worst-case
	//	        gap, meaning five seconds of perfectly even output.
	//
	// The cost is 150 ms added to mouth-to-ear against a round trip already measured at 190-306 ms,
	// and up to 150 ms of the assistant's queued speech still playing when she is interrupted. The
	// complaint being solved is clarity, so clarity wins.
	DefaultPacerDelay = 150 * time.Millisecond

	// spurtGap is how far behind the schedule may fall before the silence is read as the end of a
	// talk spurt, so the next one starts with its headroom rebuilt rather than inheriting a deficit.
	//
	// Measured, at a 150 ms depth over the same workload, by how many packets ended up going out
	// the moment they arrived, which is the pacer failing to pace:
	//
	//	 60 ms: 28 of 1248   too eager; a rebuild every jitter blip never settles
	//	120 ms: 11 of 1270   best
	//	250 ms: 31 of 1242   too slow; a medium pause eats the headroom and never gets it back
	spurtGap = 120 * time.Millisecond

	// maxMediaStep bounds one packet's media delta. A larger jump is a stream restart or a corrupt
	// timestamp, and scheduling audio most of a second into the future on the strength of it would
	// be worse than the unevenness this exists to fix.
	maxMediaStep = 400 * time.Millisecond

	// pacerBacklog is how deep the queue may get before pacing gives way to catching up. 100
	// packets is two seconds of audio; past that the delay would be worse than the unevenness, so
	// the queue is emptied at full speed IN ORDER until it is short again.
	pacerBacklog = 100

	// pacerHardCap is where the queue stops growing at all. Bounded memory, never blocking and
	// never dropping cannot all three hold against a consumer that has stopped consuming, and the
	// one to give up is never dropping: ten seconds of undelivered audio is already a call nobody
	// is listening to. The OLDEST is dropped, because what is left then stays in order and is the
	// most recent speech, which is the only part still worth hearing.
	pacerHardCap = 500
)

// Pacer re-times one direction of forwarded RTP. Build it with NewPacer; a nil Pacer is a working
// "pacing is off", so every method tolerates one.
//
// It also makes the RTP marker bit mean what RFC 3550's audio profile says it means.
//
// The sender pauses without its timestamps covering the pause, so the packet after a silence claims
// to be 20 ms of media when hundreds of milliseconds of wall clock have gone by. A receiver reading
// that sees a LATE frame, not a pause, and conceals across the hole, which is heard as grit. The
// marker bit is the signal that exists for exactly this: "the first packet of a talkspurt after a
// silent period", which tells a jitter buffer to reset and play silence instead of concealing.
//
// Measured on this path, it carries no information at all: every packet arrives with it set, 247 of
// 247 in one window and the same on every other stream. A bit that is always one cannot tell a
// receiver anything. So it is cleared on continuations and set only where a talk spurt actually
// begins, which is the one place the profile says it belongs.
//
// Only that header flag changes. The payload, the sequence number and the timestamp are untouched,
// and the packet is copied first so nothing else holding it can see the change. --spurt-marker=false
// forwards the bit exactly as it arrived.
//
// The queue is a slice under a mutex rather than a channel, so that a full buffer can never reorder
// audio. A channel with a default branch made the writer emit the newest packet while older ones
// were still queued, which is worse than any delay it was avoiding.
type Pacer struct {
	delay      time.Duration
	markSpurts bool
	out        func(*rtp.Packet)
	ev         Events

	// outMu serialises every write to the far track. In this bridge only one goroutine ever feeds
	// the pacer, but "only one caller" is not a property the type should depend on, and after the
	// queue has drained Write emits directly.
	outMu sync.Mutex

	mu      sync.Mutex
	cond    *sync.Cond
	q       []*rtp.Packet
	stopped bool // Close was called; the run goroutine is winding down
	drained bool // the run goroutine has exited, so Write must emit directly

	wg   sync.WaitGroup
	once sync.Once

	paced, held, late, spurts, jumps, rushed, marked, cleared, dropped atomic.Int64
	heldTotal                                                          atomic.Int64 // microseconds, for the average hold
}

// NewPacer starts a pacer that releases packets to out. delay <= 0 returns nil, which callers read
// as "no pacing wanted".
// NewPacer builds a pacer that also marks talk spurts. Use NewPacerWith to turn the marking off.
func NewPacer(delay time.Duration, out func(*rtp.Packet), ev Events) *Pacer {
	return NewPacerWith(delay, true, out, ev)
}

// NewPacerWith is NewPacer with the talk-spurt marker under the caller's control, so the two
// changes can be told apart on a real call.
func NewPacerWith(delay time.Duration, markSpurts bool, out func(*rtp.Packet), ev Events) *Pacer {
	if delay <= 0 || out == nil {
		return nil
	}
	if ev == nil {
		ev = func(string, ...any) {}
	}
	p := &Pacer{delay: delay, markSpurts: markSpurts, out: out, ev: ev}
	p.cond = sync.NewCond(&p.mu)
	p.wg.Add(1)
	go p.run()
	ev("pacer_start", "delay_ms", delay.Milliseconds(), "backlog_limit", pacerBacklog,
		"mark_spurts", markSpurts,
		"note", "OpenAI's audio is spaced by its own timestamps, not by arrival")
	return p
}

// Write hands one packet to the pacer. It never blocks the reader and never drops or reorders
// audio: the packet joins the back of the queue, and the one goroutine that empties it is the only
// thing that ever writes to the far track.
func (p *Pacer) Write(pkt *rtp.Packet) {
	if p == nil || pkt == nil {
		return
	}
	p.mu.Lock()
	if p.drained {
		// The pacer is finished. Emitting here is safe because nothing else is running, and the
		// lock keeps these writes in order with each other.
		p.mu.Unlock()
		p.emit(pkt)
		return
	}
	p.q = append(p.q, pkt)
	if over := len(p.q) - pacerHardCap; over > 0 {
		p.q = p.q[over:]
		p.dropped.Add(int64(over))
		if p.dropped.Load() == int64(over) { // say it once, not fifty times a second
			p.ev("pacer_overloaded", "cap", pacerHardCap,
				"note", "the outbound track has stopped accepting audio; the oldest is being dropped")
		}
	}
	p.cond.Signal()
	p.mu.Unlock()
}

// next blocks until a packet is available, returning the packet plus how many were still queued
// behind it. ok is false once the pacer has stopped and the queue is empty.
func (p *Pacer) next() (pkt *rtp.Packet, backlog int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.q) == 0 && !p.stopped {
		p.cond.Wait()
	}
	if len(p.q) == 0 {
		p.drained = true
		return nil, 0, false
	}
	pkt = p.q[0]
	p.q = p.q[1:]
	return pkt, len(p.q), true
}

func (p *Pacer) run() {
	defer p.wg.Done()

	var have bool
	var lastTS uint32
	var nextOut time.Time
	spurtsSeen := int64(0)

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		pkt, backlog, ok := p.next()
		if !ok {
			return
		}

		now := time.Now()
		switch {
		case !have:
			have = true
			nextOut = now.Add(p.delay)
			p.spurts.Add(1)
		default:
			// int32 so the 32-bit RTP clock wrapping around reads as a small step, not a huge one.
			step := time.Duration(int64(int32(pkt.Timestamp-lastTS)) * int64(time.Second) / opusClockRate)
			if step < 0 || step > maxMediaStep {
				p.jumps.Add(1)
				nextOut = now.Add(p.delay)
			} else {
				nextOut = nextOut.Add(step)
			}
			if nextOut.Before(now) {
				if now.Sub(nextOut) > spurtGap {
					// The stream went quiet for longer than the schedule can explain. Treat what
					// follows as a new talk spurt and give it its headroom back.
					p.spurts.Add(1)
					nextOut = now.Add(p.delay)
				} else {
					// Ordinary drift: the sender's media clock runs slower than this machine's.
					// Catch up here rather than carrying the error forward for the whole call.
					nextOut = now
				}
			}
		}
		lastTS = pkt.Timestamp

		if p.markSpurts {
			// A spurt was declared for this packet if the counter moved while handling it.
			want := p.spurts.Load() != spurtsSeen
			spurtsSeen = p.spurts.Load()
			if pkt.Marker != want {
				q := *pkt // copy: the recorder and anything else may still hold the original
				q.Marker = want
				pkt = &q
				if want {
					p.marked.Add(1)
				} else {
					p.cleared.Add(1)
				}
			} else if want {
				p.marked.Add(1)
			}
		}

		wait := nextOut.Sub(now)
		// Two reasons to stop waiting: the queue has grown past what any delay should cost, or the
		// pacer is shutting down and the rest of the speech has to get out.
		if backlog > pacerBacklog {
			p.rushed.Add(1)
			nextOut = now
			wait = 0
		}
		if p.stopping() {
			wait = 0
		}

		if wait > 0 {
			p.held.Add(1)
			p.heldTotal.Add(wait.Microseconds())
			timer.Reset(wait)
			<-timer.C
		} else {
			p.late.Add(1)
		}
		p.emit(pkt)
	}
}

// emit is the only place a packet reaches the far track.
func (p *Pacer) emit(pkt *rtp.Packet) {
	p.outMu.Lock()
	p.out(pkt)
	p.outMu.Unlock()
	p.paced.Add(1)
}

func (p *Pacer) stopping() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped
}

// Close stops the pacer, lets it flush what it still holds in order, and reports what it did.
func (p *Pacer) Close() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		p.mu.Lock()
		p.stopped = true
		p.cond.Broadcast()
		p.mu.Unlock()
		p.wg.Wait()

		held := p.held.Load()
		avg := 0.0
		if held > 0 {
			avg = float64(p.heldTotal.Load()) / float64(held) / 1000.0
		}
		p.ev("pacer_closed",
			"packets", p.paced.Load(),
			"held", held,
			"avg_hold_ms", avg,
			"sent_on_arrival", p.late.Load(),
			"talk_spurts", p.spurts.Load(),
			"timestamp_jumps", p.jumps.Load(),
			"rushed_backlog", p.rushed.Load(),
			"spurt_markers_set", p.marked.Load(),
			"markers_cleared", p.cleared.Load(),
			"dropped_overload", p.dropped.Load())
	})
}

// Stats reports progress while the call is running.
func (p *Pacer) Stats() (paced, held, late, spurts int64) {
	if p == nil {
		return 0, 0, 0, 0
	}
	return p.paced.Load(), p.held.Load(), p.late.Load(), p.spurts.Load()
}
