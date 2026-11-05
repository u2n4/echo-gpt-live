// Command answertest asks the bridge's router one question the way a delegation would, with no
// call, no Echo and no voice model: only the cheap model, the tool registry and the free search.
//
// It exists to MEASURE. The question "why does she take nine seconds" cannot be answered from a
// live call, where the router's rounds are mixed with the voice model's own latency, and it
// cannot be answered by reasoning about it either. This runs the same code path the call runs and
// prints every round with its duration, so a change to the model, the reasoning effort or the
// tools is a number before it is an opinion.
//
// Cost: tokens only, a fraction of a cent per question. It never opens a live session and never
// touches the bridge's port.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/u2n4/echo-gpt-live/internal/openai"
	"github.com/u2n4/echo-gpt-live/internal/tools"
)

func main() {
	var (
		keyFile  = flag.String("openai-key-file", os.ExpandEnv("$HOME/.config/acv/openai.key"), "OpenAI API key file")
		model    = flag.String("model", openai.SearchModel, "the router model to measure")
		effort   = flag.String("effort", "", "reasoning effort: empty = the bridge's default, or minimal|low|medium|off")
		search   = flag.String("web-search", "free", "free|private|off, as in the bridge")
		prefetch = flag.Bool("prefetch", true, "search for the words before the first round, as the bridge does")
		where    = flag.String("context", "", "what the router is told about where and when (empty = a weekday afternoon)")
		n        = flag.Int("n", 1, "how many times to ask, for a spread")
		deadl    = flag.Duration("deadline", 25*time.Second, "the bridge's delegation deadline")
	)
	flag.Parse()
	question := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if question == "" {
		fmt.Fprintln(os.Stderr, "usage: answertest [flags] <the question, as he would say it>")
		os.Exit(2)
	}
	b, err := os.ReadFile(*keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "openai key:", err)
		os.Exit(2)
	}
	apiKey := strings.TrimSpace(string(b))

	reg := tools.NewRegistry()
	var web *tools.Web
	switch *search {
	case "free":
		web = tools.NewWeb(nil, tools.SearchViaReader)
	case "private":
		web = tools.NewWeb(nil, tools.SearchDirect)
	case "off":
	default:
		fmt.Fprintln(os.Stderr, "--web-search must be free, private or off")
		os.Exit(2)
	}
	if web != nil {
		reg.Add(web.SearchTool())
		reg.Add(web.OpenTool())
	}
	reg.Add(tools.EndCall())

	start := time.Now()
	ev := func(name string, kv ...any) {
		var sb strings.Builder
		fmt.Fprintf(&sb, "%7.2fs  %-28s", time.Since(start).Seconds(), name)
		for i := 0; i+1 < len(kv); i += 2 {
			fmt.Fprintf(&sb, " %v=%v", kv[i], kv[i+1])
		}
		fmt.Println(sb.String())
	}

	ans := &openai.Answerer{
		APIKey: apiKey, Model: *model, Tools: reg, OnEvent: ev,
		MaxOutputTokens: 1500, Deadline: *deadl, Reasoning: *effort,
	}
	if web != nil && *prefetch {
		ans.Prefetch = web.Speculative
	}
	ctxText := *where
	if ctxText == "" {
		ctxText = "The user is at home. It is " +
			time.Now().In(riyadh()).Format("Monday 2 January 2006, 15:04") + " local time."
	}
	ans.SetContext(ctxText)

	for i := 0; i < *n; i++ {
		start = time.Now()
		fmt.Printf("=== %s effort=%q prefetch=%v run %d/%d: %s\n", *model, *effort, *prefetch, i+1, *n, question)
		answer, err := ans.Answer(context.Background(), openai.Question{Said: question})
		if err != nil {
			fmt.Printf("=== FAILED after %.2fs: %v\n\n", time.Since(start).Seconds(), err)
			continue
		}
		fmt.Printf("=== %.2fs  %s\n\n", time.Since(start).Seconds(), answer)
	}
}

func riyadh() *time.Location {
	if loc, err := time.LoadLocation("Asia/Riyadh"); err == nil {
		return loc
	}
	return time.FixedZone("AST", 3*3600)
}
