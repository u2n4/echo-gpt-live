package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/u2n4/echo-gpt-live/internal/bridge"
)

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
