package bridge

import (
	"testing"
	"time"
	"unicode/utf8"
)

// The phrases the owner actually says, in the spellings a transcriber actually produces.
func TestFarewellHearsRealGoodbyes(t *testing.T) {
	for _, said := range []string{
		"يلا مع السلامة",
		"طيب خلاص مع السلامه",
		"تصبح على خير",
		"في أمان الله",
		"ok that is everything, goodbye",
		"alright, good night",
		"hang up",
		"اقفل المكالمة",
	} {
		f := NewFarewell(nil, nil)
		if !f.Feed(said) {
			t.Errorf("did not hear a goodbye in %q", said)
		}
	}
}

// Cutting someone off mid-sentence is the failure that matters, so a farewell word inside a longer
// sentence must not end the call.
func TestFarewellIgnoresGoodbyeWordsMidSentence(t *testing.T) {
	for _, said := range []string{
		"مع السلامة يعني وش تقصد",
		"goodbye is a strange word, do you think",
		"before we say goodbye I wanted to ask something else",
		"لا تقفل المكالمة انتظر",
		"السلام عليكم",
		"خلاص",
		"باي",
	} {
		f := NewFarewell(nil, nil)
		if f.Feed(said) {
			t.Errorf("ended the call on %q, which is not a goodbye", said)
		}
	}
}

// A transcript arrives in fragments, and the phrase can straddle two of them.
func TestFarewellMatchesAcrossFragments(t *testing.T) {
	f := NewFarewell(nil, nil)
	if f.Feed("طيب يلا ") {
		t.Fatal("armed too early")
	}
	if !f.Feed("مع السلامة") {
		t.Fatal("a phrase split across two fragments was missed")
	}
}

// What the transcriber delivered on the 22:08 call of 2026-09-13, fragment by fragment, exactly as
// logged: the final letter of السلامة never came. That is still a goodbye.
func TestFarewellHearsTheGoodbyeAsTranscribedOnThe2208Call(t *testing.T) {
	f := NewFarewell(nil, nil)
	for _, frag := range []string{"، خلاص", " خلاص", " بعدين", " بكلم", "ك", "، يلا", " مع السلام"} {
		f.Feed(frag)
	}
	// "بعدين بكلمك" is a goodbye of its own and arms first; "يلا" is him still talking and
	// withdraws it; the dropped-letter "مع السلام" must then be heard as the goodbye it was.
	phrase, _, armed := f.Armed()
	if !armed || phrase != "مع السلام" {
		t.Fatalf("armed=%v phrase=%q; want the transcriber's \"مع السلام\" to end the call", armed, phrase)
	}
}

// Fragments keep their own spacing: "مع " then "السلامة" must not become one word. This is the
// case normalising each fragment before joining it broke.
func TestFarewellKeepsTheSpaceBetweenFragments(t *testing.T) {
	f := NewFarewell(nil, nil)
	if f.Feed("مع ") {
		t.Fatal("armed too early")
	}
	if !f.Feed("السلامة") {
		t.Fatal("the space between two fragments was lost and the phrase missed")
	}
}

// Speaking again withdraws a goodbye that has not taken effect yet.
func TestFarewellIsWithdrawnBySpeakingAgain(t *testing.T) {
	f := NewFarewell(nil, nil)
	if !f.Feed("مع السلامة") {
		t.Fatal("should have armed")
	}
	if !f.IsArmed() {
		t.Fatal("should be armed")
	}
	f.Feed("لا لحظة عندي سؤال ثاني")
	if f.IsArmed() {
		t.Fatal("the user kept talking; the call must stay open")
	}
	if _, due := f.Due(0); due {
		t.Fatal("a withdrawn goodbye must never come due")
	}
}

// The grace period exists so the assistant can finish saying goodbye.
func TestFarewellWaitsOutItsGrace(t *testing.T) {
	f := NewFarewell(nil, nil)
	f.Feed("goodbye")
	if _, due := f.Due(time.Hour); due {
		t.Fatal("came due before its grace period")
	}
	if phrase, due := f.Due(0); !due || phrase != "goodbye" {
		t.Fatalf("after the grace it must come due, got %q %v", phrase, due)
	}
	if _, due := f.Due(0); due {
		t.Fatal("it must come due exactly once")
	}
}

// Arabic spelling varies more than a literal match can survive, so the folding is part of the
// contract, not an implementation detail.
func TestFarewellFoldsArabicSpelling(t *testing.T) {
	for _, said := range []string{
		"مَعَ السَّلامَة", // with diacritics
		"مع السلامة",      // ta marbuta
		"مع السلامه",      // ha
		"مع    السلامة",   // stretched spacing
		"مع السلامة.",     // trailing punctuation
	} {
		f := NewFarewell(nil, nil)
		if !f.Feed(said) {
			t.Errorf("spelling variant not matched: %q", said)
		}
	}
}

// A caller must be able to turn it off and to replace the list.
func TestFarewellIsConfigurable(t *testing.T) {
	off := NewFarewell([]string{""}, nil)
	if off.Feed("goodbye") {
		t.Fatal("an empty list must disable detection")
	}
	custom := NewFarewell([]string{"كفى"}, nil)
	if custom.Feed("goodbye") {
		t.Fatal("a replaced list must not keep the defaults")
	}
	if !custom.Feed("طيب كفى") {
		t.Fatal("the replacement list was not used")
	}
}

// A goodbye said long ago must not end a call that has moved on.
func TestFarewellForgetsOldSpeech(t *testing.T) {
	f := NewFarewell(nil, nil)
	f.Feed("goodbye")
	f.Feed("no wait")
	long := ""
	for i := 0; i < 30; i++ {
		long += "some more conversation "
	}
	if f.Feed(long) {
		t.Fatal("an old goodbye ended a call that had moved on")
	}
}

// A transcriber emits fragments of its own after the speaker stops. Punctuation or a stray token
// must not hold the line open: that would leave the session billing until the idle timer, which is
// the cost this whole feature exists to avoid.
func TestFarewellSurvivesTrailingTranscriptNoise(t *testing.T) {
	for _, trailing := range []string{".", " ", "!", "؟", "…", "a", "ا"} {
		f := NewFarewell(nil, nil)
		if !f.Feed("مع السلامة") {
			t.Fatal("should have armed")
		}
		f.Feed(trailing)
		if !f.IsArmed() {
			t.Errorf("a trailing %q withdrew the goodbye", trailing)
		}
	}
}

// Real words still withdraw it, which is the whole point of the grace period.
func TestFarewellStillWithdrawsOnRealWords(t *testing.T) {
	f := NewFarewell(nil, nil)
	f.Feed("goodbye")
	f.Feed("actually wait")
	if f.IsArmed() {
		t.Fatal("real speech must take the goodbye back")
	}
}

// "Do not hang up" and "how do I say goodbye" both END in a farewell phrase and both mean the
// opposite of one. Codex found that a plain suffix match closes the call on either.
func TestFarewellIgnoresNegatedAndQuotedGoodbyes(t *testing.T) {
	for _, said := range []string{
		"do not hang up",
		"dont hang up",
		"how do I say goodbye",
		"what does goodbye mean",
		"لا تقفل المكالمة",
		"ما ابغى اقفل المكالمة",
		"كيف اقول مع السلامة",
		"وش يعني مع السلامة",
	} {
		f := NewFarewell(nil, nil)
		if f.Feed(said) {
			t.Errorf("ended the call on %q, which asks for the opposite", said)
		}
	}
}

// A phrase split inside a word across two deltas must still match, which it cannot if the detector
// invents a space between fragments.
func TestFarewellMatchesAPhraseSplitInsideAWord(t *testing.T) {
	f := NewFarewell(nil, nil)
	f.Feed("طيب مع الس")
	if !f.Feed("لامة") {
		t.Fatal("a phrase split inside a word was missed")
	}
}

// A goodbye armed at the end of one call must not end the next one.
func TestFarewellResetForgetsAnArmedGoodbye(t *testing.T) {
	f := NewFarewell(nil, nil)
	f.Feed("goodbye")
	if !f.IsArmed() {
		t.Fatal("should be armed")
	}
	f.Reset()
	if f.IsArmed() {
		t.Fatal("reset must disarm")
	}
	if _, due := f.Due(0); due {
		t.Fatal("a reset goodbye must never come due")
	}
}

// A real call on 13 September 2026: he said goodbye and the line stayed open. These are the
// spellings a Gulf speaker actually uses, fed the way the transcript arrives - in fragments.
func TestFarewellRealGoodbyes(t *testing.T) {
	says := []string{
		"مع السلامة",
		"يلا مع السلامة",
		"خلاص يلا مع السلامة",
		"طيب مع السلامة",
		"اوكي مع السلامه",
		"مع السلامة.",
		"مَعَ السَّلَامَة",
	}
	for _, s := range says {
		f := NewFarewell(nil, nil)
		if !f.Feed(s) {
			t.Errorf("did not hear a goodbye in %q", s)
		}
	}
}

// The transcript arrives in pieces, and a phrase split across two of them still has to match.
func TestFarewellAcrossFragments(t *testing.T) {
	f := NewFarewell(nil, nil)
	if f.Feed("يلا ") {
		t.Fatal("closed on the first fragment")
	}
	if f.Feed("مع الس") {
		t.Fatal("closed mid-word")
	}
	if !f.Feed("لامة") {
		t.Fatal("a goodbye split across fragments was missed")
	}
}

// The window used to be trimmed at a BYTE offset. Arabic is two bytes a letter, so every trim cut a
// letter in half. This drives the window past its limit with Arabic and then says goodbye.
func TestFarewellWindowIsRuneSafe(t *testing.T) {
	f := NewFarewell(nil, nil)
	// Well past farewellTail characters of ordinary speech, all multi-byte.
	for i := 0; i < 40; i++ {
		if f.Feed("كلام كثير عن اشياء ثانية ") {
			t.Fatalf("closed on ordinary speech at fragment %d", i)
		}
	}
	if !utf8.ValidString(f.recent) {
		t.Fatalf("the window is not valid UTF-8 after trimming: %q", f.recent)
	}
	if !f.Feed("يلا مع السلامة") {
		t.Fatal("a goodbye after a long conversation was missed")
	}
}

// The grace period is measured from when SHE stops talking, not from when he said goodbye.
// Measured from the goodbye it lands inside her own farewell: on the 13:47 call he said it at
// 13:47:15.8 and the line dropped at 13:47:17.8, mid-word.
func TestFarewellArmedCarriesWhenItWasHeard(t *testing.T) {
	f := NewFarewell(nil, nil)
	if _, _, ok := f.Armed(); ok {
		t.Fatal("armed before anyone said anything")
	}
	before := time.Now()
	if !f.Feed("يلا مع السلامة") {
		t.Fatal("goodbye missed")
	}
	phrase, at, ok := f.Armed()
	if !ok {
		t.Fatal("not armed after a goodbye")
	}
	if phrase == "" {
		t.Error("the phrase that closed the call should be reportable")
	}
	if at.Before(before) {
		t.Errorf("armedAt %v is before the goodbye %v", at, before)
	}
	// Armed does not consume, so a watcher can poll it while she finishes her sentence.
	for i := 0; i < 3; i++ {
		if _, _, ok := f.Armed(); !ok {
			t.Fatalf("the goodbye was consumed by looking at it (poll %d)", i)
		}
	}
	f.Disarm()
	if _, _, ok := f.Armed(); ok {
		t.Error("Disarm did not disarm")
	}
}

// The 15:03 call on 13 September: he said goodbye THREE times and the line stayed open every time.
// Her reply came back through the Echo's own microphone, was transcribed as HIS input, and
// withdrew his goodbye - at +8.7 s, +0.4 s and +5.3 s after each one.
func TestHerOwnVoiceDoesNotTakeBackHisGoodbye(t *testing.T) {
	speaking := true
	f := NewFarewell(nil, nil)
	f.Speaking = func() bool { return speaking }

	if !f.Feed("يلا مع السلامة") {
		t.Fatal("goodbye missed")
	}
	// She answers. Her voice loops back and is transcribed as him.
	f.Feed("مع السلامة، إلى اللقاء، أتمنى لك يوماً سعيداً")
	if !f.IsArmed() {
		t.Fatal("her own reply took back his goodbye - this is the bug from the 15:03 call")
	}

	// He really does speak again, after she has stopped. That still takes it back.
	speaking = false
	f.Feed("لا لحظة، عندي سؤال ثاني")
	if f.IsArmed() {
		t.Error("he spoke again and the goodbye should have been withdrawn")
	}
}

// The asymmetry is deliberate and worth pinning: while she talks, an utterance can still END the
// call, it just cannot keep it alive. Ending sooner is the safe direction.
func TestAGoodbyeStillMatchesWhileSheIsSpeaking(t *testing.T) {
	f := NewFarewell(nil, nil)
	f.Speaking = func() bool { return true }
	if !f.Feed("خلاص يلا مع السلامة") {
		t.Error("a goodbye spoken over her must still be heard")
	}
}

// With no Speaking function wired the old behaviour stands, so a caller that does not set it is
// not silently changed.
func TestWithdrawalStillWorksWithoutTheSpeakingHook(t *testing.T) {
	f := NewFarewell(nil, nil)
	if !f.Feed("مع السلامة") {
		t.Fatal("goodbye missed")
	}
	f.Feed("لحظة صبر عندي طلب")
	if f.IsArmed() {
		t.Error("without the hook, real speech must still withdraw")
	}
}
