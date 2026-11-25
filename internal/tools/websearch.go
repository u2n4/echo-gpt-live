package tools

// Searching the web without paying for it.
//
// OpenAI's built-in web_search is one cent a call. That is not a large number until you notice what
// triggers it: a sentence spoken in a room, by anyone, as often as anyone speaks. A hundred idle
// questions is a dollar, and nothing about the mechanism makes the hundredth question cost less
// than the first.
//
// What is paid for here instead is only the few hundred tokens the snippets add to a gpt-5-nano
// call: at five cents a million that is about two thousandths of a cent, roughly five hundred times
// less. That ratio is the entire reason this file exists.
//
// MEASURED, 13 September 2026, from the bridge's own machine:
//
//	lite.duckduckgo.com     HTTP 202, an "anomaly" challenge page, on every request after the first
//	                        few. Still tried first: it is the fastest and the most private when the
//	                        block is not in force, and a blocked attempt costs about 200 ms.
//	html.duckduckgo.com     the same 202.
//	www.mojeek.com          HTTP 200 with no results in the body at all.
//	six public SearXNG      403 or 429.
//	r.jina.ai               HTTP 200, real results, 0.65-3.8 s. Free and needs no account.
//	*.wikipedia.org         HTTP 200, reliable, and useless for a price or a score.
//
// So the search is a ladder, cheapest and most private first, and it stops at the first rung that
// answers. The reader hop is the one that actually works today, and it has a cost that is not
// money: a third party sees the question. That is why `--web-search=private` exists to refuse it.
//
// One thing this deliberately does NOT do: it never takes a URL. The model supplies words, never an
// address, for the same reason nothing else in this package does. Opening a page happens in
// openresult.go, and there too the model names a result the engine showed it, never an address.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// How much of the web comes back. Six results at 220 characters is enough for DDG snippets;
// Keenable returns clean extracted page excerpts (with prices and facts already pulled out),
// so we keep up to 520 characters per result and skip the slow extra HTML page peek.
const (
	freeResults     = 6
	freeSnippet     = 220
	keenableSnippet = 520
)

// searchAgent identifies the bridge honestly. An empty User-Agent is served a page with no results
// at all, which reads exactly like "nothing was found".
const searchAgent = "Mozilla/5.0 (X11; Linux x86_64) echo-gpt-live/1.0"

// SearchDepth says how far down the ladder a search may go.
type SearchDepth int

const (
	// SearchDirect talks only to the search engine itself. Nothing else learns the question, and
	// when the engine is blocking this address it finds nothing.
	SearchDirect SearchDepth = iota
	// SearchViaReader adds the keyless reader service, which fetches the engine's page on our
	// behalf. It is what makes search work from this house, and it means one more company sees the
	// words he said.
	SearchViaReader
)

type webResult struct {
	Title   string
	Snippet string
	Where   string
	// URL is where the result actually leads, decoded from the engine's redirect. The model never
	// sees it as an address to choose; open_result reaches it by the result's number.
	URL string
	// Rich is true when the search backend already extracted substantial clean page text into
	// Snippet (e.g. Keenable), so Speculative/SearchTool can skip the slow HTML peek step.
	Rich bool
}

// Web is the free search and the page it opens afterwards. They share one memory - the results of
// the last search - so that "open result 3" means something the model was shown, not something
// it typed.
type Web struct {
	client      *http.Client // the engines and the reader service
	pages       *http.Client // result pages: public addresses only, checked at dial time
	depth       SearchDepth
	keenableKey string // optional X-API-Key for api.keenable.ai/v1/search (100k/mo free + realtime)
	mu          sync.Mutex
	last        []webResult
}

// NewWeb builds the search and its reader.
func NewWeb(client *http.Client, depth SearchDepth) *Web {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &Web{client: client, pages: pageClient(), depth: depth}
}

// SetKeenableKey configures an optional Keenable API key (free tier: 100k requests/month +
// realtime mode). When empty, Keenable's keyless /v1/search/public endpoint is used instead.
func (w *Web) SetKeenableKey(key string) *Web {
	w.keenableKey = strings.TrimSpace(key)
	return w
}

// WebSearch is the free search tool on its own, for callers that do not open pages.
func WebSearch(client *http.Client, depth SearchDepth) *Tool {
	return NewWeb(client, depth).SearchTool()
}

func (w *Web) keep(hits []webResult) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.last = hits
}

func (w *Web) result(n int) (webResult, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n < 1 || n > len(w.last) {
		return webResult{}, false
	}
	return w.last[n-1], true
}

// SearchTool is the free search. It is a Read: it changes nothing, so it runs without asking.
func (w *Web) SearchTool() *Tool {
	return &Tool{
		Name: "web_search",
		Description: "Free web search for anything current, post-training, or unfamiliar: apps, software, " +
			"tools, websites, products, brands, games, people, news, prices, scores, or any name " +
			"you are not 100% sure about (including English names phonetically transcribed in Arabic). " +
			"Always search with the inferred English name for tech/apps/brands, and include 'Saudi Arabia' " +
			"or 'SAR' for Saudi prices/products.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "what to search for: a short phrase, not the whole question",
				},
			},
			"required": []string{"query"},
		},
		Access:  Read,
		Timeout: 12 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			q := strings.TrimSpace(str(args["query"]))
			if q == "" {
				return "", fmt.Errorf("nothing to search for")
			}
			// Characters, not bytes: an Arabic letter is two bytes, and a byte cut in the middle
			// of one sent the engine an invalid tail.
			if r := []rune(q); len(r) > 300 {
				q = string(r[:300])
			}
			hits, err := searchNow(ctx, w.client, w.depth, w.keenableKey, q)
			if err != nil {
				return "", err
			}
			if len(hits) == 0 {
				return fmt.Sprintf("The search found nothing about %q.", q), nil
			}
			w.keep(hits)
			// Rich backends (Keenable) already return full extracted page excerpts, so skip the
			// 2.5s raw-HTML peek unless results only have short engine snippets.
			if hasRichSnippets(hits) {
				return render(hits), nil
			}
			// The first three pages come with the snippets, unasked, so the answer does not
			// need another round to open them and can speak from more than one site.
			return render(hits) + w.peek(ctx, hits), nil
		},
	}
}

func hasRichSnippets(hits []webResult) bool {
	for _, h := range hits {
		if h.Rich && len([]rune(h.Snippet)) >= 80 {
			return true
		}
	}
	return false
}

// searchNow runs every rung AT ONCE and answers with the best one that came back.
//
// Preference is ordered by quality and reliability:
//   - rank 0: Keenable Search API (independent 100B+ index, rich extracted snippets, ~0.4-0.9s)
//   - rank 1: DuckDuckGo direct
//   - rank 2: DuckDuckGo via reader (fallback)
//   - rank 3: Wikipedia (encyclopedic last resort)
//
// Running together means a dead or blocked rung adds zero serial latency, and the moment the best
// outstanding rung lands, every lower rung is cancelled immediately.
func searchNow(ctx context.Context, client *http.Client, depth SearchDepth, keenableKey, q string) ([]webResult, error) {
	type rung struct {
		rank int // lower is better
		name string
		run  func(context.Context, *http.Client, string) ([]webResult, error)
	}
	var rungs []rung
	if depth >= SearchViaReader {
		key := keenableKey
		rungs = append(rungs, rung{0, "keenable", func(ctx context.Context, c *http.Client, q string) ([]webResult, error) {
			return keenableSearch(ctx, c, key, q)
		}})
	}
	rungs = append(rungs, rung{1, "direct", ddgDirect})
	if depth >= SearchViaReader {
		rungs = append(rungs, rung{2, "reader", ddgViaReader})
	}
	rungs = append(rungs, rung{3, "wikipedia", wikipediaSearch})

	type answer struct {
		rank int
		name string
		hits []webResult
		err  error
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // whoever loses the race is dropped mid-flight rather than waited on

	out := make(chan answer, len(rungs))
	for _, r := range rungs {
		go func(r rung) {
			defer func() {
				if p := recover(); p != nil {
					out <- answer{r.rank, r.name, nil, fmt.Errorf("%s panicked: %v", r.name, p)}
				}
			}()
			hits, err := r.run(ctx, client, q)
			out <- answer{r.rank, r.name, hits, err}
		}(r)
	}

	// Waiting for every rung would make the whole thing as slow as the SLOWEST one, which is the
	// opposite of the point. Answer as soon as nothing better is still possible: track which ranks
	// are still outstanding, and the moment the best result in hand cannot be beaten by anything
	// still running, stop.
	pending := map[int]bool{}
	for _, r := range rungs {
		pending[r.rank] = true
	}
	best := answer{rank: 99}
	var lastErr error
	for len(pending) > 0 {
		a := <-out
		delete(pending, a.rank)
		switch {
		case a.err != nil:
			lastErr = fmt.Errorf("%s: %w", a.name, a.err)
		case len(a.hits) == 0:
		case a.rank < best.rank:
			best = a
		}
		if best.rank == 99 {
			continue // nothing usable yet; keep waiting
		}
		better := false
		for rank := range pending {
			if rank < best.rank {
				better = true
				break
			}
		}
		if !better {
			break // nothing outstanding can improve on this
		}
	}
	if best.rank < 99 {
		if len(best.hits) > freeResults {
			best.hits = best.hits[:freeResults]
		}
		return best.hits, nil
	}
	if lastErr != nil {
		// "Nothing was found" and "the search itself did not work" are different sentences to say
		// out loud, so they are different returns here.
		return nil, fmt.Errorf("the search did not go through: %w", lastErr)
	}
	return nil, nil
}

func render(hits []webResult) string {
	var b strings.Builder
	for i, r := range hits {
		if i >= freeResults {
			break
		}
		fmt.Fprintf(&b, "%d. %s", i+1, oneLine(r.Title, 120))
		if r.Where != "" {
			b.WriteString(" (" + oneLine(r.Where, 60) + ")")
		}
		if r.Snippet != "" {
			limit := freeSnippet
			if r.Rich {
				limit = keenableSnippet
			}
			b.WriteString(": " + oneLine(r.Snippet, limit))
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// ---- rung zero: Keenable Search API (independent index, rich extracted snippets) ----

// keenableBase is a variable only so tests can point this at a local httptest server.
var keenableBase = "https://api.keenable.ai"

var (
	// realtimeCueRE picks Keenable's realtime mode for breaking news, live prices, scores, and
	// today's events; everything else uses Keenable's pro mode (higher accuracy on apps/docs/facts).
	realtimeCueRE = regexp.MustCompile(`(?i)اليوم|امس|أمس|الآن|الان|عاجل|اخبار|أخبار|خبر|مباراه|مباراة|نتيجه|نتيجة|نتائج|فاز|سعر|اسعار|أسعار|بكم|الذهب|الدولار|سهم|اسهم|بيتكوين|طقس|الجو|وش صار|اخر|آخر|` +
		`today|yesterday|now|breaking|news|score|match|who won|price|cost|gold|dollar|stock|bitcoin|weather|latest|live`)
	mdImageRE = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	mdLinkRE  = regexp.MustCompile(`\[([^\]]+)\]\(https?://[^)]+\)`)
)

func cleanKeenableSnippet(s string) string {
	s = mdImageRE.ReplaceAllString(s, " ")
	s = mdLinkRE.ReplaceAllString(s, "$1")
	s = strings.ReplaceAll(s, "###", " ")
	s = strings.ReplaceAll(s, "##", " ")
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "|", " ")
	return clean(s)
}

func keenableSearch(ctx context.Context, client *http.Client, apiKey, q string) ([]webResult, error) {
	endpoint := strings.TrimRight(keenableBase, "/") + "/v1/search/public"
	payload := map[string]any{
		"query": q,
	}
	if apiKey != "" {
		endpoint = strings.TrimRight(keenableBase, "/") + "/v1/search"
		mode := "pro"
		if realtimeCueRE.MatchString(q) {
			mode = "realtime"
		}
		payload["mode"] = mode
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", searchAgent)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	} else {
		req.Header.Set("X-Keenable-Title", "echo-gpt-live")
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 512<<10))
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("answered %d", res.StatusCode)
	}
	var parsed struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
			Snippet     string `json:"snippet"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("unreadable keenable response: %w", err)
	}
	var out []webResult
	for _, r := range parsed.Results {
		title := clean(r.Title)
		if title == "" {
			continue
		}
		snip := r.Snippet
		if strings.TrimSpace(snip) == "" {
			snip = r.Description
		}
		snip = cleanKeenableSnippet(snip)
		u := resultURL(r.URL)
		out = append(out, webResult{
			Title:   title,
			Snippet: snip,
			Where:   host(u),
			URL:     u,
			Rich:    true,
		})
		if len(out) >= freeResults {
			break
		}
	}
	return out, nil
}

// ---- rung one: the engine itself ----

// One pass over the lite HTML, in document order. A result is a title, optionally a snippet, and
// optionally a visible domain, and the middle one is genuinely optional — DuckDuckGo omits it for
// results it has no abstract for — so these cannot be matched separately and zipped by index.
//
// (?s) is load-bearing and was missing: the page puts the opening tag and the text on different
// lines, and without it `.` stops at the newline, so every snippet silently disappeared while the
// titles and domains — which do sit on one line — came through and made it look fine.
var liteRE = regexp.MustCompile(`(?s)<a([^>]*class=['"]result-link['"][^>]*)>(.*?)</a>` +
	`|class=['"]result-snippet['"][^>]*>(.*?)</td>` +
	`|class=['"]link-text['"][^>]*>(.*?)</span>`)

var (
	tagRE  = regexp.MustCompile(`<[^>]*>`)
	hrefRE = regexp.MustCompile(`href=['"]([^'"]*)['"]`)
)

// resultURL turns the engine's link into the page it leads to. DuckDuckGo wraps every result in
// a redirect through its own host with the destination in `uddg`; the destination is what a
// reader can open, and the redirect is what it must not follow blindly.
func resultURL(href string) string {
	href = strings.TrimSpace(html.UnescapeString(href))
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	if strings.HasSuffix(u.Host, "duckduckgo.com") {
		if dest := u.Query().Get("uddg"); dest != "" {
			if d, err := url.Parse(dest); err == nil && (d.Scheme == "http" || d.Scheme == "https") {
				return dest
			}
		}
		return ""
	}
	return href
}

// ddgBase is a variable only so the tests can point this at a local server. Nothing reachable from
// a spoken sentence can change it.
var ddgBase = "https://lite.duckduckgo.com"

func ddgDirect(ctx context.Context, client *http.Client, q string) ([]webResult, error) {
	return ddgLiteFrom(ctx, client, ddgBase+"/lite/?q="+url.QueryEscape(q), nil)
}

func ddgLiteFrom(ctx context.Context, client *http.Client, u string, hdr map[string]string) ([]webResult, error) {
	body, err := get(ctx, client, u, 512<<10, hdr)
	if err != nil {
		return nil, err
	}
	page := string(body)

	var out []webResult
	for _, m := range liteRE.FindAllStringSubmatch(page, -1) {
		switch {
		case m[2] != "":
			r := webResult{Title: clean(m[2])}
			if h := hrefRE.FindStringSubmatch(m[1]); h != nil {
				r.URL = resultURL(h[1])
			}
			out = append(out, r)
		case m[3] != "":
			if len(out) > 0 && out[len(out)-1].Snippet == "" {
				out[len(out)-1].Snippet = clean(m[3])
			}
		case m[4] != "":
			if len(out) > 0 && out[len(out)-1].Where == "" {
				out[len(out)-1].Where = clean(m[4])
			}
		}
		if len(out) > freeResults*2 {
			break // plenty; the rest of the page is navigation
		}
	}
	if len(out) == 0 && strings.Contains(page, "anomaly") {
		// The challenge page arrives as HTTP 202, so status alone cannot tell this from an honest
		// empty result. Naming it is what stops the next person re-measuring it.
		return nil, fmt.Errorf("the engine is refusing this address as automated traffic")
	}
	return out, nil
}

// ---- rung two: the reader ----

// readerBase fetches a page on our behalf and returns its text. It needs no account and no key.
var readerBase = "https://r.jina.ai/"

// numbered matches the reader's markdown rendering of a result list: a number, then the title as
// a link with the engine's redirect as its address, then lines for the snippet and the visible
// domain. Markdown rather than plain text because the address is what open_result needs, and the
// plain rendering drops it.
//
// The separators are NON-BREAKING spaces, U+00A0, not ordinary ones. Go's `\s` is ASCII-only, so a
// pattern written with it matches the page as a person reads it and not the page as it arrives.
// \pZ is every Unicode space, which is what is actually on the wire.
var numbered = regexp.MustCompile(`(?m)^[\pZ\t]*(\d{1,2})\.[\pZ\t]*\[(.*?)\]\((\S+?)\)[\pZ\t]*$`)

func ddgViaReader(ctx context.Context, client *http.Client, q string) ([]webResult, error) {
	target := "https://lite.duckduckgo.com/lite/?q=" + url.QueryEscape(q)
	body, err := get(ctx, client, readerBase+target, 512<<10, map[string]string{
		"X-Return-Format": "markdown",
		"X-Retain-Images": "none",
	})
	if err != nil {
		return nil, err
	}
	text := string(body)

	locs := numbered.FindAllStringSubmatchIndex(text, -1)
	var out []webResult
	for i, loc := range locs {
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		r := webResult{Title: clean(text[loc[4]:loc[5]]), URL: resultURL(text[loc[6]:loc[7]])}
		// Everything between this title and the next number is the snippet and the domain. The
		// domain is the line with no spaces in it, which is what a host looks like and a sentence
		// does not.
		var snip []string
		for _, line := range strings.Split(text[loc[1]:end], "\n") {
			line = strings.TrimSpace(strings.ReplaceAll(line, "**", ""))
			if line == "" {
				continue
			}
			// A host has no spaces in it and a sentence does. strings.Fields splits on Unicode
			// space, so this is not fooled by the U+00A0 the reader separates columns with.
			if len(strings.Fields(line)) == 1 && strings.Contains(line, ".") && r.Where == "" {
				r.Where = line
				continue
			}
			snip = append(snip, line)
		}
		r.Snippet = clean(strings.Join(snip, " "))
		if r.Title != "" {
			out = append(out, r)
		}
		if len(out) >= freeResults {
			break
		}
	}
	return out, nil
}

// ---- rung three: the encyclopedia ----

// wikipediaSearch is the last rung because it answers a different question: it knows what a thing
// is and never knows what a thing costs today. It is here because it has never once refused us.
func wikipediaSearch(ctx context.Context, client *http.Client, q string) ([]webResult, error) {
	lang := "en"
	for _, r := range q {
		if r >= 0x0600 && r <= 0x06FF { // Arabic
			lang = "ar"
			break
		}
	}
	u := fmt.Sprintf("https://%s.wikipedia.org/w/api.php?action=query&format=json&list=search"+
		"&srlimit=%d&srprop=snippet&srsearch=%s", lang, freeResults, url.QueryEscape(q))
	body, err := get(ctx, client, u, 256<<10, nil)
	if err != nil {
		return nil, err
	}
	var page struct {
		Query struct {
			Search []struct {
				Title   string `json:"title"`
				Snippet string `json:"snippet"`
			} `json:"search"`
		} `json:"query"`
	}
	if json.Unmarshal(body, &page) != nil {
		return nil, fmt.Errorf("the encyclopedia sent something unreadable")
	}
	var out []webResult
	for _, s := range page.Query.Search {
		out = append(out, webResult{
			Title:   clean(s.Title),
			Snippet: clean(s.Snippet),
			Where:   lang + ".wikipedia.org",
		})
	}
	return out, nil
}

// ---- shared ----

func get(ctx context.Context, client *http.Client, u string, limit int64, hdr map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", searchAgent)
	req.Header.Set("Accept-Language", "ar,en;q=0.8")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, limit))
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("answered %d", res.StatusCode)
	}
	if err != nil {
		return nil, err
	}
	return body, nil
}

// clean turns a fragment of search-result HTML into something speakable.
func clean(s string) string {
	s = tagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
