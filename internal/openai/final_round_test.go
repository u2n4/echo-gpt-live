package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStalledRoundIsNeverRebilled(t *testing.T) {
	var calls atomic.Int64
	a := &Answerer{HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, context.DeadlineExceeded
	})}}
	_, err := a.postRound(context.Background(), map[string]any{"model": "test"})
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("err=%v requests=%d; a stalled generation must not be replayed", err, calls.Load())
	}
}

func TestRouterDoesNotStoreSpeechOrProfile(t *testing.T) {
	a := &Answerer{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if stored, ok := body["store"].(bool); !ok || stored {
			t.Fatal("Responses storage was not disabled")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"completed"}`)), Header: make(http.Header)}, nil
	})}}
	if _, err := a.post(context.Background(), map[string]any{"input": "test"}); err != nil {
		t.Fatal(err)
	}
}

func TestBackendCompletionStopsScannerProducer(t *testing.T) {
	producers := func() int {
		stack := make([]byte, 1<<20)
		n := runtime.Stack(stack, true)
		return strings.Count(string(stack[:n]), "(*Backend).consume.func")
	}
	before := producers()
	b := &Backend{Model: "test", FirstEvent: time.Second, Stall: time.Second}
	input := "data: {\"type\":\"response.completed\"}\n" + strings.Repeat("data: {}\n", 10000)
	start := time.Now()
	if err := b.consume(context.Background(), strings.NewReader(input), start, start, &AskResult{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for producers() > before && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if producers() > before {
		t.Fatal("scanner producer remained blocked after completion")
	}
}

func TestHangupAlreadyGoneDoesNotRetry(t *testing.T) {
	var calls atomic.Int64
	c := NewLive("test", "")
	c.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
	})}
	if err := c.HangupRetry(context.Background(), "live_test", 3); err != nil || calls.Load() != 1 {
		t.Fatalf("err=%v requests=%d; a missing session cannot keep billing", err, calls.Load())
	}
}

func TestBackendCleanEOFReturnsImmediately(t *testing.T) {
	b := &Backend{Model: "test", FirstEvent: time.Second, Stall: time.Second}
	start := time.Now()
	err := b.consume(context.Background(), strings.NewReader(""), start, start, &AskResult{})
	if err == nil || time.Since(start) >= 500*time.Millisecond {
		t.Fatalf("EOF must be a stream failure without waiting for the watchdog: %v", err)
	}
}
