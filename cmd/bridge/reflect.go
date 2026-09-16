package main

// Learning from a call, after it has ended.

import (
	"context"
	"time"
)

// reflect asks the cheap model what this conversation was worth remembering, and rewrites the
// profile with the answer.
//
// It runs detached, AFTER the hangup. That placement is the whole design: he never waits for it, it
// is never billed against a live session, and if it fails the only consequence is that one
// conversation taught the assistant nothing.
//
// One at a time. Two calls in quick succession would otherwise each read the profile, each rewrite
// it from what they read, and the second would erase the first.
func (a *app) reflect(said string) {
	if !a.reflecting.CompareAndSwap(false, true) {
		a.ev("reflect_skipped", "note", "one is already running")
		return
	}
	defer a.reflecting.Store(false)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	snapshot := a.mem.Facts()
	facts, err := a.assist.answerer.Reflect(ctx, snapshot, said)
	if err != nil {
		a.ev("reflect_failed", "err", err.Error(),
			"note", "the profile is unchanged, which is the right failure")
		return
	}
	if len(facts) == 0 {
		a.ev("reflect_nothing", "note", "nothing in that conversation was worth keeping")
		return
	}
	before := len(snapshot)
	// From the snapshot the model saw: anything he asked to remember while it was thinking is
	// kept, not overwritten.
	if err := a.mem.ReplaceFrom(snapshot, facts); err != nil {
		a.ev("reflect_save_failed", "err", err.Error())
		return
	}
	a.ev("reflect_saved", "was", before, "now", len(facts))
}
