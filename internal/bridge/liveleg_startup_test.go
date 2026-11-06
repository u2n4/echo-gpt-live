package bridge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/u2n4/echo-gpt-live/internal/openai"
)

type sessionTransport func(*http.Request) (*http.Response, error)

func (f sessionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type unexpectedEOFReader struct{}

func (unexpectedEOFReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestRecoveredForkNeverFallsBack(t *testing.T) {
	for _, body := range []string{
		`{"session":{"id":"live_recovered"},"transport":{"sdp":"v=0\r\n"}}`,
		`{"session":{"id":"live_recovered"},"transport":`,
	} {
		for _, cleanup := range []int{200, 401} {
			c := openai.NewLive("fake-key", "")
			forks, creates, hangups := 0, 0, 0
			c.HTTP = &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/fork"):
					forks++
					return &http.Response{StatusCode: 201, Body: io.NopCloser(io.MultiReader(strings.NewReader(body), unexpectedEOFReader{}))}, nil
				case strings.HasSuffix(r.URL.Path, "/live_recovered/hangup"):
					hangups++
					return &http.Response{StatusCode: cleanup, Body: io.NopCloser(strings.NewReader("{}"))}, nil
				default:
					creates++
					return nil, errors.New("unexpected second paid session")
				}
			})}
			_, _, err := createLiveSession(context.Background(), c, "live_source", "offer", openai.LiveSessionConfig{}, func(string, ...any) {})
			if !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, openai.ErrSessionCreated) || forks != 1 || creates != 0 || hangups != 1 {
				t.Fatalf("cleanup=%d err=%v forks=%d creates=%d hangups=%d", cleanup, err, forks, creates, hangups)
			}
		}
	}
}

func TestExpiredForkStillFallsBack(t *testing.T) {
	c := openai.NewLive("fake-key", "")
	forks, creates := 0, 0
	c.HTTP = &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/fork") {
			forks++
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"expired"}}`))}, nil
		}
		creates++
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(`{"session":{"id":"live_fresh"},"transport":{"sdp":"v=0\r\n"}}`))}, nil
	})}
	sess, forked, err := createLiveSession(context.Background(), c, "live_source", "offer", openai.LiveSessionConfig{}, func(string, ...any) {})
	if err != nil || forked || sess == nil || sess.ID != "live_fresh" || forks != 1 || creates != 1 {
		t.Fatalf("sess=%v forked=%v err=%v forks=%d creates=%d", sess, forked, err, forks, creates)
	}
}

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
