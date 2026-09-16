package bridge

// Holding his first words until there is somewhere for them to go.
//
// The Echo's leg connects about 1.3 s after its offer, and he hears Alexa's chime and starts
// talking. OpenAI's leg starts about 3 s after the same offer. The forwarder used to be attached
// only when the second was up, so everything he said in between was read from the track and
// dropped on the floor: measured on the 20:52 call of 2026-09-13, 1.7 s of speech, which he
// reported as "she does not hear me at the start".
//
// A HoldSink keeps those packets in order and hands them over the moment the destination exists.
// It is bounded, because a session that never comes up must not grow a buffer forever, and a flush
// longer than a few seconds is not the start of a conversation any more.

import (
	"sync"

	"github.com/pion/rtp"
)

// HoldSink is an audio sink that stores packets until Release gives it a destination, and forwards
// directly from then on.
type HoldSink struct {
	mu      sync.Mutex
	held    []*rtp.Packet
	max     int
	dropped int
	forward func(*rtp.Packet) // nil while holding
}

// NewHoldSink holds at most maxPackets; beyond that the OLDEST are dropped, because the words
// nearest to the moment she can finally hear are the ones that still belong to the conversation.
func NewHoldSink(maxPackets int) *HoldSink {
	if maxPackets <= 0 {
		maxPackets = 1
	}
	return &HoldSink{max: maxPackets}
}

// Write keeps the packet while holding, and forwards it once released.
//
// Forwarding happens under the lock on purpose: a packet arriving in the middle of Release cannot
// overtake the ones that were held before it, so the destination always sees them in order.
func (h *HoldSink) Write(p *rtp.Packet) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.forward != nil {
		h.forward(p)
		return
	}
	if len(h.held) >= h.max {
		h.held = h.held[1:]
		h.dropped++
	}
	h.held = append(h.held, p)
}

// Release forwards everything held, in order, then everything that arrives after. It reports how
// many packets were held and how many the cap had already discarded.
func (h *HoldSink) Release(forward func(*rtp.Packet)) (held, dropped int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.held {
		forward(p)
	}
	held, dropped = len(h.held), h.dropped
	h.held = nil
	h.forward = forward
	return held, dropped
}

// Drop discards what was held: the destination never came.
func (h *HoldSink) Drop() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(h.held)
	h.held = nil
	return n
}
