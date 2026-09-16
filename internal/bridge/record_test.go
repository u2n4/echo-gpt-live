package bridge

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// opusFrame is a minimal valid Opus packet: TOC byte with config 15 (hybrid, 20 ms), mono, one
// frame, plus a little payload. Enough for the Ogg writer to accept it.
func opusFrame(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: ts, SSRC: 1},
		Payload: []byte{0x78, 0x01, 0x02, 0x03, 0x04, 0x05},
	}
}

// padding is what a WebRTC sender emits for bandwidth probing: a header and no Opus frame.
func padding(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: ts, SSRC: 1},
		Payload: nil,
	}
}

// The bug this guards: one padding-only packet used to end the recording, and a real call was lost
// after 54 of 999 packets. The recording has to survive the packets that carry no audio.
func TestRecorderSurvivesPaddingOnlyPackets(t *testing.T) {
	dir := t.TempDir()
	var events []string
	r := NewRecorder(dir, "one-direction", func(ev string, kv ...any) { events = append(events, ev) })
	if r == nil {
		t.Fatal("recorder should have opened")
	}

	ts := uint32(0)
	seq := uint16(0)
	for i := 0; i < 100; i++ {
		if i == 54 {
			r.Write(padding(seq, ts))
		} else {
			r.Write(opusFrame(seq, ts))
		}
		seq++
		ts += 960
	}
	// Writing is asynchronous now, so the count is only final once the recorder has closed: the
	// media path must never wait for a disk.
	r.Close()
	if got := r.Packets(); got != 99 {
		t.Fatalf("recorded %d packets, want 99: the padding packet is skipped, the rest are kept", got)
	}

	fi, err := os.Stat(filepath.Join(dir, "one-direction.ogg"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() < 1000 {
		t.Fatalf("file is %d bytes; the audio after the padding packet was lost", fi.Size())
	}
	for _, e := range events {
		if e == "record_write_gave_up" {
			t.Fatal("one padding packet must not make the recorder give up")
		}
	}
}

// A nil Recorder is how recording stays optional, so every method has to tolerate one.
func TestNilRecorderIsSafe(t *testing.T) {
	var r *Recorder
	r.Write(opusFrame(1, 960))
	r.Close()
	if r.Packets() != 0 {
		t.Fatal("a nil recorder counts nothing")
	}
	if NewRecorder("", "x", nil) != nil {
		t.Fatal("an empty directory means recording is off")
	}
}

// Close must report what was captured even when nothing valid ever arrived, so a silent direction
// is visible in the log rather than being an absent file.
func TestRecorderReportsWhatItCaptured(t *testing.T) {
	dir := t.TempDir()
	var closed map[string]any
	r := NewRecorder(dir, "quiet", func(ev string, kv ...any) {
		if ev != "record_closed" {
			return
		}
		closed = map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			closed[kv[i].(string)] = kv[i+1]
		}
	})
	for i := 0; i < 5; i++ {
		r.Write(padding(uint16(i), uint32(i)*960))
	}
	r.Write(opusFrame(99, 960*99))
	r.Close()

	if closed == nil {
		t.Fatal("closing must report")
	}
	if closed["packets"] != int64(1) {
		t.Fatalf("packets = %v, want 1", closed["packets"])
	}
	if closed["padding_only_packets"] != int64(5) {
		t.Fatalf("padding_only_packets = %v, want 5", closed["padding_only_packets"])
	}
}

// The recorder sits on the media path, so a slow disk must cost the recording and never the call.
// Codex flagged the original synchronous write as able to postpone real audio by however long the
// filesystem took.
func TestRecorderNeverBlocksTheMediaPath(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir, "slow-disk", nil)
	if r == nil {
		t.Fatal("recorder should have opened")
	}
	defer r.Close()

	// Far more audio than the queue can hold, handed over as fast as a reader would.
	start := time.Now()
	for i := 0; i < 4000; i++ {
		r.Write(opusFrame(uint16(i), uint32(i)*960))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("handing over 80 s of audio took %v; the caller is waiting for the disk", d)
	}
}
