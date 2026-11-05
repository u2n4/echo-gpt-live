package bridge

// Recording both audio directions, so "the voice is not clear" becomes something anyone can
// listen to and measure instead of something to guess about.
//
// The Opus packets are written into an Ogg container EXACTLY as they crossed the bridge: no
// decode, no re-encode, no resample. So `from-openai.ogg` is bit-for-bit the audio the Echo was
// asked to play, and `from-echo.ogg` is bit-for-bit what the microphone sent. If the first one
// sounds clean, the fault is downstream in Amazon's path or the speaker; if it sounds bad, the
// fault is upstream and the Echo is innocent.

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

// maxConsecutiveWriteFailures separates a packet the container cannot hold from a file that can
// no longer be written. A bad packet is followed by good ones; a broken file fails every time.
const maxConsecutiveWriteFailures = 50

// recordQueue is how much audio may wait to be written before recording starts dropping rather
// than delaying the call. Five seconds is far more than a disk needs and far less than anyone
// would tolerate as added latency.
//
// The queue exists because the recorder sits on the media path: it is fed from the same goroutine
// that forwards audio, so a slow filesystem would have postponed real packets by however long the
// write took. Diagnostics must never be able to do that, and a dropped page of a recording costs
// nothing next to a stalled conversation.
const recordQueue = 250

// Recorder writes one direction of Opus RTP to an Ogg file.
type Recorder struct {
	mu     sync.Mutex
	w      *oggwriter.OggWriter
	path   string
	spilt  bool // a second file was opened after the first died; only one is allowed
	n      int64
	empty  int64 // padding-only packets, which carry no Opus frame
	failed int64
	consec int
	closed bool
	ev     Events

	in       chan *rtp.Packet
	done     chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once
	dropped  atomic.Int64 // packets the queue could not take; the call keeps going
}

// NewRecorder creates dir if needed and opens <dir>/<name>.ogg for 48 kHz Opus.
// A nil Recorder is safe to use: every method tolerates it, so recording stays optional.
func NewRecorder(dir, name string, ev Events) *Recorder {
	if dir == "" {
		return nil
	}
	if ev == nil {
		ev = func(string, ...any) {}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		ev("record_open_failed", "dir", dir, "err", err.Error())
		return nil
	}
	p := filepath.Join(dir, name+".ogg")
	// Opus in WebRTC is always 48 kHz; the channel count in the container is the stream's own.
	w, err := oggwriter.New(p, 48000, 1)
	if err != nil {
		ev("record_open_failed", "path", p, "err", err.Error())
		return nil
	}
	ev("record_open", "path", p)
	r := &Recorder{
		w:    w,
		path: p,
		ev:   ev,
		in:   make(chan *rtp.Packet, recordQueue),
		done: make(chan struct{}),
	}
	r.wg.Add(1)
	go r.run()
	return r
}

// run is the only goroutine that touches the file, so nothing on the media path ever waits for a
// disk.
func (r *Recorder) run() {
	defer r.wg.Done()
	for {
		select {
		case pkt := <-r.in:
			r.write(pkt)
		case <-r.done:
			for {
				select {
				case pkt := <-r.in:
					r.write(pkt)
				default:
					return
				}
			}
		}
	}
}

// Write records one packet.
//
// A single packet the container rejects must not end the recording. The first version of this gave
// up on the first error and lost a whole call after 54 packets, because WebRTC senders emit
// padding-only packets with an empty payload and Pion's Ogg writer calls those "invalid Opus
// packet". Those carry no audio, so they are skipped; any other failure is skipped too, and only a
// long unbroken run of them is treated as a broken file.
func (r *Recorder) Write(p *rtp.Packet) {
	if r == nil || p == nil {
		return
	}
	select {
	case r.in <- p:
	default:
		// The writer is behind. Losing a page of the recording is the right trade against
		// delaying the audio this recording exists to explain.
		r.dropped.Add(1)
	}
}

// write does the actual container work, on the recorder's own goroutine.
func (r *Recorder) write(p *rtp.Packet) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.w == nil {
		return
	}
	if len(p.Payload) == 0 {
		r.empty++
		return
	}
	if err := r.w.WriteRTP(p); err != nil {
		r.failed++
		r.consec++
		if r.failed == 1 {
			r.ev("record_write_failed", "path", r.path, "err", err.Error(), "after_packets", r.n,
				"action", "packet skipped; the recording continues")
		}
		if r.consec >= maxConsecutiveWriteFailures {
			r.ev("record_write_gave_up", "path", r.path, "after_packets", r.n,
				"consecutive_failures", r.consec, "err", err.Error())
			_ = r.w.Close()
			r.w = nil
			// A dead file must not cost the rest of the call: the recording continues in a
			// second file, once. A second death (the disk is gone) closes for good rather
			// than chaining part files nobody can play. Muse found it.
			if r.spilt {
				r.closed = true
				return
			}
			r.spilt = true
			spill := r.path + ".part2"
			w, oerr := oggwriter.New(spill, 48000, 1)
			if oerr != nil {
				r.ev("record_open_failed", "path", spill, "err", oerr.Error())
				r.closed = true
				return
			}
			r.ev("record_open", "path", spill, "note", "continuation after the first file died")
			r.w, r.path, r.consec = w, spill, 0
		}
		return
	}
	r.consec = 0
	r.n++
}

// Close finishes the container and reports what was captured.
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		close(r.done)
		r.wg.Wait()
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.w == nil {
		return
	}
	r.closed = true
	err := r.w.Close()
	r.w = nil
	size := int64(-1)
	if fi, serr := os.Stat(r.path); serr == nil {
		size = fi.Size()
	}
	kv := []any{"path", r.path, "packets", r.n, "bytes", size,
		"approx_seconds", float64(r.n) * 0.02} // 20 ms per Opus packet in this pipeline
	if r.empty > 0 {
		kv = append(kv, "padding_only_packets", r.empty)
	}
	if r.failed > 0 {
		kv = append(kv, "skipped_packets", r.failed)
	}
	if d := r.dropped.Load(); d > 0 {
		kv = append(kv, "dropped_queue_full", d)
	}
	if err != nil {
		kv = append(kv, "close_err", err.Error())
	}
	r.ev("record_closed", kv...)
}

// Packets reports how many packets have been recorded.
func (r *Recorder) Packets() int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}
