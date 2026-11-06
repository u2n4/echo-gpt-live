package main

import (
	"sync"
	"sync/atomic"
	"testing"
)

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
