package bridge

import (
	"testing"
	"time"
)

// A goodbye the model understood joins the same wait as a matched phrase, timed from when he said
// it, and does not double up with a phrase match that got there first.
func TestFarewellArmByTheModel(t *testing.T) {
	f := NewFarewell(nil, nil)
	said := time.Now().Add(-3 * time.Second)
	if !f.Arm("end_call", said) {
		t.Fatal("the first goodbye is new")
	}
	if f.Arm("end_call", said) {
		t.Fatal("the same goodbye again is not new; the caller must not answer it twice")
	}

	phrase, at, ok := f.Armed()
	if !ok || phrase != "end_call" {
		t.Fatalf("armed = %q, %v; want end_call", phrase, ok)
	}
	if !at.Equal(said) {
		t.Errorf("the wait is measured from his goodbye (%v), not from the arm (%v)", said, at)
	}

	// He keeps talking: the goodbye is withdrawn, exactly as for a matched phrase.
	f.Feed("لا لا انتظري عندي سؤال ثاني")
	if _, _, ok := f.Armed(); ok {
		t.Error("speech after the goodbye must take it back")
	}

	// A phrase match first, then the model: one goodbye, the earlier one.
	f.Feed("يلا مع السلامه")
	_, first, ok := f.Armed()
	if !ok {
		t.Fatal("the phrase should arm")
	}
	f.Arm("end_call", time.Now().Add(-10*time.Second))
	if _, at, _ := f.Armed(); !at.Equal(first) {
		t.Error("a second arming must not move the goodbye")
	}

	// A zero time means now.
	f.Disarm()
	before := time.Now()
	f.Arm("end_call", time.Time{})
	if _, at, _ := f.Armed(); at.Before(before) {
		t.Error("a zero time should read as now")
	}

	// Nil is safe: the feature can be off.
	var off *Farewell
	off.Arm("end_call", time.Now())
}
