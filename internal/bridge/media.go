package bridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

// playOggOpus loops an Ogg/Opus file (one 20 ms packet per page, i.e. produced with
// ffmpeg -page_duration 20000) into a sample track at real time until ctx ends.
// Returns the number of samples written.
func playOggOpus(ctx context.Context, path string, track *webrtc.TrackLocalStaticSample, onErr func(error)) {
	for ctx.Err() == nil {
		f, err := os.Open(path)
		if err != nil {
			onErr(err)
			return
		}
		ogg, _, err := oggreader.NewWith(f)
		if err != nil {
			_ = f.Close()
			onErr(err)
			return
		}
		var lastGranule uint64
		ticker := time.NewTicker(20 * time.Millisecond)
		for ctx.Err() == nil {
			payload, hdr, err := ogg.ParseNextPage()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				onErr(err)
				break
			}
			// granule positions are 48 kHz sample counts
			n := hdr.GranulePosition - lastGranule
			lastGranule = hdr.GranulePosition
			dur := time.Duration(n) * time.Second / 48000
			if dur <= 0 || dur > 120*time.Millisecond {
				dur = 20 * time.Millisecond
			}
			if len(payload) == 0 {
				continue
			}
			if err := track.WriteSample(media.Sample{Data: payload, Duration: dur}); err != nil {
				onErr(err)
			}
			select {
			case <-ctx.Done():
			case <-ticker.C:
			}
		}
		ticker.Stop()
		_ = f.Close()
	}
}

// playH264 loops an Annex-B .h264 file (all frames IDR with repeated SPS/PPS) into a
// sample track at fps frames per second. Each access unit (SPS+PPS+IDR) is written as ONE
// sample so the payloader emits it under one RTP timestamp with the marker on the last packet.
func playH264(ctx context.Context, path string, fps float64, track *webrtc.TrackLocalStaticSample, onErr func(error), onFrame func(n int, bytes int)) {
	if fps <= 0 {
		fps = 1
	}
	frameDur := time.Duration(float64(time.Second) / fps)
	aus, err := loadAccessUnits(path)
	if err != nil {
		onErr(err)
		return
	}
	if len(aus) == 0 {
		onErr(errors.New("h264 file contains no access units"))
		return
	}
	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()
	i, n := 0, 0
	for ctx.Err() == nil {
		au := aus[i%len(aus)]
		if err := track.WriteSample(media.Sample{Data: au, Duration: frameDur}); err != nil {
			onErr(err)
			return
		}
		n++
		if onFrame != nil {
			onFrame(n, len(au))
		}
		i++
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// loadAccessUnits splits an Annex-B stream into access units, each ending with a VCL NAL
// (type 1 or 5). Non-VCL NALs (SPS 7, PPS 8, SEI 6, AUD 9) are kept in front of their VCL.
func loadAccessUnits(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := h264reader.NewReader(f)
	if err != nil {
		return nil, err
	}
	var aus [][]byte
	var cur bytes.Buffer
	startCode := []byte{0, 0, 0, 1}
	for {
		nal, err := r.NextNAL()
		if errors.Is(err, io.EOF) || nal == nil {
			break
		}
		if err != nil {
			return nil, err
		}
		t := nal.Data[0] & 0x1f
		if t == 9 { // AUD: skip
			continue
		}
		cur.Write(startCode)
		cur.Write(nal.Data)
		if t == 1 || t == 5 {
			aus = append(aus, append([]byte(nil), cur.Bytes()...))
			cur.Reset()
		}
	}
	return aus, nil
}

// reflector forwards received audio RTP back to the local track after an optional delay.
type reflector struct {
	mu      sync.Mutex
	closed  bool
	stopped chan struct{}
	track   *webrtc.TrackLocalStaticRTP
	delay   time.Duration
	ch      chan delayed
}

type delayed struct {
	at  time.Time
	pkt *rtp.Packet
}

func newReflector(track *webrtc.TrackLocalStaticRTP, delay time.Duration) *reflector {
	r := &reflector{track: track, delay: delay, ch: make(chan delayed, 1024), stopped: make(chan struct{})}
	go r.run()
	return r
}

func (r *reflector) push(p *rtp.Packet) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	select {
	case r.ch <- delayed{at: time.Now().Add(r.delay), pkt: p}:
	default: // drop if the delay buffer is full; never block the receive loop
	}
}

func (r *reflector) run() {
	defer close(r.stopped)
	for d := range r.ch {
		if w := time.Until(d.at); w > 0 {
			time.Sleep(w)
		}
		// Pion rewrites SSRC and payload type to the local track's binding; the
		// Opus payload, sequence number, timestamp and marker are forwarded unchanged.
		_ = r.track.WriteRTP(d.pkt)
	}
}

func (r *reflector) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		close(r.ch)
	}
}

// PlayOggOnce streams an Ogg/Opus file once, in real time, into a sample track.
// Exported for the OpenAI leg test, which speaks one utterance then listens.
//
// burst is how much of the file leaves unpaced, as fast as it can be written, before real-time
// pacing begins. It reproduces what a HoldSink does at session start - a second or two of speech
// arriving all at once - so an experiment can ask whether the far side hears it. Zero paces
// everything.
func PlayOggOnce(ctx context.Context, path string, track *webrtc.TrackLocalStaticSample, burst time.Duration, onErr func(error)) {
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
	var lastGranule uint64
	var sent time.Duration
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		payload, hdr, err := ogg.ParseNextPage()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			onErr(err)
			return
		}
		n := hdr.GranulePosition - lastGranule
		lastGranule = hdr.GranulePosition
		dur := time.Duration(n) * time.Second / 48000
		if dur <= 0 || dur > 120*time.Millisecond {
			dur = 20 * time.Millisecond
		}
		if len(payload) > 0 {
			if err := track.WriteSample(media.Sample{Data: payload, Duration: dur}); err != nil {
				onErr(err)
			}
		}
		sent += dur
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
