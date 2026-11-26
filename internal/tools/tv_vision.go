package tools

// TV Computer-Use Vision + Deep-Link Agent (tv_vision).
//
// Sees the live TV screen through tvd's Huawei ADB screenshot endpoint (~2 s JPEG) and drives
// the Xiaomi TV Box via tvd's Android TV Remote v2 commands (D-pad sequences, text typing,
// app launch, and YouTube/Netflix deep links).
//
// Capabilities:
//   - describe / where_are_we: see what is currently on screen, check Continue Watching in
//     Netflix/YouTube/Home to tell which show & episode we left off on, or inspect HDMI 2.
//   - search_play: ultra-fast YouTube deep-link search (~0.6 s) or Netflix title deep-link +
//     in-app Search & text typing fallback with Vision selection.
//   - resume / navigate: multi-step Computer-Use loop (up to 4 vision steps) that sees the TV,
//     navigates menus, types into search boxes via send_text, picks the requested movie/series/
//     episode, and presses play.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/u2n4/echo-gpt-live/internal/tv"
)

// tvVisionURL is a variable so tests can point OpenAI Vision calls at a local httptest server.
var tvVisionURL = "https://api.openai.com/v1/responses"

// ytSearchBase is a variable so tests can stub YouTube HTML search.
var ytSearchBase = "https://www.youtube.com/results"

// TVVisionConfig wires the TV Vision Computer-Use tool.
type TVVisionConfig struct {
	Client      TVDoer
	APIKey      string
	Model       string       // default "gpt-6.2-sol"
	Effort      string       // default "medium" ("low"|"medium"|"high"|"off")
	ServiceTier string       // optional Fast Mode ("fast" or "priority")
	HTTP        *http.Client // optional
	Web         *Web         // optional: used for fast Netflix title ID lookup
	OnActive    func()       // optional: activates fast-path remote mode when done
}

type visionStep struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

type visionDecision struct {
	Done   bool         `json:"done"`
	Say    string       `json:"say"`
	Action string       `json:"action"` // done | sequence | text | launch | launch_url | switch_input | wait
	Steps  []visionStep `json:"steps"`
	Text   string       `json:"text"`
	App    string       `json:"app"`
	URL    string       `json:"url"`
	Input  string       `json:"input"`
}

// TVVision returns the TV Computer-Use tool that sees the screen and controls Netflix/YouTube/TV.
func TVVision(cfg TVVisionConfig) *Tool {
	if cfg.Model == "" {
		cfg.Model = "gpt-6.2-sol"
	}
	if cfg.Effort == "" {
		cfg.Effort = "low"
	}
	httpClient := cfg.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}

	return &Tool{
		Name: "tv_vision",
		Description: "See the live TV screen and perform smart TV Computer-Use navigation (Netflix, YouTube, Shahid, Xiaomi Home, or cameras). " +
			"Use this whenever the user asks: what is on the TV screen ('وش على الشاشة'), what was the last movie/series/video we watched " +
			"('وش آخر فلم شاهدناه', 'وش آخر مسلسل كنا نشوفه', 'وش آخر شي شفناه في نتفلكس أو يوتيوب'), where we left off ('وين وصلنا في المسلسل'), " +
			"to resume/continue watching a show ('كملي المسلسل' / 'شغلي الحلقة اللي وقفنا عندها'), to search for and play a specific movie, " +
			"series, episode, or YouTube video ('دوري على فلم سبايدر مان الجديد', 'شغلي مقطع ... في يوتيوب'), or to navigate menus and type into TV search boxes.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"mode": map[string]any{
					"type":        "string",
					"description": "describe (read current screen), last_watched (find the last watched movie/series/video in Netflix Continue Watching or YouTube History), search_play (find & play a movie/series/video), resume (resume or pick episode from Continue Watching), navigate (general visual D-pad + typing navigation)",
					"enum":        []string{"describe", "last_watched", "search_play", "resume", "navigate"},
				},
				"goal": map[string]any{
					"type":        "string",
					"description": "what the user wants to know or accomplish on the TV screen (in Arabic or English)",
				},
				"app": map[string]any{
					"type":        "string",
					"description": "optional target app or input: netflix, youtube, shahid, cameras, or empty for current screen",
					"enum":        []string{"", "netflix", "youtube", "shahid", "cameras"},
				},
				"query": map[string]any{
					"type":        "string",
					"description": "movie, show, or video title to search for / type into TV search (prefer original title or clear Arabic/English name)",
				},
				"episode": map[string]any{
					"type":        "string",
					"description": "optional season/episode hint if the user asked for a specific episode",
				},
			},
			"required": []string{"mode", "goal"},
		},
		Access:       Read,
		DirectReturn: true,
		Timeout:      42 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			if cfg.Client == nil {
				return "", fmt.Errorf("التلفزيون غير مربوط حالياً")
			}
			mode := strings.ToLower(strings.TrimSpace(str(args["mode"])))
			goal := strings.TrimSpace(str(args["goal"]))
			app := strings.ToLower(strings.TrimSpace(str(args["app"])))
			query := strings.TrimSpace(str(args["query"]))
			episode := strings.TrimSpace(str(args["episode"]))
			if goal == "" {
				goal = query
			}
			if goal == "" {
				goal = "صف وش على شاشة التلفزيون الحين"
			}

			// Mark fast-path remote active at the end if we interacted with the TV.
			activate := func() {
				if cfg.OnActive != nil {
					cfg.OnActive()
				}
			}

			// 1. Handle cameras view explicitly if requested.
			if app == "cameras" {
				res, err := cfg.Client.Do(ctx, "switch_input", map[string]any{"input": "cameras"}, 9*time.Second)
				if err != nil || !res.OK {
					return "ما قدرت أحوّل على الكاميرات الحين.", nil
				}
				sleepCtx(ctx, 1200*time.Millisecond)
				if mode == "describe" && cfg.APIKey != "" {
					if ans, err := describeTVScreen(ctx, cfg.Client, httpClient, cfg.APIKey, cfg.Model, cfg.Effort, cfg.ServiceTier, goal, app); err == nil && ans != "" {
						return ans, nil
					}
				}
				return "تم، حوّلت التلفزيون على الكاميرات.", nil
			}

			// 2. Ultra-fast YouTube direct search & play (~1.1 s, zero screenshots needed!).
			// Fires for search_play AND navigate when the user named a video/channel to play on YouTube.
			if (mode == "search_play" || mode == "navigate") && app == "youtube" && query != "" {
				if vid, title := findYouTubeVideo(ctx, httpClient, query); vid != "" {
					deepURL := "https://www.youtube.com/watch?v=" + vid
					res, err := cfg.Client.Do(ctx, "launch_url", map[string]any{"url": deepURL}, 8*time.Second)
					if err == nil && res.OK {
						activate()
						if title != "" {
							return "تم، شغّلت لك في يوتيوب: " + oneLine(title, 90) + ".", nil
						}
						return "تم، شغّلت «" + oneLine(query, 60) + "» على يوتيوب.", nil
					}
				}
				// Fallback to Vision in-app YouTube search + send_text below.
			}

			// 3. Netflix search_play: prefer real in-app Netflix Search (guaranteed to match the local
			// regional Saudi Netflix catalog and show all matching movies/seasons/episodes!). Use direct
			// title deep-link ONLY when user didn't ask to browse/search newest options and no Vision key exists.
			if mode == "search_play" && app == "netflix" && query != "" && cfg.APIKey == "" && cfg.Web != nil {
				if nfID := findNetflixTitleID(ctx, cfg.Web, query); nfID != "" {
					deepURL := fmt.Sprintf("https://www.netflix.com/title/%s?source=30", nfID)
					if res, err := cfg.Client.Do(ctx, "launch_url", map[string]any{"url": deepURL}, 7*time.Second); err == nil && res.OK {
						activate()
						return "تم، فتحت «" + oneLine(query, 60) + "» في نتفلكس.", nil
					}
				}
			}

			if cfg.APIKey == "" {
				return "ميزة رؤية الشاشة تحتاج مفتاح OpenAI مفعّل.", nil
			}

			// 4. Default app for last_watched when unspecified (movies/series -> netflix).
			if mode == "last_watched" && app == "" {
				g := strings.ToLower(goal)
				if strings.Contains(g, "يوتيوب") || strings.Contains(g, "youtube") || strings.Contains(g, "مقطع") || strings.Contains(g, "فيديو") {
					app = "youtube"
				} else {
					app = "netflix"
				}
			}

			// 5. Ensure target app is open if requested (and not already in front).
			if app == "netflix" || app == "youtube" || app == "shahid" {
				st, serr := cfg.Client.Do(ctx, "state", nil, 2*time.Second)
				alreadyOpen := serr == nil && st.OK && appMatchesPackage(app, st.State.App)
				if !alreadyOpen {
					if lres, lerr := cfg.Client.Do(ctx, "launch", map[string]any{"app": app}, 7*time.Second); lerr == nil && lres.OK {
						sleepCtx(ctx, 1100*time.Millisecond)
					}
				}
			}

			// 6. Pure describe mode (single screenshot -> ~2.5 s).
			if mode == "describe" {
				ans, err := describeTVScreen(ctx, cfg.Client, httpClient, cfg.APIKey, cfg.Model, cfg.Effort, cfg.ServiceTier, goal, app)
				if err != nil {
					return "ما قدرت أشوف شاشة التلفزيون الحين، تأكد إن التلفزيون شغّال.", nil
				}
				return ans, nil
			}

			// 7. Full TV Computer-Use loop (last_watched / navigate / resume / in-app search + typing).
			fullGoal := goal
			if mode == "last_watched" {
				if app == "youtube" {
					fullGoal = "اعرف واقرأ بالاسم آخر فيديو أو فيديوهين تمت مشاهدتهما في يوتيوب: ادخل الحساب إن ظهرت شاشة اختيار الحساب، ثم افتح القائمة الجانبية (يمين في الواجهة العربية) وادخل على المكتبة 🔖 ثم السجل (History) وانتظر حتى تظهر الفيديوهات فعلياً على الشاشة ثم اقرأ عناوينها بدقة في say (لا تتوقف أبداً قبل ما تشوف عناوين الفيديوهات فعلياً على الشاشة!). " + goal
				} else {
					fullGoal = "اعرف واقرأ بالاسم آخر فلم أو مسلسل تمت مشاهدته في نتفلكس (Continue Watching / متابعة المشاهدة): إذا ظهرت شاشة Choose a Profile اضغط select للدخول لأول ملف، ثم انزل بالصفوف في Home أو ادخل تبويب My Netflix (up ثم right 3 مرات ثم select) ولا تتوقف حتى تشوف عناوين متابعة المشاهدة فعلياً على الشاشة وتقرأها بالاسم في say. " + goal
				}
			} else if mode == "search_play" && app == "netflix" {
				fullGoal = "ابحث عن «" + query + "» داخل نتفلكس وشغّله أو افتح صفحته: إذا ظهرت شاشة Choose a Profile اضغط select، ثم افتح أيقونة البحث 🔍 في الشريط العلوي (up ثم left من Home ثم select) ثم اضغط down مرة واحدة للدخول على الكيبورد (a)، ثم استخدم action='text' لكتابة الاسم بالإنجليزي المختصر (مثل spider man)، ثم انتقل باليمين (right) لأقرب نتيجة مطابقة وافتحها وشغّلها (أو اختر الحلقة المطلوبة). " + goal
			}
			if query != "" && !strings.Contains(strings.ToLower(fullGoal), strings.ToLower(query)) {
				fullGoal += " | المطلوب البحث عن: " + query
			}
			if episode != "" {
				fullGoal += " | الحلقة المطلوبة: " + episode
			}
			return runVisionLoop(ctx, cfg.Client, httpClient, cfg.APIKey, cfg.Model, cfg.Effort, cfg.ServiceTier, app, fullGoal, query, mode == "last_watched", 5, activate)
		},
	}
}

func appMatchesPackage(app, pkg string) bool {
	pkg = strings.ToLower(pkg)
	switch app {
	case "netflix":
		return strings.Contains(pkg, "netflix")
	case "youtube":
		return strings.Contains(pkg, "youtube")
	case "shahid":
		return strings.Contains(pkg, "shahid")
	default:
		return false
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func describeTVScreen(ctx context.Context, client TVDoer, httpClient *http.Client, apiKey, model, effort, serviceTier, goal, app string) (string, error) {
	sc, err := captureTVScreen(ctx, client, app)
	if err != nil {
		return "", err
	}
	sys := "أنتِ مساعدة صوتية ذكية (أسترا) تشوفين شاشة التلفزيون الحين وتردّين باللهجة الخليجية الدافئة في جملة أو جملتين قصيرتين واضحتين فقط (بدون نقاط ولا تنسيق، وبدون مناداة المستخدم بأي اسم شخصي، وبدون أسئلة متابعة). " +
		"إذا كانت الشاشة سوداء تماماً فاذكري بلطف إنه الظاهر فيه فيديو محمي (DRM) شغّال أو الشاشة مطفية. " +
		"إذا ظاهر اسم مسلسل أو فيلم أو صف متابعة المشاهدة أو رقم موسم/حلقة فاذكريه بدقة."
	return callVisionText(ctx, httpClient, apiKey, model, effort, serviceTier, sys, "سؤال المستخدم: "+goal, sc.ImageB64, 700)
}

// captureTVScreen requires fresh source evidence from tvd before a frame can leave the bridge.
func captureTVScreen(ctx context.Context, client TVDoer, app string) (tv.Result, error) {
	args := map[string]any{"max_width": 896, "quality": 7}
	required := ""
	switch app {
	case "cameras":
		required = "hdmi2"
	case "netflix", "youtube", "shahid":
		required = "hdmi3"
	}
	if required != "" {
		args["required_input"] = required
	}
	sc, err := client.Do(ctx, "screenshot", args, 7*time.Second)
	if err == nil && required == "hdmi3" && sc.Error == "input_mismatch" {
		sw, switchErr := client.Do(ctx, "switch_input", map[string]any{"input": "xiaomi"}, 9*time.Second)
		if switchErr != nil || !sw.OK {
			return tv.Result{}, fmt.Errorf("could not switch to the Xiaomi input")
		}
		sleepCtx(ctx, 800*time.Millisecond)
		sc, err = client.Do(ctx, "screenshot", args, 7*time.Second)
	}
	if err != nil || !sc.OK || sc.ImageB64 == "" {
		return tv.Result{}, fmt.Errorf("screenshot failed: %v %s", err, sc.Error)
	}
	if !sc.InputVerified || (required != "" && sc.Input != required) ||
		(required == "" && sc.Input == "hdmi2") {
		return tv.Result{}, fmt.Errorf("screenshot source unverified or not requested")
	}
	return sc, nil
}

// isPrematureVisionAsk detects when the Vision model tried to stop mid-task and ask the user
// permission ("سويت كذا، تبيني أشغل اللي بعده؟") instead of finishing the goal autonomously.
// Deliberately matches explicit permission/mid-progress phrases rather than bare punctuation so
// legitimate movie/video titles ending in '?' or '؟' are never rejected.
func isPrematureVisionAsk(say string) bool {
	s := strings.TrimSpace(say)
	if s == "" {
		return false
	}
	for _, marker := range []string{
		"تبيني أ", "تبيني ا", "تبغيني أ", "تبغيني ا", "تحب أكمل", "تحبين أكمل",
		"ما بدأت أي تشغيل", "ما دخلت الفيديوهات", "للحين تتحمل", "بس ما دخلت",
		"ما في مقطع مفتوح", "ولا شغلت أي شي",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func runVisionLoop(ctx context.Context, client TVDoer, httpClient *http.Client, apiKey, model, effort, serviceTier, app, goal, query string, mustReadResult bool, maxSteps int, onActive func()) (string, error) {
	var history []string
	var lastSay string
	for step := 0; step < maxSteps; step++ {
		if ctx.Err() != nil {
			break
		}
		sc, err := captureTVScreen(ctx, client, app)
		if err != nil {
			return "ما قدرت أتأكد من مدخل شاشة التلفزيون الحين.", nil
		}
		dec, err := decideVisionStep(ctx, httpClient, apiKey, model, effort, serviceTier, sc.ImageB64, goal, query, history, step+1, maxSteps)
		if err != nil {
			if lastSay != "" {
				return lastSay, nil
			}
			return "حاولت أتصفح الشاشة بس ما قدرت أحدد الخطوة الجاية.", nil
		}
		if dec.Say != "" {
			lastSay = dec.Say
		}
		hasWork := (dec.Action == "sequence" && len(dec.Steps) > 0) ||
			(dec.Action == "text" && strings.TrimSpace(dec.Text) != "") ||
			(dec.Action == "launch" && dec.App != "") ||
			(dec.Action == "launch_url" && dec.URL != "") ||
			(dec.Action == "switch_input" && dec.Input != "")
		// Never stop early to ask permission mid-task ("تبيني أفتح؟ / تبيني أشغل؟") on action goals!
		if !hasWork && (dec.Done || dec.Action == "done") {
			if !mustReadResult && step < maxSteps-1 && isPrematureVisionAsk(dec.Say) {
				history = append(history, fmt.Sprintf("step %d: REJECTED premature stop (%q) — DO NOT ask the user! You ALREADY have permission; execute the next TV action NOW!", step+1, dec.Say))
				sleepCtx(ctx, 400*time.Millisecond)
				continue
			}
			if onActive != nil {
				onActive()
			}
			if dec.Say != "" {
				return dec.Say, nil
			}
			return "تم، نفّذت المطلوب على التلفزيون.", nil
		}

		// Execute the chosen action on tvd.
		switch dec.Action {
		case "switch_input":
			inp := strings.ToLower(strings.TrimSpace(dec.Input))
			if inp == "" {
				inp = "xiaomi"
			}
			if res, derr := client.Do(ctx, "switch_input", map[string]any{"input": inp}, 9*time.Second); derr != nil || !res.OK {
				history = append(history, fmt.Sprintf("step %d: switch_input %s failed", step+1, inp))
				continue
			}
			if onActive != nil {
				onActive()
			}
			history = append(history, fmt.Sprintf("step %d: switched HDMI input to %s", step+1, inp))
			sleepCtx(ctx, 800*time.Millisecond)
		case "sequence":
			var steps []map[string]any
			total := 0
			for _, s := range dec.Steps {
				k, ok := normalizeTVKey(s.Key)
				if !ok {
					continue
				}
				cnt := clampInt(s.Count, 1, 8)
				if total+cnt > 12 {
					cnt = 12 - total
				}
				if cnt <= 0 {
					break
				}
				total += cnt
				steps = append(steps, map[string]any{"key": k, "count": cnt, "delay_ms": 70})
			}
			if len(steps) > 0 {
				if res, derr := client.Do(ctx, "sequence", map[string]any{"steps": steps}, 5*time.Second); derr != nil || !res.OK {
					history = append(history, fmt.Sprintf("step %d: sequence failed", step+1))
					continue
				}
				if onActive != nil {
					onActive()
				}
				history = append(history, fmt.Sprintf("step %d: pressed %d key groups", step+1, len(steps)))
				// If this was a final Play/Select click (not a read-only lookup), we can finish cleanly!
				if dec.Done && !mustReadResult && !isPrematureVisionAsk(dec.Say) {
					if dec.Say != "" {
						return dec.Say, nil
					}
					return "تم، شغّلته على التلفزيون.", nil
				}
				sleepCtx(ctx, 450*time.Millisecond)
			}
		case "text":
			txt := strings.TrimSpace(dec.Text)
			if txt == "" {
				txt = query
			}
			if txt != "" {
				// Clear any leftover search text cleanly before typing (Netflix keeps old queries!).
				if app == "netflix" {
					_, _ = client.Do(ctx, "key", map[string]any{"key": "backspace", "count": 10, "delay_ms": 40}, 3*time.Second)
					_, _ = client.Do(ctx, "key", map[string]any{"key": "backspace", "count": 10, "delay_ms": 40}, 3*time.Second)
					sleepCtx(ctx, 200*time.Millisecond)
				}
				// Pace 180 ms per char so Netflix's animated 6x6 JS keyboard NEVER drops a single letter!
				if res, derr := client.Do(ctx, "text", map[string]any{"text": txt, "pace_ms": 180}, 10*time.Second); derr != nil || !res.OK {
					history = append(history, fmt.Sprintf("step %d: text typing failed", step+1))
					continue
				}
				if onActive != nil {
					onActive()
				}
				history = append(history, fmt.Sprintf("step %d: cleared & typed %q", step+1, txt))
				sleepCtx(ctx, 1000*time.Millisecond)
			}
		case "launch":
			if dec.App != "" {
				if res, derr := client.Do(ctx, "launch", map[string]any{"app": strings.ToLower(dec.App)}, 7*time.Second); derr != nil || !res.OK {
					history = append(history, fmt.Sprintf("step %d: launch %s failed", step+1, dec.App))
					continue
				}
				if onActive != nil {
					onActive()
				}
				history = append(history, fmt.Sprintf("step %d: launched %s", step+1, dec.App))
				sleepCtx(ctx, 1100*time.Millisecond)
			}
		case "launch_url":
			if dec.URL != "" {
				if res, derr := client.Do(ctx, "launch_url", map[string]any{"url": dec.URL}, 7*time.Second); derr != nil || !res.OK {
					history = append(history, fmt.Sprintf("step %d: launch_url failed", step+1))
					continue
				}
				if onActive != nil {
					onActive()
				}
				history = append(history, fmt.Sprintf("step %d: opened url", step+1))
				sleepCtx(ctx, 1000*time.Millisecond)
			}
		case "wait":
			history = append(history, fmt.Sprintf("step %d: waited", step+1))
			sleepCtx(ctx, 700*time.Millisecond)
		default:
			if lastSay != "" {
				return lastSay, nil
			}
			return "تم، وصلت لأقرب شي أقدر عليه على الشاشة.", nil
		}
	}
	if onActive != nil {
		onActive()
	}
	if lastSay != "" {
		return lastSay, nil
	}
	return "ما قدرت أتأكد إن الطلب اكتمل على التلفزيون.", nil
}

// visionPlaybookSystem is kept 100% static so OpenAI's prompt cache hits across every vision step!
const visionPlaybookSystem = `You are the AUTONOMOUS TV Computer-Use controller for a Huawei Vision TV + Xiaomi Google TV Box (controlled with D-pad + remote keys + text typing + HDMI switching).

Look carefully at the current TV screenshot (note which tile/button currently has the white highlight/focus ring!).
Return ONLY a single valid JSON object (no markdown fences) with these keys:
{
  "done": true/false,
  "say": "short natural Gulf Arabic statement of what you DID or FOUND (1 sentence, NO questions, NO asking permission, NO personal names)",
  "action": "sequence" | "text" | "launch" | "switch_input" | "wait" | "done",
  "steps": [{"key": "up|down|left|right|select|back|home|backspace|play|pause|play_pause", "count": 1}],
  "text": "string to type into the focused search box when action is text",
  "app": "netflix|youtube|shahid",
  "input": "xiaomi|cameras"
}

IRONCLAD AUTONOMOUS RULES (100% VERIFIED ON THIS EXACT TV):
0. NEVER STOP MID-TASK TO ASK PERMISSION! NEVER ask "تبيني أفتح...؟", "تبيني أشغل...؟", "تحب أكمل؟", or report intermediate progress like "الحين فتحت البحث، تبيني أكمل؟"! You ALREADY have full user permission to finish the ENTIRE goal end-to-end inside this loop (switch HDMI if cameras are showing -> open app -> pick profile -> navigate -> search -> type -> select -> press Play)!
1. CAMERAS ON SCREEN: If you see security camera feeds (4-way or live CCTV cameras) while the user asked for Netflix/YouTube/Shahid/Xiaomi, return {"done":false,"action":"switch_input","input":"xiaomi","say":"بحول على الشاومي"} immediately!
2. NEVER use the "search" remote key — on this Xiaomi Box it launches Google Assistant instead of in-app search!
3. SPLASH / LOADING SCREEN: If you see a loading spinner or red "NETFLIX" / YouTube logo still loading, return {"done":false,"action":"wait","say":"جاري التحميل"}. NEVER set done=true while loading!
4. PROFILE / ACCOUNT PICKER: If Netflix ("Choose a Profile") or YouTube shows profile avatars, press "select" immediately with done=false!
5. COMPLETE NETFLIX 2026 TV PLAYBOOK (EXACT PIXEL-VERIFIED UI):
   - Top bar order (left to right): [Profile Avatar] | [🔍 Search] | [Home] | [Shows] | [Movies] | [My Netflix].
   - NEVER press left past 🔍 Search or you will open Profile Switcher! From Home tab: left x1 = 🔍 Search, right x1 = Shows, right x2 = Movies, right x3 = My Netflix.
   - NETFLIX SEARCH & TYPING:
     a) When you select 🔍 Search, focus starts ON the 🔍 top icon — NOT inside the keyboard! Press "down" x1 so the white box lands on 'a' in the 6x6 keyboard (rows: a-f, g-l, m-r, s-x, y-4, 5-0). Combine in ONE sequence from Home: [{"key":"up","count":1},{"key":"left","count":1},{"key":"select","count":1},{"key":"down","count":1}]!
     b) On the Search screen with focus inside the 6x6 keyboard, return {"done":false,"action":"text","text":"spider man"} — old text is cleared automatically before typing!
     c) Use short clean lowercase English spelling for Hollywood/foreign titles (e.g. "spider man", "batman", no hyphens).
     d) After typing, search results appear in a 4-column poster grid to the RIGHT of the 6x6 keyboard while focus stays on the last letter typed (col 0..5). Count exact right presses (6 - col to reach col 0 of posters, plus extra right presses for the target poster) and press "select" in ONE sequence!
   - NETFLIX DETAILS & EPISODES: On a Details page, press "select" on Play/Resume (done=true!) or move to Episodes & More to pick a specific Season & Episode!
   - NETFLIX CONTINUE WATCHING / LAST WATCHED: Scroll "down" on Home looking for "Continue Watching" / "متابعة المشاهدة", OR open "My Netflix" tab (up -> right x3 -> select)!
6. YOUTUBE TV UI (ARABIC RTL): Far-right icon rail (press "right" to enter rail): [Avatar] -> [🔍 Search] -> [🏠 Home] -> [⚡ Shorts] -> [📺 Subscriptions] -> [🔖 Library/المكتبة (3 steps DOWN from Home!)].
   - Last watched on YouTube: enter far-right rail -> move to 🔖 المكتبة -> left/select -> read exact video titles in السجل (History)!
7. Combine multiple D-pad moves + select in ONE "sequence" to finish in the fewest possible steps!
8. WHEN TO SET done=true:
   - Set "done": false whenever you are still navigating, switching HDMI, loading, opening Search, typing text, or selecting a channel/show!
   - Set "done": true ONLY when the target video/movie is ACTUALLY started/playing (right on the final Play/Select press), OR when the requested titles/answer are ACTUALLY visible and read in "say", OR on the final step.`

func decideVisionStep(ctx context.Context, httpClient *http.Client, apiKey, model, effort, serviceTier, b64, goal, query string, history []string, step, maxSteps int) (visionDecision, error) {
	histStr := "none yet"
	if len(history) > 0 {
		histStr = strings.Join(history, "; ")
	}
	userPrompt := fmt.Sprintf("Step %d of %d.\nUser goal: %s\nSearch query (if any): %s\nActions already taken: %s\n",
		step, maxSteps, goal, query, histStr)
	if step == maxSteps {
		userPrompt += "FINAL STEP: set done=true with a crisp Gulf Arabic statement (no questions) of what is on screen."
	}

	raw, err := callVisionText(ctx, httpClient, apiKey, model, effort, serviceTier, visionPlaybookSystem, userPrompt, b64, 950)
	if err != nil {
		return visionDecision{}, err
	}
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	if start := strings.IndexByte(raw, '{'); start >= 0 {
		if end := strings.LastIndexByte(raw, '}'); end > start {
			raw = raw[start : end+1]
		}
	}
	var dec visionDecision
	if err := json.Unmarshal([]byte(raw), &dec); err != nil {
		// If the model returned plain Arabic text instead of JSON, treat it as done.
		if raw != "" {
			return visionDecision{Done: true, Action: "done", Say: raw}, nil
		}
		return visionDecision{}, err
	}
	return dec, nil
}

func callVisionText(ctx context.Context, httpClient *http.Client, apiKey, model, effort, serviceTier, sysPrompt, userPrompt, imgB64 string, maxTokens int) (string, error) {
	mime := "image/jpeg"
	// PNG base64 starts with iVBOR
	if strings.HasPrefix(imgB64, "iVBOR") {
		mime = "image/png"
	}
	input := []any{}
	if strings.TrimSpace(sysPrompt) != "" {
		input = append(input, map[string]any{"role": "system", "content": sysPrompt})
	}
	input = append(input, map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "input_text", "text": userPrompt},
			map[string]any{
				"type":      "input_image",
				"image_url": "data:" + mime + ";base64," + imgB64,
				"detail":    "auto",
			},
		},
	})
	body := map[string]any{
		"model":             model,
		"store":             false,
		"max_output_tokens": maxTokens,
		"prompt_cache_key":  "astra-vision-v1",
		"input":             input,
	}
	if eff := strings.TrimSpace(effort); eff != "off" && (strings.HasPrefix(model, "gpt-6") || strings.HasPrefix(model, "gpt-5")) {
		if eff == "" {
			eff = "low"
		}
		body["reasoning"] = map[string]any{"effort": eff}
	}
	if t := strings.TrimSpace(serviceTier); t != "" && t != "default" {
		body["service_tier"] = t
	}
	postOnce := func(b map[string]any) (int, []byte, error) {
		raw, err := json.Marshal(b)
		if err != nil {
			return 0, nil, err
		}
		rctx, cancel := context.WithTimeout(ctx, 18*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(rctx, http.MethodPost, tvVisionURL, bytes.NewReader(raw))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		res, err := httpClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer res.Body.Close()
		payload, err := io.ReadAll(io.LimitReader(res.Body, 512<<10))
		return res.StatusCode, payload, err
	}
	status, payload, err := postOnce(body)
	if err != nil {
		return "", err
	}
	if status == 400 && (body["service_tier"] != nil || body["reasoning"] != nil || body["prompt_cache_key"] != nil) {
		msg := string(payload)
		retried := false
		if body["prompt_cache_key"] != nil && strings.Contains(msg, "prompt_cache_key") {
			delete(body, "prompt_cache_key")
			retried = true
		}
		if body["service_tier"] != nil && strings.Contains(msg, "service_tier") {
			delete(body, "service_tier")
			retried = true
		}
		if body["reasoning"] != nil && strings.Contains(msg, "reasoning") {
			delete(body, "reasoning")
			retried = true
		}
		if retried {
			status, payload, err = postOnce(body)
			if err != nil {
				return "", err
			}
		}
	}
	if status >= 400 {
		return "", fmt.Errorf("vision API HTTP %d", status)
	}
	var parsed struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, o := range parsed.Output {
		if o.Type == "message" {
			for _, c := range o.Content {
				if c.Text != "" {
					sb.WriteString(c.Text)
				}
			}
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

var (
	ytVideoIDRE    = regexp.MustCompile(`"videoRenderer":\{"videoId":"([A-Za-z0-9_-]{11})"`)
	ytTitleNearRE  = regexp.MustCompile(`"title":\{"runs":\[\{"text":"([^"]{2,120})"`)
	netflixTitleRE = regexp.MustCompile(`netflix\.com/(?:[a-z]{2}(?:-[a-z]{2})?/)?title/(\d{5,12})`)
)

// findYouTubeVideo performs a fast single-GET YouTube search (~1.1 s) and returns the top videoId + title.
// Note: YouTube's ytInitialData videoRenderer JSON sits around byte ~900KB-1.5MB in the HTML response,
// so we must read up to 2MB with a standard Chrome User-Agent.
func findYouTubeVideo(ctx context.Context, client *http.Client, query string) (string, string) {
	sctx, cancel := context.WithTimeout(ctx, 4500*time.Millisecond)
	defer cancel()
	u := ytSearchBase + "?search_query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, u, nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "ar,en;q=0.8")
	res, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return "", ""
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil || len(raw) == 0 {
		return "", ""
	}
	idx := ytVideoIDRE.FindSubmatchIndex(raw)
	if idx == nil {
		return "", ""
	}
	vid := string(raw[idx[2]:idx[3]])
	title := ""
	end := idx[1] + 1200
	if end > len(raw) {
		end = len(raw)
	}
	if tm := ytTitleNearRE.FindSubmatch(raw[idx[0]:end]); tm != nil {
		title = strings.ReplaceAll(string(tm[1]), `\"`, `"`)
	}
	return vid, title
}

// findNetflixTitleID searches the web quickly for a Netflix title/<ID> matching query.
func findNetflixTitleID(ctx context.Context, web *Web, query string) string {
	sctx, cancel := context.WithTimeout(ctx, 2800*time.Millisecond)
	defer cancel()
	hits, err := searchNow(sctx, web.client, web.depth, web.keenableKey, "site:netflix.com/title "+query)
	if err != nil {
		return ""
	}
	for _, h := range hits {
		if m := netflixTitleRE.FindStringSubmatch(h.URL); m != nil {
			return m[1]
		}
	}
	return ""
}
