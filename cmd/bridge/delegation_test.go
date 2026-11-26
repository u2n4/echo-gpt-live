package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/u2n4/echo-gpt-live/internal/openai"
)

type delegationTransport func(*http.Request) (*http.Response, error)

func (d delegationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return d(r) }

type delegationSpeaker struct{ spoken chan string }

func (s delegationSpeaker) Commentary(_ context.Context, _ string, text string) error {
	s.spoken <- text
	return nil
}
func (s delegationSpeaker) Thinking(context.Context, string, string) error { return nil }

func TestBusyDelegationAcknowledgesAndUsesCorrectedArabicRequest(t *testing.T) {
	started := make(chan struct{})
	bodies := make(chan string, 2)
	const oldRequest = "شغلي الحلقة الثالثة"
	const correction = "لا، أقصد الحلقة اللي قبلها"
	router := &openai.Answerer{APIKey: "test", HTTP: &http.Client{Transport: delegationTransport(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(string(body), correction) {
			select {
			case <-started:
			default:
				close(started)
			}
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		bodies <- string(body)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"الحلقة الثانية"}]}]}`))}, nil
	})}}
	a := &assistant{answerer: router, window: newTranscriptWindow(time.Minute), pending: &pendingWrite{}, ev: func(string, ...any) {}}
	a.newCall("test")
	ctx, _ := a.current()
	t.Cleanup(a.cancel)
	s := delegationSpeaker{spoken: make(chan string, 4)}
	a.window.add(oldRequest)
	go a.onDelegation(ctx, s, "old")
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not start")
	}
	a.window.add(oldRequest)
	a.onDelegation(ctx, s, "duplicate")
	if got := <-s.spoken; !strings.Contains(got, "أشتغل") {
		t.Fatalf("missing busy acknowledgement: %q", got)
	}
	a.window.add(correction)
	go a.onDelegation(ctx, s, "corrected")
	select {
	case body := <-bodies:
		if !strings.Contains(body, oldRequest) {
			t.Fatal("correction lost original request context")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("correction was not routed after canceling the old request")
	}
	select {
	case <-s.spoken:
	case <-time.After(time.Second):
		t.Fatal("no acknowledgement")
	}
	select {
	case got := <-s.spoken:
		if !strings.Contains(got, "الثانية") {
			t.Fatalf("wrong reply: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no reply to correction")
	}
}
