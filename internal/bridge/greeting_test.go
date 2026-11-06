package bridge

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestSpliceIsIdentityWithoutAGreeting(t *testing.T) {
	var s TimelineSplice
	p := &rtp.Packet{Header: rtp.Header{SequenceNumber: 5000, Timestamp: 999}}
	if s.Translate(p) || p.SequenceNumber != 5000 || p.Timestamp != 999 {
		t.Fatalf("nothing noted, so nothing should change: %+v", p.Header)
	}
}

func TestSpliceContinuesTheGreetingAcrossTheRealGap(t *testing.T) {
	var s TimelineSplice
	last := &rtp.Packet{Header: rtp.Header{SequenceNumber: 100, Timestamp: 48000}}
	s.Note(last, 960)
	time.Sleep(200 * time.Millisecond)

	const base = 100000
	first := &rtp.Packet{Header: rtp.Header{SequenceNumber: 60000, Timestamp: base}}
	if !s.Translate(first) {
		t.Fatal("the first packet after a greeting must be spliced")
	}
	if first.SequenceNumber != 101 {
		t.Fatalf("sequence should continue at 101, got %d", first.SequenceNumber)
	}
	gap := int64(first.Timestamp) - int64(48000+960)
	if gap < 200*48 || gap > 700*48 { // the sleep, plus scheduling slack
		t.Fatalf("timestamp should advance by the wall-clock pause (~9600 ticks), advanced %d", gap)
	}

	// the offsets are fixed: a 20 ms step in, a 20 ms step out, and a reorder stays a reorder
	second := &rtp.Packet{Header: rtp.Header{SequenceNumber: 60001, Timestamp: base + 960}}
	s.Translate(second)
	if second.SequenceNumber != 102 || second.Timestamp != first.Timestamp+960 {
		t.Fatalf("second packet should follow the first by one and 960: %+v after %+v", second.Header, first.Header)
	}
	late := &rtp.Packet{Header: rtp.Header{SequenceNumber: 59999, Timestamp: base - 960}}
	s.Translate(late)
	if late.SequenceNumber != 100 || late.Timestamp != first.Timestamp-960 {
		t.Fatalf("a reordered packet keeps its own delta: %+v", late.Header)
	}
}

func TestPlayGreetingWritesOneContinuousTalkspurt(t *testing.T) {
	const path = "../../media/hala.ogg"
	if _, err := os.Stat(path); err != nil {
		t.Skip("media/hala.ogg is not in this checkout")
	}
	var s TimelineSplice
	var got []*rtp.Packet
	var sentAt []time.Time
	n, err := PlayGreeting(context.Background(), path, func(p *rtp.Packet) error {
		got = append(got, p)
		sentAt = append(sentAt, time.Now())
		return nil
	}, &s)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(got) || n < 30 || n > 60 {
		t.Fatalf("expected the ~0.8 s clip as 30-60 packets, wrote %d", n)
	}
	if !got[0].Marker {
		t.Fatal("the first packet starts a talkspurt and must carry the marker")
	}
	for i := 1; i < len(got); i++ {
		if got[i].Marker {
			t.Fatalf("packet %d carries a marker mid-spurt", i)
		}
		if got[i].SequenceNumber != got[i-1].SequenceNumber+1 {
			t.Fatalf("sequence jumps at %d: %d then %d", i, got[i-1].SequenceNumber, got[i].SequenceNumber)
		}
		if got[i].Timestamp-got[i-1].Timestamp != 960 {
			t.Fatalf("timestamp step at %d is %d, want 960", i, got[i].Timestamp-got[i-1].Timestamp)
		}
	}
	if el := sentAt[len(sentAt)-1].Sub(sentAt[0]); el < time.Duration(n-1)*20*time.Millisecond-time.Millisecond {
		t.Fatalf("%d packets left in %v; the greeting must be paced, not burst", n, el)
	}
	// the splice now continues from the last packet
	next := &rtp.Packet{Header: rtp.Header{SequenceNumber: 1, Timestamp: 1}}
	s.Translate(next)
	if next.SequenceNumber != got[len(got)-1].SequenceNumber+1 {
		t.Fatalf("OpenAI's first packet should follow the greeting's last, got %d after %d", next.SequenceNumber, got[len(got)-1].SequenceNumber)
	}
	if step := int64(int32(next.Timestamp - got[len(got)-1].Timestamp)); step < 960 || step > 48000 {
		t.Fatalf("post-greeting timestamp step = %d ticks", step)
	}
}
