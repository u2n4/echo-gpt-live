package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/u2n4/echo-gpt-live/internal/bridge"
)

func TestGreetingOwnsContextUntilPlaybackFinishes(t *testing.T) {
	connected := make(chan struct{})
	playing := make(chan context.Context, 1)
	finish := make(chan struct{})
	defer close(finish)
	started := time.Now()
	done := startGreeting(connected, func(ctx context.Context) {
		playing <- ctx
		<-finish
	})
	select {
	case <-done:
		t.Fatal("greeting gate released before connection")
	case <-playing:
		t.Fatal("greeting played before connection")
	default:
	}
	close(connected)
	var ctx context.Context
	select {
	case ctx = <-playing:
	case <-time.After(time.Second):
		t.Fatal("greeting did not start after connection")
	}
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(started.Add(8*time.Second)) || deadline.After(time.Now().Add(8*time.Second)) {
		t.Errorf("greeting deadline = %v, present = %v; want eight-second context", deadline, ok)
	}
	if ctx.Err() != nil {
		t.Errorf("greeting context ended during playback: %v", ctx.Err())
	}
	select {
	case <-done:
		t.Fatal("greeting gate released during playback")
	default:
	}
	finish <- struct{}{}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("greeting gate not released after playback")
	}
	if ctx.Err() != context.Canceled {
		t.Errorf("playback context after completion = %v, want canceled", ctx.Err())
	}
}

func TestTransportRetirementInvalidatesDialOwnership(t *testing.T) {
	s := &bridge.Session{ID: "test_session"}
	a := &app{sess: s}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a.retireSession(s) }()
	}
	wg.Wait()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sess == s {
		t.Fatal("an in-flight dial can still attach after transport-down")
	}
}

func TestSupersededConfirmationCallbacksCannotClearCurrentTimer(t *testing.T) {
	a := &assistant{window: newTranscriptWindow(time.Minute)}
	a.deferConfirm(context.Background(), "test", time.Now())
	old := a.confirmGeneration
	a.deferConfirm(context.Background(), "test", time.Now())
	current := a.confirmTimer
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a.finishConfirm(old, context.Background(), "test", time.Now()) }()
	}
	wg.Wait()
	if a.confirmTimer != current {
		t.Fatal("stale callback cleared the replacement timer")
	}
	a.stopConfirm()
}

func TestRetiredOfferCannotReplaceANewCall(t *testing.T) {
	a := &app{}
	if err := a.admitOffer("old"); err != nil {
		t.Fatal(err)
	}
	a.seenOffers["old"] = time.Now()
	a.sess = &bridge.Session{ID: "new"}
	if err := a.admitOffer("old"); err == nil {
		t.Fatal("retired offer was readmitted")
	}
	if a.sess.ID != "new" {
		t.Fatal("late directive replaced the current session")
	}
}

func TestLateCleanupCannotCloseANewerCallsResources(t *testing.T) {
	old := &bridge.Session{ID: "old"}
	current := &bridge.Session{ID: "new"}
	a := &app{sess: current, resourceSession: current}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a.closeLegFor(old, "late_disconnect"); a.teardownFor(old, "late_idle") }()
	}
	wg.Wait()
	if a.sess != current || a.resourceSession != current {
		t.Fatal("old cleanup retired the new call")
	}
}
