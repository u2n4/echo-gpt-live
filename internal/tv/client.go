// Package tv is the bridge's side of tvd (tvd/server.py): one signed POST per command to the
// daemon that drives the Xiaomi box. tvd listens on loopback only, so the connection is cheap;
// it is still kept alive, because the remote presses keys several times a second.
package tv

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// scope must match tvd/auth.py SCOPE byte for byte: the signature covers it, so a signature made
// for the bridge's own /rtc routes can never pass at tvd, and the other way round.
const scope = "tv-v1\nPOST\n/tv/cmd\n"

// minBudget is the least a command may be given: tvd keeps 0.4 s back for its reply and refuses
// to start anything with less than 0.8 s left.
const minBudget = 1300 * time.Millisecond

// Sign returns the hex HMAC-SHA256 tvd expects for one request.
func Sign(key []byte, ts string, body []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(scope + ts + "."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// State is what tvd last saw of the box.
type State struct {
	Power     string  `json:"power"` // on | standby | unknown
	Connected bool    `json:"connected"`
	App       string  `json:"app"`
	Observed  float64 `json:"observed_at"`
}

// Result is tvd's answer. OK false with an Error code is a command tvd refused or could not
// finish (off, unreachable, timeout, busy, bad_request, ...); it is not a transport error.
type Result struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	State    State  `json:"state"`
	ImageB64 string `json:"image_b64,omitempty"`
	Input    string `json:"input,omitempty"`
	MS       int    `json:"ms"`
}

// Client sends commands to one tvd.
type Client struct {
	URL  string // base, e.g. http://127.0.0.1:8831
	Key  []byte
	HTTP *http.Client
}

// New builds a client with its own kept-alive transport and no redirects.
func New(url string, key []byte) *Client {
	return &Client{
		URL: strings.TrimRight(url, "/"),
		Key: key,
		HTTP: &http.Client{
			Transport: &http.Transport{MaxIdleConns: 2, MaxIdleConnsPerHost: 2, IdleConnTimeout: 60 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Do runs one action with args, giving tvd budget to finish it (at least minBudget).
func (c *Client) Do(ctx context.Context, action string, args map[string]any, budget time.Duration) (Result, error) {
	if c == nil || len(c.Key) < 32 {
		return Result{}, errors.New("tv: client not configured")
	}
	if budget < minBudget {
		budget = minBudget
	}
	nonce := make([]byte, 18)
	if _, err := rand.Read(nonce); err != nil {
		return Result{}, err
	}
	if args == nil {
		args = map[string]any{}
	}
	deadline := time.Now().Add(budget)
	body, err := json.Marshal(map[string]any{
		"v": 1, "nonce": base64.RawURLEncoding.EncodeToString(nonce),
		"action": action, "args": args,
		"deadline": float64(deadline.UnixMilli()) / 1000,
	})
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithDeadline(ctx, deadline.Add(300*time.Millisecond))
	defer cancel()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/tv/cmd", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-TV-Timestamp", ts)
	req.Header.Set("X-TV-Signature", Sign(c.Key, ts, body))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return Result{}, err
	}
	var out Result
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, fmt.Errorf("tv: status %d, unreadable reply", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("tv: status %d (%s)", resp.StatusCode, out.Error)
	}
	return out, nil
}
