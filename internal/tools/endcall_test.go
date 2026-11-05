package tools

import (
	"context"
	"errors"
	"testing"
)

// The floor under the model: what he SAID has to contain a goodbye, with nothing negating it.
func TestEndCallNeedsAFarewellInWhatHeSaid(t *testing.T) {
	goodbye := []string{
		"يلا مع السلامة الله يحفظك",
		"مع السلام",      // the transcriber drops the final ة
		"تصبحين على خير", // ى and ة spelled either way
		"في أمان الله",
		"يلا باي",
		"يلا سلام",
		"خلاص كفاية اقفلي المكالمة",
		"سكري الخط",
		"انهي الاتصال",
		"بكلمك بعدين",
		"bye bye",
		"ok thanks that's all",
		"good night Sol",
		"hang up please",
	}
	notGoodbye := []string{
		"خلاص",
		"طيب خلاص",
		"تمام شكرا",
		"الله يعطيك العافية",
		"السلام عليكم كيف حالك",          // the greeting that opens a call
		"كم سعر السكر اليوم",             // sugar, not hanging up
		"الخط عندي مو واضح",              // a line, not the line
		"لا تقفلين المكالمة انتظري",      // negated
		"كيف اقول مع السلامة بالانجليزي", // a question about the words
		"don't hang up",
		"what does goodbye mean",
		"ok thanks",
		"stop",
		"",
	}
	for _, s := range goodbye {
		if !isFarewell(s) {
			t.Errorf("%q is a goodbye and must pass", s)
		}
	}
	for _, s := range notGoodbye {
		if isFarewell(s) {
			t.Errorf("%q is not a goodbye and must be refused", s)
		}
	}
}

func TestEndCallDecidesFromTheTranscriptNotTheArgument(t *testing.T) {
	tool := EndCall()
	var ending *ErrCallEnding

	// He said goodbye: the signal, whatever the model wrote.
	ctx := WithSaid(context.Background(), "يلا مع السلامة")
	if _, err := tool.Run(ctx, map[string]any{"words": "anything"}); !errors.As(err, &ending) {
		t.Fatalf("a goodbye in the transcript must signal ErrCallEnding, got %v", err)
	}
	// He ended a topic; the model claims a goodbye. The transcript wins.
	ctx = WithSaid(context.Background(), "طيب خلاص")
	_, err := tool.Run(ctx, map[string]any{"words": "مع السلامة"})
	if err == nil || errors.As(err, &ending) {
		t.Fatalf("the model's argument must not end the call, got %v", err)
	}
	// No transcript on the context at all: a page or a test cannot supply one.
	_, err = tool.Run(context.Background(), map[string]any{"words": "مع السلامة"})
	if err == nil || errors.As(err, &ending) {
		t.Fatalf("without a transcript the call must stay open, got %v", err)
	}
	// Through the registry, the signal survives intact.
	reg := NewRegistry()
	reg.Add(tool)
	_, err = reg.Call(WithSaid(context.Background(), "bye bye"), "end_call", `{}`, false)
	if !errors.As(err, &ending) {
		t.Fatalf("the registry must pass the signal through, got %v", err)
	}
}
