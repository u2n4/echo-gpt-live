package bridge

import (
	"sync"
	"testing"

	"github.com/pion/rtp"
)

func seqPacket(n uint16) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: n}}
}

func TestHoldSinkKeepsOrderAcrossRelease(t *testing.T) {
	h := NewHoldSink(10)
	for i := uint16(1); i <= 3; i++ {
		h.Write(seqPacket(i))
	}
	var got []uint16
	held, dropped := h.Release(func(p *rtp.Packet) { got = append(got, p.SequenceNumber) })
	if held != 3 || dropped != 0 {
		t.Fatalf("held=%d dropped=%d, want 3 and 0", held, dropped)
	}
	h.Write(seqPacket(4))
	if len(got) != 4 {
		t.Fatalf("got %v, want the three held packets and then the fourth", got)
	}
	for i, s := range got {
		if s != uint16(i+1) {
			t.Fatalf("out of order at %d: %v", i, got)
		}
	}
}

func TestHoldSinkDropsTheOldestBeyondTheCap(t *testing.T) {
	h := NewHoldSink(2)
	for i := uint16(1); i <= 5; i++ {
		h.Write(seqPacket(i))
	}
	var got []uint16
	held, dropped := h.Release(func(p *rtp.Packet) { got = append(got, p.SequenceNumber) })
	if held != 2 || dropped != 3 {
		t.Fatalf("held=%d dropped=%d, want 2 and 3", held, dropped)
	}
	if len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("kept %v, want the newest two", got)
	}
}

func TestHoldSinkDropForgetsEverything(t *testing.T) {
	h := NewHoldSink(10)
	h.Write(seqPacket(1))
	if n := h.Drop(); n != 1 {
		t.Fatalf("dropped %d, want 1", n)
	}
	held, _ := h.Release(func(*rtp.Packet) { t.Fatal("nothing should be forwarded") })
	if held != 0 {
		t.Fatalf("held %d after Drop", held)
	}
}

// A writer racing the release must never let a later packet overtake an earlier one.
func TestHoldSinkWriterCannotOvertakeTheFlush(t *testing.T) {
	h := NewHoldSink(1000)
	for i := uint16(1); i <= 200; i++ {
		h.Write(seqPacket(i))
	}
	var mu sync.Mutex
	var got []uint16
	forward := func(p *rtp.Packet) {
		mu.Lock()
		got = append(got, p.SequenceNumber)
		mu.Unlock()
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		h.Release(forward)
	}()
	go func() {
		defer wg.Done()
		for i := uint16(201); i <= 400; i++ {
			h.Write(seqPacket(i))
		}
	}()
	wg.Wait()
	if len(got) != 400 {
		t.Fatalf("forwarded %d packets, want 400", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Fatalf("packet %d came after %d", got[i], got[i-1])
		}
	}
}
