package main

// Hearing a yes.
//
// A write tool refuses until the owner agrees out loud. That agreement is the only thing standing
// between a sentence anybody in the room can say and a change on disk, so it is worth being exact
// about what counts as one.
//
// The first version of this asked whether the transcript CONTAINED a word meaning yes. An
// independent review took nine seconds to break it: "لا تسوها" — do not do it — contains "سوها",
// and "do not do it" contains "do it". A refusal read as permission. The same shape of bug had
// already been found once in this project, in the farewell matcher, which is how a lesson becomes
// a rule:
//
//	A SAFETY PREDICATE IS NEVER A SUBSTRING TEST.
//
// So this works the other way round. It splits what was heard into words, and:
//
//   - any refusal word, anywhere, means no. Refusal wins over agreement, always.
//   - a word that begins with a negative particle — لا, ما, مو, مب — is a refusal, because Arabic
//     attaches them and "لاتسوها" is one word. This also catches innocent words like "لازم", which
//     is the right way to be wrong.
//   - agreement must be a SHORT, WHOLE utterance: at most four words, one of which is an
//     affirmative. "نعم" and "ايه سوها" are agreement. A sentence with يعني ... نعم ... buried in it
//     is not, because it is a person talking, not a person answering.
//   - anything else is neither, and is treated as a new question instead of an answer.
//
// This cannot stop somebody who means to cause trouble; a person who can speak in the room can say
// "نعم". It stops accidents, and pretending otherwise would be worse than admitting it.

import (
	"strings"
	"sync"
	"time"
)

// heard is what the room said after a write was proposed.
type heard int

const (
	heardNothing heard = iota // he has not answered yet
	heardNo                   // he refused, or said something with a refusal in it
	heardYes                  // a short, whole agreement
)

// affirmatives are complete answers to "shall I?". Gulf Arabic first, since that is the room.
var affirmatives = map[string]bool{
	"نعم": true, "ايه": true, "ايوه": true, "اي": true, "اوك": true, "اوكي": true,
	"تمام": true, "زين": true, "سوها": true, "سويها": true, "اكتبها": true, "نفذ": true,
	"موافق": true, "اكيد": true,
	"yes": true, "yeah": true, "yep": true, "ok": true, "okay": true, "sure": true,
	"confirm": true, "ahead": true, "proceed": true,
}

// refusals end it. Anything here anywhere in the utterance means no.
var refusals = map[string]string{
	"no": "no", "nope": "no", "not": "not", "dont": "don't", "cancel": "cancel",
	"stop": "stop", "wait": "wait", "nevermind": "never mind", "forget": "forget",
	"كلا": "كلا", "الغ": "ألغِ", "الغيها": "ألغِها", "بطل": "بطّل", "توقف": "توقف",
	"خطا": "خطأ", "غلط": "غلط",
}

// negativePrefixes are the particles Arabic attaches to the front of a word. A word starting with
// one is treated as a refusal without looking at the rest of it.
var negativePrefixes = []string{"لا", "ما", "مو", "مب", "مش"}

// confirmMaxWords is what makes an agreement an ANSWER rather than a word that happened to occur.
// Four covers "ايه سوها", "yes go ahead", "نعم نفذ الحين".
const confirmMaxWords = 4

// listen decides what the room said. It is deliberately quick to say no and slow to say yes.
func listen(s string) heard {
	words := normalizeWords(s)
	if len(words) == 0 {
		return heardNothing
	}
	for _, w := range words {
		if _, no := refusals[w]; no {
			return heardNo
		}
		// A whole affirmative is not a negative particle with something attached: "موافق" begins
		// with "مو" and could never say yes until this line existed (found by the 2026-09-16 fleet).
		if affirmatives[w] {
			continue
		}
		for _, p := range negativePrefixes {
			if strings.HasPrefix(w, p) {
				return heardNo
			}
		}
	}
	if len(words) > confirmMaxWords {
		// He is talking, not answering. Treated as a new question, never as permission.
		return heardNothing
	}
	// A yes is an utterance made ONLY of agreement: "نعم", "ايه سوها", "yes go ahead". One
	// affirmative inside a short sentence is not one: "تمام وش الجو اليوم" opens with a discourse
	// marker and asks about the weather, and it used to grant the write (the 2026-09-16 gate).
	// "اي" alone is yes; in front of another word it is "which" ("اي يوم؟"). Grok found that one.
	sawYes := false
	for _, w := range words {
		switch {
		case w == "اي" && len(words) > 1:
			return heardNothing
		case affirmatives[w]:
			sawYes = true
		case fillers[w]:
		default:
			return heardNothing
		}
	}
	if sawYes {
		return heardYes
	}
	return heardNothing
}

// fillers may accompany a yes without changing it: "نعم نفذ الحين", "yes please", "go ahead".
var fillers = map[string]bool{
	"الحين": true, "هالحين": true, "طيب": true, "خلاص": true, "يلا": true, "لو": true, "سمحت": true,
	"now": true, "please": true, "go": true, "do": true, "it": true, "then": true,
}

// normalizeWords turns recognition output into comparable words: Arabic spelling varies between
// recognisers (أ إ آ for ا, ى for ي, ة for ه) and punctuation is not a word.
func normalizeWords(s string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r == 'أ' || r == 'إ' || r == 'آ' || r == 'ٱ':
			b.WriteRune('ا')
		case r == 'ى':
			b.WriteRune('ي')
		case r == 'ة':
			b.WriteRune('ه')
		case r >= 0x064B && r <= 0x0652: // harakat, which recognisers add or drop at random
		case r == '\'' || r == '’':
			// don't -> dont, so one apostrophe convention is not a hole in the refusal list
		case r >= 0x0600 && r <= 0x06FF, r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Fields(b.String())
}

// pendingWrite is the ONE call that was refused and is waiting to be agreed to.
//
// It holds the exact tool and the exact arguments, not the question that led to them. Replaying the
// question instead would mean asking the model again with permission already granted, and the model
// is free to come back with a different write, or three of them. Permission is for a specific act.
type pendingWrite struct {
	mu    sync.Mutex
	tool  string
	args  string // raw JSON, exactly as the model asked for it
	what  string // the sentence that was said out loud
	deleg string // the delegation the question was asked under, so the result can be spoken into it
	at    time.Time
	call  string // which Echo session armed it; a later call cannot use it
}

// arm replaces whatever was waiting: he was just asked about THIS act, so a yes now means this
// one. It reports whether an earlier proposal was superseded, so the caller can say so.
func (p *pendingWrite) arm(call, tool, args, what, deleg string) (superseded string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tool != "" && p.call == call {
		superseded = p.tool
	}
	p.call, p.tool, p.args, p.what, p.deleg, p.at = call, tool, args, what, deleg, time.Now()
	return superseded
}

// take is waiting followed by clear, as one step. Two listeners can hear the same yes - the
// transcript as it arrives, and a delegation the model raises about it - and the write must run
// once, for whichever got here first.
func (p *pendingWrite) take(call string, window time.Duration) (tool, args, what, deleg string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tool == "" || p.call != call || time.Since(p.at) >= window {
		return "", "", "", "", false
	}
	tool, args, what, deleg = p.tool, p.args, p.what, p.deleg
	p.call, p.tool, p.args, p.what, p.deleg = "", "", "", "", ""
	return tool, args, what, deleg, true
}

// waiting returns the armed call if it belongs to this call and is still fresh. It does NOT clear:
// the caller clears once it knows what was said, so a delegation that arrives while he is still
// thinking does not silently spend his permission.
func (p *pendingWrite) waiting(call string, window time.Duration) (tool, args, what string, at time.Time, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tool == "" || p.call != call || time.Since(p.at) >= window {
		return "", "", "", time.Time{}, false
	}
	return p.tool, p.args, p.what, p.at, true
}

func (p *pendingWrite) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.call, p.tool, p.args, p.what, p.deleg = "", "", "", "", ""
}
