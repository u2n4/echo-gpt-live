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

	"github.com/u2n4/echo-gpt-live/internal/memory"
	"github.com/u2n4/echo-gpt-live/internal/openai"
	"github.com/u2n4/echo-gpt-live/internal/tools"
	"github.com/u2n4/echo-gpt-live/internal/tv"
)

func main() {
	var (
		keyFile      = flag.String("openai-key-file", os.ExpandEnv("$HOME/.config/acv/openai.key"), "OpenAI API key file")
		keenableKey  = flag.String("keenable-key-file", os.ExpandEnv("$HOME/.config/acv/keenable.key"), "optional Keenable API key file")
		model        = flag.String("model", openai.SearchModel, "the router model to measure")
		effort       = flag.String("effort", "", "reasoning effort: empty = the bridge's default, or minimal|low|medium|off")
		tier         = flag.String("tier", "", "optional OpenAI service_tier (fast|priority)")
		visionModel  = flag.String("vision-model", "gpt-6.2-sol", "TV Vision model")
		visionEffort = flag.String("vision-effort", "low", "TV Vision reasoning effort (low|medium|high|off)")
		visionTier   = flag.String("vision-tier", "fast", "TV Vision service_tier")
		search       = flag.String("web-search", "free", "free|private|openai|off, as in the bridge")
		prefetch     = flag.Bool("prefetch", true, "search for the words before the first round, as the bridge does")
		homeTools    = flag.Bool("home-tools", false, "register household tools (prayer, weather, memory, shopping, reminders, timer, quran, convert)")
		prayerMethod = flag.Int("prayer-method", 0, "Aladhan prayer calculation method when --home-tools is set (0 = Shia Ja'fari)")
		tvURL        = flag.String("tv-url", "", "optional tvd URL (e.g. http://127.0.0.1:8831) to include tv_control")
		tvKeyFile    = flag.String("tv-key-file", os.ExpandEnv("$HOME/.config/tvd/signing.key"), "tvd signing key file")
		where        = flag.String("context", "", "what the router is told about where and when (empty = Riyadh default)")
		n            = flag.Int("n", 1, "how many times to ask, for a spread")
		deadl        = flag.Duration("deadline", 25*time.Second, "the bridge's delegation deadline")
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
	payForSearch := false
	keenableAPIKey := ""
	if *keenableKey != "" {
		if kb, kerr := os.ReadFile(*keenableKey); kerr == nil {
			keenableAPIKey = strings.TrimSpace(string(kb))
		}
	}
	if keenableAPIKey == "" {
		keenableAPIKey = strings.TrimSpace(os.Getenv("KEENABLE_API_KEY"))
	}
	var web *tools.Web
	switch *search {
	case "free":
		web = tools.NewWeb(nil, tools.SearchViaReader).SetKeenableKey(keenableAPIKey)
	case "private":
		web = tools.NewWeb(nil, tools.SearchDirect)
	case "openai":
		payForSearch = true
	case "off":
	default:
		fmt.Fprintln(os.Stderr, "--web-search must be free, private, openai or off")
		os.Exit(2)
	}
	if web != nil {
		reg.Add(web.SearchTool())
		reg.Add(web.OpenTool())
	}
	if *homeTools {
		tmp, _ := os.MkdirTemp("", "answertest-*")
		defer os.RemoveAll(tmp)
		reg.Add(tools.Weather(24.7136, 46.6753, "Asia/Riyadh", nil))
		reg.Add(tools.PrayerWithMethod(24.7136, 46.6753, "Asia/Riyadh", *prayerMethod, nil))
		if mem, merr := memory.Open(tmp + "/memory.md"); merr == nil {
			reg.Add(tools.RememberThis(mem, nil))
			reg.Add(tools.RecallMemory(mem))
			reg.Add(tools.WhatYouKnow(mem))
			reg.Add(tools.ForgetMemory(mem, nil))
		}
		reg.Add(tools.ShoppingList(tmp + "/shopping.md"))
		rs := tools.NewReminderStore(tmp + "/reminders.json")
		reg.Add(tools.Reminders(rs, riyadh()))
		reg.Add(tools.TodayReminders(rs, riyadh()))
		reg.Add(tools.KitchenTimer(tools.NewTimerManager()))
		reg.Add(tools.QuranLookup(nil))
		reg.Add(tools.Convert(nil))
	}
	if *tvURL != "" {
		if tk, terr := os.ReadFile(*tvKeyFile); terr == nil {
			tc := tv.New(*tvURL, []byte(strings.TrimSpace(string(tk))))
			reg.Add(tools.TVControl(tc, nil))
			reg.Add(tools.TVVision(tools.TVVisionConfig{
				Client:      tc,
				APIKey:      apiKey,
				Model:       *visionModel,
				Effort:      *visionEffort,
				ServiceTier: *visionTier,
				Web:         web,
			}))
		}
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
		WebSearch: payForSearch, MaxOutputTokens: 2200, Deadline: *deadl,
		Reasoning: *effort, ServiceTier: *tier,
	}
	if web != nil && *prefetch {
		ans.Prefetch = web.Speculative
	}
	ctxText := *where
	if ctxText == "" {
		ctxText = "He is at home in Riyadh, Saudi Arabia. It is " +
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
