package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSpeakOnceAtWaitsForSelectedEvent(t *testing.T) {
	for _, selected := range []string{"connected", "started"} {
		t.Run(selected, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var played []string
			speak := speakOnceAt(ctx, selected, func(when string) { played = append(played, when) })
			other := "started"
			if selected == other {
				other = "connected"
			}
			speak(other)
			if len(played) != 0 {
				t.Fatalf("played before %s: %v", selected, played)
			}
			speak(selected)
			speak(selected)
			if len(played) != 1 || played[0] != selected {
				t.Fatalf("playback events = %v, want [%s]", played, selected)
			}
			cancel()
			speakOnceAt(ctx, selected, func(string) { t.Fatal("played after cancellation") })(selected)
		})
	}
}

func TestGateTouchPublicationRacesWithAudio(t *testing.T) {
	defer gateTouchFn.Store(nil)
	var touched atomic.Int64
	fn := func() { touched.Add(1) }
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 10000; i++ {
			gateTouchFn.Store(&fn)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 10000; i++ {
			gateTouch()
		}
	}()
	wg.Wait()
	gateTouch()
	if touched.Load() == 0 {
		t.Fatal("published idle gate was never touched")
	}
}
