package tools

// Ending the call because he said goodbye, in whatever words he used.
//
// The bridge already hears a goodbye by matching phrases in his transcript, and that path is fast
// and needs no model. It is also a list, and on the 16 September 2026 call he said goodbye in
// words that were not on it, so the line stayed open until Alexa dropped it. A list can never be
// finished. The model, on the other hand, knows a goodbye when it hears one in any wording, and
// it is already asked for help after one; this tool is what that help does.
//
// The model is also too eager, and it can be lied to. MEASURED with gpt-4.1-mini, nine
// utterances, the tool described as carefully as words allow: it hung up on "خلاص", on "طيب
// خلاص" and on "تمام شكرا", which are how he ends a TOPIC, not a call. And a model that reads
// web pages can be told by a page what to call. So the decision is not made from the model's
// argument at all: the tool checks the TRANSCRIPT the delegation is answering, which the bridge
// put on the context and no page can write, for a farewell or an instruction to hang up, with
// nothing negating it. The model supplies the understanding - it is the one that decides to
// ask - and this check is the floor under it.
//
// It is a Read in the registry's terms: nothing is written, and hanging up is what the call is
// going to do anyway. On the other side of it the grace period still runs, in which anything he
// says takes the goodbye back.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// ErrCallEnding is how end_call reports itself: not as a sentence for the model to read and reply
// to, but as a signal that stops the rounds. Nothing spoken after a goodbye improves it, and a
// second model round is the silence he would hear instead of the line closing. The caller that
// owns the call is the one that hangs up; the tool only says that he asked.
type ErrCallEnding struct{}

func (*ErrCallEnding) Error() string { return "the call is ending" }

// farewellRE is what a goodbye or a hang-up instruction contains, after normalisation. Narrow in
// meaning and tolerant in spelling: "مع السلام" with or without its final letter, "سلام" only as
// a word of its own (so "السلام عليكم", which opens a call, is not a goodbye), and a hang-up verb
// only when the call, the line or the connection is its object (so "السكر" and "الخط" in a
// sentence about sugar or handwriting are not). "خلاص" and "شكرا" are deliberately absent.
var farewellRE = regexp.MustCompile(
	`مع السلام|(^| )سلام( |$)|يلا سلام|باي|يحفظك|علي خير|امان الله|اللقاء|كلمك بعدين|` +
		`(اقفل|اقفلي|قفل|قفلي|سكر|سكري|انهي|انه|خلص|خلصي)[^ ]* (ال)?(مكالم|اتصال|خط|لاين)|` +
		`(^| )bye|goodbye|good ?night|hang up|end (the|this) call|close (the|this) call|` +
		`see you|talk to you later|that'?s all|that is all|so long|farewell|take care`)

// blockers are the words that turn a farewell into something else when they sit just before it:
// "do not hang up" and "how do I say goodbye" both contain a farewell and neither is one. The
// bridge's phrase matcher keeps the same list.
var blockers = map[string]bool{
	"لا": true, "ما": true, "مو": true, "مب": true, "بدون": true, "not": true, "dont": true, "don't": true, "do": true, "never": true, "without": true,
	"كيف": true, "وش": true, "ايش": true, "متى": true, "ليش": true, "how": true, "what": true, "when": true, "why": true, "say": true, "means": true, "meaning": true,
}

// isFarewell reports whether what he said contains a parting or a request to hang up, with no
// negation or question in the three words before it.
func isFarewell(said string) bool {
	n := normaliseWords(said)
	loc := farewellRE.FindStringIndex(n)
	if loc == nil {
		return false
	}
	before := strings.Fields(strings.TrimSpace(n[:loc[0]]))
	from := len(before) - 3
	if from < 0 {
		from = 0
	}
	for _, w := range before[from:] {
		if blockers[w] {
			return false
		}
	}
	return true
}

// normaliseWords folds the spellings a transcriber varies: harakat and tatweel gone, the alefs
// as one, ة as ه, ى as ي, lower case, single spaces. The bridge's farewell matcher does the same.
func normaliseWords(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 0x064B && r <= 0x0652, r == 0x0640, r == 0x0670:
			continue
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
		case r == '\'':
			b.WriteRune(r) // "don't" and "that's" keep their shape
		case unicode.IsPunct(r):
			continue
		default:
			b.WriteRune(r)
		}
		lastSpace = false
	}
	return strings.TrimSpace(b.String())
}

// EndCall registers the tool.
func EndCall() *Tool {
	return &Tool{
		Name: "end_call",
		Description: "End this phone call. Call it ONLY when he has said goodbye (مع السلامة, باي, " +
			"الله يحفظك, تصبح على خير, في أمان الله, bye, goodbye, good night, see you) or explicitly " +
			"asked to end, close or hang up the call (اقفلي المكالمة, سكري الخط, hang up, end the " +
			"call). Words like خلاص, طيب, تمام, كفاية, شكرا, ok, thanks, done, enough, stop on their " +
			"own are NOT a goodbye: he is finishing a topic, not the call. Never for 'wait', a pause, " +
			"a negation (لا تقفلين), or a question about goodbyes. Nothing needs to be said after it.",
		Access:  Read,
		Timeout: 2 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			// The transcript, not the argument: a page the model read cannot put words here.
			if !isFarewell(Said(ctx)) {
				return "", fmt.Errorf("what he said contains no goodbye and no request to hang up, so the call stays open; answer him normally")
			}
			return "", &ErrCallEnding{}
		},
	}
}
