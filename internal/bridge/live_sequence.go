package bridge

import "github.com/pion/rtp"

const (
	sequenceJumpLimit                = 64
	timestampJumpLimit        uint32 = opusClockRate * 400 / 1000
	timestampReorderTolerance        = opusClockRate * 120 / 1000
)

// liveSequenceTranslator repairs the one discontinuity OpenAI may put between
// its probe packet and the real stream. Once chosen, the offsets stay fixed so
// every later gap, reorder and wrap retains the sender's original delta.
type liveSequenceTranslator struct {
	seen       int
	firstSeq   uint16
	firstTS    uint32
	firstDur   uint32
	durationOK bool
	seqOffset  uint16
	tsOffset   uint32
}

type liveSequenceRebase struct {
	oldSeq, newSeq uint16
	oldTS, newTS   uint32
}

func (t *liveSequenceTranslator) translate(pkt *rtp.Packet) (liveSequenceRebase, bool) {
	if pkt == nil {
		return liveSequenceRebase{}, false
	}
	t.seen++
	if t.seen == 1 {
		t.firstSeq, t.firstTS = pkt.SequenceNumber, pkt.Timestamp
		t.firstDur, t.durationOK = opusPacketDurationTicks(pkt.Payload)
		return liveSequenceRebase{}, false
	}
	if t.seen == 2 && t.durationOK {
		expectedSeq := t.firstSeq + 1
		expectedTS := t.firstTS + t.firstDur
		seqDelta := int32(int16(pkt.SequenceNumber - t.firstSeq))
		tsDelta := int64(int32(pkt.Timestamp - expectedTS))
		seqJump := seqDelta > sequenceJumpLimit || seqDelta < -sequenceJumpLimit
		tsJump := tsDelta > int64(timestampJumpLimit) || tsDelta < -int64(timestampReorderTolerance)
		if seqJump || tsJump {
			oldSeq, oldTS := pkt.SequenceNumber, pkt.Timestamp
			t.seqOffset = expectedSeq - oldSeq
			t.tsOffset = expectedTS - oldTS
			pkt.SequenceNumber += t.seqOffset
			pkt.Timestamp += t.tsOffset
			return liveSequenceRebase{oldSeq: oldSeq, newSeq: pkt.SequenceNumber, oldTS: oldTS, newTS: pkt.Timestamp}, true
		}
	}
	if t.seqOffset != 0 || t.tsOffset != 0 {
		pkt.SequenceNumber += t.seqOffset
		pkt.Timestamp += t.tsOffset
	}
	return liveSequenceRebase{}, false
}

// opusPacketDurationTicks returns the duration represented by one Opus RTP
// payload in 48 kHz timestamp ticks (RFC 6716 section 3.1).
func opusPacketDurationTicks(payload []byte) (uint32, bool) {
	if len(payload) == 0 {
		return 0, false
	}
	config, code := payload[0]>>3, payload[0]&3
	var samplesPerFrame uint32
	switch {
	case config <= 11: // SILK: 10, 20, 40, 60 ms.
		samplesPerFrame = [...]uint32{480, 960, 1920, 2880}[config%4]
	case config <= 15: // Hybrid: 10 or 20 ms.
		samplesPerFrame = [...]uint32{480, 960}[config%2]
	default: // CELT: 2.5, 5, 10 or 20 ms.
		samplesPerFrame = [...]uint32{120, 240, 480, 960}[config%4]
	}
	frames := uint32(1)
	switch code {
	case 1, 2:
		frames = 2
	case 3:
		if len(payload) < 2 {
			return 0, false
		}
		frames = uint32(payload[1] & 0x3f)
		if frames == 0 {
			return 0, false
		}
	}
	duration := samplesPerFrame * frames
	if duration > opusClockRate*120/1000 {
		return 0, false
	}
	return duration, true
}
