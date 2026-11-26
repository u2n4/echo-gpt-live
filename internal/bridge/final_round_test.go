package bridge

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestReflectorCloseRacesWithPush(t *testing.T) {
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000}, "audio", "test")
	if err != nil {
		t.Fatal(err)
	}
	r := newReflector(track, 0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				r.push(&rtp.Packet{})
			}
		}()
	}
	closed := make(chan struct{})
	go func() { r.close(); r.close(); close(closed) }()
	wg.Wait()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close waited for delayed media")
	}
	r.push(&rtp.Packet{})
	select {
	case <-r.stopped:
	case <-time.After(time.Second):
		t.Fatal("reflector did not drain queued packets")
	}
	if len(r.ch) != 0 {
		t.Fatal("close discarded queued packets")
	}
}

func TestEventTypeUsesTopLevelJSON(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"type": "session.started"}`, "session.started"},
		{`{"error":{"type":"session.started"},"type":"error"}`, "error"},
		{`{"error":{"type":"session.started"}}`, ""},
		{`not JSON`, ""},
		{`{"type":"` + strings.Repeat("x", 129) + `"}`, ""},
	} {
		if got := eventType([]byte(tc.raw)); got != tc.want {
			t.Errorf("eventType(%q)=%q want %q", tc.raw, got, tc.want)
		}
	}
}

func TestSenderReportSnapshotIsCoherent(t *testing.T) {
	l := &LiveLeg{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(1); i < 10000; i++ {
			l.senderReport.Store(&liveSenderReport{packets: i, at: time.Unix(i, 0)})
		}
	}()
	for i := 0; i < 10000; i++ {
		n, at := l.SenderReport()
		if n != 0 && at.Unix() != n {
			t.Errorf("incoherent report: packets=%d at=%v", n, at)
			break
		}
	}
	wg.Wait()
}

func TestFarewellWithdrawalAccumulatesTranscriptFragments(t *testing.T) {
	f := NewFarewell(nil, nil)
	if !f.Feed("goodbye") {
		t.Fatal("goodbye was not armed")
	}
	for _, fragment := range []string{" w", "a", "i", "t"} {
		f.Feed(fragment)
	}
	if _, _, armed := f.Armed(); armed {
		t.Fatal("fragmented new speech did not withdraw goodbye")
	}
}
