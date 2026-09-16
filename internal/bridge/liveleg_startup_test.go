package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestLiveStartupNeedsTransportAndSession(t *testing.T) {
	for _, tc := range []struct {
		name      string
		connected bool
		started   bool
		wantErr   bool
	}{
		{name: "both", connected: true, started: true},
		{name: "transport only", connected: true, wantErr: true},
		{name: "session only", started: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newLiveStartup()
			if tc.connected {
				s.connected()
			}
			if tc.started {
				s.started()
			}
			err := s.wait(context.Background(), context.Background(), 10*time.Millisecond)
			if (err != nil) != tc.wantErr {
				t.Fatalf("wait error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestLiveStartupSessionSignalIsIdempotent(t *testing.T) {
	s := newLiveStartup()
	s.started() // sideband
	s.started() // data channel
	s.connected()
	if err := s.wait(context.Background(), context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestLiveStartupReadySignalsDoNotOverrideCancellation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancelLeg bool
	}{
		{name: "dial context"},
		{name: "leg context", cancelLeg: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newLiveStartup()
			s.connected()
			s.started()
			ctx, cancel := context.WithCancel(context.Background())
			legCtx, cancelLeg := context.WithCancel(context.Background())
			if tc.cancelLeg {
				cancelLeg()
			} else {
				cancel()
			}
			defer cancel()
			defer cancelLeg()
			if err := s.wait(ctx, legCtx, time.Second); err == nil {
				t.Fatal("ready startup succeeded after cancellation")
			}
		})
	}
}

func TestLiveStartupAudioIsReleasedInOrder(t *testing.T) {
	l := &LiveLeg{}
	l.holdStartupAudio(10)
	startupSink := l.audioSink.Load()

	for i := uint16(1); i <= 3; i++ {
		p := seqPacket(i)
		(*l.audioSink.Load())(p)
	}
	var got []uint16
	l.SetAudioSink(func(p *rtp.Packet) { got = append(got, p.SequenceNumber) })
	if l.audioSink.Load() != startupSink {
		t.Fatal("release replaced the startup wrapper and can let a later packet overtake a waiting writer")
	}
	p := seqPacket(4)
	(*l.audioSink.Load())(p)

	if len(got) != 4 {
		t.Fatalf("got %v, want four packets", got)
	}
	for i, seq := range got {
		if seq != uint16(i+1) {
			t.Fatalf("got %v, want [1 2 3 4]", got)
		}
	}
}

func TestDetachingDropsLiveStartupAudio(t *testing.T) {
	l := &LiveLeg{}
	l.holdStartupAudio(10)
	(*l.audioSink.Load())(seqPacket(1))
	l.SetAudioSink(nil)
	if l.audioSink.Load() != nil {
		t.Fatal("startup sink remained attached")
	}
	if l.startupAudio != nil {
		t.Fatal("startup hold was not cleared")
	}
}
