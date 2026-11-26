package main

import (
	"sync"
	"testing"
)

func TestStateHistoryConcurrentCallbacks(t *testing.T) {
	s := &stateHistory{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.add("test")
				_ = s.snapshot()
			}
		}()
	}
	wg.Wait()
	if got := len(s.snapshot()); got != 800 {
		t.Fatalf("states=%d want 800", got)
	}
}

func TestPostRejectsMalformedURLWithoutPanic(t *testing.T) {
	if _, _, err := post(":", nil, "/bad", map[string]any{}); err == nil {
		t.Fatal("malformed URL accepted")
	}
}
