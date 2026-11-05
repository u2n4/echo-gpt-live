package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A trimmed copy of what lite.duckduckgo.com returned on 13 September 2026, including the case that
// breaks the obvious implementation: the second result has no snippet, so matching titles and
// snippets separately and zipping them by index puts the first result's text on the second.
const liteFixture = `<html><body><table>
<tr><td valign="top">1.&nbsp;</td><td>
  <a rel="nofollow" href="//duckduckgo.com/l/?uddg=x" class='result-link'>&#1587;&#1593;&#1585; &amp; price</a>
</td></tr>
<tr><td>&nbsp;</td><td class='result-snippet'>
                    The <b>gold</b> price today in Saudi Arabia.
                  </td></tr>
<tr><td>&nbsp;</td><td><span class='link-text'>gold.com/sa/</span></td></tr>
<tr><td valign="top">2.&nbsp;</td><td>
  <a rel="nofollow" href="//duckduckgo.com/l/?uddg=y" class='result-link'>No snippet here</a>
</td></tr>
<tr><td>&nbsp;</td><td><span class='link-text'>nosnippet.example</span></td></tr>
</table></body></html>`

// What the reader returns for the same search, in markdown mode (a trimmed copy from 16 September
// 2026): a nav block with no numbers, then numbered results whose title is a link through the
// engine's redirect, a bolded snippet, and the visible domain as the last unspaced line.
const readerFixture = `Title: ذهب at DuckDuckGo

URL Source: https://lite.duckduckgo.com/lite/?q=ذهب

Markdown Content:
DuckDuckGo

1.[أسعار الذهب اليوم في السعودية](https://duckduckgo.com/l/?uddg=https%3A%2F%2Fdrhint.com%2Far%2Fgold%2Drate%2Dsaudi%2Darabia&rut=abc)
**أسعار** الذهب اليوم في السعودية بالريال السعودي لحظة بلحظة.
drhint.com/ar/gold-rate-saudi-arabia

2.[Gold Price in Saudi Arabia](https://duckduckgo.com/l/?uddg=https%3A%2F%2Flivepriceofgold.com%2Fsaudi%2Darabia%2Dgold%2Dprice.html&rut=def)
livepriceofgold.com/saudi-arabia-gold-price.html
`

func TestDDGLiteParse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "" {
			t.Error("the query never reached the engine")
		}
		_, _ = w.Write([]byte(liteFixture))
	}))
	defer srv.Close()

	got, err := ddgLiteFrom(context.Background(), srv.Client(), srv.URL+"/lite/?q=x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 results, got %d: %+v", len(got), got)
	}
	if got[0].Title != "سعر & price" {
		t.Errorf("title not decoded: %q", got[0].Title)
	}
	if got[0].Snippet != "The gold price today in Saudi Arabia." {
		t.Errorf("snippet not cleaned: %q", got[0].Snippet)
	}
	if got[0].Where != "gold.com/sa/" {
		t.Errorf("domain: %q", got[0].Where)
	}
	// The whole point of the single-pass scan: a missing snippet must stay missing.
	if got[1].Snippet != "" {
		t.Errorf("result 2 has no snippet in the page but got %q", got[1].Snippet)
	}
	if got[1].Where != "nosnippet.example" {
		t.Errorf("result 2 domain: %q", got[1].Where)
	}
}

// TestDDGBlockedIsNotEmpty is the measured failure: the challenge page arrives as HTTP 202, so
// status alone cannot tell a block from an honest empty result.
func TestDDGBlockedIsNotEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`<html><body><div id="anomaly-modal">challenge</div></body></html>`))
	}))
	defer srv.Close()

	_, err := ddgLiteFrom(context.Background(), srv.Client(), srv.URL+"/lite/?q=x", nil)
	if err == nil {
		t.Fatal("a challenge page must not read as an empty result")
	}
	if !strings.Contains(err.Error(), "automated") {
		t.Errorf("the reason should name the block: %v", err)
	}
}

func TestReaderParse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Return-Format") != "markdown" {
			t.Errorf("the reader needs to be asked for markdown, which carries the links; got %q", r.Header.Get("X-Return-Format"))
		}
		_, _ = w.Write([]byte(readerFixture))
	}))
	defer srv.Close()

	old := readerBase
	readerBase = srv.URL + "/"
	defer func() { readerBase = old }()

	got, err := ddgViaReader(context.Background(), srv.Client(), "ذهب")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 results, got %d: %+v", len(got), got)
	}
	if !strings.HasPrefix(got[0].Title, "أسعار الذهب") {
		t.Errorf("title: %q", got[0].Title)
	}
	if got[0].Where != "drhint.com/ar/gold-rate-saudi-arabia" {
		t.Errorf("domain: %q", got[0].Where)
	}
	if !strings.Contains(got[0].Snippet, "بالريال") {
		t.Errorf("snippet: %q", got[0].Snippet)
	}
	// The second has no snippet line, only a domain. It must not borrow the first one's.
	if got[1].Snippet != "" {
		t.Errorf("result 2 snippet should be empty, got %q", got[1].Snippet)
	}
	if got[1].Where != "livepriceofgold.com/saudi-arabia-gold-price.html" {
		t.Errorf("result 2 domain: %q", got[1].Where)
	}
	// The address is decoded out of the engine's redirect, because that is what open_result reads.
	if got[0].URL != "https://drhint.com/ar/gold-rate-saudi-arabia" {
		t.Errorf("result 1 url: %q", got[0].URL)
	}
	if got[1].URL != "https://livepriceofgold.com/saudi-arabia-gold-price.html" {
		t.Errorf("result 2 url: %q", got[1].URL)
	}
	// The navigation above the results has no numbers and must not become a result.
	for _, r := range got {
		if strings.Contains(r.Title, "Any Time") || strings.Contains(r.Title, "All Regions") {
			t.Errorf("navigation leaked into the results: %q", r.Title)
		}
	}
}

// TestLadderFallsThrough proves the ladder does its job: a blocked engine is not the end of the
// search. Wikipedia is real here because a fake one would test the fake.
func TestLadderFallsThrough(t *testing.T) {
	if testing.Short() {
		t.Skip("network")
	}
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`<html>anomaly</html>`))
	}))
	defer blocked.Close()

	oldD, oldR := ddgBase, readerBase
	ddgBase = blocked.URL
	readerBase = blocked.URL + "/" // the reader is blocked too, so only the encyclopedia is left
	defer func() { ddgBase, readerBase = oldD, oldR }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := WebSearch(&http.Client{Timeout: 10 * time.Second}, SearchViaReader).
		Run(ctx, map[string]any{"query": "الرياض"})
	if err != nil {
		t.Fatalf("both engines blocked and the encyclopedia did not answer: %v", err)
	}
	if !strings.Contains(out, "wikipedia.org") {
		t.Fatalf("expected the last rung to answer:\n%s", out)
	}
	t.Logf("%s", out)
}

// TestWebSearchLive hits the real ladder. It is the only thing that catches an engine changing its
// page, which is the failure this tool is most likely to have.
func TestWebSearchLive(t *testing.T) {
	if testing.Short() {
		t.Skip("network")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	out, err := WebSearch(&http.Client{Timeout: 12 * time.Second}, SearchViaReader).
		Run(ctx, map[string]any{"query": "كم سعر الذهب اليوم في السعودية"})
	if err != nil {
		t.Fatalf("live search: %v", err)
	}
	if !strings.HasPrefix(out, "1.") {
		t.Fatalf("no numbered results came back:\n%s", out)
	}
	t.Logf("%d chars, roughly %d tokens:\n%s", len(out), len(out)/3, out)
}
