package tools

// Smart natural-language TV control tool (Tier 2 of the hybrid TV remote).
//
// Tier 1 (cmd/bridge/remote.go) handles short, fast-path clicks in ~250 ms with zero LLM latency.
// Tier 2 (this tool) lets the router model understand ANY natural Arabic/Gulf phrasing — such as
// "اضغطي مرتين على الزر اللي في النص", "اضغطي مرتين على زر اللي يوقف المقطع", "حطي ميوت",
// "روحي تحت مرتين وبعدين يمين واضغطي", "وش شغال على التلفزيون", or typing into a search box —
// and send an atomic command or sequence to tvd.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/u2n4/echo-gpt-live/internal/tv"
)

// TVDoer matches tv.Client.Do so tests need no live tvd.
type TVDoer interface {
	Do(ctx context.Context, action string, args map[string]any, budget time.Duration) (tv.Result, error)
}

var tvKeyAliases = map[string]string{
	"up": "up", "down": "down", "left": "left", "right": "right",
	"select": "select", "ok": "select", "center": "select", "middle": "select", "dpad_center": "select",
	"enter": "enter", "back": "back", "home": "home", "menu": "menu", "backspace": "backspace", "del": "backspace",
	"settings": "settings", "info": "info", "captions": "captions",
	"subtitles": "captions", "all_apps": "all_apps", "app_switch": "app_switch", "guide": "guide",
	"play": "play", "pause": "pause", "play_pause": "play_pause", "stop": "stop",
	"fast_forward": "fast_forward", "ff": "fast_forward", "forward": "fast_forward",
	"rewind": "rewind", "rw": "rewind", "next": "next", "previous": "previous", "prev": "previous",
	"skip_forward": "skip_forward", "skip_backward": "skip_backward",
	"volume_up": "volume_up", "volume_down": "volume_down",
	"mute_toggle": "mute_toggle", "mute": "mute_toggle",
	"channel_up": "channel_up", "channel_down": "channel_down",
	"page_up": "page_up", "page_down": "page_down",
}

var tvKeyArabic = map[string]string{
	"up": "فوق", "down": "تحت", "left": "يسار", "right": "يمين",
	"select": "الزر اللي في النص", "enter": "إدخال", "back": "رجوع", "home": "الرئيسية",
	"menu": "القائمة", "settings": "الإعدادات", "search": "البحث", "info": "المعلومات",
	"captions": "الترجمة", "all_apps": "التطبيقات", "app_switch": "تبديل التطبيقات", "guide": "الدليل",
	"play": "تشغيل", "pause": "توقيف", "play_pause": "زر التوقيف والتشغيل", "stop": "إيقاف",
	"fast_forward": "تقديم", "rewind": "ترجيع", "next": "المقطع التالي", "previous": "المقطع السابق",
	"skip_forward": "تقديم سريع", "skip_backward": "ترجيع سريع",
	"volume_up": "رفع الصوت", "volume_down": "تخفيض الصوت", "mute_toggle": "كتم الصوت",
	"channel_up": "القناة التالية", "channel_down": "القناة السابقة",
	"page_up": "صفحة للأعلى", "page_down": "صفحة للأسفل",
}

// TVControl returns the natural-language Xiaomi TV Box remote tool.
// onActive is called after a successful control command (other than power_off/state) so the
// fast-path remote can stay ready for immediate follow-up clicks.
func TVControl(client TVDoer, onActive func()) *Tool {
	return &Tool{
		Name: "tv_control",
		Description: "Full remote control for the living-room Huawei Vision TV + Xiaomi TV Box. Supports power on/off, " +
			"status check, switching TV HDMI inputs (switch_input: 'cameras' / 'hdmi2' for security cameras, " +
			"'xiaomi' / 'hdmi3' for Xiaomi TV Box, 'hdmi1', 'av' — switches cleanly to fullscreen with no menu), " +
			"opening apps (youtube, netflix, shahid), opening a direct YouTube/Netflix/Shahid deep link (launch_url), " +
			"volume up/down, mute/unmute/toggle_mute, play/pause/play_pause/stop, ALL remote buttons (up, down, left, " +
			"right, select [center/OK button 'الزر اللي في النص'], back, home, menu, settings, search, captions, info, " +
			"all_apps, fast_forward, rewind, next, previous, channel_up, channel_down), repeating any button (count 1..10), " +
			"multi-step sequences (e.g. down twice then right then select), and typing text into TV search boxes.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"description": "the TV operation to run",
					"enum": []string{
						"power_on", "power_off", "state", "launch", "launch_url", "switch_input",
						"volume_step", "set_mute", "toggle_mute",
						"play", "pause", "play_pause", "stop",
						"key", "sequence", "text",
					},
				},
				"input": map[string]any{
					"type":        "string",
					"description": "HDMI input when action is 'switch_input': cameras (HDMI 2), xiaomi (HDMI 3), hdmi1, hdmi2, hdmi3, or av",
					"enum":        []string{"cameras", "xiaomi", "hdmi1", "hdmi2", "hdmi3", "av"},
				},
				"url": map[string]any{
					"type":        "string",
					"description": "https YouTube, Netflix, or Shahid deep link URL when action is 'launch_url'",
				},
				"key": map[string]any{
					"type": "string",
					"description": "button name when action is 'key': up, down, left, right, select (center/OK button), " +
						"back, home, menu, settings, search, captions, info, all_apps, app_switch, play, pause, " +
						"play_pause, stop, fast_forward, rewind, next, previous, skip_forward, skip_backward, " +
						"volume_up, volume_down, mute_toggle, channel_up, channel_down, page_up, page_down",
				},
				"count": map[string]any{
					"type":        "integer",
					"description": "number of times to press the button (1..10), e.g. 2 for 'مرتين'",
				},
				"steps": map[string]any{
					"type":        "array",
					"description": "ordered button sequence when action is 'sequence', up to 12 total presses",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"key":   map[string]any{"type": "string"},
							"count": map[string]any{"type": "integer"},
						},
						"required": []string{"key"},
					},
				},
				"volume_steps": map[string]any{
					"type":        "integer",
					"description": "volume change steps when action is 'volume_step' (-5..-1 to lower, 1..5 to raise)",
				},
				"mute": map[string]any{
					"type":        "boolean",
					"description": "true to mute, false to unmute when action is 'set_mute'",
				},
				"app": map[string]any{
					"type":        "string",
					"description": "app to open when action is 'launch': youtube, netflix, or shahid",
				},
				"text": map[string]any{
					"type":        "string",
					"description": "text to type into the TV search box when action is 'text'",
				},
			},
			"required": []string{"action"},
		},
		Access:       Read, // immediate execution without spoken confirmation
		DirectReturn: true, // return crisp spoken Arabic immediately without a second LLM round
		Timeout:      12 * time.Second,
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			action := strings.ToLower(strings.TrimSpace(str(args["action"])))
			if action == "" {
				return "", fmt.Errorf("ما تحدد أمر التلفزيون")
			}

			var (
				res     tv.Result
				err     error
				replyOK string
			)

			switch action {
			case "power_on":
				res, err = client.Do(ctx, "power_on", nil, 7*time.Second)
				replyOK = "تم، شغّلت التلفزيون."
			case "power_off":
				res, err = client.Do(ctx, "power_off", nil, 4*time.Second)
				replyOK = "تم، طفّيت التلفزيون."
			case "state":
				res, err = client.Do(ctx, "state", nil, 2*time.Second)
				if err == nil && res.OK {
					return formatTVState(res.State), nil
				}
			case "launch":
				app := strings.ToLower(strings.TrimSpace(str(args["app"])))
				if app == "" {
					return "", fmt.Errorf("أي تطبيق أفتح؟")
				}
				res, err = client.Do(ctx, "launch", map[string]any{"app": app}, 7*time.Second)
				replyOK = "تم، فتحت " + tvAppArabic(app) + "."
			case "launch_url":
				u := strings.TrimSpace(str(args["url"]))
				if u == "" {
					return "", fmt.Errorf("ما فيه رابط أفتحه على التلفزيون")
				}
				res, err = client.Do(ctx, "launch_url", map[string]any{"url": u}, 7*time.Second)
				replyOK = "تم، شغّلته على التلفزيون."
			case "switch_input":
				inp := strings.ToLower(strings.TrimSpace(str(args["input"])))
				if inp == "" {
					return "", fmt.Errorf("على أي مدخل HDMI أحوّل؟")
				}
				res, err = client.Do(ctx, "switch_input", map[string]any{"input": inp}, 9*time.Second)
				replyOK = "تم، حوّلت التلفزيون على " + tvInputArabic(inp) + "."
			case "volume_step":
				steps := intArg(args["volume_steps"], intArg(args["steps"], 0))
				if steps == 0 {
					return "", fmt.Errorf("حددي أرفع ولا أوطي الصوت")
				}
				if steps > 5 {
					steps = 5
				}
				if steps < -5 {
					steps = -5
				}
				res, err = client.Do(ctx, "volume_step", map[string]any{"steps": steps}, 3*time.Second)
				if steps > 0 {
					replyOK = "تم، رفعت الصوت."
				} else {
					replyOK = "تم، وطّيت الصوت."
				}
			case "set_mute":
				mute, ok := args["mute"].(bool)
				if !ok {
					res, err = client.Do(ctx, "toggle_mute", nil, 3*time.Second)
					replyOK = "تم، بدّلت كتم الصوت."
				} else {
					res, err = client.Do(ctx, "set_mute", map[string]any{"mute": mute}, 3*time.Second)
					// If the TV box did not report its mute state, only fall back to toggle on mute=true
					// (never risk muting when the user explicitly asked to unmute!).
					if err == nil && !res.OK && res.Error == "unknown" && mute {
						res, err = client.Do(ctx, "toggle_mute", nil, 3*time.Second)
					}
					if mute {
						replyOK = "تم، كتمت صوت التلفزيون."
					} else {
						replyOK = "تم، رجّعت صوت التلفزيون."
					}
				}
			case "toggle_mute":
				res, err = client.Do(ctx, "toggle_mute", nil, 3*time.Second)
				replyOK = "تم، كتمت أو رجّعت الصوت."
			case "play", "pause", "play_pause", "stop":
				cnt := clampInt(intArg(args["count"], 1), 1, 10)
				res, err = client.Do(ctx, action, map[string]any{"count": cnt, "delay_ms": 70}, 4*time.Second)
				replyOK = formatKeyReply(action, cnt)
			case "key":
				k, ok := normalizeTVKey(str(args["key"]))
				if !ok {
					return "", fmt.Errorf("الزر %q غير معروف", str(args["key"]))
				}
				cnt := clampInt(intArg(args["count"], 1), 1, 10)
				res, err = client.Do(ctx, "key", map[string]any{"key": k, "count": cnt, "delay_ms": 70}, 4*time.Second)
				replyOK = formatKeyReply(k, cnt)
			case "sequence":
				rawSteps, ok := args["steps"].([]any)
				if !ok || len(rawSteps) == 0 {
					return "", fmt.Errorf("ما فيه خطوات لتنفيذها")
				}
				var steps []map[string]any
				total := 0
				for _, item := range rawSteps {
					m, ok := item.(map[string]any)
					if !ok {
						continue
					}
					k, ok := normalizeTVKey(str(m["key"]))
					if !ok {
						return "", fmt.Errorf("الزر %q غير معروف", str(m["key"]))
					}
					cnt := clampInt(intArg(m["count"], 1), 1, 10)
					if total+cnt > 12 {
						cnt = 12 - total
					}
					if cnt <= 0 {
						break
					}
					total += cnt
					steps = append(steps, map[string]any{"key": k, "count": cnt, "delay_ms": 70})
				}
				if len(steps) == 0 {
					return "", fmt.Errorf("ما فيه خطوات صالحة")
				}
				res, err = client.Do(ctx, "sequence", map[string]any{"steps": steps}, 5*time.Second)
				replyOK = "تم، نفّذت الخطوات على التلفزيون."
			case "text":
				txt := strings.TrimSpace(str(args["text"]))
				if txt == "" {
					return "", fmt.Errorf("وش أكتب على التلفزيون؟")
				}
				res, err = client.Do(ctx, "text", map[string]any{"text": txt, "pace_ms": 180}, 8*time.Second)
				replyOK = "تم، كتبت «" + oneLine(txt, 60) + "» على التلفزيون."
			default:
				return "", fmt.Errorf("أمر التلفزيون %q غير مدعوم", action)
			}

			if err != nil {
				return "", fmt.Errorf("ما قدرت أوصل للتلفزيون: %w", err)
			}
			if !res.OK {
				return tvErrorArabic(res.Error), nil
			}
			if onActive != nil && action != "power_off" && action != "state" {
				onActive()
			}
			return replyOK, nil
		},
	}
}

func normalizeTVKey(k string) (string, bool) {
	v, ok := tvKeyAliases[strings.ToLower(strings.TrimSpace(k))]
	return v, ok
}

func intArg(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return def
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func formatKeyReply(key string, count int) string {
	ar := tvKeyArabic[key]
	if ar == "" {
		ar = key
	}
	switch count {
	case 1:
		return "تم، ضغطت " + ar + "."
	case 2:
		return "تم، ضغطت " + ar + " مرتين."
	default:
		return fmt.Sprintf("تم، ضغطت %s %d مرات.", ar, count)
	}
}

func tvAppArabic(app string) string {
	switch app {
	case "youtube":
		return "يوتيوب"
	case "netflix":
		return "نتفلكس"
	case "shahid":
		return "شاهد"
	default:
		return app
	}
}

func formatTVState(st tv.State) string {
	switch st.Power {
	case "standby":
		return "التلفزيون طافي الحين."
	case "on":
		if st.App == "" {
			return "التلفزيون شغّال الحين."
		}
		app := st.App
		switch {
		case strings.Contains(app, "youtube"):
			app = "يوتيوب"
		case strings.Contains(app, "netflix"):
			app = "نتفلكس"
		case strings.Contains(app, "shahid"):
			app = "شاهد"
		case strings.Contains(app, "launcher"):
			app = "الشاشة الرئيسية"
		}
		return "التلفزيون شغّال، ومفتوح عليه " + app + "."
	default:
		return "حالة التلفزيون غير معروفة حالياً."
	}
}

func tvInputArabic(inp string) string {
	switch inp {
	case "cameras", "hdmi2":
		return "الكاميرات"
	case "xiaomi", "hdmi3":
		return "الشاومي"
	case "hdmi1":
		return "HDMI 1"
	case "av":
		return "AV"
	default:
		return inp
	}
}

func tvErrorArabic(code string) string {
	switch code {
	case "off":
		return "التلفزيون طافي الحين، قولي شغّلي التلفزيون أول."
	case "unreachable":
		return "ما قدرت أوصل للتلفزيون، تأكدي إنه شغّال وموصل بالشبكة."
	case "unconfigured":
		return "تحويل مداخل الهواوي غير مضبوط في الإعدادات حالياً."
	case "timeout":
		return "التلفزيون تأخر في الاستجابة، جربي مرة ثانية."
	case "busy":
		return "التلفزيون مشغول بأمر ثاني، جربي بعد ثانية."
	default:
		return "ما زبط أمر التلفزيون (" + code + ")."
	}
}
