package tools

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenBody struct{}

func (brokenBody) Read(p []byte) (int, error) { return copy(p, "partial page"), io.ErrUnexpectedEOF }
func (brokenBody) Close() error               { return nil }

func TestPartialHTTPBodiesAreErrors(t *testing.T) {
	client := &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/plain"}}, Body: brokenBody{}}, nil
	})}
	if _, err := get(context.Background(), client, "https://example.com", 100, nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("search read: %v", err)
	}
	if _, err := fetchPage(context.Background(), client, "https://example.com"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("page read: %v", err)
	}
	v := &VaultClient{Base: "https://example.com", HTTP: client}
	if _, err := v.do(context.Background(), "GET", "/search", nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("vault read: %v", err)
	}
}

func TestInjectedHomeClientCannotFollowRedirects(t *testing.T) {
	var leaked atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	defer origin.Close()
	client := origin.Client()
	tool := HomeNetwork([]Endpoint{{Name: "test", URL: origin.URL}}, client)
	if _, err := tool.Run(context.Background(), map[string]any{"what": "test"}); err != nil {
		t.Fatal(err)
	}
	if leaked.Load() != 0 {
		t.Fatal("injected client followed a redirect outside its configured endpoint")
	}
	if client.CheckRedirect != nil {
		t.Fatal("caller-owned client was mutated")
	}
}

func TestVaultRefusesPlaintextRemoteBeforeSendingKey(t *testing.T) {
	var sent atomic.Int64
	v := &VaultClient{Base: "http://example.com", Key: "test-key", HTTP: &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) { sent.Add(1); return nil, errors.New("must not dial") })}}
	if _, err := v.do(context.Background(), "GET", "/search", nil); err == nil {
		t.Fatal("remote HTTP accepted")
	}
	if sent.Load() != 0 {
		t.Fatal("plaintext request reached transport")
	}
}

type testMemory struct{ facts []string }

func (m *testMemory) Remember(s string) error { m.facts = append(m.facts, s); return nil }
func (m *testMemory) Facts() []string         { return append([]string(nil), m.facts...) }
func TestCancelledRememberCannotPersist(t *testing.T) {
	m := &testMemory{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := RememberThis(m).Run(ctx, map[string]any{"fact": "test"})
	if !errors.Is(err, context.Canceled) || len(m.facts) != 0 {
		t.Fatalf("err=%v facts=%v", err, m.facts)
	}
}

func TestOneLineNeverSplitsArabic(t *testing.T) {
	got := oneLine(strings.Repeat("س", 10), 3)
	if !utf8.ValidString(got) || got != "سسس..." {
		t.Fatalf("invalid clipping: %q", got)
	}
}

func TestPrayerSentenceUsesSuppliedCivilTime(t *testing.T) {
	timings := map[string]string{"Fajr": "05:00", "Dhuhr": "12:00", "Asr": "15:00", "Maghrib": "18:00", "Isha": "19:00"}
	got := prayerSentenceAt(timings, time.Date(2026, 1, 1, 17, 0, 0, 0, time.FixedZone("test", 3*3600)))
	if !strings.Contains(got, "المغرب 18:00") {
		t.Fatalf("next prayer: %s", got)
	}
}
