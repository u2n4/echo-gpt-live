// echosim imitates the Amazon side of an Alexa RTCSessionController session for local
// tests without the physical Echo: it builds an offer with the same shape as the real
// Echo offer (Opus/PCMU/PCMA audio sendrecv with audio-level + transport-cc extmaps,
// H.264 video sendrecv, non-trickle), signs it with the shared HMAC key exactly like the
// Lambda does, POSTs it to rtcdiag, applies the answer, exchanges media for -duration,
// then POSTs /rtc/disconnected (imitating Alexa's directive) and prints a JSON summary.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"

	"github.com/u2n4/echo-gpt-live/internal/alexa"
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func post(base string, key []byte, path string, v any) (int, []byte) {
	body, _ := json.Marshal(v)
	ts, sig := alexa.Sign(key, body)
	req, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ACV-Timestamp", ts)
	req.Header.Set("X-ACV-Signature", sig)
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	must(err)
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func main() {
	base := flag.String("url", "http://127.0.0.1:8787", "rtcdiag signaling base URL")
	keyFile := flag.String("key-file", os.ExpandEnv("$HOME/.config/acv/signing.key"), "shared HMAC key")
	dur := flag.Duration("duration", 40*time.Second, "how long to keep the session before sending disconnected")
	audioFile := flag.String("audio-file", "media/tone440_30s.ogg", "Ogg/Opus to send toward rtcdiag")
	speakFor := flag.Duration("speak-for", 0, "keep repeating the audio for this long and then go quiet. 0 means for the whole session. This is how a user who stops talking is simulated, which is what an idle timer and a goodbye both need")
	withVideo := flag.Bool("video", true, "offer an H.264 video m-line like the Echo")
	flag.Parse()

	key, err := os.ReadFile(*keyFile)
	must(err)
	key = []byte(strings.TrimSpace(string(key)))

	m := &webrtc.MediaEngine{}
	must(m.RegisterDefaultCodecs())
	must(m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: "urn:ietf:params:rtp-hdrext:ssrc-audio-level"}, webrtc.RTPCodecTypeAudio))
	ir := &interceptor.Registry{}
	must(webrtc.RegisterDefaultInterceptors(m, ir))
	se := webrtc.SettingEngine{}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	must(err)

	audio, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"}, "audio", "blackbird")
	must(err)
	_, err = pc.AddTransceiverFromTrack(audio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
	must(err)
	if *withVideo {
		_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
		must(err)
	}

	var inPkts, inVideo atomic.Int64
	var seqJumps, tsJumps atomic.Int64
	var firstIn atomic.Int64
	states := []string{}
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		states = append(states, s.String())
		fmt.Fprintln(os.Stderr, "[echosim] ice", s)
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) { fmt.Fprintln(os.Stderr, "[echosim] pc", s) })
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		fmt.Fprintf(os.Stderr, "[echosim] remote track %s %s pt=%d ssrc=%d\n", tr.Kind(), tr.Codec().MimeType, tr.PayloadType(), tr.SSRC())
		// What the Echo's own receiver would see: a sequence that jumps by more than a few
		// packets, forward or back, is what its replay protection and jitter buffer judge, and it
		// is the thing the bridge now rebases before forwarding.
		var lastSeq uint16
		var lastTS uint32
		var lastAt time.Time
		var have bool
		for {
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			if tr.Kind() == webrtc.RTPCodecTypeAudio {
				if inPkts.Add(1) == 1 {
					firstIn.Store(time.Now().UnixMilli())
				}
				if have {
					if d := int32(int16(pkt.SequenceNumber - lastSeq)); d > 64 || d < -64 {
						seqJumps.Add(1)
					}
					// A timestamp jump is one the clock does not explain: the timeline moved
					// backwards, or forwards by more than the wall-clock pause since the previous
					// packet plus 400 ms of slack. A two-second silence that advances the
					// timestamp by two seconds is a pause, not a jump.
					d := int64(int32(pkt.Timestamp - lastTS))
					wall := time.Since(lastAt).Milliseconds() * 48
					if d < -48000*120/1000 || d-wall > 48000*400/1000 {
						tsJumps.Add(1)
					}
				}
				lastSeq, lastTS, lastAt, have = pkt.SequenceNumber, pkt.Timestamp, time.Now(), true
			} else {
				inVideo.Add(1)
			}
		}
	})

	offer, err := pc.CreateOffer(nil)
	must(err)
	gc := webrtc.GatheringCompletePromise(pc)
	must(pc.SetLocalDescription(offer))
	<-gc
	sdp := pc.LocalDescription().SDP
	sid := uuid.NewString()
	t0 := time.Now()
	code, body := post(*base, key, "/rtc/offer", map[string]any{"v": 1, "type": "offer", "sessionId": sid, "sdp": sdp, "ts": float64(time.Now().UnixNano()) / 1e9, "nonce": uuid.NewString()})
	answerMs := time.Since(t0).Milliseconds()
	if code != 200 {
		fmt.Fprintf(os.Stderr, "[echosim] offer rejected: %d %s\n", code, body)
		os.Exit(1)
	}
	var ans struct {
		SDP string `json:"sdp"`
	}
	must(json.Unmarshal(body, &ans))
	must(pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.SDP}))

	// send audio from file at real time
	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()
	var outPkts atomic.Int64
	go func() {
		quietAt := time.Time{}
		if *speakFor > 0 {
			quietAt = time.Now().Add(*speakFor)
		}
		for ctx.Err() == nil {
			if !quietAt.IsZero() && time.Now().After(quietAt) {
				fmt.Fprintln(os.Stderr, "[echosim] done speaking; staying quiet for the rest of the session")
				return
			}
			f, err := os.Open(*audioFile)
			if err != nil {
				fmt.Fprintln(os.Stderr, "[echosim] audio file:", err)
				return
			}
			ogg, _, err := oggreader.NewWith(f)
			if err != nil {
				f.Close()
				return
			}
			tk := time.NewTicker(20 * time.Millisecond)
			for ctx.Err() == nil {
				if !quietAt.IsZero() && time.Now().After(quietAt) {
					break
				}
				p, _, err := ogg.ParseNextPage()
				if err != nil {
					break
				}
				if len(p) > 0 {
					_ = audio.WriteSample(media.Sample{Data: p, Duration: 20 * time.Millisecond})
					outPkts.Add(1)
				}
				<-tk.C
			}
			tk.Stop()
			f.Close()
		}
	}()
	<-ctx.Done()
	code, _ = post(*base, key, "/rtc/disconnected", map[string]any{"v": 1, "type": "disconnected", "sessionId": sid, "ts": float64(time.Now().UnixNano()) / 1e9, "nonce": uuid.NewString()})
	time.Sleep(500 * time.Millisecond)
	_ = pc.Close()

	// answer summary lines
	var lines []string
	for _, ln := range strings.Split(strings.ReplaceAll(ans.SDP, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(ln, "m=") || strings.HasPrefix(ln, "a=group") || strings.HasPrefix(ln, "a=setup") || strings.HasPrefix(ln, "a=inactive") || strings.HasPrefix(ln, "a=sendrecv") || strings.HasPrefix(ln, "a=sendonly") || strings.HasPrefix(ln, "a=recvonly") || strings.HasPrefix(ln, "a=extmap") ||
			// the negotiated codec parameters decide audio quality, so a simulator that cannot
			// show them cannot answer the only question worth asking about a voice bridge
			strings.HasPrefix(ln, "a=rtpmap") || strings.HasPrefix(ln, "a=fmtp") {
			lines = append(lines, ln)
		}
	}
	out, _ := json.MarshalIndent(map[string]any{
		"sessionId": sid, "answer_status": 200, "answer_ms": answerMs, "disconnect_status": code,
		"ice_states": states, "audio_pkts_sent": outPkts.Load(), "audio_pkts_received": inPkts.Load(), "audio_seq_jumps": seqJumps.Load(), "audio_ts_jumps": tsJumps.Load(), "video_pkts_received": inVideo.Load(),
		"first_audio_in_ms_after_offer": firstIn.Load() - t0.UnixMilli(), "answer_lines": lines,
	}, "", "  ")
	fmt.Println(string(out))
}
