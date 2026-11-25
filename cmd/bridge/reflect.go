package main

// Learning from a call, after it has ended.
// Updates both layers of household memory: the 80-fact active profile (memory.md) and the
// unlimited searchable archive (memory_archive.jsonl).

import (
	"context"
	"time"
)

func (a *app) reflect(said string) {
	if !a.reflecting.CompareAndSwap(false, true) {
		a.ev("reflect_skipped", "note", "one is already running")
		return
	}
	defer a.reflecting.Store(false)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	snapshot := a.mem.Facts()
	facts, archiveNotes, err := a.assist.answerer.ReflectFull(ctx, snapshot, said)
	if err != nil {
		a.ev("reflect_failed", "err", err.Error(),
			"note", "the profile is unchanged, which is the right failure")
		return
	}
	if len(facts) == 0 && len(archiveNotes) == 0 {
		a.ev("reflect_nothing", "note", "nothing in that conversation was worth keeping")
		return
	}
	before := len(snapshot)
	if len(facts) > 0 {
		if err := a.mem.ReplaceFrom(snapshot, facts); err != nil {
			a.ev("reflect_save_failed", "err", err.Error())
		}
	}
	if len(archiveNotes) > 0 {
		if err := a.mem.Archive(archiveNotes); err != nil {
			a.ev("reflect_archive_failed", "err", err.Error())
		}
	}
	a.ev("reflect_saved", "was", before, "now", len(a.mem.Facts()), "archived_new", len(archiveNotes), "archive_total", a.mem.ArchiveCount())
}
