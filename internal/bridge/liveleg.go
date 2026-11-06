package bridge

// The OpenAI-facing half of the bridge: a second PeerConnection that carries the same Opus
// payloads the Echo produced, with no transcoding in between.
//
// Both legs negotiate `opus/48000/2` with `minptime=10;useinbandfec=1`, so a packet received from
// Amazon can be written straight onto this leg's track. Pion rewrites SSRC and payload type to
// match this leg's binding. Opus payloads and marker bits stay unchanged. On the
// return path, a discontinuity between OpenAI's probe and second packet is rebased
// once so the Echo receives a continuous sequence and timestamp timeline.
//
// Two shape differences from the Alexa leg, both deliberate:
//   - NO video. The Echo's offer carries an H.264 m-line and Amazon needs video FLOWING to keep
//     its session alive, but that is Amazon's requirement, not OpenAI's. gpt-live-1 declares
//     image and video unsupported, so this leg offers audio only.
//   - A data channel labelled `oai-events`. GPT-Live's docs uniformly assume one on WebRTC and an
//     audio-only Live offer is undocumented; creating it costs nothing and removes the risk.
//     Session control still happens server-side over the sideband socket.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/u2n4/echo-gpt-live/internal/openai"
)

// LiveLegConfig configures the OpenAI-facing PeerConnection.
type LiveLegConfig struct {
	MuxIP        string // bind ICE to one local address ("auto" = egress)
	UDPPort      int    // 0 = ephemeral (a second mux port would collide with the Alexa leg)
	ExcludeIface []string
	STUN         string
	GatherWait   time.Duration
	// OpusFmtp is what we ask OpenAI's encoder to produce. What the Echo plays is encoded here
	// and forwarded verbatim, so this line is the only control we have over its quality.
	OpusFmtp string
	// SetMarker applies marker=true to every packet forwarded to OpenAI. Undocumented, but the
	// one prior-art bridge (shinyoshiaki/alexa-chat-gpt) did it deliberately. Off by default.
	SetMarker bool
	// MaxAge hangs up the session no matter what else happens. GPT-Live bills wall clock, so a
	// stuck session with no directive and no silence must still have a ceiling. 0 disables.
	MaxAge time.Duration
}

// LiveLeg is one established GPT-Live session plus its media transport.
type LiveLeg struct {
	SessionID string

	pc       *webrtc.PeerConnection
	outTrack *webrtc.TrackLocalStaticRTP
	client   *openai.LiveClient
	sideband *openai.LiveSideband
	ev       Events
	cfg      LiveLegConfig
	getter   stats.Getter
	inSSRC   atomic.Uint32 // the remote audio track's SSRC, once OnTrack has fired

	cancel context.CancelFunc
	ctx    context.Context

	inPkts, outPkts atomic.Int64
	endedAt         atomic.Int64 // unix nanos when Close ran; Age freezes there
	firstIn         atomic.Int64 // ms after dial
	lastVoice       atomic.Int64 // unix nanos of the last packet that carried speech, not silence
	senderReport    atomic.Pointer[liveSenderReport]
	started         time.Time
	closeOnce       sync.Once

	// audioSink receives OpenAI's speech, to be forwarded toward the Echo.
	audioSink atomic.Pointer[func(*rtp.Packet)]
	// startupAudio is installed before the PeerConnection can deliver a remote track. DialLive
	// returns only after the session is ready, so the caller cannot install its real sink sooner.
	// Keep those first packets instead of silently dropping the start of the assistant's greeting.
	sinkMu       sync.Mutex
	startupAudio *HoldSink
	// onSpeech and onSidebandEvent are set after DialLive returns, while the readers it started are
	// already running. Plain function fields were a data race; these are not. Codex found it.
	onSpeech        atomic.Pointer[func()]
	onSidebandEvent atomic.Pointer[func(typ string, raw []byte)]
	onClose         atomic.Pointer[func(reason string)]
	// sidebandLive says whether the control socket is still delivering. The transcript is the only
	// evidence this bridge has of a PERSON speaking rather than the Echo streaming an empty room,
	// so when it stops arriving the caller has to know: an idle timer driven by a dead socket would
	// close a conversation that is still happening.
	sidebandLive   atomic.Bool
	sidebandLostAt atomic.Int64 // unix nanos when the reader gave up for good; 0 otherwise
}

type liveStartup struct {
	pcConnected    chan struct{}
	sessionStarted chan struct{}
	pcOnce         sync.Once
	sessionOnce    sync.Once
}

var errLiveStartupTimeout = errors.New("the session did not become ready")

var errLegClosed = errors.New("the leg closed while starting")

func newLiveStartup() *liveStartup {
	return &liveStartup{pcConnected: make(chan struct{}), sessionStarted: make(chan struct{})}
}

func (s *liveStartup) connected() { s.pcOnce.Do(func() { close(s.pcConnected) }) }
func (s *liveStartup) started()   { s.sessionOnce.Do(func() { close(s.sessionStarted) }) }

func (s *liveStartup) wait(ctx, legCtx context.Context, timeout time.Duration) error {
	canceled := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if legCtx.Err() != nil {
			return errLegClosed
		}
		return nil
	}
	if err := canceled(); err != nil {
		return err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	for _, ready := range []<-chan struct{}{s.pcConnected, s.sessionStarted} {
		select {
		case <-ready:
		case <-ctx.Done():
			return ctx.Err()
		case <-legCtx.Done():
			return errLegClosed
		case <-t.C:
			return errLiveStartupTimeout
		}
		if err := canceled(); err != nil {
			return err
		}
	}
	return canceled()
}

// SetOnSpeech installs the callback fired when either side produces audio.
func (l *LiveLeg) SetOnSpeech(f func()) {
	if f == nil {
		l.onSpeech.Store(nil)
		return
	}
	l.onSpeech.Store(&f)
}

// SetOnSidebandEvent installs the callback for decoded control events.
func (l *LiveLeg) SetOnSidebandEvent(f func(typ string, raw []byte)) {
	if f == nil {
		l.onSidebandEvent.Store(nil)
		return
	}
	l.onSidebandEvent.Store(&f)
}

// SidebandLive reports whether the control socket is still delivering events. False means the
// transcript has stopped arriving, and anything that treats "no transcript" as "nobody is here"
// must stop doing so.
func (l *LiveLeg) SidebandLive() bool { return l.sidebandLive.Load() }

// SidebandLostFor is how long ago the control socket was given up on, or zero while it is alive
// or still being reattached. The packet fallback that feeds the idle timer after a loss is
// bounded by it: the Echo streams the room continuously, so packets alone never stop.
func (l *LiveLeg) SidebandLostFor() time.Duration {
	at := l.sidebandLostAt.Load()
	if at == 0 {
		return 0
	}
	return time.Since(time.Unix(0, at))
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// SetAudioSink installs the destination for OpenAI's inbound speech, and SetAudioSink(nil) detaches
// it. Same trap as the Alexa leg: &f for a nil f is a non-nil pointer to a nil function, which
// passes a nil check and then panics on the call.
func (l *LiveLeg) SetAudioSink(f func(*rtp.Packet)) {
	l.sinkMu.Lock()
	defer l.sinkMu.Unlock()
	if h := l.startupAudio; h != nil {
		if f == nil {
			h.Drop()
			l.startupAudio = nil
			l.audioSink.Store(nil)
		} else {
			h.Release(f)
			l.startupAudio = nil
		}
		// Keep HoldSink.Write installed after release. A reader may already have loaded that
		// function while Release held its lock; swapping to f here would let a later packet call f
		// directly and overtake that waiting reader. Released HoldSink forwards directly itself.
		return
	}
	if f == nil {
		l.audioSink.Store(nil)
		return
	}
	l.audioSink.Store(&f)
}

func (l *LiveLeg) holdStartupAudio(maxPackets int) {
	l.startupAudio = NewHoldSink(maxPackets)
	startupSink := l.startupAudio.Write
	l.audioSink.Store(&startupSink)
}

// WriteAudio forwards one Opus packet from the Echo to OpenAI.
func (l *LiveLeg) WriteAudio(p *rtp.Packet) error {
	if l.outTrack == nil {
		return errors.New("live leg has no outbound track")
	}
	if l.cfg.SetMarker {
		// Copy the header rather than mutating the caller's packet: the same packet may also be
		// handed to other sinks (logging, recording) and must not be altered underneath them.
		q := *p
		q.Header = p.Header
		q.Header.Marker = true
		if err := l.outTrack.WriteRTP(&q); err != nil {
			return err
		}
	} else if err := l.outTrack.WriteRTP(p); err != nil {
		return err
	}
	l.outPkts.Add(1)
	return nil
}

// sidebandStallAfter is how long the control socket may say nothing, while audio is being sent to
// the session, before it is treated as dead. OpenAI announces every chunk of input audio on it,
// about five times a second, so ten seconds of silence is fifty missing announcements. Measured on
// the 17:23 call of 2026-09-14: the socket went from fifty events per ten seconds to none at 61 s,
// stayed "open" with no error, and the transcript-driven idle timer ended the call at 119 s while
// he was talking and she was answering.
const sidebandStallAfter = 10 * time.Second

// voicePayloadMin separates OpenAI's speech from its silence, by the one thing visible without
// decoding: the size of the Opus frame.
//
// Measured on the 22:08 call of 2026-09-13, from the recording: while she is silent OpenAI still
// sends fifty packets a second, each a constant 72 bytes (and 3-byte frames in the first second);
// while she speaks they run 100-262 bytes. The sideband's session.output_audio.delta is NOT a
// speech signal - it flowed at four to seven a second through sixteen seconds of digital silence
// (-112 dBFS in the recording). Everything that waited for "her to stop talking" was waiting for
// an event that never stops.
const voicePayloadMin = 90

// LastVoice is when the assistant last produced audible speech, or the zero time if never.
func (l *LiveLeg) LastVoice() time.Time {
	ns := l.lastVoice.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Stats reports what has crossed this leg.
func (l *LiveLeg) Stats() (in, out int64, firstInMs int64) {
	return l.inPkts.Load(), l.outPkts.Load(), l.firstIn.Load()
}

// Age reports how long the leg was up, which is what GPT-Live bills.
//
// It stops at the moment of closure. It used to keep running, so a leg closed by the max-age guard
// or by a dead transport and cleaned up later charged the budget for all the time in between, when
// the session had already been hung up and was costing nothing. Codex found it.
func (l *LiveLeg) Age() time.Duration {
	if l.started.IsZero() {
		return 0
	}
	if end := l.endedAt.Load(); end != 0 {
		return time.Unix(0, end).Sub(l.started)
	}
	return time.Since(l.started)
}

// Sideband exposes the control socket so commentary and thinking can be appended.
func (l *LiveLeg) Sideband() *openai.LiveSideband { return l.sideband }

// DialLive builds the OpenAI-facing PeerConnection, creates (or forks) a GPT-Live session, and
// returns once the SDP answer has been applied. forkFrom, when non-empty, resumes a stored
// session instead of starting a new conversation.
func DialLive(
	ctx context.Context,
	client *openai.LiveClient,
	sessCfg openai.LiveSessionConfig,
	cfg LiveLegConfig,
	forkFrom string,
	ev Events,
) (*LiveLeg, error) {
	if ev == nil {
		ev = func(string, ...any) {}
	}
	l := &LiveLeg{client: client, ev: ev, cfg: cfg}
	l.ctx, l.cancel = context.WithCancel(context.Background())
	l.holdStartupAudio(150)
	startup := newLiveStartup()

	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: opusCap(), PayloadType: 111}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.ConfigureRTCPReports(ir); err != nil {
		return nil, err
	}
	// Pion's own count of packets received on this leg, taken below the application. Set beside
	// the count the reader has taken, it says whether a silent leg is one nothing reached or one
	// the reader stopped draining.
	sf, err := stats.NewInterceptor()
	if err != nil {
		return nil, err
	}
	ir.Add(sf)
	sf.OnNewPeerConnection(func(_ string, g stats.Getter) { l.getter = g })
	se := webrtc.SettingEngine{}
	configureLiveReplayProtection(&se)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	// Same lesson as the Alexa leg: a mux bound to every address ignores the interface and IP
	// filters, so bind to one address instead of filtering.
	bindIP := cfg.MuxIP
	if bindIP == "auto" {
		bindIP = EgressIPv4()
	}
	if bindIP != "" {
		se.SetNAT1To1IPs([]string{bindIP}, webrtc.ICECandidateTypeHost)
		ev("live_leg_bind_hint", "ip", bindIP)
	}
	if len(cfg.ExcludeIface) > 0 {
		bad := cfg.ExcludeIface
		se.SetInterfaceFilter(func(n string) bool {
			for _, p := range bad {
				if p != "" && strings.HasPrefix(n, p) {
					return false
				}
			}
			return true
		})
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))

	pcCfg := webrtc.Configuration{}
	if cfg.STUN != "" {
		pcCfg.ICEServers = []webrtc.ICEServer{{URLs: []string{"stun:" + cfg.STUN}}}
	}
	pc, err := api.NewPeerConnection(pcCfg)
	if err != nil {
		return nil, err
	}
	l.pc = pc

	l.outTrack, err = webrtc.NewTrackLocalStaticRTP(opusCap(), "audio", "echo")
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	if _, err = pc.AddTransceiverFromTrack(l.outTrack, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv}); err != nil {
		_ = pc.Close()
		return nil, err
	}

	dc, err := pc.CreateDataChannel(openai.LiveDataChannelLabel, nil)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	dc.OnOpen(func() { ev("live_leg_datachannel_open") })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		t := eventType(msg.Data)
		ev("live_dc_event", "type", t, "bytes", len(msg.Data))
		if t == "session.started" {
			startup.started()
		}
	})

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		ev("live_leg_ice", "state", s.String())
		// A dead transport cannot carry audio, but the session keeps billing until hung up.
		if s == webrtc.ICEConnectionStateFailed || s == webrtc.ICEConnectionStateClosed {
			go l.Close("transport_" + s.String())
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		ev("live_leg_pc", "state", s.String(), "ms", time.Since(l.started).Milliseconds())
		if s == webrtc.PeerConnectionStateConnected {
			startup.connected()
		}
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, rc *webrtc.RTPReceiver) {
		ev("live_leg_remote_track", "kind", tr.Kind().String(), "mime", tr.Codec().MimeType,
			"pt", tr.PayloadType(), "fmtp", tr.Codec().SDPFmtpLine, "ssrc", uint32(tr.SSRC()))
		l.inSSRC.Store(uint32(tr.SSRC()))
		go l.drainRTCP(rc, ev)
		go l.readInbound(tr)
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		_ = pc.Close()
		return nil, err
	}
	wait := cfg.GatherWait
	if wait <= 0 {
		wait = 3 * time.Second
	}
	select {
	case <-gathered:
	case <-time.After(wait):
		ev("live_leg_gather_timeout", "waited", wait.String())
	}
	offerSDP := setOpusFmtp(pc.LocalDescription().SDP, cfg.OpusFmtp)
	ev("live_leg_offer", "has_m_video", strings.Contains(offerSDP, "m=video"),
		"has_m_application", strings.Contains(offerSDP, "m=application"), "bytes", len(offerSDP))

	l.started = time.Now()
	var sess *openai.LiveSession
	forked := false
	if forkFrom != "" {
		sess, err = client.Fork(ctx, forkFrom, offerSDP)
		if err != nil {
			// A fork can fail because the stored session expired; a fresh session is still
			// better than no assistant, so fall back and say so.
			ev("live_fork_failed", "source", forkFrom, "err", err.Error(), "action", "creating a fresh session")
			sess, err = client.CreateSession(ctx, offerSDP, sessCfg)
		} else {
			forked = true
			ev("live_forked", "from", forkFrom, "to", sess.ID)
		}
	} else {
		sess, err = client.CreateSession(ctx, offerSDP, sessCfg)
	}
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	l.SessionID = sess.ID
	// The session is on the ledger already: the client wrote the line right after the budget
	// charge, before it handed the session back, so no gap between the two exists to be killed in.
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sess.Answer}); err != nil {
		// The session exists and is billing. Closing only the transport left the hangup to a side
		// effect of Pion's state machine; say it outright. Grok found it.
		l.Close("bad_answer")
		return nil, fmt.Errorf("apply OpenAI answer: %w", err)
	}
	summarizeLiveAnswer(ev, sess.Answer)

	if sb, aerr := client.Attach(ctx, sess.ID); aerr != nil {
		ev("live_attach_failed", "err", aerr.Error(), "note", "control falls back to the data channel")
	} else {
		l.sideband = sb
		// A fork inherits the stored session's instructions, which were written for another day:
		// the date, the weather and the memory in them are stale. Append today's.
		if forked && sessCfg.Instructions != "" {
			if ierr := sb.Instructions(ctx, sessCfg.Instructions); ierr != nil {
				ev("live_fork_instructions_failed", "err", ierr.Error())
			}
		}
		handle := func(typ string, raw []byte) {
			ev("live_sideband_event", "type", typ)
			if typ == "session.started" {
				startup.started()
			}
			if h := l.onSidebandEvent.Load(); h != nil {
				(*h)(typ, raw)
			}
		}
		go func() {
			defer func() {
				if p := recover(); p != nil {
					ev("live_sideband_panic_recovered", "panic", fmt.Sprint(p))
					l.sidebandLive.Store(false)
				}
			}()
			l.sidebandLive.Store(true)
			for attempt := 0; ; attempt++ {
				err := sb.Read(l.ctx, handle)
				if l.ctx.Err() != nil {
					l.sidebandLive.Store(false)
					return
				}
				// The socket ended - kicked by the stall watcher below, or dropped by the network.
				// The session is still up and billing, so the transcript it carries is still the
				// only thing that can end the call for the right reason. Attach again.
				if attempt >= 3 {
					l.sidebandLive.Store(false)
					l.sidebandLostAt.Store(time.Now().UnixNano())
					ev("live_sideband_lost", "err", errString(err), "attempts", attempt,
						"note", "transcript-driven idle detection falls back to packet arrival for a bounded time")
					return
				}
				rctx, rcancel := context.WithTimeout(l.ctx, 10*time.Second)
				rerr := sb.Reattach(rctx)
				rcancel()
				if rerr != nil {
					l.sidebandLive.Store(false)
					ev("live_sideband_lost", "err", errString(err), "reattach_err", rerr.Error(),
						"note", "transcript-driven idle detection falls back to packet arrival")
					return
				}
				ev("live_sideband_reattached", "sockets", sb.Attached(), "after", errString(err))
				l.sidebandLive.Store(true)
			}
		}()
		// The stall watcher. A socket that has delivered nothing for this long while audio is
		// still being sent to the session is not idle, it is dead without saying so, and the
		// reader above cannot tell: Read blocks. Closing the socket makes it return.
		go func() {
			defer func() {
				if p := recover(); p != nil {
					ev("live_stallwatch_panic_recovered", "panic", fmt.Sprint(p))
				}
			}()
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			var lastOut int64
			for {
				select {
				case <-l.ctx.Done():
					return
				case <-t.C:
				}
				out := l.outPkts.Load()
				sending := out > lastOut
				lastOut = out
				if !sending || !l.sidebandLive.Load() || sb.Silent() < sidebandStallAfter {
					continue
				}
				ev("live_sideband_stalled", "silent_s", sb.Silent().Seconds(),
					"note", "audio is flowing to the session and its socket has said nothing; reattaching")
				sb.Kick()
				// Give the reader time to notice and reattach before judging again.
				select {
				case <-l.ctx.Done():
					return
				case <-time.After(sidebandStallAfter):
				}
			}
		}()
	}

	if cfg.MaxAge > 0 {
		go func() {
			defer func() {
				if p := recover(); p != nil {
					ev("live_maxage_panic_recovered", "panic", fmt.Sprint(p))
				}
			}()
			select {
			case <-l.ctx.Done():
			case <-time.After(cfg.MaxAge):
				ev("live_max_age_reached", "max", cfg.MaxAge.String(),
					"note", "hanging up regardless: GPT-Live bills wall clock")
				l.Close("max_age")
			}
		}()
	}

	if err := startup.wait(ctx, l.ctx, 20*time.Second); err == nil {
		// The OpenAI session id goes in HERE, at the open, not only at the close. It used to appear
		// for the first time on live_leg_close, which is precisely useless for the question that
		// matters: WHICH session is still billing right now. A session that never closes then has
		// no id anywhere in the log, and only the ledger file knows.
		ev("live_session_started", "ms_after_dial", time.Since(l.started).Milliseconds(),
			"session_id", l.SessionID)
	} else if errors.Is(err, errLiveStartupTimeout) {
		// A session that never says it started is not one to hand a conversation to. It used to be
		// returned as if it had, and then billed until a timer noticed nobody was there. Measured
		// starts are 2.8-3.7 s; twenty is not a limit anyone will meet by being slow.
		ev("live_session_started_timeout", "note", "transport and session.started did not both arrive; hung up rather than billed")
		l.Close("never_started")
		return nil, fmt.Errorf("the session never started: %w", errLiveStartupTimeout)
	} else if ctx.Err() != nil {
		l.Close("dial_cancelled")
		return nil, ctx.Err()
	} else {
		return nil, err
	}
	return l, nil
}

func (l *LiveLeg) readInbound(tr *webrtc.TrackRemote) {
	// Every packet read here is forwarded to the Echo immediately, so OpenAI's delivery rhythm
	// becomes the Echo's playout problem. Measure it.
	pace := newPaceMeter(l.ev, "openai")
	defer pace.done()
	var sequence liveSequenceTranslator
	for {
		pkt, _, err := tr.ReadRTP()
		if err != nil {
			l.ev("live_leg_audio_end", "err", err.Error(), "pkts", l.inPkts.Load())
			return
		}
		pace.mark(pkt.Timestamp, pkt.Marker)
		n := l.inPkts.Add(1)
		if n == 1 {
			l.firstIn.Store(time.Since(l.started).Milliseconds())
			l.ev("live_first_audio", "ms_after_dial", l.firstIn.Load(),
				"pt", pkt.PayloadType, "payload_bytes", len(pkt.Payload))
		}
		// The first few packets by the numbers a receiver routes and replay-checks on. Twice on
		// 2026-09-14 exactly ONE packet was delivered from a stream OpenAI's own sender report
		// said was 447 packets long; whether the second packet had a different SSRC or a sequence
		// number the first had already passed is what these lines settle.
		if n <= 6 {
			l.ev("live_rtp_head", "n", n, "ssrc", pkt.SSRC, "seq", pkt.SequenceNumber,
				"ts", pkt.Timestamp, "marker", pkt.Marker, "pt", pkt.PayloadType, "payload_bytes", len(pkt.Payload))
		}
		if len(pkt.Payload) > voicePayloadMin {
			l.lastVoice.Store(time.Now().UnixNano())
		}
		if f := l.onSpeech.Load(); f != nil {
			(*f)()
		}
		if rebase, ok := sequence.translate(pkt); ok {
			l.ev("live_rtp_rebased", "old_seq", rebase.oldSeq, "new_seq", rebase.newSeq,
				"old_ts", rebase.oldTS, "new_ts", rebase.newTS)
		}
		if sink := l.audioSink.Load(); sink != nil && *sink != nil {
			(*sink)(pkt)
		}
	}
}

// Close hangs up the GPT-Live session, which is what stops the per-minute clock, then tears down
// the transport. Idempotent.
func (l *LiveLeg) Close(reason string) {
	l.closeOnce.Do(func() {
		l.endedAt.CompareAndSwap(0, time.Now().UnixNano())
		in, out, firstMs := l.Stats()
		age := l.Age()
		l.ev("live_leg_close", "reason", reason, "session_id", l.SessionID,
			"age_s", age.Seconds(), "billed_usd_estimate", openai.EstimateLiveCostUSD(age),
			"pkts_from_openai", in, "pkts_to_openai", out, "first_audio_ms", firstMs)
		if l.SessionID != "" {
			// Stop the clock first, and keep trying: a failed hangup leaves money running.
			hctx, hcancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer hcancel()
			if err := l.client.HangupRetry(hctx, l.SessionID, 3); err != nil {
				l.ev("live_hangup_failed", "err", err.Error(), "session_id", l.SessionID,
					"note", "still on the ledger; the next start will sweep it")
			} else if l.client.Ledger != nil {
				l.client.Ledger.Remove(l.SessionID)
			}
			// The time is charged HERE, on every path that ends a session: a dial that failed after
			// the session existed (bad answer, never started, ledger unwritable) and a leg closed
			// as stale used to record only the init charge, because the caller's close was the
			// only place that charged. Found by the 2026-09-16 fleet.
			if l.client.Budget != nil {
				l.client.Budget.Spent(age)
			}
		}
		// Cancel BEFORE closing the socket: the reader tells a socket that died from one that
		// was closed on purpose by the context, and would otherwise try to reattach to a session
		// that has just been hung up.
		l.cancel()
		if l.sideband != nil {
			_ = l.sideband.Close()
		}
		_ = l.pc.Close()
		// Whoever owns this leg is told AFTER the hangup, on its own goroutine: the owner's cleanup
		// calls Close again, and a second Close from inside the first would wait on this Once
		// forever.
		if f := l.onClose.Load(); f != nil && *f != nil {
			go (*f)(reason)
		}
	})
}

// SetOnClose installs the callback for a leg that hung up on its own - the ten-minute cap, a dead
// transport - so the call around it can end too. It used to stop only the billing: the Echo stayed
// connected to a detached sink, the idle timer was fed by the Echo's packets because the transcript
// had gone with the session, and he was left talking into a dead line until Alexa gave up. Grok
// found it.
func (l *LiveLeg) SetOnClose(f func(reason string)) {
	if f == nil {
		l.onClose.Store(nil)
		return
	}
	l.onClose.Store(&f)
}

func (l *LiveLeg) drainRTCP(rc *webrtc.RTPReceiver, ev Events) {
	for {
		pkts, _, err := rc.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch v := p.(type) {
			case *rtcp.Goodbye:
				ev("live_rtcp", "type", "BYE", "reason", v.Reason)
			case *rtcp.SenderReport:
				// OpenAI's own count of what it has SENT. On the 01:49 call of 2026-09-14 one
				// RTP packet arrived in a hundred seconds while the transcript said she was
				// answering the whole time; whether her audio was never sent or sent and lost on
				// the way is exactly what this number settles, and it was not being kept.
				l.senderReport.Store(&liveSenderReport{packets: int64(v.PacketCount), at: time.Now()})
			}
		}
	}
}

// BelowTheReader returns how many RTP packets Pion has received and decrypted on the remote audio
// stream, counted by the interceptor before the application reads anything, plus how long ago
// the last one came. Zero when no track has been bound or the stats are unavailable. Compared
// with what the reader has taken, it separates "nothing arrived" from "the reader stopped".
func (l *LiveLeg) BelowTheReader() (packets uint64, lastAgo time.Duration) {
	ssrc := l.inSSRC.Load()
	if ssrc == 0 || l.getter == nil {
		return 0, 0
	}
	st := l.getter.Get(ssrc)
	if st == nil {
		return 0, 0
	}
	in := st.InboundRTPStreamStats
	if in.LastPacketReceivedTimestamp.IsZero() {
		return in.PacketsReceived, 0
	}
	return in.PacketsReceived, time.Since(in.LastPacketReceivedTimestamp)
}

// SenderReport returns OpenAI's latest claim of how many RTP packets it has sent, and when it
// said so. Zero and the zero time if it has not reported yet.
func (l *LiveLeg) SenderReport() (packets int64, at time.Time) {
	report := l.senderReport.Load()
	if report == nil {
		return 0, time.Time{}
	}
	return report.packets, report.at
}

type liveSenderReport struct {
	packets int64
	at      time.Time
}

func eventType(b []byte) string {
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(b, &event) != nil || len(event.Type) > 128 {
		return ""
	}
	return event.Type
}

func summarizeLiveAnswer(ev Events, sdp string) {
	var keep []string
	cands := map[string]int{}
	for _, ln := range strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(ln, "a=candidate:"):
			f := strings.Fields(ln)
			if len(f) >= 8 {
				cands[f[7]+"/"+f[2]]++
			}
		case strings.HasPrefix(ln, "m="), strings.HasPrefix(ln, "a=setup"), strings.HasPrefix(ln, "a=rtpmap"),
			strings.HasPrefix(ln, "a=fmtp"), strings.HasPrefix(ln, "a=extmap"), strings.HasPrefix(ln, "a=ice-lite"),
			strings.HasPrefix(ln, "a=sendrecv"), strings.HasPrefix(ln, "a=recvonly"), strings.HasPrefix(ln, "a=sendonly"),
			strings.HasPrefix(ln, "a=end-of-candidates"):
			keep = append(keep, ln)
		}
	}
	ev("live_answer_summary", "lines", keep, "candidates", cands,
		"has_m_application", strings.Contains(sdp, "m=application"),
		"has_m_video", strings.Contains(sdp, "m=video"),
		"trickle", !strings.Contains(sdp, "a=end-of-candidates"))
}
