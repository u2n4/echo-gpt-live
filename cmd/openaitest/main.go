// openaitest proves the OpenAI leg on its own, before any Alexa involvement: it builds an
// AUDIO-ONLY WebRTC offer (no data channel, no video), posts it to the Realtime calls
// endpoint with the standard API key, attaches the server-side sideband control socket,
// speaks one line of audio from a file, and reports exactly what OpenAI negotiated and sent.
//
// It answers the questions a blog post cannot: does an audio-only offer with no
// m=application get accepted, what Opus fmtp and header extensions come back, what ICE
// candidate types OpenAI publishes, and whether downstream audio is paced at real time.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"

	"github.com/u2n4/echo-gpt-live/internal/bridge"
	"github.com/u2n4/echo-gpt-live/internal/diag"
	"github.com/u2n4/echo-gpt-live/internal/openai"
)

func speakOnceAt(runCtx context.Context, speakAt string, play func(string)) func(string) {
	var speakOnce sync.Once
	return func(when string) {
		if when != speakAt || runCtx.Err() != nil {
			return
		}
		speakOnce.Do(func() { play(when) })
	}
}

func main() {
	var (
		keyFile  = flag.String("key-file", os.ExpandEnv("$HOME/.config/acv/openai.key"), "file holding the OpenAI API key")
		api      = flag.String("api", "live", "live|realtime - which voice API to exercise")
		model    = flag.String("model", "", "voice model (default gpt-live-1 for live, gpt-realtime-2.1-mini for realtime)")
		backend  = flag.String("backend", openai.BackendModel, "delegation backend model for heavy work")
		deleg    = flag.String("delegation", "client", "client|responses|none - client keeps the effort choice in this bridge")
		effort   = flag.String("backend-effort", openai.BackendEffortDefault, "fixed effort when --delegation responses")
		idle     = flag.Duration("idle-close", 90*time.Second, "close the session after this long without speech (GPT-Live bills silence)")
		ask      = flag.String("ask-backend", "", "also send this question to the backend model and report time and cost")
		askKind  = flag.String("ask-kind", "", "tool|quick|hard - selects the reasoning effort for --ask-backend")
		stall    = flag.Duration("backend-stall", openai.DefaultStall, "backend is dead if its stream goes this long with no event (progress-based, not a deadline)")
		firstEv  = flag.Duration("backend-first-event", openai.DefaultFirstEvent, "backend is dead if the stream says nothing at all within this")
		audioF   = flag.String("audio-file", "media/hello.ogg", "Ogg/Opus (20 ms pages) to send as the user's voice")
		burst    = flag.Duration("burst", 0, "send this much of the audio file at once, unpaced, then the rest at real time. Reproduces the bridge's hold-and-flush at session start; the input transcript says whether OpenAI heard the burst")
		rtpBurst = flag.Bool("rtp-burst", false, "send the --burst part as raw RTP with continuous sequence numbers and timestamps written all at once (WriteRTP, the way the bridge flushes its hold), then the rest at real time; the default sends samples that are stamped as they go")
		speakAt  = flag.String("speak-at", "started", "started|connected - when the audio file starts: after session.started (the documented order) or as soon as the peer connection is connected, which is when the bridge releases its hold since 3f60316. The input transcript says whether OpenAI heard what arrived in between")
		dur      = flag.Duration("duration", 45*time.Second, "how long to stay connected")
		instr    = flag.String("instructions", "You are Sol, a concise voice assistant. Reply in one short sentence.", "system instructions")
		runsDir  = flag.String("runs-dir", "runs", "directory for logs")
		budgetP  = flag.String("budget-file", os.ExpandEnv("$HOME/.local/state/rtcdiag/spend.json"), "the same daily spend counter the bridge uses, so test sessions count against the ceiling; empty = uncounted")
		budgetUS = flag.Float64("daily-budget-usd", 2.0, "refuse to open a session once this much has been spent today (0 = count only)")
	)
	flag.Parse()
	if *api != "live" && *api != "realtime" {
		fmt.Fprintln(os.Stderr, "--api must be live or realtime")
		os.Exit(2)
	}

	key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if key == "" {
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "no API key: set OPENAI_API_KEY or write it to %s\n", *keyFile)
			os.Exit(2)
		}
		key = strings.TrimSpace(string(b))
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "API key is empty")
		os.Exit(2)
	}
	lg, err := diag.NewLogger(*runsDir, "openai-"+time.Now().UTC().Format("20060102T150405Z"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer lg.Close()

	// --- audio-only PeerConnection: one Opus track, no data channel, no video ---
	m := &webrtc.MediaEngine{}
	opus := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: opus, PayloadType: 111}, webrtc.RTPCodecTypeAudio); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	se := webrtc.SettingEngine{}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	wapi := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))
	pc, err := wapi.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pc.Close()

	// Two kinds of sender. The sample track stamps timestamps as it writes, which is how a live
	// microphone behaves. The RTP track sends packets with the sequence numbers and timestamps
	// they carry, which is how the bridge flushes the packets it held while OpenAI's leg came up:
	// a burst of 1-2 s of audio whose timestamps say it was captured 1-2 s ago. --rtp-burst
	// reproduces that exactly, so the transcript says whether OpenAI keeps or drops such a burst.
	track, err := webrtc.NewTrackLocalStaticSample(opus, "audio", "sol")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	rtpTrack, err := webrtc.NewTrackLocalStaticRTP(opus, "audio", "sol")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var sender webrtc.TrackLocal = track
	if *rtpBurst {
		sender = rtpTrack
	}
	if _, err := pc.AddTransceiverFromTrack(sender, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var inPkts atomic.Int64
	var firstInMs, lastInMs atomic.Int64
	start := time.Now()
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) { lg.Event("ice_connection_state", "state", s.String()) })
	connected := make(chan struct{})
	var connectedOnce sync.Once
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		lg.Event("peer_connection_state", "state", s.String(), "ms", time.Since(start).Milliseconds())
		if s == webrtc.PeerConnectionStateConnected {
			connectedOnce.Do(func() { close(connected) })
		}
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		lg.Event("openai_remote_track", "kind", tr.Kind().String(), "mime", tr.Codec().MimeType, "pt", tr.PayloadType(), "fmtp", tr.Codec().SDPFmtpLine, "ssrc", uint32(tr.SSRC()))
		var prev time.Time
		var maxGap, burst int64
		// Stalls, not just the single worst gap: how many arrival gaps exceeded each bound, and how
		// much wall clock they cost beyond the 20 ms of media each packet carries. This is what the
		// Echo hears as her voice cutting out, and the bridge's pacer can only cover a stall shorter
		// than its depth.
		var over100, over300, over600 int64
		var stallMs int64
		var prevTS uint32
		for {
			p, _, err := tr.ReadRTP()
			if err != nil {
				lg.Event("openai_audio_end", "pkts", inPkts.Load(), "max_inter_packet_ms", maxGap, "max_burst", burst,
					"gaps_over_100ms", over100, "gaps_over_300ms", over300, "gaps_over_600ms", over600,
					"stall_ms_total", stallMs)
				return
			}
			now := time.Now()
			n := inPkts.Add(1)
			gateTouch()
			if n <= 6 {
				lg.Event("openai_rtp_head", "n", n, "ssrc", p.SSRC, "seq", p.SequenceNumber, "ts", p.Timestamp,
					"marker", p.Marker, "payload_bytes", len(p.Payload))
			}
			if n == 1 {
				firstInMs.Store(time.Since(start).Milliseconds())
				prevTS = p.Timestamp
				lg.Event("openai_first_audio", "ms_since_start", firstInMs.Load(), "pt", p.PayloadType, "payload_bytes", len(p.Payload))
			} else {
				// pacing evidence: a real-time paced sender gives ~20 ms gaps; a bursty
				// sender gives long gaps punctuated by many back-to-back packets.
				gap := now.Sub(prev).Milliseconds()
				if gap > maxGap {
					maxGap = gap
				}
				if gap <= 1 {
					burst++
				}
				switch {
				case gap > 600:
					over600++
					fallthrough
				case gap > 300:
					over300++
					fallthrough
				case gap > 100:
					over100++
					stallMs += gap - 20
					// Each one, with when: a gap between two sentences is a pause, a gap inside one
					// is a stall, and only the transcript's timing tells them apart.
					lg.Event("openai_arrival_gap", "ms_since_start", time.Since(start).Milliseconds(), "gap_ms", gap,
						"marker", p.Marker, "ts_step", int64(p.Timestamp-prevTS))
				}
				prevTS = p.Timestamp
			}
			prev = now
			lastInMs.Store(time.Since(start).Milliseconds())
		}
	})

	// GPT-Live's docs uniformly assume a data channel on WebRTC and an audio-only Live offer is
	// undocumented, so create one: it costs nothing and removes the uncertainty. Label is fixed.
	dcStarted := make(chan struct{})
	if *api == "live" {
		dc, derr := pc.CreateDataChannel(openai.LiveDataChannelLabel, nil)
		if derr != nil {
			fmt.Fprintln(os.Stderr, derr)
			os.Exit(1)
		}
		var once sync.Once
		dc.OnOpen(func() { lg.Event("live_datachannel_open", "label", dc.Label()) })
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			var env struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(msg.Data, &env); err != nil {
				lg.Event("live_dc_parse_error", "bytes", len(msg.Data))
				return
			}
			lg.Event("live_dc_event", "type", env.Type, "bytes", len(msg.Data))
			// the docs say to wait for session.started before sending application commands
			if env.Type == "session.started" {
				once.Do(func() { close(dcStarted) })
			}
		})
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	gc := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	<-gc
	offerSDP := pc.LocalDescription().SDP
	lg.SaveSDP("openai-offer.sdp", offerSDP)
	lg.Event("offer_ready", "has_m_application", strings.Contains(offerSDP, "m=application"), "has_m_video", strings.Contains(offerSDP, "m=video"), "bytes", len(offerSDP))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var sessionID, answerSDP string
	// hangup runs exactly once, on every exit that follows a successful create: the idle gate, the
	// normal end, and the error exits below (os.Exit skips defers, so those call it before exiting).
	// A test session that was created and then abandoned billed until its own idle limit.
	var hangupOnce sync.Once
	var hangupFn func()
	hangup := func() {
		if hangupFn != nil {
			hangupOnce.Do(hangupFn)
		}
	}
	defer hangup()
	sessionStart := time.Now()

	// The file is spoken once, from whichever moment --speak-at names; the first caller wins.
	runCtx, cancel := context.WithTimeout(ctx, *dur)
	defer cancel()
	speak := speakOnceAt(runCtx, *speakAt, func(when string) {
		if _, err := os.Stat(*audioF); err != nil {
			lg.Event("no_audio_file", "path", *audioF, "note", "session will stay silent; use --audio-file to speak")
			return
		}
		lg.Event("speak_start", "at", when, "ms_since_start", time.Since(start).Milliseconds(), "rtp_burst", *rtpBurst)
		onErr := func(e error) { lg.Event("audio_file_error", "err", e.Error()) }
		if *rtpBurst {
			go playRTP(runCtx, *audioF, rtpTrack, *burst, onErr)
			return
		}
		go bridge.PlayOggOnce(runCtx, *audioF, track, *burst, onErr)
	})
	if *speakAt == "connected" {
		go func() {
			select {
			case <-connected:
				speak("connected")
			case <-runCtx.Done():
			}
		}()
	}

	switch *api {
	case "live":
		mdl := *model
		if mdl == "" {
			mdl = openai.LiveModel
		}
		lc := openai.NewLive(key, mdl)
		lc.OnEvent = lg.Event
		if *budgetP != "" {
			// A test session is a billed session. It used to be invisible to the day's ceiling.
			bud, berr := openai.NewBudget(*budgetP, *budgetUS, lg.Event)
			if berr != nil {
				fmt.Fprintln(os.Stderr, "budget:", berr)
				os.Exit(2)
			}
			bud.Reserve = *dur
			lc.Budget = bud
		}
		store := true
		cfg := openai.LiveSessionConfig{
			Model:        mdl,
			Instructions: *instr,
			// store:true is what makes fork possible, and fork is how a conversation survives
			// the Echo tearing its own RTC session down.
			Store: &store,
		}
		switch *deleg {
		case "client":
			cfg.Delegation = openai.ClientDelegation()
		case "responses":
			cfg.Delegation = openai.ResponsesDelegation(*backend, *effort, "Answer briefly and concretely.")
		}
		ls, cerr := lc.CreateSession(runCtx, offerSDP, cfg)
		if cerr != nil {
			lg.Event("openai_create_failed", "err", cerr.Error())
			fmt.Fprintln(os.Stderr, "create live session:", cerr)
			os.Exit(1)
		}
		sessionID, answerSDP = ls.ID, ls.Answer
		opened := time.Now()
		hangupFn = func() {
			hctx, cancelHangup := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancelHangup()
			if err := lc.HangupRetry(hctx, ls.ID, 3); err != nil {
				lg.Event("openaitest_hangup_failed", "session_id", ls.ID, "err", err.Error())
				fmt.Fprintln(os.Stderr, "hangup:", err)
			}
			// The day's counter is shared with the bridge; a test session that only recorded its
			// init charge undercounted every minute it stayed open. Muse found it.
			lc.Budget.Spent(time.Since(opened))
		}
		go func() { <-runCtx.Done(); hangup() }()
		if serr := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerSDP}); serr != nil {
			lg.Event("set_remote_failed", "err", serr.Error())
			fmt.Fprintln(os.Stderr, "set remote:", serr)
			hangup()
			os.Exit(1)
		}
		if sb, aerr := lc.Attach(runCtx, ls.ID); aerr != nil {
			// not fatal: the data channel still carries events
			lg.Event("live_attach_failed", "err", aerr.Error())
		} else {
			defer func() {
				if err := sb.Close(); err != nil {
					lg.Event("live_sideband_close_failed", "err", err.Error())
				}
			}()
			go func() {
				if err := sb.Read(runCtx, func(typ string, raw []byte) {
					if strings.Contains(typ, "transcript") || strings.Contains(typ, "text") || strings.Contains(typ, "delta") {
						lg.Event("live_sideband_event", "type", typ, "chars", len(raw))
						return
					}
					lg.Event("live_sideband_event", "type", typ, "bytes", len(raw))
				}); err != nil {
					lg.Event("live_sideband_read_failed", "err", err.Error())
				}
			}()
		}
		select {
		case <-runCtx.Done():
			hangup()
		case <-dcStarted:
			lg.Event("live_session_started", "ms_since_start", time.Since(start).Milliseconds())
		case <-time.After(15 * time.Second):
			lg.Event("live_session_started_timeout", "note", "no session.started arrived on the data channel")
		}
	default:
		mdl := *model
		if mdl == "" {
			mdl = openai.DefaultModel
		}
		cl := openai.New(key, mdl)
		cl.OnEvent = lg.Event
		call, cerr := cl.CreateCall(runCtx, offerSDP)
		if cerr != nil {
			lg.Event("openai_create_failed", "err", cerr.Error())
			fmt.Fprintln(os.Stderr, "create call:", cerr)
			os.Exit(1)
		}
		sessionID, answerSDP = call.ID, call.Answer
		hangupFn = func() {
			hctx, cancelHangup := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancelHangup()
			if err := cl.Hangup(hctx, call.ID); err != nil {
				lg.Event("openaitest_hangup_failed", "session_id", call.ID, "err", err.Error())
				fmt.Fprintln(os.Stderr, "hangup:", err)
			}
		}
		go func() { <-runCtx.Done(); hangup() }()
		if serr := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerSDP}); serr != nil {
			lg.Event("set_remote_failed", "err", serr.Error())
			fmt.Fprintln(os.Stderr, "set remote:", serr)
			hangup()
			os.Exit(1)
		}
		sb, aerr := cl.Attach(runCtx, call.ID)
		if aerr != nil {
			lg.Event("openai_attach_failed", "err", aerr.Error())
			fmt.Fprintln(os.Stderr, "attach:", aerr)
			hangup()
			os.Exit(1)
		}
		defer func() {
			if err := sb.Close(); err != nil {
				lg.Event("realtime_sideband_close_failed", "err", err.Error())
			}
		}()
		go func() {
			if err := sb.Read(runCtx); err != nil {
				lg.Event("realtime_sideband_read_failed", "err", err.Error())
			}
		}()
		if uerr := sb.SessionUpdate(runCtx, map[string]any{
			"type":         "realtime",
			"instructions": *instr,
			"audio": map[string]any{
				"input":  map[string]any{"turn_detection": map[string]any{"type": "server_vad"}},
				"output": map[string]any{"voice": "cedar"},
			},
		}); uerr != nil {
			lg.Event("session_update_failed", "err", uerr.Error())
		}
	}
	lg.SaveSDP("openai-answer.sdp", answerSDP)
	summarizeAnswer(lg, answerSDP)

	// GPT-Live bills wall clock including silence, and muting the microphone does not close the
	// session, so an idle session must be hung up rather than left open.
	gate := openai.NewIdleGate(*idle, func(idleFor time.Duration) {
		lg.Event("idle_close", "idle_s", idleFor.Seconds(),
			"billed_usd_so_far", openai.EstimateLiveCostUSD(time.Since(sessionStart)))
		hangup()
		stop()
	})
	touch := gate.Touch
	gateTouchFn.Store(&touch)
	defer gate.Stop()

	// Optional: one straight-to-backend measurement so the heavy path has real numbers too.
	if *ask != "" {
		be := openai.NewBackend(key, *backend)
		be.OnEvent = lg.Event
		be.Stall = *stall
		be.FirstEvent = *firstEv
		eff := openai.BackendEffortFor(*askKind)
		if openai.AnnounceWait(eff) {
			lg.Event("backend_announce", "effort", eff,
				"note", "slow enough that the voice layer should tell the user to expect a pause")
		}
		be.OnProgress = func(phase string, chars int, elapsed time.Duration) {
			// one line per 500 chars keeps the log readable while still proving liveness
			if chars%500 < 40 {
				lg.Event("backend_progress", "phase", phase, "chars", chars, "seconds", elapsed.Seconds())
			}
		}
		if r, aerr := be.Ask(runCtx, *askKind, "Answer briefly and concretely.", *ask, 2048); aerr != nil {
			lg.Event("backend_failed", "err", aerr.Error())
		} else {
			lg.Event("backend_result", "effort", r.Effort, "seconds", r.Elapsed.Seconds(),
				"first_event_s", r.FirstEventAfter.Seconds(), "longest_gap_s", r.LongestGap.Seconds(),
				"events", r.Events, "reasoning_chars", r.ReasoningChars,
				"reasoning_tokens", r.ReasoningTokens, "truncated", r.Truncated, "status", r.Status,
				"cost_usd", r.CostUSD, "answer_chars", len(r.Text))
		}
	}

	// speak the file once (now, unless --speak-at connected already did), then listen
	speak("started")
	<-runCtx.Done()

	hangup()
	out := map[string]any{
		"api": *api, "session_id": sessionID,
		"billed_usd_estimate":       openai.EstimateLiveCostUSD(time.Since(sessionStart)),
		"audio_packets_from_openai": inPkts.Load(),
		"first_audio_ms":            firstInMs.Load(),
		"last_audio_ms":             lastInMs.Load(),
		"run_dir":                   lg.RunDir,
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

func summarizeAnswer(lg *diag.Logger, sdp string) {
	var keep []string
	cands := map[string]int{}
	for _, ln := range strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(ln, "a=candidate:"):
			f := strings.Fields(ln)
			if len(f) >= 8 {
				cands[f[7]+"/"+f[2]]++
			}
		case strings.HasPrefix(ln, "m="), strings.HasPrefix(ln, "a=group"), strings.HasPrefix(ln, "a=setup"),
			strings.HasPrefix(ln, "a=rtpmap"), strings.HasPrefix(ln, "a=fmtp"), strings.HasPrefix(ln, "a=extmap"),
			strings.HasPrefix(ln, "a=ice-lite"), strings.HasPrefix(ln, "a=sendrecv"), strings.HasPrefix(ln, "a=recvonly"),
			strings.HasPrefix(ln, "a=sendonly"), strings.HasPrefix(ln, "a=inactive"), strings.HasPrefix(ln, "a=end-of-candidates"):
			keep = append(keep, ln)
		}
	}
	lg.Event("openai_answer_summary",
		"lines", keep, "candidates", cands,
		"has_m_application", strings.Contains(sdp, "m=application"),
		"has_m_video", strings.Contains(sdp, "m=video"),
		"ice_lite", strings.Contains(sdp, "a=ice-lite"),
		"trickle", !strings.Contains(sdp, "a=end-of-candidates"))
}

// gateTouchFn is installed once the idle gate exists; inbound audio before that needs no gating.
var gateTouchFn atomic.Pointer[func()]

func gateTouch() {
	if fn := gateTouchFn.Load(); fn != nil {
		(*fn)()
	}
}

// playRTP is the bridge's hold-and-flush in miniature: the first `burst` of the file leaves as raw
// RTP in one go with continuous sequence numbers and 20 ms timestamp steps, the rest at real time
// on the same timeline. What the bridge does to the packets it held is exactly this.
func playRTP(ctx context.Context, path string, track *webrtc.TrackLocalStaticRTP, burst time.Duration, onErr func(error)) {
	f, err := os.Open(path)
	if err != nil {
		onErr(err)
		return
	}
	defer f.Close()
	ogg, _, err := oggreader.NewWith(f)
	if err != nil {
		onErr(err)
		return
	}
	seq := uint16(time.Now().UnixNano())
	ts := uint32(time.Now().UnixNano() / 1000)
	var sent time.Duration
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		payload, _, err := ogg.ParseNextPage()
		if err != nil {
			return
		}
		if len(payload) == 0 {
			continue
		}
		p := &rtp.Packet{Header: rtp.Header{Version: 2, Marker: sent == 0, SequenceNumber: seq, Timestamp: ts}, Payload: payload}
		if err := track.WriteRTP(p); err != nil {
			onErr(err)
		}
		seq++
		ts += 960
		sent += 20 * time.Millisecond
		if sent <= burst {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func clip(s string, n int) string {
	// by runes: Arabic is two bytes a letter, and a cut inside one leaves invalid UTF-8
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
