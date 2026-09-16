package bridge

import (
	"testing"

	"github.com/pion/rtp"
)

// Detaching the sink is what teardown does, and it used to arm a panic on the media goroutine:
// SetAudioSink(nil) stored &f for a nil f, which is a NON-NIL pointer to a NIL function, so the
// reader's nil check passed and the call panicked. That sat on the ordinary shutdown path and could
// kill the process before the OpenAI hangup had been sent. Codex found it by reading the setter.
func TestDetachingTheEchoSinkCannotPanic(t *testing.T) {
	s := &Session{}
	s.SetAudioSink(func(*rtp.Packet) {})
	if s.audioSink.Load() == nil {
		t.Fatal("a real sink should be installed")
	}
	s.SetAudioSink(nil)
	if p := s.audioSink.Load(); p != nil {
		t.Fatal("detaching must store a nil pointer, not a pointer to a nil function")
	}

	// And the reader's guard must survive a pointer to a nil function however it got there.
	var empty func(*rtp.Packet)
	s.audioSink.Store(&empty)
	if sink := s.audioSink.Load(); sink != nil && *sink != nil {
		t.Fatal("the guard would have called a nil function")
	}
}

// The OpenAI leg has the same shape and had the same trap.
func TestDetachingTheOpenAISinkCannotPanic(t *testing.T) {
	l := &LiveLeg{}
	l.SetAudioSink(func(*rtp.Packet) {})
	if l.audioSink.Load() == nil {
		t.Fatal("a real sink should be installed")
	}
	l.SetAudioSink(nil)
	if p := l.audioSink.Load(); p != nil {
		t.Fatal("detaching must store a nil pointer, not a pointer to a nil function")
	}
}

// The callbacks are installed after DialLive has already started its readers, so they must not be
// plain fields. A nil one must detach rather than arm the same trap.
func TestLegCallbacksAreSafeToSetAndClear(t *testing.T) {
	l := &LiveLeg{}
	called := 0
	l.SetOnSpeech(func() { called++ })
	if f := l.onSpeech.Load(); f == nil {
		t.Fatal("OnSpeech should be installed")
	} else {
		(*f)()
	}
	if called != 1 {
		t.Fatalf("called = %d, want 1", called)
	}
	l.SetOnSpeech(nil)
	if l.onSpeech.Load() != nil {
		t.Fatal("clearing OnSpeech must store a nil pointer")
	}
	l.SetOnSidebandEvent(func(string, []byte) {})
	if l.onSidebandEvent.Load() == nil {
		t.Fatal("OnSidebandEvent should be installed")
	}
	l.SetOnSidebandEvent(nil)
	if l.onSidebandEvent.Load() != nil {
		t.Fatal("clearing OnSidebandEvent must store a nil pointer")
	}
}

// A leg whose sideband never opened must report it, so nothing treats "no transcript" as "nobody is
// here" and hangs up on a live conversation.
func TestSidebandHealthDefaultsToDead(t *testing.T) {
	l := &LiveLeg{}
	if l.SidebandLive() {
		t.Fatal("a leg with no sideband must not claim one")
	}
}
