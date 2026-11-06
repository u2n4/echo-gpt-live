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
	// taken is the ordinal of the last fragment handed out as a question; the next question
	// starts after it. An ordinal, not a time: on a clock with coarse ticks (Windows: 0.5-15 ms)
	// a fragment added in the same tick as the take read as older than it and came back in the
	// next question, which is the "she answered my greeting again" fault by another road.
	taken uint64
	n     uint64 // fragments ever added to this call; each part carries its own
}

type windowPart struct {
	at   time.Time
	ord  uint64
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
	w.n++
	w.buf = append(w.buf, windowPart{at: now, ord: w.n, text: text})
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
	// The first question of a call reaches back only as far as a question can be long. The whole
	// window reached back to his greeting, which the first delegation then answered as if it were
	// part of the question. Grok found it.
	cut := time.Now().Add(-firstQuestionReach)
	var b strings.Builder
	for _, p := range w.buf {
		if p.ord <= w.taken || (w.taken == 0 && p.at.Before(cut)) {
			continue
		}
		b.WriteString(p.text)
		w.taken = p.ord
	}
	return strings.TrimSpace(b.String())
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

// lastAt is when the most recent fragment of his speech arrived: the moment his goodbye was
// heard, for a goodbye the model understood a round later.
func (w *transcriptWindow) lastAt() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) == 0 {
		return time.Time{}
	}
	return w.buf[len(w.buf)-1].at
}

func (w *transcriptWindow) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = nil
	w.whole.Reset()
	w.taken, w.n = 0, 0
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
	// hangUp ends the call after a goodbye the model understood, timed from when he said it. Set
	// by the bridge to arm the same farewell the phrase match uses; nil when farewells are off.
	// It reports whether the goodbye was new: a phrase match may have heard, and answered, the
	// same words already.
	hangUp func(at time.Time) bool

	// confirmMu guards confirmTimer only; the permission itself lives in pendingWrite.
	confirmMu         sync.Mutex
	confirmTimer      *time.Timer
	confirmGeneration uint64
}

// confirmQuiet is how long a heard yes waits for the rest of its utterance. The transcript
// arrives in fragments, and acting on the first yes-shaped one executed the write before the
// "no" in the same breath had landed. A refusal is acted on the moment it is heard; only the
// yes waits. Muse found it.
const confirmQuiet = 800 * time.Millisecond

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
	a.stopConfirm()
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
	a.stopConfirm()
}

// setRefresh installs the per-call context rebuild under the same lock as every other field of
// the call: attachLive writes it while a settle on another goroutine may read it.
func (a *assistant) setRefresh(fn func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refresh = fn
}

func (a *assistant) refresher() func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refresh
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
	switch said {
	case heardNothing:
		return
	case heardNo:
		// Final the moment it is heard: a yes still waiting out its quiet window is dropped.
		a.stopConfirm()
		a.act(ctx, call, said)
	default:
		// A yes waits for the rest of its utterance; the words are read again when it fires.
		a.deferConfirm(ctx, call, at)
	}
}

// act spends the permission on what was heard. The speaker is checked BEFORE the permission is
// consumed: with no control socket there is nothing to run into, and his yes stays armed for
// the delegation the model will raise about it.
func (a *assistant) act(ctx context.Context, call string, said heard) {
	sp := a.speakerFor(call)
	if sp == nil {
		a.ev("confirmation_unspeakable", "note", "no control socket for this call; the permission stays armed")
		return
	}
	tool, args, _, deleg, taken := a.pending.take(call, a.confirmWithin)
	if !taken {
		return // a delegation about the same yes got there first
	}
	// Off the transcript thread: a write can take seconds, and the next fragment must not wait.
	go a.settle(ctx, sp, deleg, tool, args, said)
}

// stopConfirm drops a yes waiting out its quiet window. The permission stays armed; only the
// timer goes.
func (a *assistant) stopConfirm() {
	a.confirmMu.Lock()
	defer a.confirmMu.Unlock()
	a.confirmGeneration++
	if a.confirmTimer != nil {
		a.confirmTimer.Stop()
		a.confirmTimer = nil
	}
}

// deferConfirm (re)starts the quiet window for a heard yes. A second yes fragment restarts it
// rather than stacking, so the take at fire time runs once. At fire time the words since the
// proposal are read again: a refusal that arrived late wins, a continuation that is no longer a
// whole yes leaves the permission armed and untouched.
func (a *assistant) deferConfirm(ctx context.Context, call string, at time.Time) {
	a.confirmMu.Lock()
	defer a.confirmMu.Unlock()
	if a.confirmTimer != nil {
		a.confirmTimer.Stop()
	}
	a.confirmGeneration++
	generation := a.confirmGeneration
	a.confirmTimer = time.AfterFunc(confirmQuiet, func() {
		a.finishConfirm(generation, ctx, call, at)
	})
}

func (a *assistant) finishConfirm(generation uint64, ctx context.Context, call string, at time.Time) {
	a.confirmMu.Lock()
	if generation != a.confirmGeneration {
		a.confirmMu.Unlock()
		return
	}
	a.confirmTimer = nil
	a.confirmMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	if said := listen(a.window.since(at)); said != heardNothing {
		a.act(ctx, call, said)
	}
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
		out = spokenFailed
	} else if tool == "remember" {
		// What she knows about him changed a second ago; the next question should know it too.
		if refresh := a.refresher(); refresh != nil {
			refresh()
		}
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
		if err := sb.Thinking(ctx, delegationID, spokenNoQuestion); err != nil {
			a.ev("delegation_send_failed", "err", err.Error())
		}
		return
	}

	a.ev("delegation_start", "id", delegationID, "chars", len(question))
	t0 := time.Now()

	// A question about the world takes a search and a page: 5-9 s measured. The voice model was
	// told to say "one moment" and on the 17:24 call it never did - three questions, 4-6 s of
	// dead air each before her answer. So the bridge says it, at once, for the questions that
	// will take a while. Two words, and only when the words call for a search.
	if tools.WorldCue(question) {
		a.say(ctx, sb, delegationID, spokenLooking)
	}

	// The previous exchange of this call rides along, so a follow-up has something to follow. The
	// question is still only what he just said; the router had no idea what "and tomorrow?" meant.
	ask := openai.Question{Said: question}
	ask.PrevQ, ask.PrevA = a.previous()

	answer, err := a.answerer.Answer(ctx, ask)
	if ctx.Err() != nil {
		return // an ended call cannot arm permission or a goodbye in its successor
	}
	if err != nil {
		var ending *tools.ErrCallEnding
		if errors.As(err, &ending) {
			// He said goodbye in his own words and the model understood it. The goodbye is timed
			// from his last words, not from this round, so the wait for her reply is not
			// lengthened by the time it took to understand him.
			a.ev("delegation_ends_call", "ms", time.Since(t0).Milliseconds())
			// Spoken, not merely known. The voice model was told to say goodbye itself and then
			// ask for help; on the 17:24 call it asked and said nothing, so he heard silence for
			// six seconds and then the line dropped. Her goodbye comes from the bridge - once:
			// when the phrase match already heard these words it has already answered them, and
			// this delegation about the same goodbye says nothing more. Muse found the double.
			fresh := a.hangUp == nil
			if a.hangUp != nil {
				at := a.window.lastAt()
				if at.IsZero() || at.After(t0) {
					at = t0
				}
				fresh = a.hangUp(at)
			}
			if fresh {
				a.say(ctx, sb, delegationID, spokenGoodbye)
			}
			return
		}
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
			a.say(ctx, sb, delegationID, ask)
			return
		}
		a.ev("delegation_failed", "err", err.Error(), "ms", time.Since(t0).Milliseconds())
		// Say something rather than leave him listening to nothing.
		a.say(ctx, sb, delegationID, spokenCouldNotFind)
		return
	}

	// The prompt says one or two sentences; the 20:52 call got 550 characters three times. A backstop
	// under the prompt: cut at the last sentence end inside the cap, so what is spoken is still whole.
	answer = spoken(answer, spokenCap)
	a.remember(question, answer)
	a.ev("delegation_answered", "ms", time.Since(t0).Milliseconds(), "chars", len(answer))
	// Through say: a delegation that already carried the "one moment" may or may not still be
	// accepting, and the answer must reach the room either way.
	a.say(ctx, sb, delegationID, answer)
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
	spokenGoodbye       = "مع السلامة."
	spokenLooking       = "ثواني، أشوف لك."
	spokenHello         = "هلا."
)
