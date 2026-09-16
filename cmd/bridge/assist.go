package main

// The half of the assistant that is not audio.
//
// gpt-live-1 answers from what it was trained on. When a question needs this machine, this house,
// his notes, or today, it raises a delegation, and everything below is what happens underneath.
//
// One detail decides the whole shape: the delegation event carries ONLY METADATA, NOT THE TASK TEXT.
// OpenAI's guide is explicit that the client has to track transcripts itself. So the bridge keeps a
// rolling window of what he has been saying, and when a delegation arrives, that window is the
// question.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/u2n4/echo-gpt-live/internal/openai"
	"github.com/u2n4/echo-gpt-live/internal/tools"
)

// transcriptWindow keeps the last little while of what the user said.
//
// Bounded by time rather than by length, because what matters is "what was he asking just now", and
// an utterance from four minutes ago is not that. Fragments arrive with their own spacing, so none
// is inserted.
type transcriptWindow struct {
	mu    sync.Mutex
	keep  time.Duration
	buf   []windowPart
	whole strings.Builder
	taken time.Time // when the last question was handed out; the next one starts here
}

type windowPart struct {
	at   time.Time
	text string
}

// firstQuestionReach is how far back the FIRST delegation of a call looks: long enough for a
// question asked at a kitchen pace, short enough to leave the greeting from the start of the call
// where it was.
const firstQuestionReach = 15 * time.Second

func newTranscriptWindow(keep time.Duration) *transcriptWindow {
	if keep <= 0 {
		keep = 45 * time.Second
	}
	return &transcriptWindow{keep: keep}
}

func (w *transcriptWindow) add(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	w.buf = append(w.buf, windowPart{at: now, text: text})
	cut := now.Add(-w.keep)
	for len(w.buf) > 0 && w.buf[0].at.Before(cut) {
		w.buf = w.buf[1:]
	}
}

// question returns what he has said SINCE THE LAST QUESTION WAS TAKEN, and marks it taken.
//
// It used to return the whole 45-second window every time, and that is where three faults on the
// 20:52 call came from at once. His greeting from forty seconds earlier was still in the window,
// so every delegation re-answered it - "she keeps saying hello". Three utterances stacked into a
// 151-201 character "question", so every answer ran to 550 characters. And after "مع السلامة" she
// was still answering the pile fourteen seconds later, so she never went quiet for the two
// seconds the goodbye needs, and the call stayed open until he ended it himself.
//
// A delegation is the model asking about what was JUST said. What was already answered is not
// part of that.
//
// Reading and marking happen under ONE lock. They used to be two, and a fragment arriving between
// them belonged to neither this question nor the next. Codex found it.
func (w *transcriptWindow) question() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	from := w.taken
	if from.IsZero() {
		// The first question of a call reaches back only as far as a question can be long. The
		// whole window reached back to his greeting, which the first delegation then answered as
		// if it were part of the question. Grok found it.
		from = time.Now().Add(-firstQuestionReach)
	}
	q := w.sinceLocked(from)
	w.taken = time.Now()
	return q
}

// since returns only what was said after a given moment, which is how a spoken confirmation is told
// apart from the request that asked for it.
func (w *transcriptWindow) since(t time.Time) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sinceLocked(t)
}

func (w *transcriptWindow) sinceLocked(t time.Time) string {
	cut := time.Now().Add(-w.keep)
	if t.After(cut) {
		cut = t
	}
	var b strings.Builder
	for _, p := range w.buf {
		if p.at.Before(cut) {
			continue
		}
		b.WriteString(p.text)
	}
	return strings.TrimSpace(b.String())
}

func (w *transcriptWindow) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = nil
	w.whole.Reset()
	w.taken = time.Time{}
}

// whole is everything he said during THIS call. The rolling window answers "what is he asking
// now"; remembering needs "what was this conversation about", and those are different lengths.
// Capped, because a ninety-minute call must not become a ninety-minute prompt.
const wholeCallCap = 24 << 10

func (w *transcriptWindow) keepWhole(text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.whole.Len() < wholeCallCap {
		w.whole.WriteString(text)
	}
}

// Whole returns everything said in this call.
func (w *transcriptWindow) Whole() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.TrimSpace(w.whole.String())
}

// TakeWhole returns everything said in this call and forgets it, so a call that is closed twice -
// once on idle, again on teardown - is learned from once and paid for once.
func (w *transcriptWindow) TakeWhole() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := strings.TrimSpace(w.whole.String())
	w.whole.Reset()
	return s
}

// assistant wires the transcript, the tools and the sideband together.
type assistant struct {
	answerer *openai.Answerer
	window   *transcriptWindow
	pending  *pendingWrite
	ev       func(string, ...any)
	// confirmWithin is how long a spoken yes stays valid.
	confirmWithin time.Duration
	inFlight      sync.Mutex // one delegation at a time; the model is waiting for this one

	mu     sync.Mutex
	call   string // the Echo session this conversation belongs to
	ctx    context.Context
	cancel context.CancelFunc
	// spk is how a result reaches the conversation when it was the TRANSCRIPT, not a delegation,
	// that settled it: his yes to a proposed write.
	spk speaker
	// lastQ and lastA are the previous question of this call and what was answered, so "and
	// tomorrow?" has something to be about. The router used to see only the words just said.
	lastQ, lastA string

	// refresh rebuilds the router's context (place, time, memory) after something in it changed
	// mid-call - a fact he just asked to remember, for instance. Set by the bridge.
	refresh func()
}

// newCall starts a fresh conversation. Nothing said before it and nothing agreed to before it
// carries over: this object outlives every call, and permission that outlives the call that granted
// it is permission the next person in the room inherits.
//
// It also returns a context that is cancelled when this call ends, so work started for it does not
// go on running, and spending, after the line is down.
func (a *assistant) newCall(id string) {
	a.mu.Lock()
	if a.cancel != nil {
		a.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.ctx, a.cancel, a.call = ctx, cancel, id
	a.spk, a.lastQ, a.lastA = nil, "", ""
	a.mu.Unlock()

	a.window.reset()
	a.pending.clear()
}

// setSpeaker gives the call its control socket, once the OpenAI leg is up.
func (a *assistant) setSpeaker(s speaker) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.spk = s
}

func (a *assistant) speakerFor(call string) speaker {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.call != call || call == "" {
		return nil
	}
	return a.spk
}

// previous returns the last exchange of this call.
func (a *assistant) previous() (q, ans string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastQ, a.lastA
}

func (a *assistant) remember(q, ans string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastQ, a.lastA = q, ans
}

// endCall cancels anything still running for the call that just finished.
func (a *assistant) endCall() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.call = ""
	a.pending.clear()
}

// current returns the live call's context and id. A zero id means there is no call, and a
// delegation that arrives then has nothing to answer into.
func (a *assistant) current() (context.Context, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx == nil {
		return context.Background(), ""
	}
	return a.ctx, a.call
}

// onTranscript feeds the window, and notices a spoken confirmation for a waiting write.
//
// The yes is acted on HERE, as the words arrive. It used to wait for the model to raise another
// delegation about it, and a model that hears "نعم" is as likely to say "تمام" and move on as to
// ask for help, in which case the thing he agreed to never happened. Codex found it.
func (a *assistant) onTranscript(text string) {
	a.window.add(text)
	a.window.keepWhole(text)

	ctx, call := a.current()
	_, _, _, at, ok := a.pending.waiting(call, a.confirmWithin)
	if !ok {
		return
	}
	said := listen(a.window.since(at))
	if said == heardNothing {
		return
	}
	tool, args, _, deleg, taken := a.pending.take(call, a.confirmWithin)
	if !taken {
		return // a delegation about the same yes got there first
	}
	sp := a.speakerFor(call)
	if sp == nil {
		a.ev("confirmation_unspeakable", "tool", tool, "note", "no control socket for this call")
		return
	}
	// Off the transcript thread: a write can take seconds, and the next fragment must not wait.
	go a.settle(ctx, sp, deleg, tool, args, said)
}

// settle runs, or declines to run, the one write he was asked about, and says what happened.
func (a *assistant) settle(ctx context.Context, sp speaker, deleg, tool, args string, said heard) {
	if said == heardNo {
		a.ev("delegation_refused", "tool", tool)
		a.say(ctx, sp, deleg, spokenDeclined)
		return
	}
	a.ev("delegation_confirmed", "tool", tool)
	out, err := a.answerer.RunTool(ctx, tool, args)
	if err != nil {
		a.ev("delegation_write_failed", "tool", tool, "err", err.Error())
		out = spokenFailed + err.Error()
	} else if tool == "remember" && a.refresh != nil {
		// What she knows about him changed a second ago; the next question should know it too.
		a.refresh()
	}
	a.say(ctx, sp, deleg, out)
}

// say speaks into the delegation the question was asked under, and into the session at large if
// that delegation is no longer accepting.
func (a *assistant) say(ctx context.Context, sp speaker, deleg, content string) {
	if err := sp.Commentary(ctx, deleg, content); err != nil {
		if deleg == "" {
			a.ev("delegation_send_failed", "err", err.Error())
			return
		}
		if err2 := sp.Commentary(ctx, "", content); err2 != nil {
			a.ev("delegation_send_failed", "err", err2.Error())
		}
	}
}

// speak is how the bridge gets a sentence back into the conversation. commentary is spoken aloud;
// thinking is only known.
type speaker interface {
	Commentary(ctx context.Context, delegationID, content string) error
	Thinking(ctx context.Context, delegationID, content string) error
}

// onDelegation answers one delegation and says the result.
func (a *assistant) onDelegation(ctx context.Context, sb speaker, delegationID string) {
	if !a.inFlight.TryLock() {
		a.ev("delegation_skipped", "reason", "one is already being answered")
		return
	}
	defer a.inFlight.Unlock()

	_, call := a.current()

	// A write is waiting to be agreed to, and only what he said AFTER it was proposed counts — the
	// request cannot confirm itself. What runs on a yes is the EXACT call that was refused, not the
	// question that produced it: permission is for one act, not for another turn of the model.
	if _, _, _, at, ok := a.pending.waiting(call, a.confirmWithin); ok {
		switch said := listen(a.window.since(at)); said {
		case heardYes, heardNo:
			// The transcript listener usually settles this first; if it has, the take fails and the
			// model's question is answered by what was already said.
			if tool, args, _, _, taken := a.pending.take(call, a.confirmWithin); taken {
				a.settle(ctx, sb, delegationID, tool, args, said)
			}
			return
		default:
			// He has not answered yet. Leave the permission armed and treat this as a new question.
		}
	}

	question := a.window.question()
	// The delegation and the transcript of the words that caused it travel on the same socket but
	// not in a fixed order. A delegation that arrives a beat before its own words is not one with
	// nothing to answer; give the words a moment to land.
	for i := 0; question == "" && i < 15 && ctx.Err() == nil; i++ {
		time.Sleep(100 * time.Millisecond)
		question = a.window.question()
		if question != "" {
			a.ev("delegation_waited_for_text", "ms", (i+1)*100)
		}
	}
	if question == "" {
		a.ev("delegation_no_question", "note", "a delegation arrived with nothing said before it")
		_ = sb.Thinking(ctx, delegationID, spokenNoQuestion)
		return
	}

	a.ev("delegation_start", "id", delegationID, "chars", len(question))
	t0 := time.Now()

	// The previous exchange of this call rides along, so a follow-up has something to follow. The
	// question is still only what he just said; the router had no idea what "and tomorrow?" meant.
	prompt := question
	if q, ans := a.previous(); q != "" {
		prompt = fmt.Sprintf("Earlier in this same call he asked: %q and was told: %q. Now he says: %s",
			clipRunes(q, 200), clipRunes(ans, 300), question)
	}

	answer, err := a.answerer.Answer(ctx, prompt)
	if err != nil {
		var need *tools.ErrNeedsConfirmation
		if errors.As(err, &need) {
			old := a.pending.arm(call, need.Tool, need.Args, need.What, delegationID)
			ask := spokenAskPermission + need.What
			if old != "" {
				// The earlier proposal is off the table; a yes now is for this one only, and he
				// is told so rather than left to guess which act his word applies to.
				ask = spokenSupersedes + ask
			}
			a.ev("delegation_asks_permission", "tool", need.Tool, "supersedes", old)
			_ = sb.Commentary(ctx, delegationID, ask)
			return
		}
		a.ev("delegation_failed", "err", err.Error(), "ms", time.Since(t0).Milliseconds())
		// Say something rather than leave him listening to nothing.
		_ = sb.Commentary(ctx, delegationID, spokenCouldNotFind)
		return
	}

	// The prompt says one or two sentences; the 20:52 call got 550 characters three times. A backstop
	// under the prompt: cut at the last sentence end inside the cap, so what is spoken is still whole.
	answer = spoken(answer, spokenCap)
	a.remember(question, answer)
	a.ev("delegation_answered", "ms", time.Since(t0).Milliseconds(), "chars", len(answer))
	if err := sb.Commentary(ctx, delegationID, answer); err != nil {
		a.ev("delegation_send_failed", "err", err.Error())
	}
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// spokenCap is how much one answer may say out loud. 300 characters of Arabic is about two
// sentences, which is what the prompt asked for and what a person waits for.
const spokenCap = 300

// spoken cuts an answer at the last sentence boundary inside the cap, in characters not bytes.
func spoken(s string, cap int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= cap {
		return string(r)
	}
	r = r[:cap]
	for i := len(r) - 1; i > cap/2; i-- {
		switch r[i] {
		case '.', '!', '?', '؟', '،', '\n':
			return strings.TrimSpace(string(r[:i+1]))
		}
	}
	return strings.TrimSpace(string(r)) + "."
}

func truncateFor(s string, n int) string {
	// by runes: Arabic is two bytes a letter, and a cut inside one leaves invalid UTF-8 in a log line
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// What she says on the paths the router cannot phrase itself. Gulf Arabic, because that is the
// room: these used to be English, and the voice model spoke them in English to him.
const (
	spokenNoQuestion    = "ما سمعت السؤال زين، عيده لو سمحت."
	spokenAskPermission = "أقدر أسويها. قل نعم وبسويها: "
	spokenCouldNotFind  = "ما قدرت أعرف الجواب."
	spokenDeclined      = "تمام، ما بسويها."
	spokenFailed        = "ما زبطت: "
	spokenSupersedes    = "خلاص نسينا الطلب الأول. "
)
