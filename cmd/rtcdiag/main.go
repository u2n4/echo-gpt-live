// rtcdiag answers Alexa RTCSessionController offers with Pion and records every
// transport/media event so the ~26 s teardown can be attributed. No OpenAI involved.
//
// It is a drop-in replacement for the July 2026 signaling service: the deployed
// AWS Lambda keeps POSTing to https://<funnel>/rtc/offer|connected|disconnected,
// which Tailscale Funnel proxies to 127.0.0.1:8787.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pion/rtp"

	"github.com/u2n4/echo-gpt-live/internal/alexa"
	"github.com/u2n4/echo-gpt-live/internal/bridge"
	"github.com/u2n4/echo-gpt-live/internal/diag"
)

type app struct {
	log         *diag.Logger
	cfg         bridge.Config
	mu          sync.Mutex
	sess        *bridge.Session
	sessN       int
	recDir      string
	rec         *bridge.Recorder
	answerDelay time.Duration
	holdAfter   time.Duration
	maxSession  time.Duration
	offerAt     time.Time
	answerAt    time.Time
}

func (a *app) ev(ev string, kv ...any) { a.log.Event(ev, kv...) }

func (a *app) Offer(id, offer string) (string, error) {
	a.mu.Lock()
	if a.sess != nil {
		old := a.sess
		a.sess = nil
		a.mu.Unlock()
		old.Close("replaced_by_new_offer")
		a.mu.Lock()
	}
	a.sessN++
	n := a.sessN
	a.mu.Unlock()

	a.log.SetSession(id)
	a.offerAt = time.Now()
	a.log.SaveSDP(fmt.Sprintf("sess%02d-offer.sdp", n), offer)
	a.ev("offer_received", "sessionId", id, "n", n, "offer_bytes", len(offer), "config", a.cfgSummary())
	s, answer, err := bridge.Answer(id, offer, a.cfg, a.ev)
	if err != nil {
		return "", err
	}
	a.log.SaveSDP(fmt.Sprintf("sess%02d-answer.sdp", n), answer)
	// Recording the Echo's own microphone is how an acoustic experiment is read back: play a known
	// signal at the speaker and measure what the room returns.
	if a.recDir != "" {
		rec := bridge.NewRecorder(filepath.Join(a.recDir, id), "from-echo", a.ev)
		a.mu.Lock()
		a.rec = rec
		a.mu.Unlock()
		s.SetAudioSink(func(p *rtp.Packet) { rec.Write(p) })
	}
	if a.answerDelay > 0 {
		a.ev("answer_delay_sleep", "ms", a.answerDelay.Milliseconds())
		time.Sleep(a.answerDelay)
	}
	a.answerAt = time.Now()
	a.mu.Lock()
	a.sess = s
	a.mu.Unlock()
	a.ev("answer_ready", "sessionId", id, "offer_to_answer_ms", time.Since(a.offerAt).Milliseconds())
	go a.watchdog(s)
	return answer, nil
}

func (a *app) watchdog(s *bridge.Session) {
	time.Sleep(a.maxSession)
	a.mu.Lock()
	cur := a.sess
	a.mu.Unlock()
	if cur == s {
		a.ev("max_session_reached", "max", a.maxSession.String())
		s.Close("max_session")
	}
}

func (a *app) Connected(id string) {
	a.ev("alexa_session_connected", "sessionId", id, "since_offer_ms", time.Since(a.offerAt).Milliseconds(), "since_answer_ms", time.Since(a.answerAt).Milliseconds())
}

func (a *app) Disconnected(id string) {
	a.ev("alexa_session_disconnected", "sessionId", id, "since_offer_ms", time.Since(a.offerAt).Milliseconds(), "since_answer_ms", time.Since(a.answerAt).Milliseconds())
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil || s.ID != id {
		return
	}
	// Keep the PeerConnection open for holdAfter so we can see whether the Amazon
	// media server keeps sending RTP/STUN after its own SessionDisconnected directive.
	go func() {
		if a.holdAfter > 0 {
			a.ev("hold_after_disconnect", "seconds", a.holdAfter.Seconds())
			time.Sleep(a.holdAfter)
		}
		a.mu.Lock()
		rec := a.rec
		a.rec = nil
		if a.sess == s {
			a.sess = nil
		}
		a.mu.Unlock()
		rec.Close()
		s.Close("alexa_disconnected")
	}()
}

func (a *app) cfgSummary() map[string]any {
	c := a.cfg
	return map[string]any{
		"audio": c.Audio, "audio_file": filepath.Base(c.AudioFile), "reflect_delay_ms": c.ReflectDelay.Milliseconds(),
		"video": c.Video, "video_file": filepath.Base(c.VideoFile), "video_fps": c.VideoFPS,
		"ext": c.Ext, "nack": c.Nack, "dtls_role": c.DTLSRole, "lite": c.Lite, "relay_only": c.RelayOnly,
		"udp_port": c.UDPPort, "public_ip": c.PublicIP, "iface": c.Iface, "exclude_ifaces": c.ExcludeIface, "mux_ip": c.MuxIP, "stun": c.STUN, "turn": c.TURN != "",
		"answer_delay_ms": a.answerDelay.Milliseconds(), "hold_after_s": a.holdAfter.Seconds(),
	}
}

func main() {
	var (
		listen   = flag.String("listen", "127.0.0.1:8787", "HTTP listen address (Tailscale Funnel proxies here)")
		keyFile  = flag.String("key-file", os.ExpandEnv("$HOME/.config/acv/signing.key"), "shared HMAC key file (same bytes as the Lambda's ACV_SIGNING_KEY)")
		runsDir  = flag.String("runs-dir", "runs", "directory for per-run logs")
		runName  = flag.String("run", "", "run name (default: timestamp)")
		udpPort  = flag.Int("udp-port", 50000, "single UDP port for ICE (0 = ephemeral)")
		publicIP = flag.String("public-ip", "", "public IPv4 to advertise as host candidate (VPS); empty = STUN srflx")
		iface    = flag.String("iface", "", "only gather candidates on this interface")
		muxIP    = flag.String("mux-ip", "auto", "bind the ICE UDP socket to this local IPv4 (\"auto\" = egress address, \"\" = every address). A mux bound to every address ignores the interface/IP filters and advertises docker/lxc/tailscale candidates the peer can never use.")
		exIface  = flag.String("exclude-ifaces", "docker,br-,lxc,veth,tailscale,virbr,cni,flannel", "comma-separated interface name prefixes to exclude from ICE (ignored when --iface is set)")
		stun     = flag.String("stun", "stun.l.google.com:19302", "STUN server host:port")
		turn     = flag.String("turn", "", "TURN URL, e.g. turn:host:3478?transport=udp")
		turnUser = flag.String("turn-user", "", "TURN username")
		turnPass = flag.String("turn-pass", os.Getenv("RTCDIAG_TURN_PASS"), "TURN password (or env RTCDIAG_TURN_PASS)")
		relay    = flag.Bool("relay-only", false, "force relay candidates only (TURN A/B test)")
		lite     = flag.Bool("ice-lite", false, "ICE-lite agent")
		dtlsRole = flag.String("dtls-role", "auto", "auto|active|passive")
		recDir   = flag.String("record-dir", "", "write the Echo's inbound audio to Ogg/Opus here, verbatim. Empty = off")
		opusFmtp = flag.String("opus-fmtp", "", "Opus fmtp to ask of the remote encoder (empty = Pion default; bridge.DefaultOpusFmtp is what the assistant uses)")
		audio    = flag.String("audio", "file", "reflect|file")
		audioF   = flag.String("audio-file", "media/tone440_30s.ogg", "Ogg/Opus file (20 ms pages)")
		rdelay   = flag.Duration("reflect-delay", 0, "delay before echoing audio back")
		video    = flag.String("video", "inactive", "reject|inactive|recvonly|sendonly|sendrecv")
		videoF   = flag.String("video-file", "media/black1080p_1fps.h264", "Annex-B H.264 file")
		fps      = flag.Float64("video-fps", 1, "video frame rate for sendonly/sendrecv")
		ext      = flag.Bool("ext", false, "negotiate ssrc-audio-level + transport-cc extensions")
		nack     = flag.Bool("nack", false, "enable NACK interceptors")
		gather   = flag.Duration("gather-wait", 3*time.Second, "max ICE gathering wait before answering")
		iceDisc  = flag.Duration("ice-disconnected", 0, "ICE disconnected timeout (0 = Pion default 5s)")
		iceFail  = flag.Duration("ice-failed", 0, "ICE failed timeout (0 = Pion default 25s)")
		iceKA    = flag.Duration("ice-keepalive", 0, "ICE keepalive interval (0 = Pion default 2s)")
		adelay   = flag.Duration("answer-delay", 0, "artificial delay before returning the answer (timer-origin test)")
		hold     = flag.Duration("hold-after-disconnect", 20*time.Second, "keep PeerConnection open after Alexa SessionDisconnected")
		maxSess  = flag.Duration("max-session", 90*time.Minute, "hard cap per session")
	)
	flag.Parse()

	key, err := os.ReadFile(*keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "key file:", err)
		os.Exit(2)
	}
	key = []byte(strings.TrimSpace(string(key)))
	if len(key) < 32 {
		fmt.Fprintln(os.Stderr, "signing key too short")
		os.Exit(2)
	}
	name := *runName
	if name == "" {
		name = time.Now().UTC().Format("20060102T150405Z")
	}
	lg, err := diag.NewLogger(*runsDir, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logger:", err)
		os.Exit(2)
	}
	defer lg.Close()

	a := &app{log: lg, answerDelay: *adelay, holdAfter: *hold, maxSession: *maxSess, recDir: *recDir, cfg: bridge.Config{
		UDPPort: *udpPort, PublicIP: *publicIP, Iface: *iface, ExcludeIface: splitCSV(*exIface), MuxIP: *muxIP, STUN: *stun, TURN: *turn, TURNUser: *turnUser, TURNPass: *turnPass,
		RelayOnly: *relay, Lite: *lite, DTLSRole: *dtlsRole, Audio: *audio, AudioFile: *audioF, ReflectDelay: *rdelay,
		Video: *video, VideoFile: *videoF, VideoFPS: *fps, Ext: *ext, Nack: *nack, GatherWait: *gather,
		OpusFmtp: *opusFmtp,
		ICEDisc:  *iceDisc, ICEFail: *iceFail, ICEKeepAlive: *iceKA,
	}}
	srv := alexa.NewServer(key, a)
	srv.OnEvent = a.ev
	hs := &http.Server{Addr: *listen, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	a.ev("rtcdiag_start", "listen", *listen, "run_dir", lg.RunDir, "config", a.cfgSummary(), "pid", os.Getpid())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			a.ev("http_error", "err", err.Error())
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s != nil {
		s.Close("shutdown")
	}
	shctx, c := context.WithTimeout(context.Background(), 3*time.Second)
	defer c()
	if err := hs.Shutdown(shctx); err != nil {
		a.ev("http_shutdown_failed", "err", err.Error())
	}
	a.ev("rtcdiag_stop")
}

// splitCSV turns "a,b" into ["a","b"], dropping empties.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
