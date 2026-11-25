package main

// The TV remote, by voice, inside a call.
//
// A TV command through the assistant's normal route is a delegation: the voice model asks for help,
// the router model thinks, a tool runs, she says it is done - 5 to 8 seconds measured. A remote
// cannot work like that. So the bridge reads his transcript itself: the moment he pauses
// (remoteQuiet), the words since the last pause are checked against a small fixed vocabulary, and a
// match goes straight to tvd on loopback (a few milliseconds there). The voice model is told to stay
// out of it (tvPolicy), and a delegation she raises about the same words anyway is answered
// silently (handles).
//
// One rule keeps ordinary conversation from moving the TV: an utterance acts only when EVERY word
// in it is part of a command, a repeat count, or a filler ("يا سول", "لو سمحت", "ممكن", "شوي").
// "شغّل التلفزيون" turns it on; "بعدين شغّل التلفزيون", "لا شغّل التلفزيون" and "شلون أطفي
// التلفزيون" do nothing, because بعدين / لا / شلون are none of those (Grok). On top of that:
//   - "شغّل/طفّ التلفزيون" and "وضع الريموت" work any time; they also switch REMOTE MODE on or off.
//   - Everything else (arrows, OK, back, home, volume, pause, apps) works only in remote mode,
//     which ends on "خلاص" or after remoteIdle with no command. Words people say all the time
//     in conversation ("اوكي", "شغّل", "ورا") are left out of the vocabulary on purpose.
//
// Her own voice comes back through the Echo's microphone as his transcript, so nothing heard while
// she is speaking counts (the same judge the farewell uses).

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/u2n4/echo-gpt-live/internal/tv"
)

const (
	// remoteQuiet is how long after his last transcript fragment the words are read. Short enough
	// to feel like a real physical remote button while still allowing a split word to land.
	remoteQuiet = 130 * time.Millisecond
	// remoteIdle ends remote mode when no command came for this long.
	remoteIdle = 2 * time.Minute
	// remoteTail bounds what is kept between pauses (characters).
	remoteTail = 200
	// remoteLookback: a power or mode phrase whose first fragment arrives this soon after other
	// words were read is judged together with them.
	remoteLookback = 600 * time.Millisecond
	// maxPresses bounds one utterance (matches tvd MAX_SEQUENCE_PRESSES).
	maxPresses = 12
)

// tvDoer is the part of tv.Client the remote uses (an interface so tests need no server).
type tvDoer interface {
	Do(ctx context.Context, action string, args map[string]any, budget time.Duration) (tv.Result, error)
}

// remoteCmd is one call to tvd.
type remoteCmd struct {
	action string
	args   map[string]any
}

func (c remoteCmd) String() string {
	if k, ok := c.args["key"]; ok {
		return fmt.Sprint(k)
	}
	if a, ok := c.args["app"]; ok {
		return fmt.Sprint(a)
	}
	if inp, ok := c.args["input"]; ok {
		return "input:" + fmt.Sprint(inp)
	}
	if s, ok := c.args["steps"]; ok {
		return fmt.Sprintf("volume%+d", s)
	}
	return c.action
}

// budgetFor is how long tvd may take for a command: waking the box or switching Huawei HDMI takes
// seconds, a key does not.
func budgetFor(action string) time.Duration {
	switch action {
	case "switch_input":
		return 9 * time.Second
	case "power_on":
		return 7 * time.Second
	case "launch":
		return 7 * time.Second // may switch the TV on first (tvd LAUNCH_TIMEOUT_S 6.8)
	case "power_off":
		return 4 * time.Second
	default:
		return 2 * time.Second
	}
}

// ---- vocabulary --------------------------------------------------------------------------------

// Multi-word phrases, rewritten to one token before the words are checked. Written the way they are
// said; every entry is normalised at init like what he says. Longer phrases are applied first.
var remotePhraseTable = []struct{ from, to string }{
	// power, any mode
	{"شغل التلفزيون", "#on"}, {"شغلي التلفزيون", "#on"}, {"شغل لي التلفزيون", "#on"}, {"شغلي لي التلفزيون", "#on"},
	{"اشغل التلفزيون", "#on"}, {"تشغل التلفزيون", "#on"}, {"تشغلين التلفزيون", "#on"}, {"تشغلي التلفزيون", "#on"},
	{"افتح التلفزيون", "#on"}, {"افتحي التلفزيون", "#on"}, {"شغل التلفاز", "#on"}, {"شغلي التلفاز", "#on"},
	{"شغل الشاشه", "#on"}, {"شغلي الشاشه", "#on"}, {"turn on the tv", "#on"}, {"turn on tv", "#on"},
	{"طف التلفزيون", "#off"}, {"طفي التلفزيون", "#off"}, {"اطف التلفزيون", "#off"}, {"اطفي التلفزيون", "#off"},
	{"طف لي التلفزيون", "#off"}, {"طفي لي التلفزيون", "#off"}, {"تطفي التلفزيون", "#off"}, {"تطفين التلفزيون", "#off"},
	{"سكر التلفزيون", "#off"}, {"سكري التلفزيون", "#off"}, {"اقفل التلفزيون", "#off"}, {"اقفلي التلفزيون", "#off"},
	{"طف الشاشه", "#off"}, {"طفي الشاشه", "#off"}, {"turn off the tv", "#off"}, {"turn off tv", "#off"},
	// open an app, any mode (the TV is switched on first when it is off)
	{"شغل يوتيوب", "#yt"}, {"شغلي يوتيوب", "#yt"}, {"شغل لي يوتيوب", "#yt"}, {"شغلي لي يوتيوب", "#yt"},
	{"شغل اليوتيوب", "#yt"}, {"شغلي اليوتيوب", "#yt"}, {"افتح يوتيوب", "#yt"}, {"افتحي يوتيوب", "#yt"},
	{"افتح لي يوتيوب", "#yt"}, {"افتحي لي يوتيوب", "#yt"}, {"افتح اليوتيوب", "#yt"}, {"افتحي اليوتيوب", "#yt"},
	{"شغل تطبيق يوتيوب", "#yt"}, {"شغلي تطبيق يوتيوب", "#yt"}, {"افتح تطبيق يوتيوب", "#yt"}, {"افتحي تطبيق يوتيوب", "#yt"},
	{"تشغلين يوتيوب", "#yt"},
	{"شغل نتفلكس", "#nf"}, {"شغلي نتفلكس", "#nf"}, {"شغل لي نتفلكس", "#nf"}, {"شغلي لي نتفلكس", "#nf"},
	{"شغل نتفليكس", "#nf"}, {"شغلي نتفليكس", "#nf"}, {"افتح نتفلكس", "#nf"}, {"افتحي نتفلكس", "#nf"},
	{"افتح لي نتفلكس", "#nf"}, {"افتحي لي نتفلكس", "#nf"}, {"افتح نتفليكس", "#nf"}, {"افتحي نتفليكس", "#nf"},
	{"شغل تطبيق نتفلكس", "#nf"}, {"شغلي تطبيق نتفلكس", "#nf"}, {"افتح تطبيق نتفلكس", "#nf"}, {"افتحي تطبيق نتفلكس", "#nf"},
	{"شغل تطبيق نتفليكس", "#nf"}, {"شغلي تطبيق نتفليكس", "#nf"}, {"افتح تطبيق نتفليكس", "#nf"}, {"افتحي تطبيق نتفليكس", "#nf"},
	{"تشغلين نتفلكس", "#nf"},
	{"شغل شاهد", "#shahid"}, {"شغلي شاهد", "#shahid"}, {"شغل لي شاهد", "#shahid"}, {"شغلي لي شاهد", "#shahid"},
	{"افتح شاهد", "#shahid"}, {"افتحي شاهد", "#shahid"}, {"افتح لي شاهد", "#shahid"}, {"افتحي لي شاهد", "#shahid"},
	{"شغل تطبيق شاهد", "#shahid"}, {"شغلي تطبيق شاهد", "#shahid"}, {"افتح تطبيق شاهد", "#shahid"}, {"افتحي تطبيق شاهد", "#shahid"},
	{"تشغلين شاهد", "#shahid"},
	// remote mode
	{"وضع الريموت", "#remote"}, {"شغل الريموت", "#remote"}, {"شغلي الريموت", "#remote"},
	{"تحكم بالتلفزيون", "#remote"}, {"تحكمي بالتلفزيون", "#remote"}, {"remote mode", "#remote"},
	{"خلاص الريموت", "#remoteoff"}, {"اطلع من الريموت", "#remoteoff"}, {"اطلعي من الريموت", "#remoteoff"},
	{"سكر الريموت", "#remoteoff"}, {"سكري الريموت", "#remoteoff"}, {"وقف الريموت", "#remoteoff"}, {"وقفي الريموت", "#remoteoff"},
	// HDMI input switching (Huawei Vision TV -> fullscreen, no menu), works in ANY mode!
	{"حول على الكاميرات", "#hdmi_cam"}, {"حولي على الكاميرات", "#hdmi_cam"},
	{"حول للكاميرات", "#hdmi_cam"}, {"حولي للكاميرات", "#hdmi_cam"},
	{"روح للكاميرات", "#hdmi_cam"}, {"روحي للكاميرات", "#hdmi_cam"},
	{"افتح الكاميرات", "#hdmi_cam"}, {"افتحي الكاميرات", "#hdmi_cam"},
	{"شغل الكاميرات", "#hdmi_cam"}, {"شغلي الكاميرات", "#hdmi_cam"},
	{"وريني الكاميرات", "#hdmi_cam"}, {"ورني الكاميرات", "#hdmi_cam"},
	{"حول اتش دي ام اي 2", "#hdmi_cam"}, {"حولي اتش دي ام اي 2", "#hdmi_cam"},
	{"حول اتش دي ام اي اثنين", "#hdmi_cam"}, {"حولي اتش دي ام اي اثنين", "#hdmi_cam"},
	{"حول hdmi 2", "#hdmi_cam"}, {"حولي hdmi 2", "#hdmi_cam"}, {"hdmi 2", "#hdmi_cam"},
	{"رجع على الشاومي", "#hdmi_mi"}, {"رجعي على الشاومي", "#hdmi_mi"},
	{"ارجع للشاومي", "#hdmi_mi"}, {"ارجعي للشاومي", "#hdmi_mi"},
	{"حول على الشاومي", "#hdmi_mi"}, {"حولي على الشاومي", "#hdmi_mi"},
	{"حول للشاومي", "#hdmi_mi"}, {"حولي للشاومي", "#hdmi_mi"},
	{"روح للشاومي", "#hdmi_mi"}, {"روحي للشاومي", "#hdmi_mi"},
	{"افتح الشاومي", "#hdmi_mi"}, {"افتحي الشاومي", "#hdmi_mi"},
	{"شغل الشاومي", "#hdmi_mi"}, {"شغلي الشاومي", "#hdmi_mi"},
	{"رجع على التلفزيون", "#hdmi_mi"}, {"رجعي على التلفزيون", "#hdmi_mi"},
	{"حول اتش دي ام اي 3", "#hdmi_mi"}, {"حولي اتش دي ام اي 3", "#hdmi_mi"},
	{"حول اتش دي ام اي ثلاث", "#hdmi_mi"}, {"حولي اتش دي ام اي ثلاث", "#hdmi_mi"},
	{"حول اتش دي ام اي ثلاثه", "#hdmi_mi"}, {"حولي اتش دي ام اي ثلاثه", "#hdmi_mi"},
	{"حول hdmi 3", "#hdmi_mi"}, {"حولي hdmi 3", "#hdmi_mi"}, {"hdmi 3", "#hdmi_mi"},
	{"حول اتش دي ام اي 1", "#hdmi_1"}, {"حولي اتش دي ام اي 1", "#hdmi_1"}, {"hdmi 1", "#hdmi_1"},
	// explicit Home button phrases that work in ANY mode (and enter remote mode)
	{"روح للهوم", "#home_any"}, {"روحي للهوم", "#home_any"},
	{"ارجع للهوم", "#home_any"}, {"ارجعي للهوم", "#home_any"},
	{"روح للرئيسيه", "#home_any"}, {"روحي للرئيسيه", "#home_any"},
	{"ارجع للرئيسيه", "#home_any"}, {"ارجعي للرئيسيه", "#home_any"},
	{"زر الهوم", "#home_any"}, {"زر الرئيسيه", "#home_any"},
	// explicit mute/volume/media phrases that work in ANY mode (and turn remote mode on)
	{"حط ميوت", "#mute_any"}, {"حطي ميوت", "#mute_any"}, {"سكت التلفزيون", "#mute_any"}, {"سكتي التلفزيون", "#mute_any"},
	{"كتم التلفزيون", "#mute_any"}, {"اكتم التلفزيون", "#mute_any"}, {"اكتمي التلفزيون", "#mute_any"},
	{"شيل الميوت", "#unmute_any"}, {"شيلي الميوت", "#unmute_any"}, {"فك الميوت", "#unmute_any"}, {"فكي الميوت", "#unmute_any"},
	{"وقف المقطع", "#pause_any"}, {"وقفي المقطع", "#pause_any"}, {"وقف الفيديو", "#pause_any"}, {"وقفي الفيديو", "#pause_any"},
	{"كمل المقطع", "#play_any"}, {"كملي المقطع", "#play_any"}, {"كمل الفيديو", "#play_any"}, {"كملي الفيديو", "#play_any"},
	{"شغل المقطع", "#play_any"}, {"شغلي المقطع", "#play_any"}, {"شغل الفيديو", "#play_any"}, {"شغلي الفيديو", "#play_any"},
	{"المقطع الجاي", "#next_any"}, {"الفيديو الجاي", "#next_any"}, {"المقطع اللي قبل", "#prev_any"}, {"الفيديو اللي قبل", "#prev_any"},
	{"قدم المقطع", "#ff_any"}, {"قدمي المقطع", "#ff_any"}, {"رجع المقطع", "#rw_any"}, {"رجعي المقطع", "#rw_any"},
	// center/OK and play-pause descriptions (remote mode)
	{"على الزر اللي في النص", "#select"}, {"على الزر اللي بالنص", "#select"},
	{"على اللي في النص", "#select"}, {"على اللي بالنص", "#select"},
	{"الزر اللي في النص", "#select"}, {"الزر اللي بالنص", "#select"}, {"اللي في النص", "#select"}, {"اللي بالنص", "#select"},
	{"اضغط اللي في النص", "#select"}, {"اضغطي اللي في النص", "#select"}, {"اضغط اللي بالنص", "#select"}, {"اضغطي اللي بالنص", "#select"},
	{"اضغط على اللي في النص", "#select"}, {"اضغطي على اللي في النص", "#select"}, {"اضغط على الزر اللي في النص", "#select"}, {"اضغطي على الزر اللي في النص", "#select"},
	{"على زر الوسط", "#select"}, {"زر الوسط", "#select"}, {"الوسط", "#select"},
	{"على الزر اللي يوقف المقطع", "#playpause"}, {"على زر اللي يوقف المقطع", "#playpause"},
	{"الزر اللي يوقف المقطع", "#playpause"}, {"زر اللي يوقف المقطع", "#playpause"},
	{"اضغط على زر اللي يوقف المقطع", "#playpause"}, {"اضغطي على زر اللي يوقف المقطع", "#playpause"},
	{"على زر التوقيف", "#playpause"}, {"زر التوقيف", "#playpause"}, {"زر الايقاف", "#playpause"},
	// navigation helpers (explicit verb + direction auto-enters remote mode too!)
	{"اطلع فوق", "#up_any"}, {"اطلعي فوق", "#up_any"}, {"روح فوق", "#up_any"}, {"روحي فوق", "#up_any"}, {"ارقى فوق", "#up_any"}, {"ارقي فوق", "#up_any"},
	{"انزل تحت", "#down_any"}, {"انزلي تحت", "#down_any"}, {"نزل تحت", "#down_any"}, {"نزلي تحت", "#down_any"}, {"روح تحت", "#down_any"}, {"روحي تحت", "#down_any"},
	{"روح يمين", "#right_any"}, {"روحي يمين", "#right_any"}, {"روح يسار", "#left_any"}, {"روحي يسار", "#left_any"},
	// volume
	{"ارفع الصوت", "#volup"}, {"ارفعي الصوت", "#volup"}, {"ارفع لي الصوت", "#volup"}, {"ارفعي لي الصوت", "#volup"}, {"علي الصوت", "#volup"}, {"علي لي الصوت", "#volup"}, {"زيد الصوت", "#volup"}, {"زيدي الصوت", "#volup"},
	{"وطي الصوت", "#voldown"}, {"وط الصوت", "#voldown"}, {"وطي لي الصوت", "#voldown"}, {"وط لي الصوت", "#voldown"}, {"نزل لي الصوت", "#voldown"}, {"نزل الصوت", "#voldown"}, {"نزلي الصوت", "#voldown"},
	{"قصر الصوت", "#voldown"}, {"قصري الصوت", "#voldown"}, {"خفض الصوت", "#voldown"}, {"خففي الصوت", "#voldown"}, {"خفف الصوت", "#voldown"},
	{"اكتم الصوت", "#mute"}, {"اكتمي الصوت", "#mute"}, {"كتم الصوت", "#mute"},
	{"رجع الصوت", "#unmute"}, {"رجعي الصوت", "#unmute"}, {"ارجع الصوت", "#unmute"}, {"ارجعي الصوت", "#unmute"},
	// fillers of more than one word
	{"لو سمحت", ""}, {"لو سمحتي", ""}, {"الله يخليك", ""}, {"الله يسعدك", ""}, {"مره ثانيه", ""}, {"كمان مره", ""},
}

// One-word commands in remote mode.
var remoteWords = map[string]remoteCmd{}

// Words that say nothing on their own and may sit around a command. A word that is none of
// command, count or filler makes the utterance conversation: nothing is pressed.
var remoteFillers = map[string]bool{}

func init() {
	key := func(k string) remoteCmd { return remoteCmd{"key", map[string]any{"key": k}} }
	app := func(a string) remoteCmd { return remoteCmd{"launch", map[string]any{"app": a}} }
	// Every entry goes through the same normalisation as what he says ("الرئيسية" is matched as
	// "الرييسيه"), so the lists can be written the way people spell them.
	add := func(c remoteCmd, words ...string) {
		for _, w := range words {
			remoteWords[normaliseRemote(w)] = c
		}
	}
	for i := range remotePhraseTable {
		remotePhraseTable[i].from = normaliseRemote(remotePhraseTable[i].from)
	}
	sort.SliceStable(remotePhraseTable, func(i, j int) bool {
		return len([]rune(remotePhraseTable[i].from)) > len([]rune(remotePhraseTable[j].from))
	})
	for _, w := range []string{"سول", "sol", "أسترا", "استرا", "astra", "يا", "طيب", "يلا", "زين", "مرات", "مره", "و", "ثم",
		"please", "شوي", "شويه", "لي", "ممكن", "ابي", "ابغى", "ابغي", "تمام", "الحين", "دحين", "هالحين", "now"} {
		remoteFillers[normaliseRemote(w)] = true
	}
	add(key("up"), "فوق", "لفوق", "اعلى", "اطلع", "اطلعي", "up", "#up")
	add(key("down"), "تحت", "لتحت", "اسفل", "انزل", "انزلي", "نزل", "نزلي", "down", "#down")
	add(key("right"), "يمين", "يمينه", "لليمين", "اليمين", "right", "#right")
	add(key("left"), "يسار", "يساره", "لليسار", "اليسار", "شمال", "الشمال", "left", "#left")
	// Not "اوكي"/"ok": he says it to her all the time to mean "fine" (Grok).
	add(key("select"), "اضغط", "اضغطي", "اكبس", "اكبسي", "دوس", "دوسي", "اختار", "اختاري", "اختر", "انتر", "enter", "select", "بالنص", "#select")
	add(key("back"), "ارجع", "ارجعي", "رجوع", "باك", "back")
	add(key("home"), "الرئيسيه", "رئيسيه", "هوم", "الهوم", "home")
	add(key("menu"), "القائمه", "قائمه", "منيو", "menu")
	add(key("settings"), "الاعدادات", "اعدادات", "settings")
	add(key("captions"), "الترجمه", "ترجمه", "captions")
	add(key("fast_forward"), "قدم", "قدمي", "تقديم", "قدام")
	add(key("rewind"), "ترجيع", "rewind")
	add(key("next"), "التالي", "الجاي", "next")
	add(key("previous"), "السابق", "previous")
	add(remoteCmd{"pause", nil}, "وقف", "وقفي", "ايقاف", "بوز", "pause")
	add(remoteCmd{"play", nil}, "كمل", "كملي", "بلاي", "play")
	add(remoteCmd{"play_pause", nil}, "#playpause")
	add(app("youtube"), "يوتيوب", "اليوتيوب", "youtube")
	add(app("netflix"), "نتفلكس", "نتفليكس", "النتفلكس", "netflix")
	add(app("shahid"), "شاهد", "shahid")
	add(remoteCmd{"switch_input", map[string]any{"input": "cameras"}}, "الكاميرات", "كاميرات")
	add(remoteCmd{"switch_input", map[string]any{"input": "xiaomi"}}, "الشاومي", "شاومي")
	remoteWords["#volup"] = remoteCmd{"volume_step", map[string]any{"steps": 1}}
	remoteWords["#voldown"] = remoteCmd{"volume_step", map[string]any{"steps": -1}}
	remoteWords["#mute"] = remoteCmd{"set_mute", map[string]any{"mute": true}}
	remoteWords["#unmute"] = remoteCmd{"set_mute", map[string]any{"mute": false}}
	remoteWords["ميوت"] = remoteCmd{"set_mute", map[string]any{"mute": true}}
	remoteWords["mute"] = remoteCmd{"set_mute", map[string]any{"mute": true}}
}

// "يمين ثلاث مرات" / "تحت 5 مرات" presses repeatedly (1..10).
var remoteCounts = map[string]int{
	"واحد": 1, "واحده": 1, "1": 1, "١": 1,
	"مرتين": 2, "ثنتين": 2, "اثنين": 2, "2": 2, "٢": 2,
	"ثلاث": 3, "ثلاثه": 3, "3": 3, "٣": 3,
	"اربع": 4, "اربعه": 4, "4": 4, "٤": 4,
	"خمس": 5, "خمسه": 5, "5": 5, "٥": 5,
	"ست": 6, "سته": 6, "6": 6, "٦": 6,
	"سبع": 7, "سبعه": 7, "7": 7, "٧": 7,
	"ثمان": 8, "ثمانيه": 8, "8": 8, "٨": 8,
	"تسع": 9, "تسعه": 9, "9": 9, "٩": 9,
	"عشر": 10, "عشره": 10, "10": 10, "١٠": 10,
}

// normaliseRemote folds spelling variants the way the farewell matcher does (harakat, alef forms,
// ة, ى, ؤ, ئ, punctuation, case) so one vocabulary entry covers every way it is written.
func normaliseRemote(s string) string {
	var b strings.Builder
	lastSpace := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 0x064B && r <= 0x0652, r == 0x0640, r == 0x0670:
			continue
		case unicode.IsSpace(r):
			if !lastSpace {
				b.WriteRune(' ')
				lastSpace = true
			}
			continue
		case r == 'أ' || r == 'إ' || r == 'آ' || r == 'ٱ':
			r = 'ا'
		case r == 'ة':
			r = 'ه'
		case r == 'ى':
			r = 'ي'
		case r == 'ؤ':
			r = 'و'
		case r == 'ئ':
			r = 'ي'
		case r == '#':
			// kept: the internal tokens
		case unicode.IsPunct(r) || unicode.IsSymbol(r):
			if !lastSpace {
				b.WriteRune(' ')
				lastSpace = true
			}
			continue
		}
		b.WriteRune(r)
		lastSpace = false
	}
	return strings.TrimSpace(b.String())
}

// remoteParse is what one utterance means to the remote.
type remoteParse struct {
	powerOn, powerOff bool
	enter, exit       bool
	cmds              []remoteCmd
	// pureCommand: every word was a command, count or filler, and there was at least one
	// command or mode word. Only a pure utterance acts.
	pureCommand bool
}

// parseRemote reads one utterance. active is whether remote mode is on before it.
func parseRemote(utter string, active bool) remoteParse {
	n := normaliseRemote(utter)
	if n == "" {
		return remoteParse{}
	}
	rest := applyPhrases(" " + n + " ")
	// Second pass without the single-word fillers, so one inside a phrase ("شغّل لو سمحت
	// التلفزيون") does not hide it (Grok). Fillers carry no meaning, so dropping them is safe.
	var kept []string
	for _, w := range strings.Fields(rest) {
		if !remoteFillers[w] {
			kept = append(kept, w)
		}
	}
	rest = applyPhrases(" " + strings.Join(kept, " ") + " ")
	tokens := collapsePressVerb(strings.Fields(rest))
	var p remoteParse
	var cmds []remoteCmd
	acted := false
	pending := 0 // a count said before its key: "ثلاث مرات يمين" (Grok)
	for _, w := range tokens {
		switch w {
		case "#on":
			p.powerOn, acted = true, true
			cmds = append(cmds, remoteCmd{"power_on", nil})
			continue
		case "#off":
			p.powerOff, acted = true, true
			cmds = append(cmds, remoteCmd{"power_off", nil})
			continue
		case "#remote":
			p.enter, acted = true, true
			continue
		case "#remoteoff":
			p.exit, acted = true, true
			continue
		case "#yt", "#nf", "#shahid":
			app := "youtube"
			switch w {
			case "#nf":
				app = "netflix"
			case "#shahid":
				app = "shahid"
			}
			cmds = append(cmds, remoteCmd{"launch", map[string]any{"app": app}})
			p.enter, acted = true, true // like turning the TV on: remote mode follows
			continue
		case "#hdmi_cam":
			cmds = append(cmds, remoteCmd{"switch_input", map[string]any{"input": "cameras"}})
			p.enter, acted = true, true
			continue
		case "#hdmi_mi":
			cmds = append(cmds, remoteCmd{"switch_input", map[string]any{"input": "xiaomi"}})
			p.enter, acted = true, true
			continue
		case "#hdmi_1":
			cmds = append(cmds, remoteCmd{"switch_input", map[string]any{"input": "hdmi1"}})
			p.enter, acted = true, true
			continue
		case "#home_any":
			cmds = append(cmds, remoteCmd{"key", map[string]any{"key": "home"}})
			p.enter, acted = true, true
			continue
		case "#up_any", "#down_any", "#right_any", "#left_any":
			k := strings.TrimSuffix(strings.TrimPrefix(w, "#"), "_any")
			c := remoteCmd{"key", map[string]any{"key": k}}
			cmds = append(cmds, c)
			if pending > 0 {
				for i := 1; i < pending; i++ {
					cmds = append(cmds, c)
				}
				pending = 0
			}
			if !active {
				p.enter = true
			}
			acted = true
			continue
		case "#mute_any":
			cmds = append(cmds, remoteCmd{"set_mute", map[string]any{"mute": true}})
			p.enter, acted = true, true
			continue
		case "#unmute_any":
			cmds = append(cmds, remoteCmd{"set_mute", map[string]any{"mute": false}})
			p.enter, acted = true, true
			continue
		case "#pause_any":
			cmds = append(cmds, remoteCmd{"pause", nil})
			p.enter, acted = true, true
			continue
		case "#play_any":
			cmds = append(cmds, remoteCmd{"play", nil})
			p.enter, acted = true, true
			continue
		case "#next_any":
			cmds = append(cmds, remoteCmd{"key", map[string]any{"key": "next"}})
			p.enter, acted = true, true
			continue
		case "#prev_any":
			cmds = append(cmds, remoteCmd{"key", map[string]any{"key": "previous"}})
			p.enter, acted = true, true
			continue
		case "#ff_any":
			cmds = append(cmds, remoteCmd{"key", map[string]any{"key": "fast_forward"}})
			p.enter, acted = true, true
			continue
		case "#rw_any":
			cmds = append(cmds, remoteCmd{"key", map[string]any{"key": "rewind"}})
			p.enter, acted = true, true
			continue
		case "خلاص":
			if !active {
				return remoteParse{} // outside remote mode it is just conversation
			}
			p.exit, acted = true, true // "خلاص", "خلاص يا سول"
			continue
		}
		if c, ok := remoteWords[w]; ok {
			cmds, acted = append(cmds, c), true
			if pending > 0 {
				if !isRepeatable(c) {
					return remoteParse{}
				}
				for i := 1; i < pending; i++ {
					cmds = append(cmds, c)
				}
				pending = 0
			}
			continue
		}
		if k, ok := remoteCounts[w]; ok && pending == 0 && (len(cmds) == 0 || isPower(cmds[len(cmds)-1])) {
			pending = k // "ثلاث مرات يمين", "شغل التلفزيون ثلاث مرات يمين"
			continue
		}
		if k, ok := remoteCounts[w]; ok && len(cmds) > 0 {
			last := cmds[len(cmds)-1]
			if !isRepeatable(last) {
				return remoteParse{} // "يوتيوب ثلاث": a count only repeats keys, volume, and media buttons
			}
			for i := 1; i < k; i++ {
				cmds = append(cmds, last)
			}
			continue
		}
		if remoteFillers[w] {
			continue
		}
		// "بعدين" / "وبعدين" AT THE START means "later" ("بعدين شغّل التلفزيون") and is rejected;
		// BETWEEN commands ("اضغط بعدين نزلي تحت 5 مرات") it means "then" and chains cleanly.
		if (w == "بعدين" || w == "وبعدين") && len(cmds) > 0 && pending == 0 {
			continue
		}
		return remoteParse{} // a word that is none of those: this was conversation
	}
	if !acted || pending > 0 || (p.powerOn && p.powerOff) {
		return remoteParse{} // only fillers, or on and off in one breath
	}
	// Multi-button pure navigation sequences ("فوق تحت يمين اضغط", "تحت 5 مرات") are unmistakably
	// TV remote commands even on the very first utterance before saying "وضع الريموت".
	if !active && !p.enter && len(cmds) >= 2 {
		allKeys := true
		for _, c := range cmds {
			if c.action != "key" {
				allKeys = false
				break
			}
		}
		if allKeys {
			p.enter = true
		}
	}
	p.enter = p.enter || p.powerOn
	p.exit = p.exit || p.powerOff
	// Keys need remote mode: on before this utterance, or switched on in it. Power actions do not.
	for _, c := range cmds {
		if c.action != "power_on" && c.action != "power_off" && !active && !p.enter {
			return remoteParse{} // keys outside remote mode are not for the remote
		}
	}
	// Everything runs in the order said, then the mode changes: "يمين خلاص" presses right and
	// leaves; "ارفع الصوت طف التلفزيون" turns it up, then off (Grok final).
	p.cmds = capPresses(cmds)
	p.pureCommand = true
	return p
}

// ---- the per-bridge remote ---------------------------------------------------------------------

type remote struct {
	tv tvDoer
	ev func(string, ...any)

	mu sync.Mutex
	// gen is the CALL token: Begin and End bump it, and Feed, flush and queued work all carry the
	// token of the call they belong to, so nothing from one call can act in the next.
	gen uint64
	// seq numbers fragments; a quiet timer only reads the words if no newer fragment arrived
	// (Timer.Stop does not stop a callback that has already started).
	seq  uint64
	raw  string
	echo bool // some fragment of the words waiting to be read arrived while she was speaking
	// overflow: the words waiting to be read outgrew remoteTail and lost their beginning, which
	// may have been what made them conversation (Codex). Such an utterance never acts.
	overflow    bool
	prefixFired bool      // a leading app/HDMI command already started while the speaker keeps talking
	firstAt     time.Time // when the first fragment of the words waiting to be read arrived
	// chainConv/chainAt: utterances that follow each other within remoteLookback form a chain;
	// chainConv says whether ANY of them was conversation (a word that is no command, count or
	// filler, or an overflow). A power or remote-mode phrase in a conversational chain does not act,
	// so pauses inside "شلون … يا سول … أطفي التلفزيون" cannot turn its end into an order. A flag,
	// not the text: truncating text could lose the "لا" that made it conversation (Codex).
	chainConv bool
	chainNeg  bool // explicit negation/question word or overflow in chain (blocks even in remote mode)
	chainAt   time.Time
	timer     *time.Timer
	active    bool
	lastCmd   time.Time // last command or mode change, for remoteIdle
	// handled is the normalised utterance that last queued presses, and when: only a delegation
	// about those same words is answered silently (handles).
	handled   string
	handledAt time.Time
	speaking  func() bool
	say       func(string)
	lastSaid  time.Time // last spoken complaint, so a broken TV is not announced on every press
	ctx       context.Context
	cancel    context.CancelFunc

	jobs chan remoteJob
}

type remoteJob struct {
	gen   uint64
	ctx   context.Context // the call's: cancelled when the call ends, which aborts a press in flight
	cmds  []remoteCmd
	heard time.Time // when the utterance was read, for end-to-end timing
}

func newRemote(c tvDoer, ev func(string, ...any)) *remote {
	if ev == nil {
		ev = func(string, ...any) {}
	}
	r := &remote{tv: c, ev: ev, jobs: make(chan remoteJob, 16)}
	go r.run()
	return r
}

// clearLocked forgets the call in progress; the caller holds r.mu.
func (r *remote) clearLocked() {
	r.gen++
	r.raw, r.echo, r.overflow, r.prefixFired, r.active, r.speaking, r.say = "", false, false, false, false, nil, nil
	r.lastCmd, r.firstAt, r.chainAt = time.Time{}, time.Time{}, time.Time{}
	r.chainConv, r.chainNeg, r.handled, r.handledAt = false, false, "", time.Time{}
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	if r.cancel != nil {
		r.cancel()
	}
	r.ctx, r.cancel = nil, nil
}

// Begin starts a call: remote mode off, nothing pending, nothing queued from before, the echo
// judge and the voice for complaints installed. The returned token goes with every Feed.
func (r *remote) Begin(speaking func() bool, say func(string)) uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearLocked()
	r.speaking, r.say = speaking, say
	r.ctx, r.cancel = context.WithCancel(context.Background())
	return r.gen
}

// Activate switches remote mode on (e.g. after Tier 2 tv_control ran a command) so subsequent
// short words like يمين / تحت / اضغط work instantly without latency.
func (r *remote) Activate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.ctx != nil {
		r.active = true
		r.lastCmd = time.Now()
	}
	r.mu.Unlock()
}

// End stops everything for the call that is ending, including a press in flight. Called BEFORE
// the hangup, which can take seconds.
func (r *remote) End() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearLocked()
}

// instantNavWords are unambiguous single-word navigation buttons that can fire LIVE mid-stream
// in remote mode as soon as the NEXT navigation word arrives (zero waiting for sentence end!).
var instantNavWords = map[string]bool{
	"فوق": true, "لفوق": true, "اعلى": true, "اطلع": true, "اطلعي": true, "up": true,
	"تحت": true, "لتحت": true, "اسفل": true, "انزل": true, "انزلي": true, "نزل": true, "نزلي": true, "down": true,
	"يمين": true, "لليمين": true, "right": true,
	"يسار": true, "لليسار": true, "شمال": true, "left": true,
	"اضغط": true, "اضغطي": true, "اكبس": true, "اكبسي": true, "دوس": true, "دوسي": true, "انتر": true, "select": true, "enter": true,
	"ارجع": true, "ارجعي": true, "رجوع": true, "باك": true, "back": true,
	"هوم": true, "الهوم": true, "الرئيسيه": true, "home": true,
	"وقف": true, "وقفي": true, "كمل": true, "كملي": true, "ميوت": true,
}

// leadingPrefixCmd checks whether utter starts (after optional fillers) with a safe, idempotent
// app/HDMI/home command followed by additional speech (e.g. "افتحي نتفلكس وشوفي آخر فلم شاهدناه"),
// so that the leading command begins executing IMMEDIATELY while the speaker finishes the rest of
// their sentence. Never fires if preceded or immediately followed by negation/doubt words.
func leadingPrefixCmd(utter string) (remoteCmd, bool) {
	n := normaliseRemote(utter)
	if n == "" || hasRemoteBlockerWord(n) {
		return remoteCmd{}, false
	}
	rest := applyPhrases(" " + n + " ")
	var kept []string
	for _, w := range strings.Fields(rest) {
		if !remoteFillers[w] {
			kept = append(kept, w)
		}
	}
	if len(kept) < 2 {
		return remoteCmd{}, false // needs trailing words; pure utterances are handled normally by flush()
	}
	// Apply phrases once more after dropping fillers in case a filler sat inside the opening phrase.
	kept = strings.Fields(applyPhrases(" " + strings.Join(kept, " ") + " "))
	if len(kept) < 2 {
		return remoteCmd{}, false
	}
	// Never fire on doubt/deferral continuations like "ولا لا" or "بعد شوي".
	switch kept[1] {
	case "ولا", "او", "بعد", "اذا", "لو", "لا", "ما", "مو", "مش", "بس":
		return remoteCmd{}, false
	}
	switch kept[0] {
	case "#yt":
		return remoteCmd{"launch", map[string]any{"app": "youtube"}}, true
	case "#nf":
		return remoteCmd{"launch", map[string]any{"app": "netflix"}}, true
	case "#shahid":
		return remoteCmd{"launch", map[string]any{"app": "shahid"}}, true
	case "#hdmi_cam":
		return remoteCmd{"switch_input", map[string]any{"input": "cameras"}}, true
	case "#hdmi_mi":
		return remoteCmd{"switch_input", map[string]any{"input": "xiaomi"}}, true
	case "#home_any":
		return remoteCmd{"key", map[string]any{"key": "home"}}, true
	default:
		return remoteCmd{}, false
	}
}

// Feed takes one fragment of his transcript for the call tok. The words are read remoteQuiet
// after the last fragment. In active remote mode, when a rapid stream of navigation words arrives
// across fragments ("فوق" -> "تحت" -> "يمين" -> "اضغط"), completed leading nav words fire IMMEDIATELY
// without waiting for the speaker to finish the whole string. Also, if a longer spoken request starts
// with a safe app/HDMI action ("افتحي نتفلكس وشوفي آخر فلم شاهدناه"), that opening action starts
// immediately in parallel while the speaker continues talking.
func (r *remote) Feed(tok uint64, text string) {
	if r == nil || strings.TrimSpace(text) == "" {
		return
	}
	var (
		streamCmds []remoteCmd
		streamCtx  context.Context
		streamNow  time.Time
		streamEv   string
	)
	r.mu.Lock()
	if tok != r.gen || r.ctx == nil {
		r.mu.Unlock()
		return // a fragment from a call that has ended
	}
	// Judged as it ARRIVES: by the time the words are read she may have just stopped, and her
	// own "يمين" would then pass for his.
	if r.speaking != nil && r.speaking() {
		r.echo = true
	}
	hadPrev := strings.TrimSpace(r.raw) != ""
	if r.raw == "" {
		r.firstAt = time.Now()
	}
	r.raw += text
	if rs := []rune(r.raw); len(rs) > remoteTail {
		r.raw = string(rs[len(rs)-remoteTail:])
		r.overflow = true
	}
	now := time.Now()
	// 1. Streaming live-fire in active remote mode: if earlier fragments already formed complete
	// standalone nav words and this new fragment starts another nav word (not a repeat count or
	// phrase continuation), fire the leading nav words right now with 0 ms wait!
	if hadPrev && r.active && !r.echo && !r.overflow && !r.chainNeg {
		if now.Sub(r.lastCmd) <= remoteIdle {
			words := strings.Fields(normaliseRemote(r.raw))
			if len(words) >= 2 {
				allInstant := true
				for _, w := range words {
					if !instantNavWords[w] {
						allInstant = false
						break
					}
				}
				if allInstant {
					// Fire all words except the trailing one (which might still get a count like "5 مرات").
					for _, w := range words[:len(words)-1] {
						if c, ok := remoteWords[w]; ok {
							streamCmds = append(streamCmds, c)
						}
					}
					if len(streamCmds) > 0 {
						streamCtx = r.ctx
						streamNow = now
						tail := words[len(words)-1]
						if len(text) > 0 && unicode.IsSpace(rune(text[len(text)-1])) {
							tail += " "
						}
						r.raw = tail
						r.lastCmd = now
						r.chainConv, r.chainNeg, r.chainAt = false, false, now
						r.handled, r.handledAt = normaliseRemote(strings.Join(words, " ")), now
						streamEv = "remote_stream_fired"
					}
				}
			}
		}
	}
	// 2. Leading action prefix while the user keeps talking ("افتحي نتفلكس وشوفي آخر فلم..."):
	// start the opening app/HDMI switch immediately so it is already ready when the user finishes!
	if len(streamCmds) == 0 && !r.prefixFired && !r.echo && !r.overflow && !r.chainNeg {
		inChain := !r.chainAt.IsZero() && r.firstAt.Sub(r.chainAt) < remoteLookback
		if !inChain || !r.chainConv {
			if cmd, ok := leadingPrefixCmd(r.raw); ok {
				r.prefixFired = true
				r.active = true
				r.lastCmd = now
				streamCmds = []remoteCmd{cmd}
				streamCtx = r.ctx
				streamNow = now
				streamEv = "remote_prefix_fired"
			}
		}
	}
	r.seq++
	seq := r.seq
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timer = time.AfterFunc(remoteQuiet, func() { r.flush(tok, seq) })
	r.mu.Unlock()

	if streamEv != "" && len(streamCmds) > 0 {
		names := make([]string, len(streamCmds))
		for i, c := range streamCmds {
			names[i] = c.String()
		}
		select {
		case r.jobs <- remoteJob{gen: tok, ctx: streamCtx, cmds: streamCmds, heard: streamNow}:
			r.ev(streamEv, "cmds", names)
		default:
			r.ev("remote_dropped", "cmds", names, "note", "too many queued; the TV is not keeping up")
		}
	}
}

func (r *remote) flush(tok, seq uint64) {
	type line struct {
		ev   string
		args []any
	}
	var logs []line
	emit := func() {
		for _, l := range logs {
			r.ev(l.ev, l.args...)
		}
	}

	r.mu.Lock()
	if tok != r.gen || seq != r.seq || r.ctx == nil {
		r.mu.Unlock()
		return
	}
	utter, echo, overflow, prefixFired, firstAt, ctx := r.raw, r.echo, r.overflow, r.prefixFired, r.firstAt, r.ctx
	r.raw, r.echo, r.overflow, r.prefixFired, r.timer = "", false, false, false, nil
	now := time.Now()
	if r.active && now.Sub(r.lastCmd) > remoteIdle {
		r.active = false
		logs = append(logs, line{"remote_mode", []any{"on", false, "why", "idle"}})
	}
	if echo || (r.speaking != nil && r.speaking()) {
		r.mu.Unlock()
		if parseRemote(utter, true).pureCommand {
			logs = append(logs, line{"remote_echo_ignored", []any{"note", "heard while she was speaking; her own voice is not his command"}})
		}
		emit()
		return
	}
	// The chain continues when this utterance began soon after the previous one was read.
	inChain := !r.chainAt.IsZero() && firstAt.Sub(r.chainAt) < remoteLookback
	if overflow {
		r.chainConv, r.chainNeg, r.chainAt = true, true, now // a long stretch of talk: nothing right after it is an order either
		r.mu.Unlock()
		logs = append(logs, line{"remote_overflow_ignored", []any{"note", "a long utterance lost its start; it never acts"}})
		emit()
		return
	}
	p := parseRemote(utter, r.active)
	if prefixFired && p.pureCommand && len(p.cmds) == 1 {
		// That single opening command already fired via remote_prefix_fired; do not run it twice.
		p.cmds = nil
	}
	conv := isConversation(utter)
	neg := hasRemoteBlockerWord(utter)
	if inChain {
		conv = conv || r.chainConv
		neg = neg || r.chainNeg
	}
	r.chainConv, r.chainNeg, r.chainAt = conv, neg, now
	// Outside remote mode, any conversational chain blocks accidental power/app triggers.
	// Inside remote mode, background TV audio constantly arrives as transcript fragments, so only
	// explicit negation/question words ("لا", "شلون", "كيف", overflow) block a real remote press.
	blocked := conv
	if r.active && !p.powerOn && !p.powerOff {
		blocked = neg
	}
	if blocked && (len(p.cmds) > 0 || (p.enter && !r.active)) {
		r.mu.Unlock()
		logs = append(logs, line{"remote_lookback_blocked", []any{"note", "a TV command inside a stretch of conversation; not an order"}})
		emit()
		return
	}
	cmds := p.cmds
	if p.enter && !r.active {
		r.active = true
		logs = append(logs, line{"remote_mode", []any{"on", true, "why", "asked"}})
	}
	if p.exit && r.active {
		r.active = false
		logs = append(logs, line{"remote_mode", []any{"on", false, "why", "asked"}})
	}
	if p.pureCommand || len(cmds) > 0 {
		r.lastCmd = now
	}
	if p.exit && len(cmds) == 0 {
		r.handled = "" // after "خلاص" the same words are no longer a press
	}
	r.mu.Unlock()
	if len(cmds) > 0 {
		names := make([]string, len(cmds))
		for i, c := range cmds {
			names[i] = c.String()
		}
		select {
		case r.jobs <- remoteJob{gen: tok, ctx: ctx, cmds: cmds, heard: now}:
			logs = append(logs, line{"remote_heard", []any{"cmds", names}})
			// Only words that were actually queued silence a delegation about them (Codex).
			r.mu.Lock()
			if tok == r.gen {
				r.handled, r.handledAt = normaliseRemote(utter), now
			}
			r.mu.Unlock()
		default:
			logs = append(logs, line{"remote_dropped", []any{"cmds", names, "note", "too many queued; the TV is not keeping up"}})
		}
	}
	emit()
}

// run executes queued commands in order, one at a time, for the life of the process.
func (r *remote) run() {
	for job := range r.jobs {
		r.runJob(job)
	}
}

func (r *remote) runJob(job remoteJob) {
	defer func() {
		if p := recover(); p != nil {
			r.ev("remote_panic", "err", fmt.Sprint(p))
		}
	}()
	for i, c := range job.cmds {
		if i > 0 && isRepeatable(c) {
			// Tight pacing gap so the Xiaomi box registers rapid repeated presses cleanly.
			select {
			case <-time.After(55 * time.Millisecond):
			case <-job.ctx.Done():
				return
			}
		}
		r.mu.Lock()
		stale := job.gen != r.gen
		r.mu.Unlock()
		if stale || job.ctx.Err() != nil {
			r.ev("remote_stale", "note", "the call these were for has ended")
			return
		}
		t0 := time.Now()
		ctx, cancel := context.WithTimeout(job.ctx, budgetFor(c.action)+time.Second)
		res, err := r.tv.Do(ctx, c.action, c.args, budgetFor(c.action))
		if err == nil && !res.OK && c.action == "set_mute" && res.Error == "unknown" {
			// Box did not report mute state: fall back to direct VOLUME_MUTE toggle.
			res, err = r.tv.Do(ctx, "toggle_mute", nil, budgetFor("toggle_mute"))
		}
		cancel()
		errCode := res.Error
		if err != nil && errCode == "" {
			errCode = "transport"
		}
		r.ev("remote_cmd", "cmd", c.String(), "ok", err == nil && res.OK, "error", errCode,
			"tv_ms", res.MS, "ms", time.Since(t0).Milliseconds(), "since_heard_ms", time.Since(job.heard).Milliseconds())
		if err == nil && res.OK {
			continue
		}
		if job.ctx.Err() == nil {
			r.complain(errCode)
		}
		return // the rest of this utterance would act on a TV in an unknown state
	}
}

// complain says one short line when the TV did not do what he asked, at most every 20 s.
func (r *remote) complain(code string) {
	line := "ما قدرت أوصل للتلفزيون."
	if code == "off" {
		line = "التلفزيون طافي، قل شغّل التلفزيون."
	}
	r.mu.Lock()
	say := r.say
	recent := time.Since(r.lastSaid) < 20*time.Second
	if !recent {
		r.lastSaid = time.Now()
	}
	r.mu.Unlock()
	if say != nil && !recent {
		say(line)
	}
}

// handles reports whether a delegation's question is one the remote already acted on, so it is
// answered silently instead of by the router (which would say it cannot control a TV).
func (r *remote) handles(question string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	handled, at := r.handled, r.handledAt
	r.mu.Unlock()
	if handled == "" || time.Since(at) > 10*time.Second {
		return false
	}
	// The delegation's question is what he said since the last one; it is about the words the
	// remote acted on when it is exactly them, or ends with them (Grok: not "any command at all").
	q := normaliseRemote(question)
	return q == handled || strings.HasSuffix(q, " "+handled)
}

// tvPolicy is the line in her instructions that keeps her out of the remote's way. It names every
// kind of command the remote acts on, so she does not answer one (her answer would come back
// through the microphone as his next words).
func tvPolicy() string {
	return "التلفزيون (شاشة هواوي + الشاومي بوكس): عندك تحكم كامل وسريع جداً بثلاث طرق: " +
		"1) الأوامر القصيرة والسريعة للريموت (فوق، تحت، يمين، يسار، اضغط، ارجع، هوم، الرئيسية، خلاص، شغّل/طفّ التلفزيون، حطي ميوت، وقفي/كملي المقطع، ارفعي/وطي الصوت، حولي على الكاميرات، رجعي على الشاومي، شغلي يوتيوب/نتفلكس/شاهد) ينفّذها الجسر مباشرة في أجزاء من الثانية بصمت تام: ممنوع تماماً تردين عليها أو تقولين «تم» أو «حاضر»! اسكتي تماماً عشان المتحدث يقدر يقول فوق تحت يمين اضغط بسرعة ورا بعض بدون أي تأخير. " +
		"2) الأوامر المركّبة أو غير المعتادة للريموت: اطلبي المساعدة فوراً لتنفّذها أداة tv_control. " +
		"3) رؤية الشاشة والبحث والكتابة في نتفلكس ويوتيوب (مثل: وش على الشاشة، وين وصلنا في المسلسل، كملي المسلسل، دوري على فلم سبايدر مان الجديد، شغلي مقطع كذا في يوتيوب، اكتبي في البحث): اطلبي المساعدة فوراً لتنفّذها أداة tv_vision."
}

// applyPhrases rewrites every phrase of remotePhraseTable (longest first) in a space-padded string.
func applyPhrases(rest string) string {
	for _, ph := range remotePhraseTable {
		repl := " "
		if ph.to != "" {
			repl = " " + ph.to + " "
		}
		rest = strings.ReplaceAll(rest, " "+ph.from+" ", repl)
	}
	return rest
}

// isConversation reports whether an utterance has any word that is not a command, a count, a
// filler, a phrase token or "خلاص": such a word means he was talking, not giving an order.
func isConversation(utter string) bool {
	rest := applyPhrases(" " + normaliseRemote(utter) + " ")
	var kept []string
	for _, w := range strings.Fields(rest) {
		if !remoteFillers[w] {
			kept = append(kept, w)
		}
	}
	fields := strings.Fields(applyPhrases(" " + strings.Join(kept, " ") + " "))
	for i, w := range fields {
		if strings.HasPrefix(w, "#") || w == "خلاص" {
			continue
		}
		if _, ok := remoteWords[w]; ok {
			continue
		}
		if _, ok := remoteCounts[w]; ok {
			continue
		}
		if (w == "بعدين" || w == "وبعدين") && i > 0 {
			continue
		}
		return true
	}
	return false
}

// hasRemoteBlockerWord reports whether utter contains an explicit negation or question word
// (used in remote mode so background TV dialogue does not block genuine remote presses).
func hasRemoteBlockerWord(utter string) bool {
	blockers := map[string]bool{
		"لا": true, "ما": true, "مو": true, "مب": true, "مش": true,
		"شلون": true, "كيف": true, "ليش": true, "متي": true, "اذا": true, "لو": true,
		"no": true, "not": true, "dont": true, "how": true, "why": true,
	}
	for _, w := range strings.Fields(normaliseRemote(utter)) {
		if blockers[w] {
			return true
		}
	}
	return false
}

// collapsePressVerb handles natural Arabic phrasing like "اضغطي مرتين فوق" or
// "اضغطي مرتين على زر اللي يوقف المقطع" where "اضغط/اضغطي" acts as the verb "press"
// rather than an extra DPAD_CENTER click before the named button.
func collapsePressVerb(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		w := tokens[i]
		if w == "اضغط" || w == "اضغطي" {
			// Check if the next token (or the token after a count) is a repeatable button.
			j := i + 1
			if j < len(tokens) {
				if _, isCnt := remoteCounts[tokens[j]]; isCnt {
					j++
				}
			}
			// Collapse only at the start of an utterance ("اضغطي مرتين فوق") or before a named
			// phrase token ("اضغطي #select" / "اضغطي #playpause"), so mid-chain rapid presses
			// like "فوق تحت يمين اضغط تحت فوق" keep "اضغط" as DPAD_CENTER.
			if j < len(tokens) && (i == 0 || strings.HasPrefix(tokens[j], "#")) {
				if c, ok := remoteWords[tokens[j]]; ok && isRepeatable(c) {
					continue // verb "press" for the following button
				}
			}
		}
		out = append(out, w)
	}
	return out
}

func isPower(c remoteCmd) bool { return c.action == "power_on" || c.action == "power_off" }

func isRepeatable(c remoteCmd) bool {
	switch c.action {
	case "key", "volume_step", "pause", "play", "play_pause", "stop":
		return true
	default:
		return false
	}
}

// capPresses keeps at most maxPresses key and volume presses, in order, and every power action:
// "يمين خمس مرات … طف التلفزيون" still turns it off.
func capPresses(cmds []remoteCmd) []remoteCmd {
	out := make([]remoteCmd, 0, len(cmds))
	presses := 0
	for _, c := range cmds {
		if !isPower(c) {
			if presses >= maxPresses {
				continue
			}
			presses++
		}
		out = append(out, c)
	}
	return out
}
