//go:build linux && cgo

package bridge

import (
	"math"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestLibOpusMicBoosterRoundtrip(t *testing.T) {
	b, err := NewMicBooster(18)
	if err != nil {
		t.Fatalf("NewMicBooster(18) failed on Linux: %v", err)
	}
	defer b.Close()

	// Encode a quiet -40 dBFS 440 Hz tone (20 ms = 960 samples @ 48 kHz) into Opus, then run
	// 12 frames through ProcessRTP and verify output is valid Opus and boosted cleanly.
	rawCodec, err := openOpusCodec(48000, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rawCodec.Close()

	amp := math.Pow(10, -40.0/20.0) * math.Sqrt2
	encBuf := make([]byte, 1500)
	decPCM := make([]int16, 960)
	var totalTime time.Duration
	var lastDB float64
	for i := 0; i < 12; i++ {
		inPCM := synthSine(amp, 440, 960)
		n, err := rawCodec.Encode(inPCM, encBuf)
		if err != nil || n <= 0 {
			t.Fatalf("seed encode failed: n=%d err=%v", n, err)
		}
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: uint16(i + 1), Timestamp: uint32(i * 960)},
			Payload: append([]byte(nil), encBuf[:n]...),
		}
		t0 := time.Now()
		out := b.ProcessRTP(pkt)
		totalTime += time.Since(t0)
		if out == nil || len(out.Payload) == 0 {
			t.Fatalf("frame %d: empty boosted payload", i)
		}
		dn, err := rawCodec.Decode(out.Payload, decPCM)
		if err != nil || dn != 960 {
			t.Fatalf("frame %d: decode boosted payload failed: dn=%d err=%v", i, dn, err)
		}
		lastDB = rmsDB(decPCM[:dn])
	}
	avgUS := totalTime.Microseconds() / 12
	t.Logf("libopus MicBooster avg=%d us/frame, boosted -40 dBFS -> %.1f dBFS", avgUS, lastDB)
	if lastDB < -27.0 || lastDB > -14.0 {
		t.Fatalf("expected boosted Opus frame near -22 dBFS, got %.1f dBFS", lastDB)
	}
}
