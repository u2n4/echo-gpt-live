package bridge

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

// The bridge's own hello.
//
// Measured on 14 recorded calls: the Echo's transport is up 1.9-2.2 s after the offer, the
// OpenAI leg 1.3-2.5 s after that, and the model's own greeting reached the Echo 5.1-7.0 s after
// the offer. Everything he said in between was held and answered, but he heard nothing, and a
// person who hears nothing for five seconds says it again or gives up. So the bridge says "hala"
// itself the moment the Echo's transport is up, from an Ogg/Opus file that is her own voice cut
// out of a recording, and the model is told not to greet.
//
// The Echo sees ONE RTP timeline. The greeting starts it; OpenAI's packets are spliced onto it
// when they arrive, with the timestamp advanced by the wall-clock gap so the pause between the
// greeting and her first answer is a pause, not a jump the far jitter buffer has to swallow.

// TimelineSplice maps a second RTP stream onto the tail of a first one.
type TimelineSplice struct {
	mu      sync.Mutex
	noted   bool // a first-stream packet has been written
	lastSeq uint16
	lastTS  uint32
	lastDur uint32
	lastAt  time.Time
	primed  bool // offsets for the second stream are fixed
	seqOff  uint16
	tsOff   uint32
}

// Note records the packet just written on the first stream and how long it lasts, in ticks.
func (t *TimelineSplice) Note(p *rtp.Packet, durTicks uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.noted, t.lastSeq, t.lastTS, t.lastDur, t.lastAt = true, p.SequenceNumber, p.Timestamp, durTicks, time.Now()
}

// Translate rewrites a second-stream packet in place so it continues the first stream. The
// offsets are chosen on the first packet and never change, so every later gap, reorder and wrap
// keeps the sender's own delta. With nothing noted it is the identity.
func (t *TimelineSplice) Translate(p *rtp.Packet) (spliced bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.noted {
		return false
	}
	if !t.primed {
		gap := uint32(time.Since(t.lastAt) * opusClockRate / time.Second)
		t.seqOff = (t.lastSeq + 1) - p.SequenceNumber
		t.tsOff = (t.lastTS + t.lastDur + gap) - p.Timestamp
		t.primed, spliced = true, true
	}
	p.SequenceNumber += t.seqOff
	p.Timestamp += t.tsOff
	return spliced
}

// PlayGreeting writes the file's Opus packets to the Echo at their own pace (one page per 20 ms
// packet, the shape media/hello.ogg and media/hala.ogg have), the first with the marker bit as a
// new talkspurt, and notes each on the splice so the next stream can follow. It returns the
// number of packets written; ctx ending part-way is not an error.
func PlayGreeting(ctx context.Context, path string, write func(*rtp.Packet) error, splice *TimelineSplice) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	ogg, _, err := oggreader.NewWith(f)
	if err != nil {
		return 0, err
	}
	seq := uint16(rand.Uint32())
	ts := rand.Uint32()
	var lastGranule uint64
	n := 0
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		payload, hdr, err := ogg.ParseNextPage()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		dur := uint32(hdr.GranulePosition - lastGranule)
		lastGranule = hdr.GranulePosition
		if d, ok := opusPacketDurationTicks(payload); ok {
			dur = d
		} else if dur == 0 || dur > opusClockRate*120/1000 {
			dur = opusClockRate * 20 / 1000
		}
		if len(payload) == 0 {
			continue
		}
		p := &rtp.Packet{Header: rtp.Header{Version: 2, Marker: n == 0, SequenceNumber: seq, Timestamp: ts}, Payload: payload}
		if err := write(p); err != nil {
			return n, err
		}
		splice.Note(p, dur)
		n++
		seq++
		ts += dur
		select {
		case <-ctx.Done():
			return n, nil
		case <-ticker.C:
		}
	}
	return n, nil
}
