package tools

// His notes.
//
// The vault is 36 MB of markdown on his Windows PC, and the bridge runs on a different physical
// machine on the same LAN. They are not the same computer and there is no share
// between them, so the vault is reached over the network through a small agent that serves exactly
// two things: a search and an append. That agent is in cmd/vaultd.
//
// Reading comes first on purpose. "Where did I put that key" is a question he asks a search box
// today, and answering it out loud costs nothing and cannot go wrong. Writing comes second and is a
// Write tool, so the caller has to hear a confirmation before anything lands on disk.
//
// There is no delete, no overwrite and no rename. A note can only ever be appended to.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// VaultClient talks to the agent on the machine that actually holds the vault.
type VaultClient struct {
	Base   string // e.g. http://<pc-on-the-lan>:8791
	Key    string // shared secret, sent as a header; the same signing key the rest of the project uses
	HTTP   *http.Client
	MaxHit int // how many search results to mention out loud
}

// noRedirect refuses to follow a redirect at all.
//
// Go's client strips sensitive headers when a redirect crosses origins, but X-Vault-Key is not one
// it knows about, so it would be carried to wherever the redirect points. A 307 or 308 would replay
// the append body there too. The configured host either answers or it does not; there is no version
// of "the notes moved" that this should act on by itself.
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func (v *VaultClient) client() *http.Client {
	if v.HTTP != nil {
		client := *v.HTTP
		client.CheckRedirect = noRedirect
		return &client
	}
	return &http.Client{Timeout: 6 * time.Second, CheckRedirect: noRedirect}
}

func (v *VaultClient) do(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	u, err := url.Parse(v.Base)
	if err != nil || u.Hostname() == "" || u.User != nil ||
		(u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()))) {
		return nil, fmt.Errorf("notes endpoint requires HTTPS or loopback HTTP without embedded credentials")
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(v.Base, "/")+path, body)
	if err != nil {
		return nil, err
	}
	if v.Key != "" {
		req.Header.Set("X-Vault-Key", v.Key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := v.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("the notes machine did not answer: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 256<<10))
	if err != nil {
		return nil, fmt.Errorf("reading notes response: %w", err)
	}
	if res.StatusCode >= 300 {
		// 3xx included: with redirects refused, a redirect IS the answer, and it is not one.
		return nil, fmt.Errorf("the notes machine answered %d", res.StatusCode)
	}
	return raw, nil
}

type vaultHit struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Snippet string `json:"snippet"`
	Updated string `json:"updated"`
}

// VaultSearch answers "where did I write that".
func VaultSearch(v *VaultClient) *Tool {
	return &Tool{
		Name: "notes_search",
		Description: "Search his own notes and read back what was found. Answers 'where did I put " +
			"X', 'وين كتبت كذا', 'what did I write about Y', 'ابحث في ملاحظاتي'. Returns the file " +
			"and the matching line.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "what to look for, in any language",
				},
			},
			"required": []string{"query"},
		},
		Access:  Read,
		Timeout: 8 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			q, _ := args["query"].(string)
			if strings.TrimSpace(q) == "" {
				return "", fmt.Errorf("nothing to search for")
			}
			raw, err := v.do(ctx, http.MethodGet, "/search?q="+url.QueryEscape(q), nil)
			if err != nil {
				return "", err
			}
			var hits []vaultHit
			if err := json.Unmarshal(raw, &hits); err != nil {
				return "", fmt.Errorf("the notes machine sent something unreadable: %w", err)
			}
			if len(hits) == 0 {
				return fmt.Sprintf("Nothing in the notes about %q.", q), nil
			}
			max := v.MaxHit
			if max <= 0 {
				max = 2
			}
			if len(hits) > max {
				hits = hits[:max]
			}
			var said []string
			for _, h := range hits {
				where := h.Path
				if h.Updated != "" {
					where += ", last changed " + h.Updated
				}
				said = append(said, fmt.Sprintf("in %s: %s", where, oneLine(h.Snippet, 160)))
			}
			return strings.Join(said, ". ") + ".", nil
		},
	}
}

// VaultAppend writes a thought down. It is a Write, so nothing lands until it has been confirmed
// out loud, and the answer repeats what was written so a mishearing is audible immediately.
func VaultAppend(v *VaultClient) *Tool {
	return &Tool{
		Name: "notes_write",
		Description: "Write a line into his notes for today. Answers 'write this down', " +
			"'اكتب في الملاحظات', 'remind me that', 'سجل عندك'. Appends only; it can never " +
			"replace or delete anything.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{
					"type":        "string",
					"description": "exactly what to write down, in his own words",
				},
			},
			"required": []string{"text"},
		},
		Access:  Write,
		Timeout: 8 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			text, _ := args["text"].(string)
			if strings.TrimSpace(text) == "" {
				return "", fmt.Errorf("there is nothing to write")
			}
			body, _ := json.Marshal(map[string]string{"text": text})
			raw, err := v.do(ctx, http.MethodPost, "/append", strings.NewReader(string(body)))
			if err != nil {
				return "", err
			}
			var out struct {
				Path string `json:"path"`
			}
			// Saying "Written" without having been told where is how a write that never happened
			// gets reported as one. An HTTP 200 carrying a login page decodes to nothing, and the
			// old code called that success.
			if err := json.Unmarshal(raw, &out); err != nil || out.Path == "" {
				return "", fmt.Errorf("the notes machine did not say it wrote anything")
			}
			where := out.Path
			// Saying it back is the safety mechanism: a misheard sentence is caught by ear.
			return fmt.Sprintf("Written in %s: %s", where, oneLine(text, 200)), nil
		},
	}
}
