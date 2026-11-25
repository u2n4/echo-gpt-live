package bridge

import (
	"bytes"
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

func translatePackets(t *testing.T, packets ...*rtp.Packet) int {
	t.Helper()
	var tr liveSequenceTranslator
	rebases := 0
	for _, p := range packets {
		if _, ok := tr.translate(p); ok {
			rebases++
		}
	}
	return rebases
}

func packet(seq uint16, ts uint32, payload []byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SSRC: 7, SequenceNumber: seq, Timestamp: ts}, Payload: append([]byte(nil), payload...)}
}

func TestLiveSequenceOrdinaryTrafficUnchanged(t *testing.T) {
	ps := []*rtp.Packet{packet(100, 1000, []byte{0xd8}), packet(101, 1960, []byte{0xd8}), packet(104, 4840, []byte{0xd8}), packet(103, 3880, []byte{0xd8})}
	if got := translatePackets(t, ps...); got != 0 {
		t.Fatalf("rebases=%d", got)
	}
	for i, want := range []uint16{100, 101, 104, 103} {
		if ps[i].SequenceNumber != want {
			t.Fatalf("packet %d seq=%d want=%d", i, ps[i].SequenceNumber, want)
		}
	}
}

func TestLiveSequenceWrapUnchanged(t *testing.T) {
	ps := []*rtp.Packet{packet(65535, 0xfffffc3f, []byte{0xd8}), packet(0, 0xffffffff, []byte{0xd8}), packet(1, 959, []byte{0xd8})}
	if got := translatePackets(t, ps...); got != 0 {
		t.Fatalf("rebases=%d", got)
	}
}

func TestOpusPacketDurations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload []byte
		want    uint32
		ok      bool
	}{
		{"observed CELT 20ms", []byte{0xd8}, 960, true},
		{"CELT 2.5ms two frames", []byte{0x81}, 240, true},
		{"SILK 60ms", []byte{0x18}, 2880, true},
		{"arbitrary frame count", []byte{0x83, 3}, 360, true},
		{"missing frame count", []byte{0x83}, 0, false},
		{"zero frame count", []byte{0x83, 0}, 0, false},
		{"over 120ms", []byte{0xdb, 7}, 0, false},
		{"empty", nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := opusPacketDurationTicks(tc.payload)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("duration=(%d,%v) want=(%d,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestLiveSequenceUsesFirstPacketOpusDuration(t *testing.T) {
	for _, tc := range []struct {
		payload []byte
		want    uint32
	}{
		{[]byte{0x80}, 120},
		{[]byte{0x81}, 240},
		{[]byte{0x18}, 2880},
	} {
		ps := []*rtp.Packet{packet(600, 9000, tc.payload), packet(900, 10, []byte{0xd8})}
		if got := translatePackets(t, ps...); got != 1 || ps[1].SequenceNumber != 601 || ps[1].Timestamp != 9000+tc.want {
			t.Fatalf("payload=%x got=(%d,%d) rebases=%d", tc.payload, ps[1].SequenceNumber, ps[1].Timestamp, got)
		}
	}
}

func TestLiveSequenceOnlyTheStartCanTriggerRebase(t *testing.T) {
	// A jump after the window is a real discontinuity, left as it is and counted.
	ps := []*rtp.Packet{packet(100, 1000, []byte{0xd8})}
	for i := 1; i <= rebaseWindowPackets; i++ {
		ps = append(ps, packet(100+uint16(i), 1000+uint32(i)*960, []byte{0xd8}))
	}
	late := packet(30000, 2000000000, []byte{0xd8})
	ps = append(ps, late)
	var tr liveSequenceTranslator
	rebases := 0
	for _, p := range ps {
		if _, ok := tr.translate(p); ok {
			rebases++
		}
	}
	if rebases != 0 || late.SequenceNumber != 30000 || late.Timestamp != 2000000000 {
		t.Fatal("a later discontinuity was rebased")
	}
	if tr.Missed() != 1 {
		t.Fatalf("the late jump should be counted as missed, got %d", tr.Missed())
	}
}

// A second probe inside the thresholds must not spend the one check: the real restart comes
// next and is measured against the packet before it, not against packet 1.
func TestLiveSequenceSecondProbeDoesNotHideTheRestart(t *testing.T) {
	ps := []*rtp.Packet{
		packet(100, 1000, []byte{0xd8}),
		packet(101, 1960, []byte{0xd8}),  // a second probe, continuous with the first
		packet(30000, 960, []byte{0xd8}), // the restart
		packet(30001, 1920, []byte{0xd8}),
	}
	if got := translatePackets(t, ps...); got != 1 {
		t.Fatalf("rebases=%d, want 1", got)
	}
	if ps[2].SequenceNumber != 102 || ps[2].Timestamp != 2920 || ps[3].SequenceNumber != 103 || ps[3].Timestamp != 3880 {
		t.Fatalf("restart not spliced onto the probes: %d/%d %d/%d", ps[2].SequenceNumber, ps[2].Timestamp, ps[3].SequenceNumber, ps[3].Timestamp)
	}
}

func TestLiveSequenceObservedRestartsAndLaterDeltas(t *testing.T) {
	for _, observed := range []struct {
		firstSeq, secondSeq uint16
		firstTS, secondTS   uint32
	}{
		{25366, 29115, 2555167995, 960},
		{25428, 10261, 1851361368, 960},
		{24527, 71, 1309033975, 960},
	} {
		ps := []*rtp.Packet{
			packet(observed.firstSeq, observed.firstTS, []byte{0xd8}),
			packet(observed.secondSeq, observed.secondTS, []byte{0xd8}),
			packet(observed.secondSeq+3, observed.secondTS+2880, []byte{0xd8}),
			packet(observed.secondSeq+2, observed.secondTS+1920, []byte{0xd8}),
		}
		if got := translatePackets(t, ps...); got != 1 {
			t.Fatalf("vector %+v rebases=%d", observed, got)
		}
		if ps[0].SequenceNumber != observed.firstSeq || ps[0].Timestamp != observed.firstTS {
			t.Fatal("first probe changed")
		}
		if ps[1].SequenceNumber != observed.firstSeq+1 || ps[1].Timestamp != observed.firstTS+960 {
			t.Fatalf("second=(%d,%d)", ps[1].SequenceNumber, ps[1].Timestamp)
		}
		if ps[2].SequenceNumber != observed.firstSeq+4 || ps[3].SequenceNumber != observed.firstSeq+3 || ps[2].Timestamp-ps[3].Timestamp != 960 {
			t.Fatal("post-rebase gap or reorder was not preserved")
		}
	}
}

func TestLiveSequenceUnknownDurationDoesNotInventTimestamp(t *testing.T) {
	// A first packet with no readable duration (padding) gives no expected timestamp, so the
	// timestamp is left alone; the sequence still needs the repair, or the whole stream is
	// rejected as "too old" by the receiver's replay window.
	ps := []*rtp.Packet{packet(100, 1000, nil), packet(500, 20, []byte{0xd8})}
	if got := translatePackets(t, ps...); got != 1 || ps[1].SequenceNumber != 101 || ps[1].Timestamp != 20 {
		t.Fatalf("got rebases=%d seq=%d ts=%d; want 1, 101, 20", got, ps[1].SequenceNumber, ps[1].Timestamp)
	}
}

func TestLiveSequenceEncryptedTwoHop(t *testing.T) {
	profile := srtp.ProtectionProfileAes128CmHmacSha1_80
	saltLen, err := profile.SaltLen()
	if err != nil {
		t.Fatal(err)
	}
	key, salt := make([]byte, 16), make([]byte, saltLen)
	context := func(window uint) *srtp.Context {
		c, err := srtp.CreateContext(key, salt, profile, srtp.SRTPReplayProtection(window))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	source, bridgeIn := context(liveReplayWindow), context(liveReplayWindow)
	bridgeOut, downstream := context(64), context(64)
	controlSource, controlDownstream := context(liveReplayWindow), context(64)
	inputs := []*rtp.Packet{
		packet(25428, 1851361368, []byte{0xd8, 1, 2}),
		packet(10261, 960, []byte{0xd8, 3, 4}),
		packet(10262, 1920, []byte{0xd8, 5, 6}),
	}
	inputs[0].Marker = true
	var tr liveSequenceTranslator
	for i, original := range inputs {
		plain, err := original.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		cipher, err := source.EncryptRTP(nil, plain, nil)
		if err != nil {
			t.Fatal(err)
		}
		decrypted, err := bridgeIn.DecryptRTP(nil, cipher, nil)
		if err != nil {
			t.Fatalf("bridge decrypt %d: %v", i, err)
		}
		var forwarded rtp.Packet
		if err := forwarded.Unmarshal(decrypted); err != nil {
			t.Fatal(err)
		}
		tr.translate(&forwarded)
		outPlain, err := forwarded.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		outCipher, err := bridgeOut.EncryptRTP(nil, outPlain, nil)
		if err != nil {
			t.Fatal(err)
		}
		gotPlain, err := downstream.DecryptRTP(nil, outCipher, nil)
		if err != nil {
			t.Fatalf("downstream decrypt %d: %v", i, err)
		}
		var got rtp.Packet
		if err := got.Unmarshal(gotPlain); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Payload, original.Payload) || got.Marker != original.Marker || got.SSRC != original.SSRC || got.PayloadType != original.PayloadType {
			t.Fatalf("packet %d media/header fields changed", i)
		}
		if i > 0 && (got.SequenceNumber != inputs[0].SequenceNumber+uint16(i) || got.Timestamp != inputs[0].Timestamp+uint32(i)*960) {
			t.Fatalf("packet %d discontinuous seq=%d ts=%d", i, got.SequenceNumber, got.Timestamp)
		}

		controlCipher, err := controlSource.EncryptRTP(nil, plain, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, controlErr := controlDownstream.DecryptRTP(nil, controlCipher, nil)
		if (i == 0) != (controlErr == nil) {
			t.Fatalf("old unnormalized path did not reproduce backward rejection at %d: %v", i, controlErr)
		}
	}
}
