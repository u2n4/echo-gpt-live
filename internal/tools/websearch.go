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
// Two things this deliberately does NOT do:
//
//   - It never opens a result page. Only the engine's own titles and snippets come back. Fetching a
//     result would mean the model reads whatever a stranger wrote as part of its own instructions,
//     and snippets already answer the question he actually asks.
//   - It never takes a URL. The model supplies words, never an address, for the same reason nothing
//     else in this package does.

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// How much of the web comes back. Four results at 220 characters is enough for the model to answer
// a spoken question and short enough that the tokens stay free in all but name.
const (
	freeResults = 4
	freeSnippet = 220
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
}

// WebSearch is the free search tool. It is a Read: it changes nothing, so it runs without asking.
func WebSearch(client *http.Client, depth SearchDepth) *Tool {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &Tool{
		Name: "web_search",
		Description: "Search the web for something current or outside your knowledge: news, prices, " +
			"scores, today's weather, anything after your training. Answers 'ابحث في النت', " +
			"'وش صار في', 'what is the price of', 'who won'. Use it only when the answer really " +
			"depends on the outside world.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "what to search for, in the language the question was asked in",
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
			if len(q) > 300 {
				q = q[:300]
			}

			return searchNow(ctx, client, depth, q)
		},
	}
}

// searchNow runs every rung AT ONCE and answers with the best one that came back.
//
// It used to be a ladder, tried in order, and the order was the cost. Measured on this machine:
//
//	rung 1  DuckDuckGo direct    282 ms and ALWAYS fails - this address is blocked
//	rung 2  the reader          3744 ms cold, ~1100 ms warm
//	rung 3  Wikipedia            813 ms
//
// So a question about the weather paid 282 ms for a rung that cannot work, then waited 3.7 s for
// the next one, in series. Run together, the whole thing costs whatever the FIRST usable answer
// costs, and the dead rung costs nothing at all because nobody waits for it.
//
// Preference is still ordered, because "fastest" and "best" are not the same: the engine's own
// results beat an encyclopedia article for "how hot is it today". So this waits for a better rung
// only while one is still plausibly coming, and answers the moment the best outstanding one lands.
func searchNow(ctx context.Context, client *http.Client, depth SearchDepth, q string) (string, error) {
	type rung struct {
		rank int // lower is better; the engine beats the encyclopedia
		name string
		run  func(context.Context, *http.Client, string) ([]webResult, error)
	}
	rungs := []rung{{0, "direct", ddgDirect}}
	if depth >= SearchViaReader {
		rungs = append(rungs, rung{1, "reader", ddgViaReader})
	}
	rungs = append(rungs, rung{2, "wikipedia", wikipediaSearch})

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
		return render(best.hits), nil
	}
	if lastErr != nil {
		// "Nothing was found" and "the search itself did not work" are different sentences to say
		// out loud, so they are different returns here.
		return "", fmt.Errorf("the search did not go through: %w", lastErr)
	}
	return fmt.Sprintf("The search found nothing about %q.", q), nil
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
			b.WriteString(": " + oneLine(r.Snippet, freeSnippet))
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// ---- rung one: the engine itself ----

// One pass over the lite HTML, in document order. A result is a title, optionally a snippet, and
// optionally a visible domain, and the middle one is genuinely optional — DuckDuckGo omits it for
// results it has no abstract for — so these cannot be matched separately and zipped by index.
//
// (?s) is load-bearing and was missing: the page puts the opening tag and the text on different
// lines, and without it `.` stops at the newline, so every snippet silently disappeared while the
// titles and domains — which do sit on one line — came through and made it look fine.
var liteRE = regexp.MustCompile(`(?s)class=['"]result-link['"][^>]*>(.*?)</a>` +
	`|class=['"]result-snippet['"][^>]*>(.*?)</td>` +
	`|class=['"]link-text['"][^>]*>(.*?)</span>`)

var tagRE = regexp.MustCompile(`<[^>]*>`)

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
		case m[1] != "":
			out = append(out, webResult{Title: clean(m[1])})
		case m[2] != "":
			if len(out) > 0 && out[len(out)-1].Snippet == "" {
				out[len(out)-1].Snippet = clean(m[2])
			}
		case m[3] != "":
			if len(out) > 0 && out[len(out)-1].Where == "" {
				out[len(out)-1].Where = clean(m[3])
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

// numbered matches the reader's plain-text rendering of a result list: a number, then the title,
// then indented lines for the snippet and the visible domain.
//
// The separators are NON-BREAKING spaces, U+00A0, not ordinary ones. Go's `\s` is ASCII-only, so a
// pattern written with it matches the page as a person reads it and not the page as it arrives.
// \pZ is every Unicode space, which is what is actually on the wire.
var numbered = regexp.MustCompile(`(?m)^[\pZ\t]*(\d{1,2})\.[\pZ\t]+(\S.*)$`)

func ddgViaReader(ctx context.Context, client *http.Client, q string) ([]webResult, error) {
	target := "https://lite.duckduckgo.com/lite/?q=" + url.QueryEscape(q)
	body, err := get(ctx, client, readerBase+target, 512<<10, map[string]string{
		"X-Return-Format": "text",
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
		r := webResult{Title: clean(text[loc[4]:loc[5]])}
		// Everything between this title and the next number is the snippet and the domain. The
		// domain is the line with no spaces in it, which is what a host looks like and a sentence
		// does not.
		var snip []string
		for _, line := range strings.Split(text[loc[5]:end], "\n") {
			line = strings.TrimSpace(line)
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
	body, _ := io.ReadAll(io.LimitReader(res.Body, limit))
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("answered %d", res.StatusCode)
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
