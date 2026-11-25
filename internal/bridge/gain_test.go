package bridge

import (
	"math"
	"testing"
)

func synthSine(amp float64, freq float64, n int) []int16 {
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		v := amp * math.Sin(2*math.Pi*freq*float64(i)/float64(agcSampleRate))
		out[i] = int16(math.Round(v * 32767.0))
	}
	return out
}

func rmsDB(pcm []int16) float64 {
	var sum float64
	for _, s := range pcm {
		v := float64(s) / 32768.0
		sum += v * v
	}
	rms := math.Sqrt(sum / float64(len(pcm)))
	if rms < 1e-6 {
		return -120
	}
	return 20 * math.Log10(rms)
}

func TestMicAGCLeavesSilenceUntouched(t *testing.T) {
	agc := NewMicAGC(18)
	// -60 dBFS quiet room noise floor should stay below the -50 dBFS gate.
	amp := math.Pow(10, -60.0/20.0) * math.Sqrt2
	for i := 0; i < 15; i++ {
		frame := synthSine(amp, 440, 960)
		g := agc.Process(frame)
		if g > 1.05 {
			t.Fatalf("frame %d: noise floor boosted unexpectedly (gain=%.2f)", i, g)
		}
	}
}

func TestMicAGCBoostsFarFieldSpeech(t *testing.T) {
	agc := NewMicAGC(18)
	// -40 dBFS RMS simulates someone speaking from across the room.
	amp := math.Pow(10, -40.0/20.0) * math.Sqrt2
	var lastDB float64
	for i := 0; i < 12; i++ {
		frame := synthSine(amp, 440, 960)
		agc.Process(frame)
		lastDB = rmsDB(frame)
	}
	// With +18 dB max boost, -40 dBFS should lift to roughly -22 dBFS.
	if lastDB < -26.0 || lastDB > -16.0 {
		t.Fatalf("expected far-field speech lifted near -22 dBFS, got %.1f dBFS", lastDB)
	}
}

func TestMicAGCNeverClipsLoudCloseSpeech(t *testing.T) {
	agc := NewMicAGC(18)
	// Start boosted on a quiet far speaker, then abruptly switch to loud close speech.
	quietAmp := math.Pow(10, -40.0/20.0) * math.Sqrt2
	for i := 0; i < 10; i++ {
		agc.Process(synthSine(quietAmp, 440, 960))
	}
	loud := synthSine(0.9, 440, 960)
	agc.Process(loud)
	for i, s := range loud {
		if s == 32767 || s == -32768 {
			t.Fatalf("hard clipped at sample %d: %d", i, s)
		}
	}
}

func TestMicAGCSuppressesSteadyTVNoiseAndPassesHumanSpeech(t *testing.T) {
	agc := NewMicAGC(12)
	// Simulate 3 seconds (150 frames) of steady -34 dBFS TV audio in the room.
	tvAmp := math.Pow(10, -34.0/20.0) * math.Sqrt2
	var tvGain float64
	for i := 0; i < 150; i++ {
		tvGain = agc.Process(synthSine(tvAmp, 440, 960))
	}
	if tvGain >= 0.85 {
		t.Fatalf("steady TV background should be suppressed below unity, got gain=%.2f", tvGain)
	}
	// Now human speaks loudly over the TV (-20 dBFS): gain must jump back up to >= 1.0 cleanly.
	speechAmp := math.Pow(10, -20.0/20.0) * math.Sqrt2
	var speechGain float64
	for i := 0; i < 6; i++ {
		speechGain = agc.Process(synthSine(speechAmp, 440, 960))
	}
	if speechGain < 1.0 {
		t.Fatalf("human speech over TV should not stay attenuated, got gain=%.2f", speechGain)
	}
}

func TestMicAGCZeroDisablesBoost(t *testing.T) {
	agc := NewMicAGC(0)
	frame := synthSine(0.01, 440, 960)
	orig := append([]int16(nil), frame...)
	if g := agc.Process(frame); g != 1.0 {
		t.Fatalf("expected unity gain, got %.2f", g)
	}
	for i := range frame {
		if frame[i] != orig[i] {
			t.Fatalf("disabled AGC mutated sample %d", i)
		}
	}
}
