// Package ring is the per-camera hot cache: the latest init segment plus
// the last ~5 minutes of fragments (~26 MB at 0.7 Mbps), with fanout to
// live-viewer subscribers.
//
// Concurrency model: the camera-owner goroutine is the only writer
// (Append/SetInit/Broadcast). Viewers call Subscribe/Unsubscribe from HTTP
// handler goroutines. The subscriber list is a copy-on-write slice behind
// an atomic.Pointer — the fanout loop iterates a snapshot and sends
// non-blocking, so a slow viewer can never stall the camera. The small
// mutex serializes snapshot+swap so a subscribe racing an append can
// neither miss nor duplicate a fragment; all sends are non-blocking so
// the critical section stays microscopic.
package ring

import (
	"sync"
	"sync/atomic"

	"github.com/sjalotra/surveillance-os/backend/internal/proto"
)

// Sub is one live-viewer subscription. Ch carries complete wire frames
// (proto envelopes) ready to write to the viewer's WebSocket.
type Sub struct {
	Ch       chan []byte
	needsKey bool // dropped-behind: withhold everything until next keyframe
}

// SubChanCap is deliberately tiny: a viewer that can't drain 4 fragments
// is too slow for delta frames and must resync on the next keyframe.
const SubChanCap = 4

type entry struct {
	seq   uint64
	ptsMs int64
	key   bool
	wire  []byte // fully encoded proto envelope
}

// Ring holds one camera's live window.
type Ring struct {
	maxAgeMs int64
	maxBytes int

	mu      sync.Mutex
	init    []byte // encoded init envelope (ftyp+moov payload)
	frags   []entry
	bytes   int
	lastKey int // index into frags of newest keyframe, -1 if none

	subs atomic.Pointer[[]*Sub]
}

// New returns a Ring retaining fragments newer than maxAgeMs (pts domain),
// hard-capped at maxBytes as a safety valve for bad timestamps.
func New(maxAgeMs int64, maxBytes int) *Ring {
	r := &Ring{maxAgeMs: maxAgeMs, maxBytes: maxBytes, lastKey: -1}
	empty := []*Sub{}
	r.subs.Store(&empty)
	return r
}

// SetInit stores the camera's init segment (ftyp+moov) as an encoded
// init envelope for late-joining viewers, and pushes it to current
// subscribers — a re-init after reconnect must reach live MSE pipelines.
func (r *Ring) SetInit(payload []byte) {
	wire, err := proto.Encode(proto.InitHeader{Type: proto.TypeInit}, payload)
	if err != nil {
		return // payload already validated upstream
	}
	r.mu.Lock()
	r.init = wire
	for _, s := range *r.subs.Load() {
		select {
		case s.Ch <- wire:
		default: // lagging sub gets init via resync path anyway
		}
	}
	r.mu.Unlock()
}

// Append encodes f, adds it to the buffer, trims by age/bytes, and fans
// the wire frame out to subscribers. Keyframes resync lagging subs.
func (r *Ring) Append(f proto.Fragment) {
	wire, err := proto.Encode(f.Header(), f.Data)
	if err != nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.frags = append(r.frags, entry{seq: f.Seq, ptsMs: f.PtsMs, key: f.Key, wire: wire})
	r.bytes += len(wire)
	if f.Key {
		r.lastKey = len(r.frags) - 1
	}
	r.trim(f.PtsMs)

	for _, s := range *r.subs.Load() {
		if s.needsKey {
			if !f.Key {
				continue // stay dark until a keyframe arrives
			}
			// Resync: drop stale deltas, start clean at the keyframe.
			s.needsKey = false
			drain(s.Ch)
		}
		select {
		case s.Ch <- wire:
		default:
			// Slow viewer: mark it and drop its backlog now — deltas are
			// useless once one is lost, so free the chan for the resync.
			s.needsKey = true
			drain(s.Ch)
		}
	}
}

// Broadcast sends a control envelope (e.g. gap) to all subscribers.
// Dropped silently for lagging subs — they're already marked for resync.
func (r *Ring) Broadcast(wire []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range *r.subs.Load() {
		select {
		case s.Ch <- wire:
		default:
		}
	}
}

// Subscribe returns the init envelope and every fragment from the most
// recent keyframe forward, plus the channel for live frames. If no
// keyframe has been seen yet the sub starts in resync mode and its first
// frame will be the next keyframe.
func (r *Ring) Subscribe() (init []byte, backlog [][]byte, s *Sub) {
	s = &Sub{Ch: make(chan []byte, SubChanCap)}

	r.mu.Lock()
	defer r.mu.Unlock()

	init = r.init
	if r.lastKey >= 0 {
		backlog = make([][]byte, 0, len(r.frags)-r.lastKey)
		for _, e := range r.frags[r.lastKey:] {
			backlog = append(backlog, e.wire)
		}
	} else {
		s.needsKey = true
	}

	old := *r.subs.Load()
	next := make([]*Sub, len(old)+1)
	copy(next, old)
	next[len(old)] = s
	r.subs.Store(&next)
	return init, backlog, s
}

// Unsubscribe removes s and closes its channel.
func (r *Ring) Unsubscribe(s *Sub) {
	r.mu.Lock()
	defer r.mu.Unlock()

	old := *r.subs.Load()
	next := make([]*Sub, 0, len(old))
	found := false
	for _, o := range old {
		if o == s {
			found = true
			continue
		}
		next = append(next, o)
	}
	if found {
		r.subs.Store(&next)
		close(s.Ch)
	}
}

// Stats reports buffer depth for /status.
func (r *Ring) Stats() (frags int, bytes int, subs int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frags), r.bytes, len(*r.subs.Load())
}

// LastPtsMs returns the newest fragment timestamp, or 0.
func (r *Ring) LastPtsMs() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.frags) == 0 {
		return 0
	}
	return r.frags[len(r.frags)-1].ptsMs
}

// trim drops fragments older than maxAge (relative to the newest pts seen)
// and enforces the byte cap. Caller holds mu.
func (r *Ring) trim(newestPts int64) {
	cut := newestPts - r.maxAgeMs
	n := 0
	for n < len(r.frags) && r.frags[n].ptsMs < cut {
		r.bytes -= len(r.frags[n].wire)
		n++
	}
	// Byte-cap safety valve; always leave at least one entry.
	for n < len(r.frags)-1 && r.bytes > r.maxBytes {
		r.bytes -= len(r.frags[n].wire)
		n++
	}
	if n == 0 {
		return
	}
	copy(r.frags, r.frags[n:])
	for i := len(r.frags) - n; i < len(r.frags); i++ {
		r.frags[i] = entry{} // release wire refs
	}
	r.frags = r.frags[:len(r.frags)-n]
	r.lastKey -= n
	if r.lastKey < 0 {
		// The newest keyframe was trimmed; find the next-newest.
		r.lastKey = -1
		for i := len(r.frags) - 1; i >= 0; i-- {
			if r.frags[i].key {
				r.lastKey = i
				break
			}
		}
	}
}

func drain(ch chan []byte) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
