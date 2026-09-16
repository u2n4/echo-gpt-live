package main

import (
	"strings"
	"testing"
	"time"
)

// Every string an independent review used to break the substring version is in here by name. This
// is the predicate that stands between a sentence anybody can say and a change on disk.
func TestListen(t *testing.T) {
	cases := []struct {
		said string
		want heard
		why  string
	}{
		// The four that broke it. Each one CONTAINS an affirmative and means the opposite.
		{"لا تسوها", heardNo, "do not do it, and it contains سوها"},
		{"لا اكتبها", heardNo, "do not write it, and it contains اكتبها"},
		{"do not do it", heardNo, "contains do it"},
		{"I do not confirm", heardNo, "contains confirm"},

		// The plain answers the assistant actually asks for, which the old version rejected.
		{"نعم", heardYes, ""},
		{"ايه", heardYes, ""},
		{"أيوه", heardYes, "hamza spelling varies between recognisers"},
		{"yes", heardYes, ""},
		{"okay", heardYes, ""},
		{"ايه سوها", heardYes, ""},
		{"yes go ahead", heardYes, ""},
		{"اي", heardYes, "on its own it is yes"},
		{"اي يوم", heardNothing, "which day? - a question, and اي is not yes in front of another word"},
		{"أي ساعة تقصد", heardNothing, "which time do you mean"},
		{"نعم نفذ", heardYes, ""},
		{"تمام سويها", heardYes, ""},
		{"موافق", heardYes, "a whole word that happens to begin with the negative particle مو"},
		{"موافق سوها", heardYes, ""},
		{"مو موافق", heardNo, "the particle on its own still refuses"},
		{"please tell me more", heardNothing, "please is politeness, not an answer; four words with it must not be permission"},
		{"تمام وش الجو اليوم", heardNothing, "opens with a discourse marker and asks about the weather; not consent"},
		{"زين وين الحين", heardNothing, "a question that happens to start with an affirmative"},
		{"ok what time is it", heardNothing, "five words anyway, and a question"},
		{"نعم نفذ الحين", heardYes, "an affirmative, a verb and a filler"},
		{"yes please", heardYes, ""},

		// Refusals in both languages.
		{"no", heardNo, ""},
		{"لا", heardNo, ""},
		{"كلا", heardNo, ""},
		{"cancel", heardNo, ""},
		{"stop", heardNo, ""},
		{"خلاص لا", heardNo, ""},
		{"don't", heardNo, "apostrophe is stripped, not treated as a word break"},
		{"dont do it", heardNo, ""},
		{"ما ابي", heardNo, ""},
		{"مو الحين", heardNo, ""},

		// Not an answer. He is talking, and a word that means yes happened to occur.
		{"", heardNothing, ""},
		{"يعني ايه يعني وش قصدك بالضبط وش تبي تسوي", heardNothing, "too long to be an answer"},
		{"I was saying yes to the other thing we talked about earlier", heardNothing, "too long"},
		{"وش الجو اليوم", heardNothing, "a new question"},
		{"كيف الجهاز", heardNothing, ""},

		// Ambiguity fails closed: a refusal anywhere beats an agreement anywhere.
		{"نعم لا", heardNo, ""},
		{"yes no wait", heardNo, ""},
	}
	for _, c := range cases {
		if got := listen(c.said); got != c.want {
			t.Errorf("listen(%q) = %v, want %v  %s", c.said, got, c.want, c.why)
		}
	}
}

// TestPendingBelongsToOneCall is the other half: permission granted on one call must not be
// spendable on the next. The assistant object outlives every call.
func TestPendingBelongsToOneCall(t *testing.T) {
	p := &pendingWrite{}
	p.arm("call-A", "notes_write", `{"text":"x"}`, "write x", "d1")

	if _, _, _, _, ok := p.waiting("call-B", time.Minute); ok {
		t.Error("a later call inherited permission granted during an earlier one")
	}
	tool, args, _, _, ok := p.waiting("call-A", time.Minute)
	if !ok {
		t.Fatal("the call that armed it cannot use it")
	}
	if tool != "notes_write" || args != `{"text":"x"}` {
		t.Errorf("the exact call must survive: %q %q", tool, args)
	}

	// Stale is the same as absent.
	p.arm("call-A", "notes_write", `{"text":"x"}`, "write x", "d1")
	if _, _, _, _, ok := p.waiting("call-A", time.Nanosecond); ok {
		t.Error("a permission older than the window is still permission")
	}

	p.clear()
	if _, _, _, _, ok := p.waiting("call-A", time.Minute); ok {
		t.Error("cleared permission survived")
	}
}

// TestWaitingDoesNotConsume: a delegation arriving while he is still thinking must not spend the
// permission he has not answered yet.
func TestWaitingDoesNotConsume(t *testing.T) {
	p := &pendingWrite{}
	p.arm("c", "notes_write", `{}`, "write", "d1")
	for i := 0; i < 3; i++ {
		if _, _, _, _, ok := p.waiting("c", time.Minute); !ok {
			t.Fatalf("permission disappeared after %d look(s) without an answer", i)
		}
	}
}

// The 20:52 call: every delegation got the whole 45-second window, so his greeting from the start
// of the call was re-answered every time. A question is what was said since the last question.
func TestQuestionIsOnlyWhatWasSaidSinceTheLastOne(t *testing.T) {
	w := newTranscriptWindow(45 * time.Second)
	w.add("السلام عليكم ")
	first := w.question()
	if !strings.Contains(first, "السلام") {
		t.Fatalf("first question should carry the greeting: %q", first)
	}
	w.add("كم درجة الحرارة")
	second := w.question()
	if strings.Contains(second, "السلام") {
		t.Errorf("the greeting was already answered and came back: %q", second)
	}
	if !strings.Contains(second, "الحرارة") {
		t.Errorf("the new question is missing: %q", second)
	}
	if w.question() != "" {
		t.Error("asking again with nothing new said should be empty")
	}
	w.reset()
	w.add("مرحبا")
	if !strings.Contains(w.question(), "مرحبا") {
		t.Error("a new call starts the window over")
	}
}

func TestSpokenCutsAtASentence(t *testing.T) {
	long := strings.Repeat("هذي جملة كاملة. ", 40) // ~640 characters
	got := spoken(long, spokenCap)
	if n := len([]rune(got)); n > spokenCap {
		t.Errorf("%d characters, cap is %d", n, spokenCap)
	}
	if !strings.HasSuffix(got, ".") {
		t.Errorf("should end on a sentence boundary: %q", got[len(got)-20:])
	}
	if spoken("short", spokenCap) != "short" {
		t.Error("a short answer must pass through untouched")
	}
}
