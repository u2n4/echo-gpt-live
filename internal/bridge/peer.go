// Package bridge builds the Alexa-facing Pion PeerConnection for diagnostics: it
// answers the Echo/Amazon SDP offer (non-trickle, all candidates inline), attaches
// configurable audio/video sources, and reports every transport/media event.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	extAudioLevel  = "urn:ietf:params:rtp-hdrext:ssrc-audio-level"
	extTransportCC = "http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01"
)

// Config selects one experimental configuration (one hypothesis test).
type Config struct {
	UDPPort      int      // single UDP mux port (0 = ephemeral ports, no mux)
	MuxIP        string   // bind the UDP mux to this local IPv4 ("auto" = egress address, "" = all)
	PublicIP     string   // NAT 1:1 public IPv4 -> host candidate (VPS); empty = use STUN srflx
	Iface        string   // only gather on this interface (empty = all)
	ExcludeIface []string // drop interfaces whose name has any of these prefixes
	STUN         string   // stun host:port (used when PublicIP empty)
	TURN         string   // turn:host:port with user/pass below (optional)
	TURNUser     string
	TURNPass     string
	RelayOnly    bool          // ICETransportPolicy relay (forced-TURN A/B test)
	Lite         bool          // ICE-lite
	DTLSRole     string        // auto|active|passive (answerer role)
	Audio        string        // bridge|reflect|file
	AudioFile    string        // ogg/opus, 20 ms pages
	ReflectDelay time.Duration // reflect mode: delay before echoing back
	Video        string        // reject|inactive|recvonly|sendonly|sendrecv
	VideoFile    string        // Annex-B .h264
	VideoFPS     float64
	OpusFmtp     string // fmtp params asked of the Echo's encoder ("" = leave Pion's default)
	// VideoStreamID is the msid stream the keepalive video belongs to. It defaults to a stream of
	// its own, separate from the audio: sharing one makes a libwebrtc-style receiver treat the two
	// as a synchronisation group, and a 1 fps keepalive is a poor clock for a voice to be pulled
	// toward. Set it to the audio's stream id to put the old behaviour back for an A/B.
	VideoStreamID string
	Ext           bool          // negotiate ssrc-audio-level + transport-cc header extensions
	Nack          bool          // enable NACK interceptors
	GatherWait    time.Duration // max wait for ICE gathering complete
	ICEDisc       time.Duration // ICE disconnected timeout
	ICEFail       time.Duration // ICE failed timeout
	ICEKeepAlive  time.Duration // ICE keepalive interval
}

// Events receives structured events from the session.
type Events func(ev string, kv ...any)

// Session is one answered Alexa RTC session.
type Session struct {
	ID          string
	cfg         Config
	ev          Events
	pc          *webrtc.PeerConnection
	getter      stats.Getter
	cancel      context.CancelFunc
	ctx         context.Context
	audioRTP    *webrtc.TrackLocalStaticRTP
	audioSmp    *webrtc.TrackLocalStaticSample
	videoSmp    *webrtc.TrackLocalStaticSample
	refl        *reflector
	remoteSSRCs sync.Map // uint32 -> kind
	// counters
	inPkts, inBytes, outPkts, vidFrames atomic.Int64
	firstIn, lastIn                     atomic.Int64 // unix ms
	offerAt, answerAt, connectedAt      time.Time
	closeOnce                           sync.Once
	mux                                 *muxHolder
	// audioSink receives every inbound Opus packet from the Echo when Audio is "bridge".
	audioSink atomic.Pointer[func(*rtp.Packet)]
	// onDown is told when the transport has failed for good, so whatever was wired to this
	// session can stop paying for it.
	onDown atomic.Pointer[func(string)]
	// connected closes once the transport is up: the earliest moment anything can be played to
	// the Echo, ~1.5 s before the OpenAI leg is ready.
	connected     chan struct{}
	connectedOnce sync.Once
	// outPace measures the cadence at which audio is handed to the Echo. Without it there is no
	// way to tell whether a pacer upstream actually changed anything.
	outPace *paceMeter
}

// SetAudioSink installs the destination for inbound Echo audio, and SetAudioSink(nil) detaches it.
// Safe to call after Answer: packets that arrive before it is set are simply not forwarded.
//
// The nil case is why this is not a one-liner. Storing &f for a nil f puts a NON-NIL pointer to a
// NIL function in the atomic, so the reader's nil check passes and the call panics in the media
// goroutine. Detaching the sink is exactly what teardown does, so that panic sat on the ordinary
// shutdown path and could kill the process before the OpenAI hangup had been sent. Codex found it
// by reading the setter.
// SetOnDown installs the callback for a transport that has failed. Same nil-pointer trap as the
// sinks: a nil f must clear, not store a pointer to nothing.
func (s *Session) SetOnDown(f func(reason string)) {
	if f == nil {
		s.onDown.Store(nil)
		return
	}
	s.onDown.Store(&f)
}

func (s *Session) SetAudioSink(f func(*rtp.Packet)) {
	if f == nil {
		s.audioSink.Store(nil)
		return
	}
	s.audioSink.Store(&f)
}

// Connected is closed once the transport to the Echo is up.
func (s *Session) Connected() <-chan struct{} { return s.connected }

// WriteAudio sends one Opus RTP packet toward the Echo. Used to forward OpenAI's speech.
func (s *Session) WriteAudio(p *rtp.Packet) error {
	if s.audioRTP == nil {
		return errors.New("no outbound RTP track: Audio mode must be \"bridge\" or \"reflect\"")
	}
	if s.outPace != nil {
		s.outPace.mark(p.Timestamp, p.Marker)
	}
	return s.audioRTP.WriteRTP(p)
}

// InboundPackets reports how much audio has arrived from the Echo.
func (s *Session) InboundPackets() int64 { return s.inPkts.Load() }

// Age reports how long ago the SDP answer was produced.
func (s *Session) Age() time.Duration {
	if s.answerAt.IsZero() {
		return 0
	}
	return time.Since(s.answerAt)
}

// muxHolder keeps one UDP socket for all sessions (created lazily).
type muxHolder struct {
	mu   sync.Mutex
	conn *net.UDPConn
	mux  ice.UDPMux
}

var sharedMux muxHolder

func (m *muxHolder) get(port int, bindIP string) (ice.UDPMux, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mux != nil {
		return m.mux, nil
	}
	ip := net.IPv4zero
	if bindIP != "" {
		if p := net.ParseIP(bindIP); p != nil && p.To4() != nil {
			ip = p.To4()
		}
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		return nil, err
	}
	m.conn = c
	m.mux = webrtc.NewICEUDPMux(nil, c)
	return m.mux, nil
}

// addrsOfIfaces returns the IP addresses of every local interface whose name starts with
// one of the given prefixes. Used to keep virtual-bridge addresses out of the SDP answer.
// EgressIPv4 reports the local IPv4 the kernel would use to reach the public internet.
// No packet is sent: a connected UDP socket only fixes the route and the source address.
func EgressIPv4() string {
	c, err := net.Dial("udp4", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

func addrsOfIfaces(prefixes []string) []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, in := range ifs {
		match := false
		for _, p := range prefixes {
			if p != "" && strings.HasPrefix(in.Name, p) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		addrs, err := in.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}

func opusCap() webrtc.RTPCodecCapability {
	return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"}
}

func h264Cap(pl string) webrtc.RTPCodecCapability {
	return webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + pl,
		RTCPFeedback: []webrtc.RTCPFeedback{{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"}},
	}
}

// buildAPI assembles MediaEngine, interceptors and SettingEngine for one config.
func buildAPI(cfg Config, ev Events) (*webrtc.API, *stats.InterceptorFactory, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: opusCap(), PayloadType: 111}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, nil, err
	}
	if cfg.Video != "reject" {
		for pt, pl := range map[webrtc.PayloadType]string{102: "640029", 112: "42e01f"} {
			if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: h264Cap(pl), PayloadType: pt}, webrtc.RTPCodecTypeVideo); err != nil {
				return nil, nil, err
			}
		}
	}
	if cfg.Ext {
		if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: extAudioLevel}, webrtc.RTPCodecTypeAudio); err != nil {
			return nil, nil, err
		}
	}
	ir := &interceptor.Registry{}
	if err := webrtc.ConfigureRTCPReports(ir); err != nil {
		return nil, nil, err
	}
	if cfg.Nack {
		if err := webrtc.ConfigureNack(m, ir); err != nil {
			return nil, nil, err
		}
	}
	if cfg.Ext {
		// generates TWCC feedback for received packets and registers the extension
		if err := webrtc.ConfigureTWCCSender(m, ir); err != nil {
			return nil, nil, err
		}
		if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, ir); err != nil {
			return nil, nil, err
		}
	}
	sf, err := stats.NewInterceptor()
	if err != nil {
		return nil, nil, err
	}
	ir.Add(sf)

	se := webrtc.SettingEngine{}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	switch {
	case cfg.Iface != "":
		want := cfg.Iface
		se.SetInterfaceFilter(func(n string) bool { return n == want })
	case len(cfg.ExcludeIface) > 0:
		// Virtual bridges (docker/lxc/veth/tailscale/virbr) yield candidates the Amazon
		// media server can never reach; they only bloat the answer and can win the pair.
		bad := cfg.ExcludeIface
		se.SetInterfaceFilter(func(n string) bool {
			for _, p := range bad {
				if p != "" && strings.HasPrefix(n, p) {
					return false
				}
			}
			return true
		})
		// The interface filter alone is not enough: with a UDP mux bound to 0.0.0.0 the
		// candidate addresses are enumerated per local IP, so the virtual bridges come
		// back anyway (measured: docker0 won the pair). Filter by IP as well, resolving
		// the excluded interface names to their addresses at startup.
		if ips := addrsOfIfaces(bad); len(ips) > 0 {
			ev("ice_excluded_ips", "ips", ips)
			se.SetIPFilter(func(ip net.IP) bool {
				for _, x := range ips {
					if ip.String() == x {
						return false
					}
				}
				return true
			})
		}
	}
	if cfg.PublicIP != "" {
		se.SetNAT1To1IPs([]string{cfg.PublicIP}, webrtc.ICECandidateTypeHost)
	}
	if cfg.UDPPort > 0 {
		bindIP := cfg.MuxIP
		if bindIP == "auto" {
			bindIP = EgressIPv4()
		}
		if bindIP != "" {
			ev("ice_mux_bind", "ip", bindIP, "port", cfg.UDPPort)
		}
		mux, err := sharedMux.get(cfg.UDPPort, bindIP)
		if err != nil {
			return nil, nil, fmt.Errorf("udp mux: %w", err)
		}
		se.SetICEUDPMux(mux)
	}
	if cfg.ICEDisc > 0 || cfg.ICEFail > 0 || cfg.ICEKeepAlive > 0 {
		se.SetICETimeouts(cfg.ICEDisc, cfg.ICEFail, cfg.ICEKeepAlive)
	}
	if cfg.Lite {
		se.SetLite(true)
	}
	switch cfg.DTLSRole {
	case "active":
		if err := se.SetAnsweringDTLSRole(webrtc.DTLSRoleClient); err != nil {
			return nil, nil, err
		}
	case "passive":
		if err := se.SetAnsweringDTLSRole(webrtc.DTLSRoleServer); err != nil {
			return nil, nil, err
		}
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))
	return api, sf, nil
}

func pcConfig(cfg Config) webrtc.Configuration {
	c := webrtc.Configuration{}
	if cfg.PublicIP == "" && cfg.STUN != "" {
		c.ICEServers = append(c.ICEServers, webrtc.ICEServer{URLs: []string{"stun:" + cfg.STUN}})
	}
	if cfg.TURN != "" {
		c.ICEServers = append(c.ICEServers, webrtc.ICEServer{URLs: []string{cfg.TURN}, Username: cfg.TURNUser, Credential: cfg.TURNPass})
	}
	if cfg.RelayOnly {
		c.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}
	return c
}

// Answer creates the session, applies the remote offer and returns the complete
// (non-trickle) SDP answer.
func Answer(id, offerSDP string, cfg Config, ev Events) (*Session, string, error) {
	api, sf, err := buildAPI(cfg, ev)
	if err != nil {
		return nil, "", err
	}
	s := &Session{ID: id, cfg: cfg, ev: ev, offerAt: time.Now(), connected: make(chan struct{})}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	sf.OnNewPeerConnection(func(_ string, g stats.Getter) { s.getter = g })

	pc, err := api.NewPeerConnection(pcConfig(cfg))
	if err != nil {
		return nil, "", err
	}
	s.pc = pc
	s.hooks()
	// Every error return below used to leave the PeerConnection, its ICE agent and the mux port
	// open with nobody holding them; a bad offer twice in a row then found the UDP port busy.
	// The session is closed on the way out unless it is being handed over. Found by the
	// 2026-09-16 fleet.
	handedOver := false
	defer func() {
		if !handedOver {
			s.Close("answer_failed")
		}
	}()

	// ---- local media (added before SetRemoteDescription; Pion pairs them by kind) ----
	switch cfg.Audio {
	case "bridge":
		// Forwarding mode: an RTP track so Opus payloads pass through untouched. Pion rewrites
		// SSRC and payload type to this track's binding; sequence, timestamp and marker are the
		// source's. Both legs negotiate opus/48000/2 with minptime=10;useinbandfec=1, so there
		// is nothing to transcode.
		s.audioRTP, err = webrtc.NewTrackLocalStaticRTP(opusCap(), "audio", "rtcdiag")
		if err != nil {
			return nil, "", err
		}
		if _, err = pc.AddTransceiverFromTrack(s.audioRTP, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv}); err != nil {
			return nil, "", err
		}
	case "reflect":
		s.audioRTP, err = webrtc.NewTrackLocalStaticRTP(opusCap(), "audio", "rtcdiag")
		if err != nil {
			return nil, "", err
		}
		if _, err = pc.AddTransceiverFromTrack(s.audioRTP, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv}); err != nil {
			return nil, "", err
		}
		s.refl = newReflector(s.audioRTP, cfg.ReflectDelay)
	default: // file
		s.audioSmp, err = webrtc.NewTrackLocalStaticSample(opusCap(), "audio", "rtcdiag")
		if err != nil {
			return nil, "", err
		}
		if _, err = pc.AddTransceiverFromTrack(s.audioSmp, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv}); err != nil {
			return nil, "", err
		}
	}
	switch cfg.Video {
	case "inactive", "recvonly":
		_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
		_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	case "sendonly", "sendrecv":
		videoStream := cfg.VideoStreamID
		if videoStream == "" {
			videoStream = "keepalive"
		}
		s.videoSmp, err = webrtc.NewTrackLocalStaticSample(h264Cap("640029"), "video", videoStream)
		if err == nil {
			dir := webrtc.RTPTransceiverDirectionSendonly
			if cfg.Video == "sendrecv" {
				dir = webrtc.RTPTransceiverDirectionSendrecv
			}
			_, err = pc.AddTransceiverFromTrack(s.videoSmp, webrtc.RTPTransceiverInit{Direction: dir})
		}
	case "reject":
		// no local video transceiver and no H264 codec registered -> Pion rejects the m-line (port 0)
	default:
		err = fmt.Errorf("unknown video mode %q", cfg.Video)
	}
	if err != nil {
		return nil, "", err
	}

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		return nil, "", fmt.Errorf("set remote: %w", err)
	}
	// For "reject", stop any transceiver Pion auto-created for the offered video m-line so
	// the answer carries m=video 0 (RFC 3264 rejection generated by the stack, not munged).
	if cfg.Video == "reject" {
		for _, t := range pc.GetTransceivers() {
			if t.Kind() == webrtc.RTPCodecTypeVideo {
				if err := t.Stop(); err != nil {
					ev("video_stop_error", "err", err.Error())
				}
			}
		}
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return nil, "", fmt.Errorf("create answer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return nil, "", fmt.Errorf("set local: %w", err)
	}
	wait := cfg.GatherWait
	if wait <= 0 {
		wait = 3 * time.Second
	}
	t0 := time.Now()
	select {
	case <-gathered:
		ev("ice_gathering_complete", "ms", time.Since(t0).Milliseconds())
	case <-time.After(wait):
		ev("ice_gathering_timeout", "ms", time.Since(t0).Milliseconds())
	}
	final := pc.LocalDescription().SDP
	if cfg.Video == "inactive" {
		final = flipVideoDirection(final, "inactive")
	}
	final = setOpusFmtp(final, cfg.OpusFmtp)
	s.answerAt = time.Now()
	s.outPace = newPaceMeter(ev, "to_echo")
	s.describeAnswer(final)
	s.startSenders()
	go s.sampleStats()
	handedOver = true
	return s, final, nil
}

// flipVideoDirection rewrites the direction attribute (sendrecv/sendonly/recvonly/inactive)
// inside the m=video section only. It touches nothing else (port, mid, ICE, DTLS, BUNDLE),
// so DTLS/ICE stay valid — unlike munging m=video to port 0, which broke the July gateway.
func flipVideoDirection(sdp, want string) string {
	crlf := strings.Contains(sdp, "\r\n")
	lines := strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n")
	inVideo := false
	for i, ln := range lines {
		if strings.HasPrefix(ln, "m=") {
			inVideo = strings.HasPrefix(ln, "m=video")
			continue
		}
		if inVideo && (ln == "a=sendrecv" || ln == "a=sendonly" || ln == "a=recvonly" || ln == "a=inactive") {
			lines[i] = "a=" + want
		}
	}
	out := strings.Join(lines, "\n")
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	return out
}

// DefaultOpusFmtp is what each leg asks the OTHER side's encoder to produce.
//
// This is the only lever we have over audio quality. Both legs forward Opus verbatim, so we never
// encode anything: what the Echo plays was encoded by OpenAI, and what GPT hears was encoded by
// the Echo. Per RFC 7587 the fmtp parameters in an SDP describe what the SENDER OF THAT SDP WANTS
// TO RECEIVE, so the answer we give the Echo steers the Echo's encoder and the offer we give
// OpenAI steers OpenAI's encoder.
//
// Both were previously left at the browser default "minptime=10;useinbandfec=1", which states no
// bitrate at all and leaves DTX to the encoder.
//
//   - maxaveragebitrate=64000 — voice headroom; the WiFi link measured 600 Mbit/s, so the bits
//     are free and the previous unstated value let either encoder settle low.
//   - usedtx=0 — no discontinuous transmission. We forward RTP with the source's own sequence and
//     timestamp, so every gap the encoder takes becomes a gap the far jitter buffer must bridge.
//   - stereo=0 / sprop-stereo=0 — one voice, mono; stop any encoder spending bits on a stereo image.
//   - maxplaybackrate=48000 — fullband is playable, do not fall back to narrowband.
const DefaultOpusFmtp = "minptime=10;useinbandfec=1;usedtx=0;stereo=0;sprop-stereo=0;maxplaybackrate=48000;maxaveragebitrate=64000"

// setOpusFmtp rewrites the a=fmtp line of the Opus payload type inside the m=audio section, and
// nothing else. Same discipline as flipVideoDirection: the port, mid, ICE and DTLS lines are left
// exactly as Pion produced them, so the description stays valid. Passing an empty params string
// leaves the SDP untouched.
func setOpusFmtp(sdp, params string) string {
	if params == "" {
		return sdp
	}
	crlf := strings.Contains(sdp, "\r\n")
	lines := strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n")
	inAudio := false
	pt := ""
	fmtpAt, rtpmapAt := -1, -1
	for i, ln := range lines {
		if strings.HasPrefix(ln, "m=") {
			if inAudio {
				break // only the first m=audio section
			}
			inAudio = strings.HasPrefix(ln, "m=audio")
			continue
		}
		if !inAudio {
			continue
		}
		if pt == "" && strings.HasPrefix(ln, "a=rtpmap:") {
			rest := strings.TrimPrefix(ln, "a=rtpmap:")
			num, codec, ok := strings.Cut(rest, " ")
			if ok && strings.HasPrefix(strings.ToLower(codec), "opus/") {
				pt, rtpmapAt = num, i
			}
			continue
		}
		if pt != "" && strings.HasPrefix(ln, "a=fmtp:"+pt+" ") {
			fmtpAt = i
		}
	}
	if pt == "" {
		return sdp // no Opus offered; leave the description alone
	}
	want := "a=fmtp:" + pt + " " + params
	switch {
	case fmtpAt >= 0:
		lines[fmtpAt] = want
	case rtpmapAt >= 0:
		lines = append(lines[:rtpmapAt+1], append([]string{want}, lines[rtpmapAt+1:]...)...)
	}
	out := strings.Join(lines, "\n")
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	return out
}

func (s *Session) describeAnswer(sdp string) {
	var lines []string
	for _, ln := range strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(ln, "m=") || strings.HasPrefix(ln, "a=group") || strings.HasPrefix(ln, "a=setup") ||
			strings.HasPrefix(ln, "a=sendrecv") || strings.HasPrefix(ln, "a=sendonly") || strings.HasPrefix(ln, "a=recvonly") ||
			strings.HasPrefix(ln, "a=inactive") || strings.HasPrefix(ln, "a=extmap") || strings.HasPrefix(ln, "a=rtpmap") ||
			// the codec parameters decide what the Echo's encoder produces, which is the whole
			// of the inbound audio quality: a summary that hides them hides the answer
			strings.HasPrefix(ln, "a=fmtp") || strings.HasPrefix(ln, "a=ice-lite") {
			lines = append(lines, ln)
		}
	}
	n := 0
	for _, ln := range strings.Split(sdp, "\n") {
		if strings.HasPrefix(ln, "a=candidate") {
			n++
		}
	}
	s.ev("answer_summary", "candidates", n, "lines", lines)
}

func (s *Session) hooks() {
	pc := s.pc
	pc.OnICEGatheringStateChange(func(st webrtc.ICEGatheringState) { s.ev("ice_gathering_state", "state", st.String()) })
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			s.ev("local_candidate", "type", c.Typ.String(), "addr", c.Address, "port", c.Port, "proto", c.Protocol.String())
		}
	})
	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) {
		s.ev("ice_connection_state", "state", st.String())
		if st == webrtc.ICEConnectionStateConnected || st == webrtc.ICEConnectionStateCompleted {
			s.logSelectedPair()
		}
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		s.ev("peer_connection_state", "state", st.String(), "since_answer_ms", s.sinceAnswer())
		if st == webrtc.PeerConnectionStateConnected && s.connectedAt.IsZero() {
			s.connectedAt = time.Now()
			s.ev("dtls_state", "state", s.dtlsState())
		}
		if st == webrtc.PeerConnectionStateConnected {
			s.connectedOnce.Do(func() { close(s.connected) })
		}
		// Failed is terminal: consent checks stopped being answered and the transport is gone.
		// Disconnected is not reported, since it recovers on its own often enough.
		if st == webrtc.PeerConnectionStateFailed {
			if f := s.onDown.Load(); f != nil && *f != nil {
				go (*f)("echo_transport_failed")
			}
		}
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, rc *webrtc.RTPReceiver) {
		s.remoteSSRCs.Store(uint32(tr.SSRC()), tr.Kind().String())
		s.ev("remote_track", "kind", tr.Kind().String(), "mime", tr.Codec().MimeType, "pt", tr.PayloadType(), "ssrc", uint32(tr.SSRC()), "fmtp", tr.Codec().SDPFmtpLine)
		go s.readRTCP(rc, "receiver")
		if tr.Kind() == webrtc.RTPCodecTypeAudio {
			go s.readAudio(tr, rc)
		} else {
			go s.drain(tr)
		}
	})
}

func (s *Session) sinceAnswer() int64 {
	if s.answerAt.IsZero() {
		return -1
	}
	return time.Since(s.answerAt).Milliseconds()
}

func (s *Session) dtlsState() string {
	if sc := s.pc.SCTP(); sc != nil && sc.Transport() != nil {
		return sc.Transport().State().String()
	}
	return "unknown"
}

func (s *Session) logSelectedPair() {
	sc := s.pc.SCTP()
	if sc == nil || sc.Transport() == nil || sc.Transport().ICETransport() == nil {
		return
	}
	p, err := sc.Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || p == nil || p.Local == nil || p.Remote == nil {
		return
	}
	s.ev("selected_pair", "local", fmt.Sprintf("%s:%d/%s", p.Local.Address, p.Local.Port, p.Local.Typ), "remote", fmt.Sprintf("%s:%d/%s", p.Remote.Address, p.Remote.Port, p.Remote.Typ))
}

func (s *Session) audioLevelID(rc *webrtc.RTPReceiver) uint8 {
	for _, e := range rc.GetParameters().HeaderExtensions {
		if e.URI == extAudioLevel {
			return uint8(e.ID)
		}
	}
	return 0
}

func (s *Session) readAudio(tr *webrtc.TrackRemote, rc *webrtc.RTPReceiver) {
	levelID := s.audioLevelID(rc)
	var lastSeq uint16
	var haveSeq bool
	var gaps int64
	var levelSum, levelN int64
	var voiceN int64
	lastReport := time.Now()
	// The Echo's own delivery rhythm, for comparison with OpenAI's. Both are forwarded untouched,
	// so whichever arrives unevenly hands that unevenness to the far jitter buffer.
	pace := newPaceMeter(s.ev, "echo")
	defer pace.done()
	for {
		pkt, _, err := tr.ReadRTP()
		if err != nil {
			s.ev("audio_read_end", "err", err.Error(), "pkts", s.inPkts.Load())
			return
		}
		pace.mark(pkt.Timestamp, pkt.Marker)
		now := time.Now().UnixMilli()
		if s.inPkts.Add(1) == 1 {
			s.firstIn.Store(now)
			s.ev("first_rtp_in", "since_answer_ms", s.sinceAnswer(), "pt", pkt.PayloadType, "ssrc", pkt.SSRC, "seq", pkt.SequenceNumber, "ts", pkt.Timestamp, "ext", pkt.Extension, "payload_bytes", len(pkt.Payload))
		}
		s.lastIn.Store(now)
		s.inBytes.Add(int64(len(pkt.Payload)))
		if haveSeq && pkt.SequenceNumber != lastSeq+1 {
			gaps++
		}
		lastSeq, haveSeq = pkt.SequenceNumber, true
		if levelID != 0 {
			if b := pkt.GetExtension(levelID); b != nil {
				var al rtp.AudioLevelExtension
				if al.Unmarshal(b) == nil {
					levelSum += int64(al.Level)
					levelN++
					if al.Voice {
						voiceN++
					}
				}
			}
		}
		if s.refl != nil {
			s.refl.push(pkt)
		}
		if sink := s.audioSink.Load(); sink != nil && *sink != nil {
			(*sink)(pkt)
		}
		if time.Since(lastReport) >= 5*time.Second {
			lastReport = time.Now()
			kv := []any{"pkts", s.inPkts.Load(), "bytes", s.inBytes.Load(), "seq_gaps", gaps}
			if levelN > 0 {
				kv = append(kv, "avg_level_dbov", -float64(levelSum)/float64(levelN), "voice_frac", float64(voiceN)/float64(levelN))
				levelSum, levelN, voiceN = 0, 0, 0
			}
			s.ev("audio_in_5s", kv...)
		}
	}
}

func (s *Session) drain(tr *webrtc.TrackRemote) {
	n := 0
	for {
		if _, _, err := tr.ReadRTP(); err != nil {
			return
		}
		n++
		if n == 1 {
			s.ev("first_video_rtp_in", "since_answer_ms", s.sinceAnswer())
		}
	}
}

// readRTCP drains and logs RTCP for a sender or receiver (required so interceptors run).
func (s *Session) readRTCP(r interface {
	ReadRTCP() ([]rtcp.Packet, interceptor.Attributes, error)
}, side string) {
	for {
		pkts, _, err := r.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch v := p.(type) {
			case *rtcp.SenderReport:
				s.ev("rtcp_in", "side", side, "type", "SR", "ssrc", v.SSRC, "pkts", v.PacketCount, "octets", v.OctetCount, "reports", len(v.Reports))
			case *rtcp.ReceiverReport:
				var blocks []map[string]any
				for _, rb := range v.Reports {
					blocks = append(blocks, map[string]any{"ssrc": rb.SSRC, "lost": rb.TotalLost, "fraction": rb.FractionLost, "jitter": rb.Jitter, "last_seq": rb.LastSequenceNumber})
				}
				s.ev("rtcp_in", "side", side, "type", "RR", "ssrc", v.SSRC, "blocks", blocks)
			case *rtcp.Goodbye:
				s.ev("rtcp_in", "side", side, "type", "BYE", "sources", v.Sources, "reason", v.Reason, "since_answer_ms", s.sinceAnswer())
			case *rtcp.PictureLossIndication:
				s.ev("rtcp_in", "side", side, "type", "PLI", "media_ssrc", v.MediaSSRC)
			case *rtcp.FullIntraRequest:
				s.ev("rtcp_in", "side", side, "type", "FIR", "media_ssrc", v.MediaSSRC)
			case *rtcp.TransportLayerNack:
				s.ev("rtcp_in", "side", side, "type", "NACK", "media_ssrc", v.MediaSSRC, "n", len(v.Nacks))
			case *rtcp.TransportLayerCC:
				s.ev("rtcp_in", "side", side, "type", "TWCC", "media_ssrc", v.MediaSSRC, "base_seq", v.BaseSequenceNumber, "status_count", v.PacketStatusCount)
			case *rtcp.ReceiverEstimatedMaximumBitrate:
				s.ev("rtcp_in", "side", side, "type", "REMB", "bitrate", v.Bitrate)
			case *rtcp.SourceDescription:
				// noisy; count only
			default:
				s.ev("rtcp_in", "side", side, "type", fmt.Sprintf("%T", p))
			}
		}
	}
}

// startSenders begins the outbound media sources and RTCP readers on senders.
func (s *Session) startSenders() {
	for _, snd := range s.pc.GetSenders() {
		if snd.Track() == nil {
			continue
		}
		params := snd.GetParameters()
		var ssrc uint32
		if len(params.Encodings) > 0 {
			ssrc = uint32(params.Encodings[0].SSRC)
		}
		s.ev("local_sender", "kind", snd.Track().Kind().String(), "ssrc", ssrc, "codec", params.Codecs)
		go s.readRTCP(snd, "sender")
	}
	if s.audioSmp != nil && s.cfg.AudioFile != "" {
		go playOggOpus(s.ctx, s.cfg.AudioFile, s.audioSmp, func(err error) { s.ev("audio_file_error", "err", err.Error()) })
	}
	if s.videoSmp != nil && s.cfg.VideoFile != "" {
		go playH264(s.ctx, s.cfg.VideoFile, s.cfg.VideoFPS, s.videoSmp, func(err error) { s.ev("video_file_error", "err", err.Error()) },
			func(n, b int) {
				s.vidFrames.Store(int64(n))
				if n == 1 || n%30 == 0 {
					s.ev("video_frame_sent", "n", n, "bytes", b)
				}
			})
	}
}

// sampleStats logs per-SSRC RTP stats and the nominated candidate pair every second.
func (s *Session) sampleStats() {
	tk := time.NewTicker(1 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tk.C:
		}
		s.sampleOnce()
	}
}

// sampleOnce takes one stats sample without being able to kill the process. Diagnostics must
// never take down the assistant: a nil dereference in here once ended a live call at 52 s.
func (s *Session) sampleOnce() {
	defer func() {
		if r := recover(); r != nil {
			s.ev("stats_panic_recovered", "panic", fmt.Sprint(r),
				"note", "a stats sample failed; the session continues")
		}
	}()
	s.logStats("stats_1s")
}

func (s *Session) logStats(ev string) {
	kv := []any{"since_answer_ms", s.sinceAnswer(), "in_pkts", s.inPkts.Load(), "in_bytes", s.inBytes.Load()}
	if v := s.vidFrames.Load(); v > 0 {
		kv = append(kv, "video_frames_sent", v)
	}
	if li := s.lastIn.Load(); li > 0 {
		kv = append(kv, "ms_since_last_in", time.Now().UnixMilli()-li)
	}
	if s.getter != nil {
		for _, snd := range s.pc.GetSenders() {
			p := snd.GetParameters()
			if len(p.Encodings) == 0 {
				continue
			}
			st := s.getter.Get(uint32(p.Encodings[0].SSRC))
			if st == nil {
				continue
			}
			// A sender's track is cleared during teardown, so this raced with a closing session
			// and took the whole bridge down with a nil dereference 52 s into a live call.
			// startSenders already guarded it; this did not.
			tr := snd.Track()
			if tr == nil {
				continue
			}
			kv = append(kv, "out_"+tr.Kind().String(), map[string]any{
				"ssrc": uint32(p.Encodings[0].SSRC), "pkts_sent": st.OutboundRTPStreamStats.PacketsSent, "bytes_sent": st.OutboundRTPStreamStats.BytesSent,
				"remote_lost": st.RemoteInboundRTPStreamStats.PacketsLost, "remote_fraction_lost": st.RemoteInboundRTPStreamStats.FractionLost,
				"remote_jitter": st.RemoteInboundRTPStreamStats.Jitter, "rtt_ms": st.RemoteInboundRTPStreamStats.RoundTripTime.Milliseconds(),
				"rr_count": st.RemoteInboundRTPStreamStats.RoundTripTimeMeasurements,
			})
		}
		s.remoteSSRCs.Range(func(k, v any) bool {
			ssrc := k.(uint32)
			st := s.getter.Get(ssrc)
			if st != nil {
				kv = append(kv, "in_"+v.(string), map[string]any{
					"ssrc": ssrc, "pkts": st.InboundRTPStreamStats.PacketsReceived, "lost": st.InboundRTPStreamStats.PacketsLost,
					"jitter": st.InboundRTPStreamStats.Jitter, "last_pkt_ms_ago": time.Since(st.InboundRTPStreamStats.LastPacketReceivedTimestamp).Milliseconds(),
					"remote_sr_pkts": st.RemoteOutboundRTPStreamStats.PacketsSent,
				})
			}
			return true
		})
	}
	// ICE candidate pair stats from the PeerConnection stats report (STUN request/response counters)
	for _, st := range s.pc.GetStats() {
		if cp, ok := st.(webrtc.ICECandidatePairStats); ok && cp.Nominated {
			kv = append(kv, "pair", map[string]any{
				"state": string(cp.State), "req_sent": cp.RequestsSent, "req_recv": cp.RequestsReceived,
				"resp_sent": cp.ResponsesSent, "resp_recv": cp.ResponsesReceived, "consent_req_sent": cp.ConsentRequestsSent,
				"rtt_s": cp.CurrentRoundTripTime, "bytes_sent": cp.BytesSent, "bytes_recv": cp.BytesReceived,
			})
		}
	}
	kv = append(kv, "ice", s.pc.ICEConnectionState().String(), "dtls", s.dtlsState(), "pc", s.pc.ConnectionState().String())
	s.ev(ev, kv...)
}

// Close tears the session down (idempotent) after logging final stats.
func (s *Session) Close(reason string) {
	s.closeOnce.Do(func() {
		s.logStats("stats_final")
		s.ev("session_close", "reason", reason, "since_answer_ms", s.sinceAnswer(), "in_pkts", s.inPkts.Load(), "video_frames_sent", s.vidFrames.Load())
		s.cancel()
		if s.refl != nil {
			s.refl.close()
		}
		// Say goodbye on the wire before pulling it. Measured on the first goodbye call that ended
		// from this side (2026-09-14 01:45): the transport was closed at +0.5 s and Alexa's own
		// disconnect arrived 6.1 s later, which is Amazon's media server noticing on its own that
		// the far end had gone. RFC 3550's BYE exists to make that noticing unnecessary: "indicates
		// that one or more sources are no longer active". Pion does not send one on Close.
		var gone []uint32
		for _, snd := range s.pc.GetSenders() {
			if p := snd.GetParameters(); len(p.Encodings) > 0 && p.Encodings[0].SSRC != 0 {
				gone = append(gone, uint32(p.Encodings[0].SSRC))
			}
		}
		if len(gone) > 0 {
			err := s.pc.WriteRTCP([]rtcp.Packet{&rtcp.Goodbye{Sources: gone, Reason: reason}})
			s.ev("rtcp_bye_sent", "sources", len(gone), "err_if_any", errText(err))
		}
		_ = s.pc.Close()
	})
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ErrNoSession is returned when acting on a missing session.
var ErrNoSession = errors.New("no such session")
