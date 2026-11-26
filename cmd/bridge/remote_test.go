package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/u2n4/echo-gpt-live/internal/tv"
)

func cmdNames(cs []remoteCmd) string {
	s := make([]string, len(cs))
	for i, c := range cs {
		s[i] = c.String()
	}
	return strings.Join(s, ",")
}

func TestParseRemote(t *testing.T) {
	cases := []struct {
		say     string
		active  bool
		on, off bool
		enter   bool
		exit    bool
		cmds    string
		pure    bool
	}{
		// power phrases work in any mode, and turn remote mode on/off
		{say: "شغلي التلفزيون", on: true, enter: true, cmds: "power_on", pure: true},
		{say: "يا سول شغّلي التلفزيون لو سمحت", on: true, enter: true, cmds: "power_on", pure: true},
		{say: "ممكن تشغلين التلفزيون", on: true, enter: true, cmds: "power_on", pure: true},
		{say: "شغل لي التلفزيون", on: true, enter: true, cmds: "power_on", pure: true},
		{say: "شغلي التلفزيون يوتيوب", on: true, enter: true, cmds: "power_on,youtube", pure: true},
		{say: "turn on the TV", on: true, enter: true, cmds: "power_on", pure: true},
		{say: "طفّي التلفزيون", active: true, off: true, exit: true, cmds: "power_off", pure: true},
		{say: "أطفي التلفزيون", off: true, exit: true, cmds: "power_off", pure: true},
		{say: "وضع الريموت", enter: true, pure: true},
		// any word that is not a command, count or filler makes it conversation (Grok's cases)
		{say: "لا شغلي التلفزيون"},
		{say: "لا تشغلي التلفزيون"},
		{say: "بعدين شغّل التلفزيون"},
		{say: "إذا خلصنا شغّل التلفزيون"},
		{say: "شغّل التلفزيون ولا لا؟"},
		{say: "شلون أطفي التلفزيون"},
		{say: "كيف اطفي التلفزيون"},
		{say: "شغل التلفزيون وطفه"},
		{say: "شغّل التلفزيون بعد شوي"}, // "in a bit" is not now (Codex)
		{say: "لا، شغّلي التلفزيون"},
		{say: "أشغّل التلفزيون", on: true, enter: true, cmds: "power_on", pure: true},
		{say: "خلاص يا سول", active: true, exit: true, pure: true},
		{say: "ارفع لي الصوت شوي", active: true, cmds: "volume+1", pure: true},
		// Grok round 3
		{say: "يلا ادخل", active: true},
		{say: "استمر", active: true},
		{say: "شغّل التلفزيون الحين", on: true, enter: true, cmds: "power_on", pure: true},
		{say: "طف التلفزيون دحين", off: true, exit: true, cmds: "power_off", pure: true},
		{say: "تمام شغّل التلفزيون", on: true, enter: true, cmds: "power_on", pure: true},
		{say: "شغّل لو سمحت التلفزيون", on: true, enter: true, cmds: "power_on", pure: true},
		{say: "ثلاث مرات يمين", active: true, cmds: "right,right,right", pure: true},
		{say: "ثلاث مرات", active: true},
		{say: "أرجع الصوت", active: true, cmds: "set_mute", pure: true},
		{say: "تمام", active: true},
		// an app any time; tvd switches the TV on first
		{say: "شغلي يوتيوب", enter: true, cmds: "youtube", pure: true},
		{say: "يا سول افتح نتفلكس", enter: true, cmds: "netflix", pure: true},
		{say: "شغل لي اليوتيوب لو سمحت", enter: true, cmds: "youtube", pure: true},
		{say: "ليش ما تشغلين يوتيوب"},
		{say: "يوتيوب"}, // the bare name needs remote mode
		// order kept, then the mode changes (Grok final)
		{say: "يمين خلاص", active: true, exit: true, cmds: "right", pure: true},
		{say: "ارفع الصوت طف التلفزيون", active: true, off: true, exit: true, cmds: "volume+1,power_off", pure: true},
		{say: "شغل التلفزيون ثلاث مرات يمين", on: true, enter: true, cmds: "power_on,right,right,right", pure: true},
		// arrows only in remote mode
		{say: "يمين"},
		{say: "يمين", active: true, cmds: "right", pure: true},
		{say: "يمين يمين", active: true, cmds: "right,right", pure: true},
		{say: "يمين ثلاث مرات", active: true, cmds: "right,right,right", pure: true},
		{say: "تحت مرتين ثم اضغط", active: true, cmds: "down,down,select", pure: true},
		{say: "يا سول اضغط", active: true, cmds: "select", pure: true},
		{say: "اختار", active: true, cmds: "select", pure: true},
		{say: "الرئيسية", active: true, cmds: "home", pure: true},
		{say: "ارجع.", active: true, cmds: "back", pure: true},
		{say: "ارفع الصوت", active: true, cmds: "volume+1", pure: true},
		{say: "ارفع الصوت شوي", active: true, cmds: "volume+1", pure: true},
		{say: "وطي الصوت مرتين", active: true, cmds: "volume-1,volume-1", pure: true},
		{say: "اكتم الصوت", active: true, cmds: "set_mute", pure: true},
		{say: "وقف", active: true, cmds: "pause", pure: true},
		{say: "كمل", active: true, cmds: "play", pure: true},
		{say: "نتفلكس", active: true, cmds: "netflix", pure: true},
		// words people say to her all the time are not commands (Grok)
		{say: "اوكي", active: true},
		{say: "ok", active: true},
		{say: "شغل", active: true},
		{say: "ورا", active: true},
		{say: "طيب", active: true},
		// conversation in remote mode moves nothing
		{say: "روح يمين عند الباب", active: true},
		{say: "وش الجو اليوم", active: true},
		{say: "يوتيوب ثلاث مرات", active: true}, // a count only repeats keys and volume
		// Mom's natural Gulf Arabic TV phrases (mute, pause video, center button x2, play_pause x2, shahid)
		{say: "حطي ميوت", enter: true, cmds: "set_mute", pure: true},
		{say: "سكتي التلفزيون", enter: true, cmds: "set_mute", pure: true},
		{say: "شيلي الميوت", enter: true, cmds: "set_mute", pure: true},
		{say: "وقفي المقطع", enter: true, cmds: "pause", pure: true},
		{say: "كملي المقطع", enter: true, cmds: "play", pure: true},
		{say: "شغلي شاهد", enter: true, cmds: "shahid", pure: true},
		{say: "مرتين فوق", active: true, cmds: "up,up", pure: true},
		{say: "اضغطي مرتين فوق", active: true, cmds: "up,up", pure: true},
		{say: "اضغطي فوق مرتين", active: true, cmds: "up,up", pure: true},
		{say: "اضغطي مرتين على الزر اللي في النص", active: true, cmds: "select,select", pure: true},
		{say: "اضغطي مرتين على زر اللي يوقف المقطع", active: true, cmds: "play_pause,play_pause", pure: true},
		{say: "روحي تحت مرتين ثم اضغطي اللي بالنص", active: true, cmds: "down,down,select", pure: true},
		{say: "قدمي المقطع", enter: true, cmds: "fast_forward", pure: true},
		{say: "المقطع الجاي", enter: true, cmds: "next", pure: true},
		// Multi-step compound commands with "بعدين" and counts up to 10
		{say: "اضغط بعدين نزلي تحت 5 مرات", active: true, cmds: "select,down,down,down,down,down", pure: true},
		{say: "اضغطي وبعدين انزلي تحت خمس مرات", active: true, cmds: "select,down,down,down,down,down", pure: true},
		{say: "تحت 7 مرات", active: true, cmds: "down,down,down,down,down,down,down", pure: true},
		{say: "افتحي تطبيق نتفلكس", enter: true, cmds: "netflix", pure: true},
		// HDMI switching & Home button in ANY mode!
		{say: "حولي على الكاميرات", enter: true, cmds: "input:cameras", pure: true},
		{say: "افتحي الكاميرات", enter: true, cmds: "input:cameras", pure: true},
		{say: "رجعي على الشاومي", enter: true, cmds: "input:xiaomi", pure: true},
		{say: "حولي على الشاومي", enter: true, cmds: "input:xiaomi", pure: true},
		{say: "حولي hdmi 2", enter: true, cmds: "input:cameras", pure: true},
		{say: "حولي hdmi 3", enter: true, cmds: "input:xiaomi", pure: true},
		{say: "روحي للهوم", enter: true, cmds: "home", pure: true},
		{say: "ارجعي للرئيسية", enter: true, cmds: "home", pure: true},
		{say: "فوق تحت يمين اضغط تحت فوق", active: true, cmds: "up,down,right,select,down,up", pure: true},
		{say: "فوق تحت يمين اضغط", active: false, enter: true, cmds: "up,down,right,select", pure: true},
		{say: "انزلي تحت 3 مرات", active: false, enter: true, cmds: "down,down,down", pure: true},
		// leaving
		{say: "خلاص", active: true, exit: true, pure: true},
		{say: "خلاص"},
		{say: "وقف الريموت", active: true, exit: true, pure: true},
	}
	for _, c := range cases {
		p := parseRemote(c.say, c.active)
		got := fmt.Sprintf("on=%v off=%v enter=%v exit=%v cmds=%s pure=%v", p.powerOn, p.powerOff, p.enter, p.exit, cmdNames(p.cmds), p.pureCommand)
		want := fmt.Sprintf("on=%v off=%v enter=%v exit=%v cmds=%s pure=%v", c.on, c.off, c.enter, c.exit, c.cmds, c.pure)
		if got != want {
			t.Errorf("%q active=%v\n got  %s\n want %s", c.say, c.active, got, want)
		}
	}
}

func TestParseCapNeverDropsPower(t *testing.T) {
	p := parseRemote(strings.Repeat("يمين خمس مرات ", 4)+"طف التلفزيون", true)
	if n := len(p.cmds); n != maxPresses+1 || p.cmds[n-1].action != "power_off" {
		t.Fatalf("got %s", cmdNames(p.cmds))
	}
}

func TestParseCapsPresses(t *testing.T) {
	p := parseRemote(strings.Repeat("يمين خمس مرات ", 4), true)
	if len(p.cmds) != maxPresses {
		t.Fatalf("got %d presses, want the cap %d", len(p.cmds), maxPresses)
	}
}

type fakeTV struct {
	mu    sync.Mutex
	calls []string
	reply func(action string) (tv.Result, error)
}

func (f *fakeTV) Do(_ context.Context, action string, args map[string]any, _ time.Duration) (tv.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, remoteCmd{action, args}.String())
	reply := f.reply
	f.mu.Unlock()
	if reply != nil {
		return reply(action)
	}
	return tv.Result{OK: true}, nil
}

func (f *fakeTV) got() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ",")
}

// waitFor polls until cond holds or a second passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRemoteFlow(t *testing.T) {
	f := &fakeTV{}
	r := newRemote(f, nil)
	tok := r.Begin(nil, nil)
	r.Feed(tok, "يمين") // not in remote mode yet: ignored
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	if f.got() != "" {
		t.Fatalf("pressed before remote mode: %s", f.got())
	}
	r.Feed(tok, "شغّلي ")
	r.Feed(tok, "التلفزيون") // one utterance across two fragments
	waitFor(t, "power on", func() bool { return f.got() == "power_on" })
	t0 := time.Now()
	r.Feed(tok, "يم")
	r.Feed(tok, "ين") // a word split across fragments
	waitFor(t, "right", func() bool { return f.got() == "power_on,right" })
	if d := time.Since(t0); d > remoteQuiet+300*time.Millisecond {
		t.Fatalf("a press took %v after the word", d)
	}
	if !r.handles("يمين") || r.handles("وش الجو اليوم") || r.handles("يسار") {
		t.Fatal("handles must cover the words just pressed and nothing else")
	}
	r.Feed(tok, "خلاص")
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	if r.handles("يمين") {
		t.Fatal("after خلاص the same words are no longer a press, so a delegation about them is answered")
	}
	r.Feed(tok, "يسار")
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	if f.got() != "power_on,right" {
		t.Fatalf("pressed after leaving remote mode: %s", f.got())
	}
}

func TestRemoteIgnoresHerEcho(t *testing.T) {
	f := &fakeTV{}
	r := newRemote(f, nil)
	var speaking atomic.Bool // read on the timer goroutine
	tok := r.Begin(speaking.Load, nil)
	speaking.Store(true)
	r.Feed(tok, "شغلي التلفزيون")
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	if f.got() != "" {
		t.Fatalf("her echo pressed: %s", f.got())
	}
	// Codex: a fragment that ARRIVED while she spoke must not pass because she stopped before
	// the words were read.
	speaking.Store(true)
	r.Feed(tok, "شغلي ")
	speaking.Store(false)
	r.Feed(tok, "التلفزيون")
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	if f.got() != "" {
		t.Fatalf("an utterance with an echo fragment pressed: %s", f.got())
	}
	r.Feed(tok, "شغلي التلفزيون")
	waitFor(t, "his own words", func() bool { return f.got() == "power_on" })
}

func TestRemoteOldTokenAndOldTimerAreRefused(t *testing.T) {
	f := &fakeTV{}
	r := newRemote(f, nil)
	old := r.Begin(nil, nil)
	tok := r.Begin(nil, nil) // the next call began
	r.Feed(old, "شغلي التلفزيون")
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	if f.got() != "" {
		t.Fatalf("a fragment from the ended call pressed: %s", f.got())
	}
	// A superseded quiet timer must not read words a newer fragment added.
	r.Feed(tok, "وضع الريموت")
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	r.Feed(tok, "يمين")
	r.flush(tok, 0) // a stale timer callback (its sequence is old)
	if f.got() != "" {
		t.Fatalf("a stale timer read the words early: %s", f.got())
	}
	waitFor(t, "the real read", func() bool { return f.got() == "right" })
}

func TestRemoteEndCancelsAPressInFlight(t *testing.T) {
	started := make(chan struct{})
	f := &fakeTV{reply: func(action string) (tv.Result, error) {
		return tv.Result{OK: true}, nil
	}}
	slow := &slowTV{fakeTV: f, started: started}
	r := newRemote(slow, nil)
	tok := r.Begin(nil, nil)
	r.Feed(tok, "شغلي التلفزيون يوتيوب") // power on, then youtube
	<-started
	r.End() // the call ends while power_on is in flight
	waitFor(t, "the press to be cancelled", func() bool { return slow.cancelled.Load() })
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(f.got(), "youtube") {
		t.Fatalf("work continued after the call ended: %s", f.got())
	}
}

// slowTV blocks a command until its context ends, like a box that is still waking.
type slowTV struct {
	*fakeTV
	started   chan struct{}
	once      sync.Once
	cancelled atomic.Bool
}

func (s *slowTV) Do(ctx context.Context, action string, args map[string]any, b time.Duration) (tv.Result, error) {
	s.fakeTV.Do(ctx, action, args, b)
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	s.cancelled.Store(true)
	return tv.Result{}, ctx.Err()
}

// Codex: "شلون [pause] أطفي التلفزيون" must not become an order because the pause split it.
func TestRemoteLooksBackAcrossAShortPause(t *testing.T) {
	f := &fakeTV{}
	r := newRemote(f, nil)
	tok := r.Begin(nil, nil)
	r.Feed(tok, "شلون")
	time.Sleep(remoteQuiet + 100*time.Millisecond) // read alone: conversation
	r.Feed(tok, "اطفي التلفزيون")                  // arrives right after
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	if f.got() != "" {
		t.Fatalf("a split question became an order: %s", f.got())
	}
	time.Sleep(remoteLookback) // a real pause, then the order on its own
	r.Feed(tok, "اطفي التلفزيون")
	waitFor(t, "the order", func() bool { return f.got() == "power_off" })
}

// Grok: "خلاص" right after other words still ends remote mode (leaving only ever stops presses).
func TestRemoteExitIsNotBlockedByTheLookBack(t *testing.T) {
	f := &fakeTV{}
	r := newRemote(f, nil)
	tok := r.Begin(nil, nil)
	r.Feed(tok, "وضع الريموت")
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	r.Feed(tok, "يعني كذا")
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	r.Feed(tok, "خلاص")
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	r.Feed(tok, "يمين")
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	if f.got() != "" {
		t.Fatalf("remote mode stayed on after خلاص: %s", f.got())
	}
}

// Codex round 4: the chain remembers conversation across filler-only pieces and truncation.
func TestRemoteConversationChain(t *testing.T) {
	gap := remoteQuiet + 100*time.Millisecond // each piece is read on its own, but within the look-back
	cases := []struct {
		name   string
		pieces []string
		want   string
	}{
		{"question split by a filler", []string{"شلون", "يا سول", "اطفي التلفزيون"}, ""},
		{"overflow just before", []string{strings.Repeat("كلام عادي ", 30), "اطفي التلفزيون"}, ""},
		{"long filler run after a negation", []string{"لا " + strings.Repeat("طيب ", 21), "شغلي التلفزيون"}, ""},
		{"a filler before the order is fine", []string{"يا سول", "شغلي التلفزيون"}, "power_on"},
		{"remote mode: no right after a no", []string{"وضع الريموت", "لا", "يمين"}, ""},
		{"remote mode: background TV dialogue does not block a real command", []string{"وضع الريموت", "قصة الفيلم جميلة جدا", "اضغط بعدين نزلي تحت 3 مرات"}, "select,down,down,down"},
	}
	for _, c := range cases {
		f := &fakeTV{}
		r := newRemote(f, nil)
		tok := r.Begin(nil, nil)
		for _, piece := range c.pieces {
			r.Feed(tok, piece)
			time.Sleep(gap)
		}
		if c.want != "" {
			waitFor(t, c.name, func() bool { return f.got() == c.want })
		} else {
			time.Sleep(150 * time.Millisecond)
		}
		if f.got() != c.want {
			t.Errorf("%s: pressed %q, want %q", c.name, f.got(), c.want)
		}
		r.End()
	}
}

// Codex: a long stretch of talk that loses its beginning must not turn into a command.
func TestRemoteOverflowNeverActs(t *testing.T) {
	f := &fakeTV{}
	r := newRemote(f, nil)
	tok := r.Begin(nil, nil)
	r.Feed(tok, strings.Repeat("كلام عادي ", 30)+"شغلي التلفزيون")
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	if f.got() != "" {
		t.Fatalf("an overflowed utterance acted: %s", f.got())
	}
}

func TestRemoteIdleEndsMode(t *testing.T) {
	f := &fakeTV{}
	r := newRemote(f, nil)
	tok := r.Begin(nil, nil)
	r.Feed(tok, "وضع الريموت")
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	r.mu.Lock()
	r.lastCmd = time.Now().Add(-remoteIdle - time.Second)
	r.mu.Unlock()
	r.Feed(tok, "يمين")
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	if f.got() != "" {
		t.Fatalf("pressed after remote mode timed out: %s", f.got())
	}
}

func TestRemoteResetDropsTheOldCall(t *testing.T) {
	f := &fakeTV{}
	r := newRemote(f, nil)
	tok := r.Begin(nil, nil)
	r.Feed(tok, "وضع الريموت")
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	r.Feed(tok, "يمين") // pending when the call ends
	r.End()
	tok = r.Begin(nil, nil)
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	r.Feed(tok, "يمين") // new call: remote mode is off again
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	if f.got() != "" {
		t.Fatalf("a press crossed calls: %s", f.got())
	}
}

func TestRemoteComplainsOnceAndStops(t *testing.T) {
	f := &fakeTV{reply: func(action string) (tv.Result, error) {
		if action == "key" {
			return tv.Result{OK: false, Error: "off"}, nil
		}
		return tv.Result{OK: true}, nil
	}}
	r := newRemote(f, nil)
	var said []string
	var mu sync.Mutex
	tok := r.Begin(nil, func(s string) { mu.Lock(); said = append(said, s); mu.Unlock() })
	r.Feed(tok, "وضع الريموت")
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	r.Feed(tok, "يمين يمين يمين") // the first press fails: the other two are not sent
	waitFor(t, "one press", func() bool { return f.got() == "right" })
	time.Sleep(100 * time.Millisecond)
	r.Feed(tok, "يسار") // fails again within 20 s: no second complaint
	waitFor(t, "second press", func() bool { return f.got() == "right,left" })
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(said) != 1 || !strings.Contains(said[0], "طافي") {
		t.Fatalf("said %q", said)
	}
}

func TestRemoteStreamingRapidFire(t *testing.T) {
	f := &fakeTV{}
	r := newRemote(f, nil)
	tok := r.Begin(nil, nil)
	r.Activate()
	// Feed rapid continuous words without pausing between them: leading words must fire live!
	r.Feed(tok, "فوق ")
	r.Feed(tok, "تحت ")
	waitFor(t, "first word fires immediately on second word", func() bool { return strings.HasPrefix(f.got(), "up") })
	r.Feed(tok, "يمين ")
	r.Feed(tok, "انتر ")
	r.Feed(tok, "تحت ")
	r.Feed(tok, "فوق")
	waitFor(t, "all rapid words fired", func() bool { return f.got() == "up,down,right,select,down,up" })
}

func TestRemoteLeadingPrefixExecutesWhileUserKeepsTalking(t *testing.T) {
	f := &fakeTV{}
	r := newRemote(f, nil)
	tok := r.Begin(nil, nil)
	// User starts with "افتحي نتفلكس" and immediately keeps explaining the rest of their request:
	// Netflix must launch RIGHT AWAY without waiting for the end of the long sentence!
	r.Feed(tok, "افتحي نتفلكس ")
	r.Feed(tok, "وشوفي وش آخر فلم شاهدناه وبعدين دوري على سبايدر مان")
	waitFor(t, "leading netflix fired immediately", func() bool { return f.got() == "netflix" })
	// Because the user kept talking, handles() must NOT silence the delegation for the rest of the request.
	if r.handles("افتحي نتفلكس وشوفي وش آخر فلم شاهدناه وبعدين دوري على سبايدر مان") {
		t.Fatal("handles must not swallow the rest of a longer spoken request")
	}

	// Doubt / negation after the opening phrase must NEVER fire!
	f2 := &fakeTV{}
	r2 := newRemote(f2, nil)
	tok2 := r2.Begin(nil, nil)
	r2.Feed(tok2, "شغلي يوتيوب ولا لا")
	time.Sleep(remoteQuiet + 100*time.Millisecond)
	if f2.got() != "" {
		t.Fatalf("doubt phrase fired: %s", f2.got())
	}
}
