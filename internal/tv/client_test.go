package tv

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var testKey = []byte(strings.Repeat("k", 32))

// The same vector is pinned in lambda/index.test.mjs and computed by tvd/auth.py.
func TestSignMatchesTvd(t *testing.T) {
	got := Sign(testKey, "1800000000", []byte(`{"v":1}`))
	if want := "97a214c1c4f95aa59f0218195ef0677ed5db4c9dddb745d014f5146ac095e353"; got != want {
		t.Fatalf("signature %s, want %s", got, want)
	}
}

func TestDoSignsAndParses(t *testing.T) {
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/tv/cmd" || r.Method != http.MethodPost {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		ts := r.Header.Get("X-TV-Timestamp")
		if Sign(testKey, ts, body) != r.Header.Get("X-TV-Signature") {
			t.Errorf("bad signature")
		}
		_ = json.Unmarshal(body, &seen)
		_, _ = w.Write([]byte(`{"ok":true,"action":"key","state":{"power":"on","connected":true},"ms":4}`))
	}))
	defer srv.Close()
	c := New(srv.URL, testKey)
	res, err := c.Do(context.Background(), "key", map[string]any{"key": "right"}, 2*time.Second)
	if err != nil || !res.OK || res.State.Power != "on" {
		t.Fatalf("res %+v err %v", res, err)
	}
	if seen["action"] != "key" || seen["v"].(float64) != 1 {
		t.Fatalf("body %v", seen)
	}
	if n, _ := seen["nonce"].(string); len(n) < 16 {
		t.Fatalf("nonce %q", n)
	}
	left := seen["deadline"].(float64) - float64(time.Now().UnixMilli())/1000
	if left < 1.5 || left > 2.1 {
		t.Fatalf("deadline %f s away", left)
	}
}

func TestBudgetFloorAndRefusals(t *testing.T) {
	var deadline float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		deadline = b["deadline"].(float64)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"ok":false,"error":"forbidden"}`))
	}))
	defer srv.Close()
	res, err := New(srv.URL, testKey).Do(context.Background(), "state", nil, 10*time.Millisecond)
	if err == nil || res.Error != "forbidden" {
		t.Fatalf("want an error carrying forbidden, got %+v %v", res, err)
	}
	if left := deadline - float64(time.Now().UnixMilli())/1000; left < 1.0 {
		t.Fatalf("budget was not raised to the floor: %f s", left)
	}
	if _, err := (&Client{URL: srv.URL, Key: []byte("short")}).Do(context.Background(), "state", nil, time.Second); err == nil {
		t.Fatal("a short key must be refused")
	}
}
