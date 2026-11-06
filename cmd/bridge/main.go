// bridge is the assistant: it answers the Echo's WebRTC offer and carries the same Opus payloads
// to and from GPT-Live, with no transcoding and no speech-to-text anywhere in the audio path.
//
//	Echo Dot ──Opus/RTP── Amazon media server (us-east-1) ──Opus/RTP── [ this ] ──Opus/RTP── GPT-Live
//	                                                                      │
//	                                                                      └── heavy work: gpt-5.6-luna
//
// Two facts from measurement shape this program:
//
//   - The Alexa leg MUST send real H.264 video. Amazon documents NO_VIDEO_ERROR as "Couldn't
//     establish connection because timeout occurred waiting on video", and a camera-category
//     endpoint that sends none is torn down after 26-28 s (measured 15 times in July 2026). With
//     a black 1 fps stream the same setup ran 231 s and Alexa never disconnected. So the video
//     track here is load-bearing, not decoration.
//   - GPT-Live bills wall clock including silence, and muting does not close a session. The
//     OpenAI leg is therefore opened when the Echo session starts, closed on inactivity, and
//     resumed later with fork rather than kept alive.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/rtp"

	"github.com/u2n4/echo-gpt-live/internal/alexa"
	"github.com/u2n4/echo-gpt-live/internal/bridge"
	"github.com/u2n4/echo-gpt-live/internal/diag"
	"github.com/u2n4/echo-gpt-live/internal/memory"
	"github.com/u2n4/echo-gpt-live/internal/openai"
	"github.com/u2n4/echo-gpt-live/internal/tools"
)

type app struct {
	log *diag.Logger
	cfg bridge.Config

	live       *openai.LiveClient
	backend    *openai.Backend
	liveCfg    bridge.LiveLegConfig
	sessCfg    openai.LiveSessionConfig
	recDir     string
	greeting   string // Ogg/Opus the bridge plays to the Echo as soon as its transport is up; "" = the model greets
	paceFor    time.Duration
	markSpurts bool
	pacer      *bridge.Pacer
	recIn      *bridge.Recorder // what the Echo's microphone sent
	recOut     *bridge.Recorder // what we asked the Echo to play
	budget     *openai.Budget
	ledger     *openai.Ledger
	maxLive    time.Duration
	// kickSidebandAfter is the test hook behind --test-kick-sideband-after. Zero in service.
	kickSidebandAfter time.Duration
	assist            *assistant
	farewell          *bridge.Farewell
	byeGrace          time.Duration
	idleFor           time.Duration
	holdFor           time.Duration
	maxSess           time.Duration
	instr             string
	logText           bool
	mem               *memory.Profile
	place             string
	tz                *time.Location

	// offerMu admits one offer at a time. Two copies of the same directive arriving together used
	// to both pass the repeat check before either had published its session, and both built one.
	offerMu         sync.Mutex
	callMu          sync.Mutex      // serialize attaching resources with closeLeg's cleanup
	resourceSession *bridge.Session // protected by callMu; late cleanup cannot touch a newer call
	closing         atomic.Bool     // set at shutdown; an offer that arrives after it is refused
	dials           sync.WaitGroup  // dials in flight, so shutdown can wait for them to close what they built

	mu       sync.Mutex
	sess     *bridge.Session
	leg      *bridge.LiveLeg
	gate     *openai.IdleGate
	lastLive string // session id to fork from on the next invocation
	sessN    int
	// The offer that made the current session and the answer it got, so a redelivery of the same
	// directive gets the same answer instead of a fresh start.
	lastOffer, lastAnswer string
	seenOffers            map[string]time.Time // retired directives must not start paid calls again

	reflecting atomic.Bool // one after-the-call memory pass at a time
}

func (a *app) ev(ev string, kv ...any) { a.log.Event(ev, kv...) }

// Offer answers the Echo, then brings up the OpenAI leg and wires the two together.
func (a *app) Offer(id, offer string) (string, error) {
	// Alexa delivers directives at least once. On the 20:52 call of 2026-09-13 the same offer -
	// same session id, same 2311 bytes - arrived twice, 1.17 s apart, after a cold Lambda had taken
	// 4.4 s to reach this host. Treating the repeat as a new invocation tore down an Echo leg that
	// was connecting and an OpenAI session that was billing ($0.0125 for nothing), then started
	// over a second later. The same offer gets the same answer: the Echo connects to the same
	// transport whichever copy of the directive it acts on.
	a.offerMu.Lock()
	defer a.offerMu.Unlock()
	if a.closing.Load() {
		return "", errors.New("the bridge is shutting down")
	}
	// Counted from HERE, not from the moment the dial goroutine starts: shutdown waits on this
	// counter, and an offer that had passed the closing check but was still answering the Echo
	// was invisible to it, so the final sweep could run before the dial it was about to make.
	a.dials.Add(1)
	launched := false
	defer func() {
		if !launched {
			a.dials.Done()
		}
	}()
	a.mu.Lock()
	if a.sess != nil && a.sess.ID == id && a.lastOffer == offer {
		answer := a.lastAnswer
		a.mu.Unlock()
		a.ev("offer_repeated", "sessionId", id,
			"note", "same session and same SDP; answered again without starting over")
		return answer, nil
	}
	a.mu.Unlock()
	if err := a.admitOffer(id); err != nil {
		return "", err
	}

	a.teardown("replaced_by_new_offer")

	a.mu.Lock()
	a.sessN++
	n := a.sessN
	a.mu.Unlock()

	a.log.SetSession(id)
	a.log.SaveSDP(fmt.Sprintf("sess%02d-offer.sdp", n), offer)
	a.ev("offer_received", "sessionId", id, "n", n, "offer_bytes", len(offer))

	// The Echo leg comes up first and on its own: if OpenAI is unreachable we still want a
	// connected, video-fed session rather than a failed invocation.
	s, answer, err := bridge.Answer(id, offer, a.cfg, a.ev)
	if err != nil {
		return "", err
	}
	a.log.SaveSDP(fmt.Sprintf("sess%02d-answer.sdp", n), answer)

	// If the Echo's transport dies without a disconnect directive ever reaching us - the Lambda
	// swallows a failed POST - the OpenAI side used to keep billing until the idle timer noticed
	// nobody was talking. The transport itself is the earlier signal.
	s.SetOnDown(func(reason string) {
		if !a.retireSession(s) {
			return
		}
		a.ev("echo_transport_down", "reason", reason, "note", "invalidating the call and hanging up OpenAI")
		s.SetAudioSink(nil)
		a.closeLegFor(s, reason)
		s.Close(reason)
	})

	a.mu.Lock()
	a.sess = s
	a.seenOffers[id] = time.Now()
	a.lastOffer, a.lastAnswer = offer, answer
	forkFrom := a.lastLive
	if a.sessCfg.Store != nil && !*a.sessCfg.Store {
		forkFrom = ""
	}
	a.mu.Unlock()

	launched = true // attachLive owns the count from here and calls Done
	go a.attachLive(s, forkFrom)
	go a.watchdog(s)
	return answer, nil
}

// Preserve early speech through the 20-second readiness window (20 ms packets).
// The former three-second cap discarded the beginning of a sentence on a slow
// startup. This is a capacity, not an added delay: release as soon as Live is ready.
const holdPackets = 1000

// attachLive dials GPT-Live and connects the two media legs.
func (a *app) attachLive(s *bridge.Session, forkFrom string) {
	defer a.dials.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if forkFrom != "" && !a.live.ForkAvailable() {
		a.ev("fork_skipped", "reason", "project forbids stored sessions; starting a fresh conversation")
		forkFrom = ""
	}
	// The instructions are rebuilt for every call, not once at startup. The date is in them, and a
	// process that has been up since Tuesday would otherwise tell him it is Tuesday.
	sessCfg := a.sessCfg
	sessCfg.Instructions = a.instr + " " + a.greetingPolicy() + " " + a.whereAndWhen()
	if m := a.mem.Text(); m != "" {
		// Reading memory costs nothing and happens before he speaks. This is the whole reason the
		// profile is a few hundred tokens of text and not a database being searched mid-question.
		sessCfg.Instructions += "\n\n" + m
	}

	// His first words go into a hold, not onto the floor. The Echo's leg is up about 1.7 s before
	// this dial returns, and he starts talking at Alexa's chime; everything said in that gap used to
	// be read from the track and dropped. It is flushed to OpenAI the moment the session starts.
	hold := bridge.NewHoldSink(holdPackets)
	s.SetAudioSink(hold.Write)

	// Her hello goes out the moment the Echo's transport is up, not when the model gets round to
	// it 3-5 s later; see internal/bridge/greeting.go for the measurements. Anything OpenAI sends
	// before the greeting has finished waits in greetGate so the Echo hears one timeline.
	splice := &bridge.TimelineSplice{}
	greetGate := bridge.NewHoldSink(holdPackets)
	greetDone := make(chan struct{})
	if a.greeting == "" {
		close(greetDone)
	} else {
		// On its own clock, not this function's. The dial below can return before the Echo's
		// transport is up, and this function's context ends with it; the greeting then stopped
		// at packet 31 of 41 on the first real call, or never started. A greeting that outlives
		// a dead session only fails its next write, which is fine; one cut short is heard.
		gctx, gcancel := context.WithTimeout(context.Background(), greetingWait)
		go func() {
			defer close(greetDone)
			defer gcancel()
			select {
			case <-s.Connected():
			case <-gctx.Done():
				a.ev("greeting_skipped", "reason", "the Echo did not connect in time")
				return
			}
			n, err := bridge.PlayGreeting(gctx, a.greeting, s.WriteAudio, splice)
			if err != nil {
				a.ev("greeting_failed", "packets", n, "err", err.Error())
				return
			}
			a.ev("greeting_played", "packets", n, "echo_session_age_s", s.Age().Seconds())
		}()
	}

	leg, err := bridge.DialLive(ctx, a.live, sessCfg, a.liveCfg, forkFrom, a.ev)
	if err != nil {
		// The Echo session stays up; the user hears silence rather than a dropped call, and the
		// reason is in the log instead of being guessed at.
		s.SetAudioSink(nil)
		hold.Drop()
		a.ev("live_dial_failed", "err", err.Error(), "note", "Echo session left connected with no assistant")
		return
	}

	a.callMu.Lock()
	defer a.callMu.Unlock()
	a.mu.Lock()
	if a.sess != s { // a newer invocation arrived, or Alexa hung up, while we were dialling
		a.mu.Unlock()
		leg.Close("stale_invocation")
		s.SetAudioSink(nil)
		hold.Drop()
		return
	}
	a.resourceSession = s
	a.leg = leg
	a.lastLive = leg.SessionID
	// When the assistant goes, the call goes. Idle used to hang up OpenAI and leave the Echo
	// connected to a detached sink: a dead line he kept talking into, for up to the ninety-minute
	// cap, because nothing was listening and nothing would ever redial. The same for a leg that
	// ends on its own - the ten-minute cap, a failed transport. Ending the whole call is also the
	// only way to find out whether Alexa hangs up when the transport goes (F-UNK-3).
	gate := openai.NewIdleGate(a.idleFor, func(idle time.Duration) {
		a.ev("idle_close", "idle_s", idle.Seconds(), "note", "no one has spoken for this long; ending the call")
		a.teardownFor(s, "idle")
	})
	a.gate = gate
	a.mu.Unlock()
	leg.SetOnClose(func(reason string) {
		a.mu.Lock()
		cur := a.leg
		a.mu.Unlock()
		if cur != leg {
			return // closed by the bridge itself, which is already cleaning up
		}
		a.ev("live_leg_ended_on_its_own", "reason", reason, "note", "ending the call around it")
		a.teardownFor(s, reason)
	})

	// The idle timer used to be fed by RTP arrival, which made it useless: the Echo streams its
	// microphone continuously, room and all, so packets never stop and the timer never fired. What
	// proves a PERSON is there is the model's own transcript, because it transcribes speech and not
	// a room. Until a transcript has ever arrived the old behaviour stands, so a session can never
	// be closed on a conversation just because transcription is unavailable.
	var sawTranscript atomic.Bool
	// Counted per call so the end-of-call line says whether the transcript was readable at all.
	var emptyDeltas, heardDeltas atomic.Int64
	// When her transcript last moved: the model's own word that it is speaking. Set against the
	// packets that actually arrive, it tells a silent line from a silent assistant.
	var lastHerWords atomic.Int64
	touchIfNoTranscript := func() {
		// Packet arrival feeds the timer only while the transcript cannot. Once the transcript has
		// arrived it is the better signal, but if the control socket then dies we go back to
		// packets rather than hang up on someone who is still talking - for a bounded time. The
		// Echo streams the room whether or not anyone is in it, so packets alone would keep a
		// call with a dead control socket open to the ten-minute cap: no goodbye can be heard,
		// no tool can run. After the grace the idle timer runs unfed and ends it.
		if !sawTranscript.Load() || (!leg.SidebandLive() && leg.SidebandLostFor() < sidebandLostGrace) {
			gate.Touch()
		}
	}
	leg.SetOnSpeech(touchIfNoTranscript)

	// The detector is reset for each call. It used to be one object for the life of the process, so
	// a goodbye armed at the end of one call could end the next one.
	farewell := a.farewell
	farewell.Reset()
	// She is "speaking" while packets that carry her VOICE are still arriving - not while output
	// audio is arriving, which it always is: OpenAI streams silence at fifty packets a second
	// between her sentences, and the sideband announces every one of them. Built on that, this
	// said "speaking" for the entire call, so his withdrawal of a goodbye was always taken for her
	// echo and the goodbye's grace never saw a quiet moment. A gap inside one sentence is far
	// shorter than 1200 ms; a real pause after she finishes is longer.
	farewell.SetSpeaking(func() bool {
		last := leg.LastVoice()
		return !last.IsZero() && time.Since(last) < 1200*time.Millisecond
	})
	assist := a.assist
	if assist != nil {
		// A new call is a new conversation AND a new set of permissions. It also gets its own
		// context, so anything started for it stops when it ends.
		assist.newCall(s.ID)
		assist.setSpeaker(leg.Sideband())
		// The model that does the WORK needs to know where and when he is just as much as the one
		// that does the talking. Only the talker was being told. Rebuilt on demand too, because a
		// fact he asks to remember mid-call belongs in the very next answer.
		refresh := func() {
			ctxText := a.whereAndWhen()
			if m := a.mem.Text(); m != "" {
				ctxText += "\n\n" + m
			}
			assist.answerer.SetContext(ctxText)
		}
		refresh()
		assist.setRefresh(refresh)
	}
	leg.SetOnSidebandEvent(func(typ string, raw []byte) {
		// A delegation is the model asking for help, and it is the only event here that is not
		// about liveness. It carries no task text, so the answer comes from the transcript window.
		if typ == "session.delegation.created" && assist != nil {
			var body struct {
				Delegation struct {
					ID     string `json:"id"`
					Target string `json:"target"`
				} `json:"delegation"`
				ID string `json:"delegation_id"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				a.ev("delegation_parse_error", "err", err.Error(), "bytes", len(raw))
				return
			}
			id := body.Delegation.ID
			if id == "" {
				id = body.ID
			}
			if id == "" {
				a.ev("delegation_no_id", "note", "nothing to answer against")
				return
			}
			// "responses" means OpenAI is running this one itself. Answering it as well would be a
			// second backend request, and a second bill, for work already in progress.
			if t := body.Delegation.Target; t != "" && t != "client" {
				a.ev("delegation_not_ours", "target", t)
				return
			}
			sb := leg.Sideband()
			if sb == nil {
				a.ev("delegation_no_sideband", "note", "nothing to answer on")
				return
			}
			dctx, call := assist.current()
			if call == "" {
				a.ev("delegation_no_call", "note", "nothing is on the line to answer into")
				return
			}
			go assist.onDelegation(dctx, sb, id)
			return
		}

		switch typ {
		case "session.input_transcript.delta", "session.output_transcript.delta":
		default:
			return
		}
		sawTranscript.Store(true)
		gate.Touch()
		if typ != "session.input_transcript.delta" {
			lastHerWords.Store(time.Now().UnixNano())
			return
		}
		// Everything downstream of here - the farewell, the question a delegation is answered with -
		// depends on this one field parsing. When it does not, nothing errors: the farewell simply
		// never matches and the assistant simply never has a question, which is indistinguishable
		// from the user not having said anything. So an event that arrives with no text in the field
		// we expect is COUNTED and named, once per call. No speech is written to the log.
		var body struct {
			Delta      string `json:"delta"`
			Text       string `json:"text"`
			Transcript string `json:"transcript"`
		}
		if json.Unmarshal(raw, &body) != nil {
			emptyDeltas.Add(1)
			return
		}
		text := body.Delta
		if text == "" {
			// Tolerate the other two spellings rather than go silent on a field rename.
			if body.Text != "" {
				text = body.Text
			} else if body.Transcript != "" {
				text = body.Transcript
			}
		}
		if text == "" {
			if n := emptyDeltas.Add(1); n == 1 {
				a.ev("transcript_delta_empty", "bytes", len(raw),
					"note", "a transcript event carried no text in delta, text or transcript; "+
						"the farewell and the assistant are both blind while this is true")
			}
			return
		}
		if n := heardDeltas.Add(1); n == 1 {
			a.ev("transcript_delta_ok", "chars", len([]rune(text)))
		}
		if a.logText {
			a.ev("heard", "text", text)
		}
		if assist != nil {
			assist.onTranscript(text)
		}
		if farewell != nil && farewell.Feed(text) {
			// The bridge says her goodbye on this path too, so the voice model never has to, and
			// so a goodbye heard by phrase and understood by the router a second later is
			// answered once, here, and not again there.
			if sb := leg.Sideband(); sb != nil {
				go func() {
					bctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := sb.Commentary(bctx, "", spokenGoodbye); err != nil {
						a.ev("farewell_say_failed", "err", err.Error())
					}
				}()
			}
		}
	})

	if farewell != nil {
		go a.watchFarewell(leg, farewell)
	}
	go a.watchHerAudio(leg, &lastHerWords)

	if a.recDir != "" {
		dir := filepath.Join(a.recDir, s.ID)
		a.mu.Lock()
		a.recIn = bridge.NewRecorder(dir, "from-echo", a.ev)
		a.recOut = bridge.NewRecorder(dir, "from-openai", a.ev)
		a.mu.Unlock()
	}

	// Echo -> OpenAI. Opus payloads pass through; Pion rewrites SSRC and payload type only.
	a.mu.Lock()
	recIn, recOut := a.recIn, a.recOut
	a.mu.Unlock()

	// Timing the forward, because the arrival meter marks a packet when the reader gets it: a slow
	// write here delays the next read and would be reported as a burst that never happened.
	toOpenAI := bridge.NewWriteMeter(a.ev, "to_openai")
	forward := func(p *rtp.Packet) {
		touchIfNoTranscript()
		recIn.Write(p) // nil-safe
		t0 := time.Now()
		err := leg.WriteAudio(p)
		toOpenAI.Observe(time.Since(t0))
		if err != nil {
			a.ev("forward_to_openai_failed", "err", err.Error())
		}
	}
	// The hold stays installed as the sink and forwards directly from here on; what it kept goes
	// first, in order, so the sentence he started at the chime arrives whole.
	held, dropped := hold.Release(forward)
	a.ev("hold_released", "packets", held, "audio_ms", held*20, "dropped", dropped)

	// OpenAI -> Echo. Measured on this path, OpenAI delivers up to 46% of its packets in bursts with
	// gaps reaching 500 ms, and forwarding that rhythm untouched made the far jitter buffer stretch
	// and accelerate continuously, which is heard as grit on a stream that loses almost nothing.
	// Amazon's own receiver reports settle it: the jitter it measures on our stream fell from a
	// 13.9 ms median to 4.9 ms once this was in, and reported loss went to zero.
	//
	// The pacer changes only WHEN a packet leaves, never its bytes, its sequence number or its
	// timestamp. Both inbound legs are bursty, incidentally: the Echo's stream is 40-45% bursty too,
	// which an earlier comment here denied on the strength of a simulator run.
	toEchoMeter := bridge.NewWriteMeter(a.ev, "to_echo")
	toEcho := func(p *rtp.Packet) {
		t0 := time.Now()
		err := s.WriteAudio(p)
		toEchoMeter.Observe(time.Since(t0))
		if err != nil {
			a.ev("forward_to_echo_failed", "err", err.Error())
		}
	}
	pacer := bridge.NewPacerWith(a.paceFor, a.markSpurts, toEcho, a.ev)
	a.mu.Lock()
	a.pacer = pacer
	a.mu.Unlock()
	if pacer != nil {
		toEcho = pacer.Write
	}
	// The splice sits after the recorder, so from-openai.ogg keeps OpenAI's own numbering, and
	// before the pacer, which spaces packets by the timestamps it is given.
	leg.SetAudioSink(func(p *rtp.Packet) {
		recOut.Write(p)
		greetGate.Write(p)
	})
	go func() {
		<-greetDone
		held, dropped := greetGate.Release(func(p *rtp.Packet) {
			if a.greeting != "" {
				splice.Translate(p)
			}
			toEcho(p)
		})
		if held > 0 || dropped > 0 {
			a.ev("greeting_gate_released", "packets", held, "dropped", dropped, "note", "OpenAI spoke before the greeting had finished; her packets waited")
		}
	}()

	a.ev("bridge_live", "openai_session", leg.SessionID, "forked_from", forkFrom,
		"echo_session_age_s", s.Age().Seconds())

	// Her hello, said through OpenAI's own stream the moment the session is up. The model was
	// told to say it and on the 17:24 call it never did: first voice at 7 s, only after he spoke.
	// The bridge-played clip says it at 2 s but both calls that used it (15:15, 18:24) lost the
	// Echo's microphone: Amazon's media server stopped sending after 8-20 packets, at the moment
	// our audio reached it, and every call whose first outbound audio came later (4 s) worked.
	// So until that is understood the clip stays off, and this is the hello: about 3.5-4 s
	// after the offer, through the same path everything else she says takes.
	if a.greeting == "" {
		if sb := leg.Sideband(); sb != nil {
			go func() {
				hctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := sb.Commentary(hctx, "", spokenHello); err != nil {
					a.ev("hello_failed", "err", err.Error())
					return
				}
				a.ev("hello_sent", "echo_session_age_s", s.Age().Seconds())
			}()
		}
	}

	if a.kickSidebandAfter > 0 {
		// Test hook only. A stalled socket cannot be ordered from OpenAI, but the path that
		// recovers from one - Read returns, a fresh attach to the same session, events resume -
		// can be run against the real service by closing the socket from this side.
		time.AfterFunc(a.kickSidebandAfter, func() {
			if sb := leg.Sideband(); sb != nil {
				a.ev("test_sideband_kick", "note", "closing the control socket on purpose; a reattach must follow")
				sb.Kick()
			}
		})
	}
}

// closeLegFor closes resources belonging to owner; nil means unconditional cleanup.
func (a *app) closeLegFor(owner *bridge.Session, reason string) {
	a.callMu.Lock()
	defer a.callMu.Unlock()
	if owner != nil && a.resourceSession != owner {
		return
	}
	s := a.resourceSession
	a.resourceSession = nil
	a.mu.Lock()
	leg, gate := a.leg, a.gate
	a.leg, a.gate = nil, nil
	a.mu.Unlock()
	if s != nil {
		s.SetAudioSink(nil)
	}
	if gate != nil {
		gate.Stop()
	}
	a.mu.Lock()
	recIn, recOut, pacer := a.recIn, a.recOut, a.pacer
	a.recIn, a.recOut, a.pacer = nil, nil, nil
	a.mu.Unlock()
	// Order matters, and it used to be wrong. The hangup is what stops the clock, so it goes FIRST,
	// ahead of every diagnostic. Flushing the pacer and the recorders first meant a slow disk could
	// hold the hangup behind it while the session kept billing. Codex found it.
	if leg != nil {
		leg.Close(reason) // hangs up, then charges the day's counter for the time it was open
	}
	// The goodbye detector belongs to the leg that just closed: its voice judge closes over
	// that leg, whose LastVoice is frozen from here on, and a goodbye it armed was for this
	// call. The next call installs its own; nothing must read a dead one in between.
	a.farewell.Reset()
	// Nothing started for this call should outlive it. A delegation still running would be talking
	// into a closed socket, and a write it had already been given permission for would land after
	// the person who granted it had hung up.
	if a.assist != nil {
		// Learn from the call that just ended — AFTER the hangup above, never before it. The whole
		// design of memory here rests on this line being on this side of leg.Close: he is not
		// waiting for it, it is not billed, and if it fails the only cost is that one conversation
		// taught it nothing. The transcript is TAKEN, not read: a call closed on idle and again on
		// teardown was being reflected on twice, and paid for twice. Codex found it.
		if leg != nil {
			if said := a.assist.window.TakeWhole(); a.mem != nil && len(said) > 40 {
				go a.reflect(said)
			}
		}
		a.assist.endCall()
	}
	// Now the diagnostics, which nobody is paying for by the minute.
	pacer.Close()
	recIn.Close()
	recOut.Close()
	bridge.FlushWriteMeters()
}

// retireSession invalidates an in-flight dial before resource cleanup can block.
func (a *app) retireSession(s *bridge.Session) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sess != s {
		return false
	}
	a.sess = nil
	return true
}

// admitOffer is called after the current-call repeat check, with offerMu held.
func (a *app) admitOffer(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seenOffers == nil {
		a.seenOffers = make(map[string]time.Time)
	}
	for key, at := range a.seenOffers {
		if time.Since(at) > 5*time.Minute {
			delete(a.seenOffers, key)
		}
	}
	if _, seen := a.seenOffers[id]; seen {
		return errors.New("this offer belongs to an ended or changed session")
	}
	return nil
}

// whereAndWhen tells the assistant where the Echo is and what time it is there.
//
// Without it the model asks him which city he is in before it can answer about the weather, and it
// has no idea what "today" means - it knows only what it was trained on, which was months ago. Both
// of those cost a round trip of billed silence to ask about something the bridge already knows.
//
// It is rebuilt per call because the date in it goes stale at midnight and this process stays up
// for weeks.
func (a *app) whereAndWhen() string {
	now := time.Now().In(a.tz)
	days := [...]string{"الأحد", "الإثنين", "الثلاثاء", "الأربعاء", "الخميس", "الجمعة", "السبت"}
	when := fmt.Sprintf("الوقت عنده الآن %s، و%s %s.", now.Format("15:04"), days[int(now.Weekday())], now.Format("2006-01-02"))
	if a.place == "" {
		return when + " لا تسأله عن الوقت، تعرفه."
	}
	return fmt.Sprintf("هو في %s. %s لا تسأله عن موقعه ولا عن مدينته ولا عن الوقت، تعرفهم.", a.place, when)
}

// greetingPolicy tells the model who says hello. With --greeting the bridge has already said it
// by the time the session starts, and a second hello on top of his first sentence is exactly the
// talking-over that the one-word rule existed to avoid.
func (a *app) greetingPolicy() string {
	if a.greeting != "" {
		return "الخط قال له \"هلا\" قبل ما تبدأ، فلا تسلّم ولا تعيدها؛ ابدأ بالاستماع، وإذا كان يتكلم أصلاً جاوبه على طول."
	}
	// The bridge says the hello on this path too (a spoken line at session start), so the model
	// is told the same thing either way: it has been said, do not say it again.
	return "الخط قال له \"هلا\" أول ما بدأت المكالمة، فلا تسلّم ولا تعيدها؛ ابدأ بالاستماع، وإذا كان يتكلم أصلاً جاوبه على طول."
}

// envOr reads a setting from the environment so the service file can carry what is personal
// (where the house is) while the defaults in this file fit anyone.
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envFloat(key string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(key)), 64)
	if err != nil {
		return 0
	}
	return v
}

// mustZone resolves the owner's clock. Saudi Arabia has never observed daylight saving, so a fixed
// +03:00 is not an approximation and is the right answer when the host has no tzdata.
func mustZone(name string) *time.Location {
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.FixedZone("+03", 3*60*60)
}

// watchHerAudio notices the call on which he hears nothing.
//
// The 01:49 call of 2026-09-14: for a hundred seconds the transcript showed her answering every
// question and the sideband announced her audio, and ONE RTP packet arrived from OpenAI. He asked
// whether she was broken; nothing in the log said so until the close line. This says so within
// seconds, and with OpenAI's own sender report beside it, so the next time it happens the log
// answers the question the last one left open: did she never send, or did it never arrive.
func (a *app) watchHerAudio(leg *bridge.LiveLeg, herWords *atomic.Int64) {
	const every = 5 * time.Second
	t := time.NewTicker(every)
	defer t.Stop()
	var lastIn int64
	var said bool
	for range t.C {
		a.mu.Lock()
		cur := a.leg
		a.mu.Unlock()
		if cur != leg {
			return
		}
		in, _, _ := leg.Stats()
		spoke := herWords.Load()
		speaking := spoke > 0 && time.Since(time.Unix(0, spoke)) < every
		if speaking && in == lastIn {
			sr, at := leg.SenderReport()
			below, lastAgo := leg.BelowTheReader()
			if !said {
				a.ev("openai_audio_missing", "pkts_received_total", in, "openai_says_sent", sr,
					"sender_report_age_s", time.Since(at).Seconds(),
					"pion_received_below_reader", below, "pion_last_packet_ms_ago", lastAgo.Milliseconds(),
					"note", "her transcript is moving but no audio has arrived from OpenAI in the last five seconds; he hears nothing")
				said = true
			}
		} else if in != lastIn && said {
			a.ev("openai_audio_resumed", "pkts_received_total", in)
			said = false
		}
		lastIn = in
	}
}

// keepWarm touches OpenAI every so often so the pooled connection is still open when a call
// arrives. It logs the first result and every change of state, never every tick.
func (a *app) keepWarm(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}
	var wasOK bool
	for first := true; ; first = false {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		t0 := time.Now()
		err := a.live.Warm(wctx)
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err != nil && (first || wasOK):
			a.ev("warm_failed", "err", err.Error(), "note", "the next call starts on a cold connection")
		case err == nil && !wasOK:
			a.ev("warm_ok", "ms", time.Since(t0).Milliseconds())
		}
		wasOK = err == nil
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// sweepOptions says which ledger entries a sweep may touch. At startup, everything a dead process
// left behind. On the periodic pass, nothing younger than two minutes (a dial is still setting it
// up) and never the leg this process is using. When an orphan is closed, the time it was open is
// charged to the day's budget, capped at the limit it would have hit on its own; one of our own
// that merely failed to hang up was charged when its leg closed.
func (a *app) sweepOptions(periodic bool) openai.SweepOptions {
	o := openai.SweepOptions{
		MaxLive: a.maxLive,
		OnClosed: func(id string, started time.Time, own bool) {
			if started.IsZero() || own {
				return
			}
			open := time.Since(started)
			if a.maxLive > 0 && open > a.maxLive {
				open = a.maxLive
			}
			a.budget.Spent(open)
			a.ev("ledger_orphan_charged", "id", id, "usd", openai.EstimateLiveCostUSD(open))
		},
	}
	if periodic {
		// A diagnostic next to the service may legitimately run longer than one call; between
		// restarts the pid is trusted four times as long, which still bounds what a reused pid
		// can hide to well under the day's budget. Muse found the short cap.
		o.MaxLive = 4 * a.maxLive
		o.MinAge = 2 * time.Minute
		o.Except = func() string {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.leg != nil {
				return a.leg.SessionID
			}
			return ""
		}
	}
	return o
}

// sweepLedger retries, for as long as this process runs, the hangups that failed. A hangup that
// fails during an outage used to be tried three times and then left "for the next start", and a
// service that stays up for weeks has no next start. Codex found it.
func (a *app) sweepLedger(ctx context.Context, every time.Duration) {
	if a.ledger == nil || every <= 0 {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
		sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		a.ledger.SweepWith(sctx, a.live, a.sweepOptions(true))
		cancel()
	}
}

// liveAge is how long the session that is billing right now has been open. Zero means none is.
func (a *app) liveAge() time.Duration {
	a.mu.Lock()
	leg := a.leg
	a.mu.Unlock()
	if leg == nil {
		return 0
	}
	return leg.Age()
}

func (a *app) teardown(reason string) { a.teardownFor(nil, reason) }

func (a *app) teardownFor(expected *bridge.Session, reason string) {
	// The session leaves the app FIRST. A dial that returns while the rest of this runs then finds
	// it gone and closes what it built, instead of installing a leg into a call that is ending.
	a.mu.Lock()
	if expected != nil && a.sess != expected {
		a.mu.Unlock()
		return
	}
	s := a.sess
	a.sess = nil
	a.mu.Unlock()
	if s != nil {
		s.SetAudioSink(nil)
	}
	a.closeLegFor(s, reason)
	if s != nil {
		s.Close(reason)
	}
}

func (a *app) watchdog(s *bridge.Session) {
	time.Sleep(a.maxSess)
	a.mu.Lock()
	cur := a.sess
	a.mu.Unlock()
	if cur == s {
		a.ev("max_session_reached", "max", a.maxSess.String())
		a.teardownFor(s, "max_session")
	}
}

func (a *app) Connected(id string) {
	a.ev("alexa_session_connected", "sessionId", id)
}

func (a *app) Disconnected(id string) {
	a.mu.Lock()
	s := a.sess
	if s != nil && s.ID == id {
		// Ownership ends here, not after the hold. A dial still in flight for this call used to
		// return during the hold, find the session still current, and install a billing leg into
		// a call Alexa had already ended. Codex found it.
		a.sess = nil
	}
	a.mu.Unlock()
	age := 0.0
	if s != nil {
		age = s.Age().Seconds()
	}
	a.ev("alexa_session_disconnected", "sessionId", id, "echo_session_age_s", age)
	if s == nil || s.ID != id {
		return
	}
	// Close the OpenAI side at once so the clock stops; hold the Echo transport briefly to
	// observe whether Amazon keeps sending after its own directive.
	s.SetAudioSink(nil)
	a.closeLegFor(s, "alexa_disconnected")
	go func() {
		if a.holdFor > 0 {
			a.ev("hold_after_disconnect", "seconds", a.holdFor.Seconds())
			time.Sleep(a.holdFor)
		}
		s.Close("alexa_disconnected")
	}()
}

func main() {
	var (
		listen  = flag.String("listen", "127.0.0.1:8787", "HTTP listen address (Tailscale Funnel proxies here)")
		keyFile = flag.String("key-file", os.ExpandEnv("$HOME/.config/acv/signing.key"), "shared HMAC key for the Lambda")
		oaiKey  = flag.String("openai-key-file", os.ExpandEnv("$HOME/.config/acv/openai.key"), "OpenAI API key file")
		runsDir = flag.String("runs-dir", "runs", "directory for per-run logs")
		runName = flag.String("run", "", "run name (default: timestamp)")
		udpPort = flag.Int("udp-port", 50000, "UDP port for the Alexa leg's ICE")
		muxIP   = flag.String("mux-ip", "auto", "bind ICE to this local IPv4 (\"auto\" = egress address)")
		exIface = flag.String("exclude-ifaces", "docker,br-,lxc,veth,tailscale,virbr,cni,flannel", "interface prefixes to keep out of ICE")
		stun    = flag.String("stun", "stun.l.google.com:19302", "STUN server host:port")
		video   = flag.String("video", "sendonly", "reject|inactive|recvonly|sendonly|sendrecv - sendonly is what keeps Amazon's session alive (measured: 231 s vs 26-28 s without video)")
		videoF  = flag.String("video-file", "media/black1080p_1fps.h264", "Annex-B H.264 sent to Amazon to keep the session alive")
		fps     = flag.Float64("video-fps", 1, "video frame rate")
		model   = flag.String("model", openai.LiveModel, "voice model")
		backend = flag.String("backend", openai.BackendModel, "backend model for heavy work")
		deleg   = flag.String("delegation", "client", "client|responses|none")
		effort  = flag.String("backend-effort", openai.BackendEffortDefault, "effort when --delegation responses")
		// A Latin spelling in the place is there for the model's ears, not his: on one call it
		// heard the city's name as an adjective and told him he was from "somewhere nice". OpenAI's
		// prompting guide asks for exactly this kind of anchor on a name that can be misheard.
		place = flag.String("place", os.Getenv("BRIDGE_PLACE"),
			"where the Echo is, said the way a person would say it, with a Latin spelling of any name "+
				"the model could mishear (e.g. \"حي كذا، مدينة كذا (City)\"). It goes into the prompt so the "+
				"assistant never asks him where he is, and so a question about the weather, the "+
				"prayer times or what is nearby has an answer without a round trip. Default $BRIDGE_PLACE; "+
				"empty leaves the location out of the prompt")
		logText = flag.Bool("log-transcript", false, "write what he says into the run log. Off by default: it is his speech. Turn it on for one call when the assistant misbehaves and the timing alone cannot say why")
		memFile = flag.String("memory-file", os.ExpandEnv("$HOME/.local/state/rtcdiag/memory.md"),
			"where what it remembers about him is kept. Plain text he can read and correct; empty turns memory off")
		lat = flag.Float64("lat", envFloat("BRIDGE_LAT"), "latitude of the house, for the weather tool. Default $BRIDGE_LAT; 0 with --lon 0 turns the weather tool off")
		lon = flag.Float64("lon", envFloat("BRIDGE_LON"), "longitude of the house. Default $BRIDGE_LON")
		tz  = flag.String("timezone", envOr("BRIDGE_TIMEZONE", "Local"), "the clock he lives on, an IANA zone name. Default $BRIDGE_TIMEZONE, else the host's zone; a name the host cannot resolve falls back to a fixed +03:00")
		// Shaped after OpenAI's own GPT-Live prompting guide (fetched 2026-09-14): a personality, a
		// listening policy, a delegation policy, and what to do at the ends of the call. The lines
		// about not greeting twice and reusing a result that still answers are there because the
		// 20:52 call of 2026-09-13 did both, and the one-word start is so he knows she can hear
		// without a greeting long enough to talk over him.
		instr = flag.String("instructions", "انت \"سول\"، مساعد صوتي في البيت.هادي ومختصر وودود بدون مبالغة. رد بالعربي الخليجي إلا إذا كلّمك بلغة ثانية. جملة أو جملتين، لا أكثر، لأن كلامك ينطق من سمّاعة صغيرة. قل الأرقام كما ينطقها الناس. "+
			"الاستماع: إذا قاطعك وانت تتكلم، اسكت فوراً واسمعه. إذا سكت عشان يفكر، انتظره ولا تعبّي السكوت. لا تعتبر السعال ولا الموسيقى ولا كلام الناس القريبين طلباً جديداً. "+
			"طلب المساعدة: إذا كان الجواب يعتمد على هذا الجهاز، أو على البيت، أو على ملاحظاته، أو على اليوم، أو على أخبار أو أسعار أو نتائج أو أي شي بعد تدريبك — اطلب المساعدة قبل ما تجاوب، ولا تخمّن النتيجة وانت تنتظر، ولا تقول «لحظة» ولا «ثواني»: الخط يقولها عنك، وانت اسكت لين يجيك الجواب. وإذا تعرف الجواب فجاوب على طول؛ طلب مساعدة ما تحتاجها يخليه ينتظر ويكلّفه. إذا جاك جواب من المساعدة وهو يكفي، استعمله ولا تطلب من جديد إلا إذا طلب هو تتأكد أو غيّر السؤال. "+
			"الذكاء: عملتك الافتراضية دائماً الريال السعودي ولا تسأل دولار ولا ريال إلا إذا هو قال دولار صراحة. السوق الافتراضي السعودية. إذا السؤال ناقص تفصيل افترض الأكثر شيوعاً عندنا وجاوب به واذكر افتراضك داخل الجواب. ممنوع أسئلة توضيحية إلا إذا استحال الجواب بدونها. ممنوع تطلب تأكيد ولا تقول تبغاني أكمل؛ التأكيد فقط لحفظ أو حذف أو إرسال أو شراء. "+
			"لا ترحّب مرة ثانية في نفس المكالمة ولا تعيد شي قلته. وإذا ما فهمت، قل ذلك بجملة وحدة ولا تخترع. إذا ودّعك أو طلب تقفل المكالمة، بأي كلمات كانت، لا تودّعه بنفسك: اطلب المساعدة على طول واسكت، والمساعدة هي اللي تودّعه وتقفل الخط.", "system instructions")
		voice     = flag.String("voice", "", "GPT-Live voice name (empty = provider default)")
		idle      = flag.Duration("idle-close", 60*time.Second, "close the OpenAI session after this long with no one speaking. Driven by the model's transcript, not by packets: the Echo streams an empty room continuously, so packet arrival never means a person is there. At $0.05 a minute this is what a forgotten session costs")
		hold      = flag.Duration("hold-after-disconnect", 20*time.Second, "keep the Echo transport open after Alexa's disconnect directive")
		maxSess   = flag.Duration("max-session", 90*time.Minute, "hard cap per Echo session")
		marker    = flag.Bool("set-marker", false, "set marker=true on packets forwarded to OpenAI (undocumented; the one prior-art bridge did this)")
		fork      = flag.Bool("fork", false, "resume the stored conversation on the next call (fork). Needs data persistence enabled on the OpenAI project: a project without it rejects store:true, and measured on 28 calls that refusal cost the first call after every restart 400-900 ms before the retry without store succeeded. Off by default so the OpenAI leg is ready sooner")
		maxLive   = flag.Duration("max-live", 10*time.Minute, "hang up the OpenAI session after this long no matter what (it bills wall clock)")
		kickSB    = flag.Duration("test-kick-sideband-after", 0, "TEST HOOK, never set in service: this long after the OpenAI leg is up, close its control socket from this side so the reattach path runs against the real service. 0 disables")
		warm      = flag.Duration("warm-openai", 60*time.Second, "touch OpenAI this often so the HTTPS connection is still open when a call arrives. Measured: creating a session took 1570 ms on a cold connection and 758 ms on a warm one, and every call was cold. Under the 90 s after which Go drops an idle connection. 0 disables")
		toolsOn   = flag.Bool("tools", false, "let the assistant use tools when it delegates: the machine's state, today's spend, his notes, things on the home network. Off by default because every tool is reachable by anyone who can speak in the room")
		searchOn  = flag.String("web-search", "free", "how it searches the web. \"free\" costs nothing beyond the few hundred tokens the snippets add: it asks DuckDuckGo directly, and when DuckDuckGo refuses this address as automated traffic (measured on this machine: it does) it goes through the keyless reader at r.jina.ai, which means that service sees the question. \"private\" is the same without the reader, so nobody outside the engine sees it and a block means no answer. \"openai\" is the built-in search at a cent a call, five hundred times more. \"off\" means it cannot search at all")
		searchMd  = flag.String("search-model", openai.SearchModel, "the cheap model that routes tool calls and runs searches. Not the voice model and not the heavy one")
		searchFx  = flag.String("search-effort", "", "how hard the router thinks: minimal|low|medium, or off for a model without reasoning. Empty is low, the least that still decides to use a tool")
		vaultURL  = flag.String("vault-url", "", "where cmd/vaultd is serving his notes, e.g. http://<pc-on-the-lan>:8791. Empty disables the notes tools")
		vaultKey  = flag.String("vault-key-file", "", "file holding the shared secret vaultd expects")
		homeEPs   = flag.String("home-endpoints", "", "things on the home network the assistant may ask, as name=url,name=url. Read-only, and the model can only pick a name")
		logSvcs   = flag.String("log-services", "bridge", "which services it may read warnings from, comma separated")
		bye       = flag.String("farewell", "", "comma-separated phrases that end the call when the user says them. Empty uses the built-in Arabic and English list; \"off\" disables it")
		byeGrace  = flag.Duration("farewell-grace", 2*time.Second, "how long to wait after hearing a goodbye, so the assistant can finish saying hers and the user can take it back")
		mark      = flag.Bool("spurt-marker", true, "set the RTP marker bit on the first packet after a pause the sender's timestamps do not cover. RFC 3550 gives it the meaning \"first packet of a talkspurt\", which tells a jitter buffer to play silence instead of concealing across the hole")
		ext       = flag.Bool("ext", false, "negotiate the ssrc-audio-level and transport-cc header extensions the Echo offers. Off by default: it changes what is negotiated with a working Amazon stack, so it is an experiment, not a default")
		vStream   = flag.String("video-stream-id", "keepalive", "msid stream for the keepalive video. Set to \"rtcdiag\" to share the audio's stream again, which is how it used to be")
		pace      = flag.Duration("pace", bridge.DefaultPacerDelay, "hold OpenAI's audio this long and release it on its own timestamps, instead of the moment it arrives. 0 disables")
		opusFmtp  = flag.String("opus-fmtp", bridge.DefaultOpusFmtp, "Opus fmtp asked of BOTH remote encoders (the Echo's and OpenAI's). Empty keeps the browser default")
		noiseRed  = flag.String("noise-reduction", "off", "OpenAI input noise reduction: far_field, near_field or off. gpt-live-1 REJECTS this (measured: unknown_parameter session.audio.input); kept for the Realtime fallback")
		recDir    = flag.String("record-dir", "", "write both audio directions to Ogg/Opus here (no transcoding: the files are exactly what crossed the bridge). Empty = off")
		greeting  = flag.String("greeting", "", "Ogg/Opus file (one 20 ms packet per page) the bridge itself plays to the Echo the moment its transport is up, ~1.5 s before the OpenAI leg is ready. media/hala.ogg is her own «هلا» cut from a recording. Measured: the model's own hello reached the Echo 5-7 s after the offer; this reaches it at ~2 s, and the model is told not to greet again. WARNING, measured twice on 2026-09-16 (15:15 and 18:24): with the clip on, Amazon's media server stopped sending the Echo's microphone after 8-20 packets, at the moment the clip's audio reached it, and she never heard him; every call whose first outbound audio came later (~4 s) worked. Leave it empty until that is understood. Empty: the bridge has her say «هلا» through the OpenAI leg as soon as the session is up (~3.5-4 s)")
		budgetUSD = flag.Float64("daily-budget-usd", 1.0, "refuse to open a new GPT-Live session once this much has been spent today. Counted on disk per UTC day and shared with every other process on this machine. 0 disables the ceiling but keeps counting")
		budgetP   = flag.String("budget-file", os.ExpandEnv("$HOME/.local/state/rtcdiag/spend.json"), "where the shared daily spend counter lives")
		ledgerP   = flag.String("ledger", os.ExpandEnv("$HOME/.local/state/rtcdiag/open-live-sessions"), "file recording OpenAI sessions that are open and billing, so a killed process can be recovered from")
	)
	flag.Parse()

	key, err := os.ReadFile(*keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "signing key:", err)
		os.Exit(2)
	}
	key = []byte(strings.TrimSpace(string(key)))
	if len(key) < 32 {
		fmt.Fprintln(os.Stderr, "signing key too short")
		os.Exit(2)
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		b, rerr := os.ReadFile(*oaiKey)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "no OpenAI API key: set OPENAI_API_KEY or write it to %s\n", *oaiKey)
			os.Exit(2)
		}
		apiKey = strings.TrimSpace(string(b))
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

	store := *fork
	sessCfg := openai.LiveSessionConfig{
		Model:        *model,
		Instructions: *instr,
		Store:        &store, // only --fork asks for storage, which a project may forbid
	}
	// audio.format is deliberately omitted on both sides: WebRTC negotiates it through SDP.
	sessCfg.Audio = map[string]any{}
	if *voice != "" {
		sessCfg.Audio["output"] = map[string]any{"voice": *voice}
	}
	// The Echo Dot is a far-field microphone in a room, which is exactly the case this setting
	// names, and on the Realtime API it filters audio before turn detection and the model see it.
	// gpt-live-1 rejects the whole session.audio.input object, so this is off by default and the
	// client drops it and retries if it is ever turned on against a model that refuses it.
	if *noiseRed != "off" && *noiseRed != "" {
		sessCfg.Audio["input"] = map[string]any{
			"noise_reduction": map[string]any{"type": *noiseRed},
		}
	}
	if len(sessCfg.Audio) == 0 {
		sessCfg.Audio = nil
	}
	switch *deleg {
	case "client":
		sessCfg.Delegation = openai.ClientDelegation()
	case "responses":
		sessCfg.Delegation = openai.ResponsesDelegation(*backend, *effort, "Answer briefly and concretely.")
	}

	a := &app{
		log:               lg,
		recDir:            *recDir,
		greeting:          *greeting,
		paceFor:           *pace,
		markSpurts:        *mark,
		byeGrace:          *byeGrace,
		idleFor:           *idle,
		holdFor:           *hold,
		kickSidebandAfter: *kickSB,
		maxSess:           *maxSess,
		instr:             *instr,
		mem:               openMemory(*memFile),
		logText:           *logText,
		place:             *place,
		tz:                mustZone(*tz),
		sessCfg:           sessCfg,
		cfg: bridge.Config{
			UDPPort: *udpPort, MuxIP: *muxIP, ExcludeIface: splitCSV(*exIface), STUN: *stun,
			// Load-bearing: audio forwards, video keeps Amazon's session alive.
			Audio: "bridge",
			Video: *video, VideoFile: *videoF, VideoFPS: *fps,
			OpusFmtp:      *opusFmtp,
			VideoStreamID: *vStream,
			Ext:           *ext,
			GatherWait:    3 * time.Second,
		},
		liveCfg: bridge.LiveLegConfig{
			MuxIP: *muxIP, ExcludeIface: splitCSV(*exIface), STUN: *stun,
			GatherWait: 3 * time.Second, SetMarker: *marker, MaxAge: *maxLive,
			OpusFmtp: *opusFmtp,
		},
	}
	if *bye != "off" {
		var phrases []string
		if *bye != "" {
			phrases = splitCSV(*bye)
		}
		a.farewell = bridge.NewFarewell(phrases, a.ev)
	}
	a.live = openai.NewLive(apiKey, *model)
	a.live.OnEvent = a.ev
	if bud, berr := openai.NewBudget(*budgetP, *budgetUSD, a.ev); berr != nil {
		// A budget that cannot be written is a budget that is not enforced, and this one exists
		// because money was spent that nobody was counting. Refuse to start rather than run blind.
		fmt.Fprintln(os.Stderr, "budget:", berr)
		os.Exit(2)
	} else {
		// Admission needs room for a whole session, not only for what is already spent.
		bud.Reserve = *maxLive
		a.live.Budget = bud
		a.budget = bud
	}

	if *toolsOn {
		reg := tools.NewRegistry()
		reg.Add(tools.Spend(*budgetP, openai.LiveRatePerMinuteUSD, *budgetUSD, a.liveAge))
		reg.Add(tools.HostStatus())
		reg.Add(tools.HostJobs(splitCSV(*logSvcs)))
		reg.Add(tools.HostLogs(splitCSV(*logSvcs)))
		// Before the search, deliberately. "How hot is it" through a general web search costs 3.7
		// seconds and returns a page about the weather; this is the reading, in half a second.
		if *lat != 0 || *lon != 0 {
			reg.Add(tools.Weather(*lat, *lon, *tz, nil))
			reg.Add(tools.Prayer(*lat, *lon, *tz, nil))
		}
		if a.mem != nil {
			reg.Add(tools.RememberThis(a.mem))
			reg.Add(tools.WhatYouKnow(a.mem))
		}
		if *vaultURL != "" {
			vk := ""
			if *vaultKey != "" {
				if b, verr := os.ReadFile(*vaultKey); verr == nil {
					vk = strings.TrimSpace(string(b))
				} else {
					fmt.Fprintln(os.Stderr, "vault key file:", verr)
					os.Exit(2)
				}
			}
			vc := &tools.VaultClient{Base: *vaultURL, Key: vk}
			reg.Add(tools.VaultSearch(vc))
			reg.Add(tools.VaultAppend(vc))
		}
		if eps := parseEndpoints(*homeEPs); len(eps) > 0 {
			reg.Add(tools.HomeNetwork(eps, nil))
		}
		// The two searches are mutually exclusive: both would register the same name, and paying a
		// cent for the second opinion of a search that already answered is the opposite of the point.
		payForSearch := false
		var web *tools.Web
		switch *searchOn {
		case "free", "":
			web = tools.NewWeb(nil, tools.SearchViaReader)
		case "private":
			web = tools.NewWeb(nil, tools.SearchDirect)
		case "openai":
			payForSearch = true
		case "off":
		default:
			fmt.Fprintf(os.Stderr, "--web-search must be free, private, openai or off, not %q\n", *searchOn)
			os.Exit(2)
		}
		if web != nil {
			reg.Add(web.SearchTool())
			reg.Add(web.OpenTool())
		}
		if a.farewell != nil {
			// A goodbye the phrase list does not have still ends the call: the model says so.
			reg.Add(tools.EndCall())
		}
		a.assist = &assistant{
			answerer: &openai.Answerer{
				APIKey: apiKey, Model: *searchMd, Tools: reg, OnEvent: a.ev,
				WebSearch: payForSearch, MaxOutputTokens: 1500, Deadline: 25 * time.Second,
				Reasoning: *searchFx,
			},
			window:        newTranscriptWindow(45 * time.Second),
			pending:       &pendingWrite{},
			ev:            a.ev,
			confirmWithin: 30 * time.Second,
		}
		if web != nil {
			a.assist.answerer.Prefetch = web.Speculative
		}
		if a.farewell != nil {
			f := a.farewell
			a.assist.hangUp = func(at time.Time) bool { return f.Arm("end_call", at) }
		}
		a.ev("tools_ready", "tools", reg.Names(), "web_search", *searchOn, "paid_search", payForSearch, "model", *searchMd)
	}
	a.backend = openai.NewBackend(apiKey, *backend)
	a.backend.OnEvent = a.ev

	// Anything left in the ledger belongs to a process that died without hanging up, and is
	// still on the clock. Close it before doing anything else. A ledger that cannot be opened
	// means a killed process could never be recovered from, so like the budget it refuses to
	// start rather than run blind.
	ledger, lerr := openai.NewLedger(*ledgerP, a.ev)
	if lerr != nil {
		fmt.Fprintln(os.Stderr, "ledger:", lerr)
		os.Exit(2)
	}
	a.live.Ledger = ledger // written by the client the moment a session exists, read by the sweeps
	a.ledger = ledger
	a.maxLive = *maxLive
	swctx, swcancel := context.WithTimeout(context.Background(), 30*time.Second)
	ledger.SweepWith(swctx, a.live, a.sweepOptions(false))
	swcancel()

	srv := alexa.NewServer(key, a)
	srv.OnEvent = a.ev
	hs := &http.Server{Addr: *listen, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	a.ev("bridge_start", "listen", *listen, "run_dir", lg.RunDir, "pid", os.Getpid(),
		"voice_model", *model, "backend_model", *backend, "delegation", *deleg,
		"video", a.cfg.Video, "video_fps", *fps, "idle_close_s", idle.Seconds(),
		"max_live_s", maxLive.Seconds(), "fork_enabled", *fork, "set_marker", *marker,
		"ledger", *ledgerP, "record_dir", *recDir,
		"opus_fmtp", *opusFmtp, "noise_reduction", *noiseRed, "pace_ms", pace.Milliseconds(),
		"video_stream_id", *vStream, "ext", *ext, "spurt_marker", *mark,
		"farewell", *bye != "off", "farewell_grace_s", byeGrace.Seconds(),
		"daily_budget_usd", *budgetUSD, "tools", *toolsOn, "web_search", *searchOn)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.keepWarm(ctx, *warm)
	go a.sweepLedger(ctx, 5*time.Minute)
	go func() {
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			a.ev("http_error", "err", err.Error())
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	// In this order: stop admitting offers, let one already being answered finish, take the call
	// down, and then wait for a dial still in flight to come back and close what it built. The
	// teardown used to run first, with the listener still accepting, so a directive arriving in
	// that window could open a session nobody would close.
	a.closing.Store(true)
	shctx, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	if err := hs.Shutdown(shctx); err != nil {
		fmt.Fprintln(os.Stderr, "http shutdown:", err)
	}
	a.teardown("shutdown")
	dialsDone := make(chan struct{})
	go func() { a.dials.Wait(); close(dialsDone) }()
	select {
	case <-dialsDone:
	case <-time.After(20 * time.Second):
		a.ev("shutdown_dial_still_running", "note", "its session is on the ledger; the next start sweeps it")
	}
	// Last thing before the lights go out: anything the ledger still holds that this process owns
	// and failed to hang up gets one more try. A clean stop used to leave that to "the next
	// start", which a stopped service does not have. Grok found it.
	finalCtx, finalCancel := context.WithTimeout(context.Background(), 20*time.Second)
	ledger.SweepWith(finalCtx, a.live, a.sweepOptions(false))
	finalCancel()
	a.ev("bridge_stop")
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// watchFarewell closes the call once a goodbye has waited out its grace period. The grace is not
// politeness: it is the window in which the user can take the goodbye back by saying anything else,
// and in which the assistant can finish her own reply before the line drops.
// farewellMaxWait caps how long a goodbye waits for the assistant to stop talking. Without it a
// model that never finishes its sentence keeps a billing session open on the strength of the very
// words that were meant to end it.
const farewellMaxWait = 10 * time.Second

// sidebandLostGrace is how long packet arrival may stand in for the transcript after the control
// socket is lost for good. With the 60 s idle timer after it, a call whose goodbye can no longer
// be heard ends within about three minutes instead of at the ten-minute cap.
const sidebandLostGrace = 2 * time.Minute

// greetingWait bounds her «هلا»: the wait for the Echo's transport plus the clip itself. The Echo
// connects 1.3-2.2 s after the offer and the clip is 0.8 s; a call that has not connected by
// then is not going to be greeted, and nothing else should wait on it.
const greetingWait = 8 * time.Second

// farewellReplyWait is how long his goodbye waits for hers to BEGIN. Measured replies start 1-3 s
// after his words; a model that says nothing at all ends the call after this instead.
const farewellReplyWait = 4 * time.Second

// watchFarewell ends the call after the user says goodbye.
//
// The grace period is measured from the moment SHE STOPS TALKING, not from the moment he said
// goodbye. Measured from the goodbye, two seconds lands in the middle of her own farewell and cuts
// it off mid-word, which is what happened on the 13:47 call: he said it at 13:47:15.8 and the line
// dropped at 13:47:17.8 while she was still speaking.
//
// And it ends the WHOLE call, not just the billing half. Closing only the OpenAI leg left the Echo
// sitting in an open Alexa call with nobody on the other end: on that same call our side hung up at
// 13:47:18 and Alexa did not disconnect until 13:47:40, twenty-three seconds of silence he had to
// end himself.
func (a *app) watchFarewell(leg *bridge.LiveLeg, f *bridge.Farewell) {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		a.mu.Lock()
		cur := a.leg
		session := a.sess
		a.mu.Unlock()
		if cur != leg || session == nil {
			return // this call is over; the watcher belongs to it
		}
		phrase, at, armed := f.Armed()
		if !armed {
			continue
		}
		// Three moments matter: his goodbye, her first sound after it, and her last. She needs a
		// moment to start (the model's reply lands 1-3 s after his words), then the grace runs from
		// her LAST sound so a goodbye said three seconds after she stopped is not already "quiet
		// enough". Counting the grace from his goodbye alone cut her reply off before it began
		// whenever she took longer than the grace to start (the 2026-09-16 fleet).
		waited := time.Since(at)
		last := leg.LastVoice()
		quiet := waited
		replied := last.After(at)
		if replied {
			quiet = time.Since(last)
		}
		switch {
		case !replied && waited < farewellReplyWait:
			continue // she has not started her goodbye yet; give her the moment
		case replied && quiet < a.byeGrace && waited < farewellMaxWait:
			continue // still saying her own goodbye; let her finish
		}
		a.mu.Lock()
		if a.leg != leg || a.sess != session {
			a.mu.Unlock()
			return
		}
		f.Disarm()
		a.mu.Unlock()
		a.ev("farewell_close", "phrase", phrase, "grace_s", a.byeGrace.Seconds(), "replied", replied,
			"quiet_s", quiet.Seconds(), "waited_s", waited.Seconds(),
			"note", "she finished, so the whole call ends: OpenAI stops billing and the Echo transport is dropped; whether Alexa then ends the call on the device is not yet known")
		// teardown, not closeLeg: the Echo session goes down too, so Alexa ends the call instead of
		// leaving him holding an open line.
		a.teardownFor(session, "farewell")
		return
	}
}

// openMemory loads the profile, or returns nil when memory is switched off. A profile that cannot
// be read is reported and then ignored: the assistant works without a memory, and refusing to start
// because of one would be the wrong trade.
func openMemory(path string) *memory.Profile {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	p, err := memory.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory:", err, "- continuing without it")
		return nil
	}
	return p
}
