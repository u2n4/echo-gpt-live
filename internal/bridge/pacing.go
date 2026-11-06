package bridge

// Measuring the RHYTHM of arriving audio, which is the one thing neither the recordings nor RTCP
// can show.
//
// The recordings hold what the packets contained; RTCP loss counters say almost nothing arrived
// late. Neither answers the question raised by a listener hearing grit with no packet loss: did the
// packets arrive evenly, or in bursts with gaps between them? This bridge forwards every packet the
// instant it arrives, so whatever rhythm comes in goes straight out, and the far jitter buffer has
// to absorb it by stretching and compressing playback. That is audible, and it is invisible to
// every measurement taken so far.
//
// A 20 ms Opus stream on a healthy path arrives one packet every 20 ms. Two numbers below say how
// far from that the reality is: the share of packets arriving within 5 ms of the one before (a
// burst), and the share arriving more than 40 ms after it (a starved gap).

import (
	"sort"
	"sync"
	"time"
)

// paceWindow is how often the rhythm is summarised. Long enough that the percentiles mean
// something, short enough to see a call change character.
const paceWindow = 5 * time.Second

const (
	burstGap  = 5 * time.Millisecond  // arrived essentially together with the previous packet
	starveGap = 40 * time.Millisecond // two frame times: the far buffer has to conceal or stretch
)

// paceMeter records the gaps between successive packets on one stream. The lock costs nothing at
// fifty packets a second and means the meter can also sit on the OUTPUT path, where the caller is a
// different goroutine depending on the audio mode.
type paceMeter struct {
	mu    sync.Mutex
	ev    Events
	label string // which stream, e.g. "openai" or "echo"

	last       time.Time
	windowFrom time.Time
	gaps       []time.Duration
	bursts     int
	starves    int
	// media vs wall: whether the sender's timestamps account for the time it spends not sending.
	// A deficit arriving in a few big steps is pauses the timestamps do not cover, which a receiver
	// reads as late frames and conceals across. A deficit arriving smoothly is a clock rate error.
	// The two need opposite fixes, so they are counted apart.
	haveTS      bool
	lastTS      uint32
	mediaTotal  time.Duration
	stepDeficit time.Duration // deficit that arrived in steps over 100 ms
	steps       int
	// markers counts packets that already carry the RTP marker bit. RFC 3550's audio profile gives
	// it the meaning "first packet of a talkspurt", so whether the sender already sets it decides
	// whether setting it ourselves is a lever at all.
	markers int
	total   int64
}

func newPaceMeter(ev Events, label string) *paceMeter {
	if ev == nil {
		ev = func(string, ...any) {}
	}
	return &paceMeter{ev: ev, label: label, gaps: make([]time.Duration, 0, 512)}
}

// mark records the arrival of one packet and emits a summary once per window. ts is the packet's
// RTP timestamp, which is what says how much MEDIA time passed while the wall clock ran.
func (p *paceMeter) mark(ts uint32, marker bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.total++
	if marker {
		p.markers++
	}
	if p.last.IsZero() {
		p.last, p.windowFrom = now, now
		p.haveTS, p.lastTS = true, ts
		return
	}
	gap := now.Sub(p.last)
	if p.haveTS {
		media := time.Duration(int64(int32(ts-p.lastTS)) * int64(time.Second) / opusClockRate)
		if media >= 0 {
			p.mediaTotal += media
			p.lastTS = ts
			if d := gap - media; d > 100*time.Millisecond {
				p.stepDeficit += d
				p.steps++
			}
		}
	}
	p.haveTS = true
	p.last = now
	p.gaps = append(p.gaps, gap)
	switch {
	case gap < burstGap:
		p.bursts++
	case gap > starveGap:
		p.starves++
	}
	if now.Sub(p.windowFrom) >= paceWindow {
		p.flush(now)
	}
}

func (p *paceMeter) flush(now time.Time) {
	n := len(p.gaps)
	if n == 0 {
		p.windowFrom = now
		return
	}
	sorted := make([]time.Duration, n)
	copy(sorted, p.gaps)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

	span := now.Sub(p.windowFrom).Seconds()
	deficit := time.Duration(span*float64(time.Second)) - p.mediaTotal
	p.ev("pace_"+p.label,
		"pkts", n,
		"pkts_per_s", float64(n)/span,
		"media_covers_frac", p.mediaTotal.Seconds()/span,
		"deficit_ms", deficit.Milliseconds(),
		"deficit_in_steps_ms", p.stepDeficit.Milliseconds(),
		"steps_over_100ms", p.steps,
		"marker_bits_seen", p.markers,
		"gap_median_ms", ms(sorted[n/2]),
		"gap_p95_ms", ms(sorted[(n*95+99)/100-1]),
		"gap_max_ms", ms(sorted[n-1]),
		"burst_frac", float64(p.bursts)/float64(n),
		"starved_frac", float64(p.starves)/float64(n),
		"note", "a healthy 20 ms stream sits near gap_median_ms 20 with both fractions near 0; "+
			"a deficit that is mostly in steps is pauses the sender's timestamps do not cover, "+
			"a deficit spread evenly is a clock rate error")

	p.gaps = p.gaps[:0]
	p.bursts, p.starves = 0, 0
	p.mediaTotal, p.stepDeficit, p.steps, p.markers = 0, 0, 0, 0
	p.windowFrom = now
}

// done emits whatever is left, so a short call still reports its rhythm.
func (p *paceMeter) done() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.gaps) > 0 {
		p.flush(time.Now())
	}
}
