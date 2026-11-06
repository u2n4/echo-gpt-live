package bridge

// Hearing the user say goodbye, so the call ends when the conversation does.
//
// GPT-Live bills wall-clock time whether anyone is speaking or not, so a session left open after
// the last word costs money for nothing. The Echo streams its microphone continuously, room noise
// included, so "no packets arrived" can never mean "nobody is here". What does mean it is the
// model's own transcript of the user: it transcribes speech, not a room.
//
// Two signals come out of that transcript. Any text at all is proof a person is talking, which is
// what an idle timer should actually be counting. And the text itself can say the conversation is
// over.
//
// A false positive here cuts someone off mid-sentence, so the phrases are deliberately few and
// unambiguous, the match must land at the END of what was said, and a detection only arms a timer
// that any further speech cancels.

import (
	"strings"
	"sync"
	"time"
	"unicode"
)

// DefaultFarewells are the phrases that end a call. Gulf Arabic and English, written the way they
// are actually said. Anything that doubles as a greeting or as mid-conversation filler is left out
// on purpose: "خلاص" alone means "enough" as often as it means "done", and "السلام عليكم" opens a
// conversation at least as often as it closes one.
var DefaultFarewells = []string{
	// Arabic, leaving
	"مع السلامه",
	// The transcriber dropped the final letter on the 22:08 call of 2026-09-13: he said مع السلامة
	// and the transcript read "مع السلام", which matched nothing, and the call stayed open until
	// he told Alexa to stop. The short form is a goodbye on its own in any case.
	"مع السلام",
	"يلا سلام",
	"بكلمك بعدين",
	"بعدين بكلمك",
	"اكلمك بعدين",
	"الى اللقاء",
	"تصبح على خير",
	"تصبحين على خير",
	"في امان الله",
	"يلا باي",
	"خلاص باي",
	"خلاص مع السلامه",
	// Arabic, an explicit instruction
	"اقفل المكالمه",
	"سكر المكالمه",
	"اقفل الاتصال",
	"انهي المكالمه",
	// English, leaving
	"goodbye",
	"good bye",
	"bye bye",
	"see you later",
	"talk to you later",
	"good night",
	// English, an explicit instruction
	"end the call",
	"hang up",
	"stop the call",
}

// normalise folds the spelling variants that would otherwise make a match miss: Arabic diacritics,
// the four ways of writing alef, the two ways of ending a word in ه or ة, and the tatweel used for
// stretching a word. Case and runs of whitespace go too.
func normalise(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 0x064B && r <= 0x0652, r == 0x0640, r == 0x0670:
			continue // harakat and tatweel carry no information for a phrase match
		case unicode.IsSpace(r):
			if !lastSpace {
				b.WriteRune(' ')
				lastSpace = true
			}
			continue
		case r == 'أ' || r == 'إ' || r == 'آ' || r == 'ٱ':
			b.WriteRune('ا')
		case r == 'ة':
			b.WriteRune('ه')
		case r == 'ى':
			b.WriteRune('ي')
		case r == 'ؤ':
			b.WriteRune('و')
		case r == 'ئ':
			b.WriteRune('ي')
		case unicode.IsPunct(r):
			continue
		default:
			b.WriteRune(r)
		}
		lastSpace = false
	}
	return strings.TrimSpace(b.String())
}

// substantive reports whether a transcript fragment is a person continuing to talk rather than the
// transcriber tidying up after them. One short token is not someone changing their mind.
func substantive(text string) bool {
	n := normalise(text)
	if len(n) < 3 {
		return false
	}
	for _, w := range strings.Fields(n) {
		if len([]rune(w)) >= 2 {
			return true
		}
	}
	return false
}

// blockers are the words that turn a farewell into something else. "Do not hang up" and "how do I
// say goodbye" both end in a farewell phrase and both mean the opposite of one, so the words in
// front of the match decide whether it counts.
var blockers = []string{
	// negation
	"لا", "ما", "مو", "مب", "بدون", "not", "dont", "do not", "never", "without",
	// a question about the words rather than the act
	"كيف", "وش", "ايش", "متى", "ليش", "how", "what", "when", "why", "say", "means", "meaning",
}

// negated reports whether the words immediately before a matched phrase cancel it.
func negated(recent, phrase string) bool {
	before := strings.TrimSpace(strings.TrimSuffix(recent, phrase))
	if before == "" {
		return false
	}
	words := strings.Fields(before)
	// Look at the last few words only: a negation five words back is not about this phrase.
	from := len(words) - 3
	if from < 0 {
		from = 0
	}
	for _, w := range words[from:] {
		for _, b := range blockers {
			if w == b {
				return true
			}
		}
	}
	return false
}

// Reset forgets everything, so a goodbye armed in one call cannot end the next one.
func (f *Farewell) Reset() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recent, f.raw, f.armed, f.matched = "", "", false, ""
	// The voice judge belongs to the leg that installed it: after that leg closes its LastVoice
	// is frozen, and a frozen "silent" would let her echo cancel his next goodbye.
	f.Speaking = nil
}

// SetSpeaking installs the voice judge under the lock Feed reads it with; a bare assignment
// from the bridge raced the call inside Feed.
func (f *Farewell) SetSpeaking(fn func() bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Speaking = fn
}

// farewellTail is how many CHARACTERS of the recent transcript are kept. A goodbye is short and
// comes last, so matching against a long history would let a phrase said minutes ago end a live
// conversation.
//
// Characters, not bytes. Arabic is two bytes per letter in UTF-8, so trimming at a byte offset cuts
// a letter in half and leaves a broken rune at the front of the window. On a language where every
// letter is multi-byte that is not a corner case, it is every single trim.
const farewellTail = 120

// Farewell watches a running transcript for the end of a conversation.
type Farewell struct {
	mu      sync.Mutex
	phrases []string
	raw     string // the last little while of transcript, spacing untouched
	recent  string // raw, normalised for matching
	ev      Events

	armedAt time.Time
	armed   bool
	matched string

	// Speaking reports whether the ASSISTANT is talking right now. Her own voice comes back in
	// through the Echo's microphone and is transcribed as HIS input, so without this her goodbye
	// cancels his. Measured on the 15:03 call: he said "مع السلامة" three times, she replied each
	// time, and each reply withdrew the goodbye 8.7 s, 0.4 s and 5.3 s later.
	Speaking func() bool
}

// NewFarewell builds a detector. An empty phrase list uses the defaults; a list of one empty string
// disables detection, which is how a caller turns the feature off.
func NewFarewell(phrases []string, ev Events) *Farewell {
	if ev == nil {
		ev = func(string, ...any) {}
	}
	var use []string
	if len(phrases) == 0 {
		phrases = DefaultFarewells
	}
	for _, p := range phrases {
		if n := normalise(p); n != "" {
			use = append(use, n)
		}
	}
	return &Farewell{phrases: use, ev: ev}
}

// Feed adds a fragment of the user's transcript and reports whether it ends the conversation.
//
// The match must land at the end of what has been said: "goodbye for now, but first..." is not a
// goodbye, and the words that follow a farewell are what tell you it was not one.
func (f *Farewell) Feed(text string) bool {
	if f == nil || len(f.phrases) == 0 {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	// No space is inserted between fragments. The transcript carries its own spacing, and adding
	// one splits words that arrive in two deltas. The RAW text is what accumulates, and the
	// normalised form is derived from it each time: normalising first trimmed a fragment's trailing
	// space, so "مع " followed by "السلامة" became one word and never matched. Codex found it.
	f.raw += text
	if r := []rune(f.raw); len(r) > farewellTail {
		f.raw = string(r[len(r)-farewellTail:])
	}
	f.recent = normalise(f.raw)
	// New speech withdraws a goodbye that has not yet taken effect, but only real speech. A
	// transcriber emits trailing fragments of its own, and a stray character or a piece of
	// punctuation arriving after "goodbye" must not hold the line open until the idle timer.
	if f.armed && substantive(f.recent) {
		// The asymmetry here is deliberate. While she is speaking, a "new utterance" is far more
		// likely to be her own voice echoing off the room than him changing his mind, so it must
		// not KEEP the call alive - that is the direction that costs money and that he noticed.
		// Matching is left alone: her echo can still END a call, which only ever ends it sooner.
		if f.Speaking != nil && f.Speaking() {
			f.ev("farewell_echo_ignored", "heard", f.matched,
				"note", "the assistant was talking; her own voice does not take back his goodbye")
		} else {
			f.armed = false
			// How much he said, never what: his words reach the log only when he switched that on.
			f.ev("farewell_withdrawn", "heard", f.matched, "then_said_chars", len([]rune(strings.TrimSpace(text))),
				"note", "the user kept talking, so the call stays open")
		}
	}
	for _, p := range f.phrases {
		if strings.HasSuffix(f.recent, p) && !negated(f.recent, p) {
			f.armed, f.armedAt, f.matched = true, time.Now(), p
			f.recent, f.raw = "", ""
			f.ev("farewell_heard", "phrase", p, "note", "closing unless the user speaks again")
			return true
		}
	}
	return false
}

// Arm records a goodbye that was understood rather than matched: the model heard him say it in
// words the phrase list does not have, and asked for the call to end. It joins the same wait as a
// matched phrase, measured from when the goodbye was said, so the line closes after she has
// finished answering it and anything he says in the meantime still takes it back. It reports
// whether this goodbye is NEW: when the phrase match got there first, one goodbye is one goodbye,
// and the caller must not answer it a second time.
func (f *Farewell) Arm(how string, at time.Time) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.armed {
		return false
	}
	if at.IsZero() {
		at = time.Now()
	}
	f.armed, f.armedAt, f.matched = true, at, how
	f.recent, f.raw = "", ""
	f.ev("farewell_heard", "phrase", how, "note", "closing unless the user speaks again")
	return true
}

// Armed reports the goodbye that is waiting, and when it was heard. The caller decides when to act
// on it, because "wait two seconds" means two seconds after SHE STOPS TALKING, and only the caller
// knows whether she is still talking.
func (f *Farewell) Armed() (phrase string, at time.Time, ok bool) {
	if f == nil {
		return "", time.Time{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.armed {
		return "", time.Time{}, false
	}
	return f.matched, f.armedAt, true
}

// Disarm forgets a goodbye the caller has acted on.
func (f *Farewell) Disarm() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed = false
}

// Due reports whether an armed farewell has waited out its grace period, which exists so the
// assistant can finish saying goodbye before the line drops.
func (f *Farewell) Due(grace time.Duration) (string, bool) {
	if f == nil {
		return "", false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.armed || time.Since(f.armedAt) < grace {
		return "", false
	}
	f.armed = false
	return f.matched, true
}

// IsArmed reports whether a goodbye is waiting out its grace period.
func (f *Farewell) IsArmed() bool {
	_, _, ok := f.Armed()
	return ok
}
