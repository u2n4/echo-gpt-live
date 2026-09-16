package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/u2n4/echo-gpt-live/internal/openai"
	"github.com/u2n4/echo-gpt-live/internal/tools"
)

// fakeSpeaker records what would have been said into the conversation.
type fakeSpeaker struct {
	said chan [2]string // delegation id, content
}

func (f *fakeSpeaker) Commentary(_ context.Context, delegationID, content string) error {
	f.said <- [2]string{delegationID, content}
	return nil
}

func (f *fakeSpeaker) Thinking(_ context.Context, delegationID, content string) error {
	return nil
}

// A yes heard on the transcript runs the agreed write at once, spoken into the delegation that
// asked, without waiting for the model to raise another delegation about it.
func TestAYesOnTheTranscriptRunsTheWriteWithoutAnotherDelegation(t *testing.T) {
	var ran atomic.Int32
	reg := tools.NewRegistry()
	reg.Add(&tools.Tool{
		Name: "note_add", Description: "test write", Access: tools.Write,
		Params: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
		Run: func(context.Context, map[string]any) (string, error) {
			ran.Add(1)
			return "Saved.", nil
		},
	})
	sp := &fakeSpeaker{said: make(chan [2]string, 4)}
	a := &assistant{
		answerer:      &openai.Answerer{Tools: reg},
		window:        newTranscriptWindow(45 * time.Second),
		pending:       &pendingWrite{},
		ev:            func(string, ...any) {},
		confirmWithin: 10 * time.Second,
	}
	a.newCall("call-1")
	a.setSpeaker(sp)
	a.pending.arm("call-1", "note_add", `{"text":"milk"}`, "add milk", "deleg-1")

	a.onTranscript(" نعم")

	select {
	case got := <-sp.said:
		if got[0] != "deleg-1" || got[1] != "Saved." {
			t.Fatalf("spoke %q into %q; want the tool's answer into the asking delegation", got[1], got[0])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("nothing was said: the yes was not acted on")
	}
	if ran.Load() != 1 {
		t.Fatalf("the write ran %d times, want exactly once", ran.Load())
	}
	// The permission is spent: a delegation raised about the same yes must not run it again.
	if _, _, _, _, ok := a.pending.take("call-1", 10*time.Second); ok {
		t.Fatal("the pending write survived the yes that consumed it")
	}
}

// A no is a no, said back, and the write never runs.
func TestANoOnTheTranscriptDeclines(t *testing.T) {
	var ran atomic.Int32
	reg := tools.NewRegistry()
	reg.Add(&tools.Tool{
		Name: "note_add", Description: "test write", Access: tools.Write,
		Params: map[string]any{"type": "object"},
		Run: func(context.Context, map[string]any) (string, error) {
			ran.Add(1)
			return "Saved.", nil
		},
	})
	sp := &fakeSpeaker{said: make(chan [2]string, 4)}
	a := &assistant{
		answerer:      &openai.Answerer{Tools: reg},
		window:        newTranscriptWindow(45 * time.Second),
		pending:       &pendingWrite{},
		ev:            func(string, ...any) {},
		confirmWithin: 10 * time.Second,
	}
	a.newCall("call-1")
	a.setSpeaker(sp)
	a.pending.arm("call-1", "note_add", `{}`, "add", "deleg-1")

	a.onTranscript(" لا تسوها")

	select {
	case got := <-sp.said:
		if got[1] != spokenDeclined {
			t.Fatalf("said %q, want the refusal", got[1])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the refusal was never spoken")
	}
	if ran.Load() != 0 {
		t.Fatal("the write ran on a no")
	}
}

// take is one step: a second listener cannot take what the first already has.
func TestTakeIsOnce(t *testing.T) {
	p := &pendingWrite{}
	p.arm("c", "note_add", `{}`, "add", "d")
	if _, _, _, deleg, ok := p.take("c", time.Minute); !ok || deleg != "d" {
		t.Fatalf("first take: ok=%v deleg=%q", ok, deleg)
	}
	if _, _, _, _, ok := p.take("c", time.Minute); ok {
		t.Fatal("second take succeeded")
	}
}
