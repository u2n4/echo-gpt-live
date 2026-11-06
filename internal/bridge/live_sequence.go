package bridge

import "github.com/pion/rtp"

const (
	sequenceJumpLimit                = 64
	timestampJumpLimit        uint32 = opusClockRate * 400 / 1000
	timestampReorderTolerance        = opusClockRate * 120 / 1000
	// rebaseWindowPackets is how far into the stream a jump is still read as OpenAI's restart
	// after its probe (measured: always at packet 2; the window leaves room for a second probe).
	// Past it a jump is a real loss or a real discontinuity, and hiding it would splice audio
	// the Echo should conceal instead.
	rebaseWindowPackets = 5
)

// liveSequenceTranslator repairs the one discontinuity OpenAI may put between
// its probe packet and the real stream. Once chosen, the offsets stay fixed so
// every later gap, reorder and wrap retains the sender's original delta.
//
// It compares each packet with the PREVIOUS one, not only packet 2 with packet 1, and stays
// armed until a rebase has happened or the window closes: a second probe inside the thresholds
// used to spend the single check, and a first packet whose payload had no readable duration
// (padding) used to disable the repair for the whole track. Either left the restart unrepaired
// and the Echo's jitter buffer silent for tens of seconds with no line in the log. Muse found
// both from the source.
type liveSequenceTranslator struct {
	seen       int
	firstSeq   uint16
	firstTS    uint32
	firstDur   uint32
	durationOK bool
	prevSeq    uint16
	prevTS     uint32
	prevDur    uint32
	prevOK     bool
	rebased    bool
	seqOffset  uint16
	tsOffset   uint32
	// missed counts jumps seen after the window that were left as they were, so a stream that
	// restarted late is distinguishable from ordinary traffic.
	missed int
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
		t.prevSeq, t.prevTS = pkt.SequenceNumber, pkt.Timestamp
		t.prevDur, t.prevOK = t.firstDur, t.durationOK
		return liveSequenceRebase{}, false
	}
	if !t.rebased {
		expectedSeq := t.prevSeq + 1
		expectedTS := t.prevTS + t.prevDur
		seqDelta := int32(int16(pkt.SequenceNumber - t.prevSeq))
		tsDelta := int64(int32(pkt.Timestamp - expectedTS))
		seqJump := seqDelta > sequenceJumpLimit || seqDelta < -sequenceJumpLimit
		// Without a readable previous duration there is no expected timestamp; the sequence
		// alone decides, and only the sequence is repaired. A timestamp that jumps FORWARD with
		// the sequence intact is a burst of loss, not a restart (a restart goes back towards
		// zero, and a 21-packet loss inside the window used to be spliced as one). Muse found it.
		tsJump := t.prevOK && tsDelta < -int64(timestampReorderTolerance)
		// Matching sequence and media gaps are packet loss, including inside the
		// probe window. Preserve the sender's timeline and leave offsets unset.
		if t.prevOK && seqDelta > 1 && tsDelta == int64(seqDelta-1)*int64(t.prevDur) {
			seqJump, tsJump = false, false
		}
		switch {
		case (seqJump || tsJump) && t.seen <= rebaseWindowPackets:
			oldSeq, oldTS := pkt.SequenceNumber, pkt.Timestamp
			t.rebased = true
			t.seqOffset = expectedSeq - oldSeq
			if t.prevOK {
				t.tsOffset = expectedTS - oldTS
			}
			pkt.SequenceNumber += t.seqOffset
			pkt.Timestamp += t.tsOffset
			t.prevSeq, t.prevTS = pkt.SequenceNumber, pkt.Timestamp
			t.prevDur, t.prevOK = opusPacketDurationTicks(pkt.Payload)
			return liveSequenceRebase{oldSeq: oldSeq, newSeq: pkt.SequenceNumber, oldTS: oldTS, newTS: pkt.Timestamp}, true
		case seqJump || tsJump:
			t.missed++
		}
	}
	t.prevSeq, t.prevTS = pkt.SequenceNumber, pkt.Timestamp
	t.prevDur, t.prevOK = opusPacketDurationTicks(pkt.Payload)
	if t.rebased {
		pkt.SequenceNumber += t.seqOffset
		pkt.Timestamp += t.tsOffset
	}
	return liveSequenceRebase{}, false
}

// Missed reports jumps that arrived after the rebase window and were left as they were.
func (t *liveSequenceTranslator) Missed() int { return t.missed }

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
