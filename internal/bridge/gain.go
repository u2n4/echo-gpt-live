package bridge

// Far-field microphone Automatic Gain Control (AGC) for the Echo Dot -> OpenAI path.
//
// Echo Dot 5's 4-mic hardware array produces a very clean silence floor (-60 to -79 dBFS measured
// on real room recordings) and loud close speech (-20 to -27 dBFS), but someone speaking from
// across the room arrives 15-22 dB quieter (-38 to -50 dBFS), which falls under GPT-Live's
// sensitivity and standby VAD threshold.
//
// Because GPT-Live does not expose server-side far_field noise_reduction (unlike the Realtime API),
// this DSP stage lifts quiet speech toward a steady -16 dBFS target while leaving room silence
// untouched (noise gate at -50 dBFS) and softly limiting close speech so it never clips.

import (
	"math"
	"sync"

	"github.com/pion/rtp"
)

const (
	agcSampleRate      = 48000
	agcTargetDBFS      = -16.0
	agcGateDBFS        = -50.0
	agcMaxGainCap      = 24.0
	agcAttack          = 0.35  // ~50 ms at 20 ms frames: catches the first syllable of a far speaker
	agcRelease         = 0.05  // ~400 ms decay: avoids pumping between syllables
	agcFastDecay       = 0.65  // fast pull-down when a close speaker interrupts loudly
	agcHPFAlpha        = 0.989 // ~85 Hz 1st-order high-pass at 48 kHz (removes AC/fan rumble)
	agcNoiseInitDBFS   = -58.0 // quiet room baseline
	agcNoiseDropAlpha  = 0.15  // fast downward adaptation when TV/noise stops
	agcNoiseRiseAlpha  = 0.015 // slow upward adaptation (~1.3 s) tracks steady TV/AC without tracking speech bursts
	agcNoiseMarginDB   = 6.5   // speech must stand at least 6.5 dB above steady room/TV floor to be boosted
	agcNoisyRoomDBFS   = -45.0 // above this steady floor, the TV or background noise is active
	agcTVSuppressGain  = 0.45  // gentle -7 dB attenuation of steady TV floor below the speech gate
)

// MicAGC is a stateful per-call mono 48 kHz adaptive gain controller with an adaptive
// room/TV noise-floor tracker so steady TV audio is never amplified into speech volume.
type MicAGC struct {
	maxGain   float64
	gain      float64
	noiseDBFS float64
	hpPrevX   float64
	hpPrevY   float64
}

// NewMicAGC creates an AGC with the given maximum boost in dB (e.g. 12 dB = 4x).
// Values <= 0 disable boosting (unity gain).
func NewMicAGC(maxBoostDB float64) *MicAGC {
	if maxBoostDB <= 0 {
		return &MicAGC{maxGain: 1.0, gain: 1.0, noiseDBFS: agcNoiseInitDBFS}
	}
	if maxBoostDB > agcMaxGainCap {
		maxBoostDB = agcMaxGainCap
	}
	return &MicAGC{
		maxGain:   math.Pow(10, maxBoostDB/20.0),
		gain:      1.0,
		noiseDBFS: agcNoiseInitDBFS,
	}
}

// Process applies an 85 Hz high-pass filter, adaptive far-field AGC, and a soft peak limiter
// in-place on a mono 16-bit PCM frame (typically 960 samples = 20 ms at 48 kHz).
// Returns the linear gain applied to this frame.
func (a *MicAGC) Process(pcm []int16) float64 {
	if a == nil || len(pcm) == 0 || a.maxGain <= 1.001 {
		return 1.0
	}

	// Pass 1: high-pass filter to strip DC/AC hum and measure speech energy (RMS).
	var sumSq float64
	var peak float64
	prevX, prevY := a.hpPrevX, a.hpPrevY
	// Reuse a tiny stack/slice buffer via float64 conversion on the fly.
	filtered := make([]float64, len(pcm))
	for i, s := range pcm {
		x := float64(s) / 32768.0
		y := agcHPFAlpha * (prevY + x - prevX)
		prevX, prevY = x, y
		filtered[i] = y
		sumSq += y * y
		if ay := math.Abs(y); ay > peak {
			peak = ay
		}
	}
	a.hpPrevX, a.hpPrevY = prevX, prevY

	rms := math.Sqrt(sumSq / float64(len(pcm)))
	dbfs := -120.0
	if rms > 1e-6 {
		dbfs = 20.0 * math.Log10(rms)
	}

	// Track steady room/TV background noise floor (minimum-statistics style asymmetric envelope):
	// drops quickly in silence, rises slowly only during continuous TV/AC background audio.
	if dbfs < a.noiseDBFS {
		a.noiseDBFS += agcNoiseDropAlpha * (dbfs - a.noiseDBFS)
	} else {
		// Cap upward tracking at -22 dBFS so even a loud TV never completely blocks close speech.
		target := math.Min(dbfs, -22.0)
		a.noiseDBFS += agcNoiseRiseAlpha * (target - a.noiseDBFS)
	}

	dynamicGate := agcGateDBFS
	if g := a.noiseDBFS + agcNoiseMarginDB; g > dynamicGate {
		dynamicGate = g
	}

	// Decide desired gain:
	// - Above the dynamic gate (human speech standing out over room/TV floor): boost smoothly
	//   toward -16 dBFS, capped by maxGain and peak headroom.
	// - Below the dynamic gate in a noisy room (TV playing): gently attenuate (-7 dB) instead of
	//   boosting so OpenAI's ASR hears the human voice clearly instead of transcribing TV actors.
	// - Below the gate in a quiet room: hold unity gain (1.0) so silence stays untouched.
	desired := 1.0
	if dbfs > dynamicGate {
		neededDB := agcTargetDBFS - dbfs
		if neededDB > 0 {
			desired = math.Pow(10, neededDB/20.0)
			if desired > a.maxGain {
				desired = a.maxGain
			}
		}
		if peak > 1e-6 && desired*peak > 0.85 {
			desired = 0.85 / peak
			if desired < 1.0 {
				desired = 1.0
			}
		}
	} else if a.noiseDBFS > agcNoisyRoomDBFS {
		desired = agcTVSuppressGain
	}

	// Smooth gain transitions: fast pull-down on sudden loud speech, snappy lift on speech,
	// gentle release during silence or TV floor suppression.
	alpha := agcRelease
	if desired > a.gain {
		alpha = agcAttack
	} else if dbfs > dynamicGate && desired < a.gain {
		alpha = agcFastDecay
	}
	prevGain := a.gain
	a.gain += alpha * (desired - a.gain)
	minGain := agcTVSuppressGain
	if a.noiseDBFS <= agcNoisyRoomDBFS {
		minGain = 1.0
	}
	if a.gain < minGain {
		a.gain = minGain
	}

	// Pass 2: linearly interpolate gain across the frame to avoid click artifacts at frame edges,
	// with a soft tanh knee above 0.85 so close shouts never hard-clip.
	n := float64(len(filtered))
	for i, s := range filtered {
		g := prevGain + (a.gain-prevGain)*(float64(i)/n)
		v := s * g
		if av := math.Abs(v); av > 0.85 {
			// Soft saturator mapping [0.85, +inf) smoothly into [0.85, 0.98).
			sign := 1.0
			if v < 0 {
				sign = -1.0
			}
			v = sign * (0.85 + 0.13*math.Tanh((av-0.85)/0.13))
		}
		scaled := math.Round(v * 32767.0)
		if scaled > 32767 {
			scaled = 32767
		} else if scaled < -32768 {
			scaled = -32768
		}
		pcm[i] = int16(scaled)
	}
	return a.gain
}

// opusCodec decodes/encodes one mono 48 kHz Opus frame (implemented via libopus.so.0 on Linux).
type opusCodec interface {
	Decode(payload []byte, pcm []int16) (int, error)
	Encode(pcm []int16, out []byte) (int, error)
	Close()
}

// MicBooster wraps an opusCodec and MicAGC to boost far-field speech in RTP Opus packets.
// Safe for concurrent calls; fails open (returns the original packet untouched) on any codec error.
type MicBooster struct {
	mu    sync.Mutex
	codec opusCodec
	agc   *MicAGC
	pcm   []int16
	enc   []byte
	maxDB float64
}

// NewMicBooster builds a per-call far-field Opus booster. Returns (nil, nil) when maxBoostDB <= 0.
// On platforms without libopus.so.0 it returns (nil, err) so the caller can fall back to passthrough.
func NewMicBooster(maxBoostDB float64) (*MicBooster, error) {
	if maxBoostDB <= 0 {
		return nil, nil
	}
	c, err := openOpusCodec(agcSampleRate, 1)
	if err != nil {
		return nil, err
	}
	return &MicBooster{
		codec: c,
		agc:   NewMicAGC(maxBoostDB),
		pcm:   make([]int16, 5760), // max Opus frame size (120 ms at 48 kHz)
		enc:   make([]byte, 1500),
		maxDB: maxBoostDB,
	}, nil
}

// ProcessRTP decodes one Opus RTP packet, applies far-field AGC, and returns a new RTP packet
// preserving header fields (sequence, timestamp, marker, SSRC, PT). On any error or empty payload,
// returns the original packet untouched.
func (b *MicBooster) ProcessRTP(p *rtp.Packet) *rtp.Packet {
	if b == nil || p == nil || len(p.Payload) <= 2 {
		return p
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.codec == nil {
		return p
	}
	n, err := b.codec.Decode(p.Payload, b.pcm)
	if err != nil || n <= 0 {
		return p
	}
	b.agc.Process(b.pcm[:n])
	m, err := b.codec.Encode(b.pcm[:n], b.enc)
	if err != nil || m <= 0 {
		return p
	}
	out := *p
	out.Payload = make([]byte, m)
	copy(out.Payload, b.enc[:m])
	return &out
}

// Close releases the underlying Opus decoder and encoder.
func (b *MicBooster) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.codec != nil {
		b.codec.Close()
		b.codec = nil
	}
}
