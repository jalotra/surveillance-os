// Package fmp4 splits a fragmented-MP4 byte stream (mp4mux fragment-duration
// output read from a gst-launch child's stdout) into init segment + fragments.
//
// Stream shape: [ftyp][moov] once, then repeating [moof][mdat] pairs. Any
// stray top-level boxes between fragments are folded into the following
// fragment so the forwarded byte stream stays identical to the input.
//
// Timestamping (documented approximation, plans/video-service.md §2.1):
// pts_ms is wall-clock ms at the moment a fragment's moof box is observed,
// not the muxer's decode-time PTS. dur_ms is the wall-clock delta between
// successive moofs (nominal fragment duration for the first fragment).
package fmp4

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"
)

var (
	typFtyp = [4]byte{'f', 't', 'y', 'p'}
	typMoof = [4]byte{'m', 'o', 'o', 'f'}
	typMdat = [4]byte{'m', 'd', 'a', 't'}
	typTraf = [4]byte{'t', 'r', 'a', 'f'}
	typTfhd = [4]byte{'t', 'f', 'h', 'd'}
	typTrun = [4]byte{'t', 'r', 'u', 'n'}
)

const (
	// maxBox is a sanity cap on a single top-level box. Fragments are
	// ~87 KB at 0.7 Mbps; 256 MB leaves huge headroom but bounds a
	// corrupt size field.
	maxBox = 256 << 20

	// fragBufSize is the pooled fragment buffer size — one second of
	// 1080p10 at 0.7 Mbps ≈ 87 KB; 256 KB covers bitrate spikes.
	fragBufSize = 256 << 10
	// maxPoolBuf keeps oversized (grown) buffers out of the pool.
	maxPoolBuf = 4 << 20
)

// fragPool backs Fragment.Data on the hot path (runtime knob per plan §3).
var fragPool = sync.Pool{
	New: func() any { return make([]byte, 0, fragBufSize) },
}

// Fragment is one self-contained fMP4 fragment (moof+mdat), the record unit
// for both the spool and the uplink. Call Release when done to return the
// pooled buffer; it is a no-op for fragments not built by Splitter.
type Fragment struct {
	Seq   uint64 // monotonic per boot_id, assigned by the agent
	PtsMs int64  // wall-clock ms when the moof was seen (approximation)
	DurMs uint32 // wall-clock delta since previous fragment's moof
	Key   bool   // fragment's first sample is a keyframe
	Data  []byte // moof+mdat (plus any stray boxes, preserving stream order)

	buf []byte // pooled backing array; nil for non-pooled fragments
}

// Release returns the fragment's buffer to the pool. Safe to call on any
// Fragment; Data is invalid afterwards.
func (f *Fragment) Release() {
	if f.buf != nil {
		if cap(f.buf) <= maxPoolBuf {
			fragPool.Put(f.buf[:0])
		}
		f.buf = nil
	}
	f.Data = nil
}

// Splitter reads MP4 boxes from r and groups them into init + fragments.
// Not safe for concurrent use; use from the capture goroutine only.
type Splitter struct {
	r          *bufio.Reader
	nominalDur uint32 // dur_ms for the first fragment
	pend       []byte // moof already consumed by ReadInit, not yet emitted
	lastPts    int64
}

// NewSplitter wraps r (a gst-launch stdout pipe). nominalDurMs should be the
// configured mp4mux fragment-duration.
func NewSplitter(r io.Reader, nominalDurMs uint32) *Splitter {
	return &Splitter{r: bufio.NewReaderSize(r, 64<<10), nominalDur: nominalDurMs}
}

// ReadInit consumes the stream through the start of the first moof and
// returns the init segment (everything before it — normally ftyp+moov).
// The moof is held for the first Next call.
func (s *Splitter) ReadInit() ([]byte, error) {
	var init []byte
	for {
		prev := len(init)
		var typ [4]byte
		var err error
		init, typ, err = s.appendBox(init)
		if err != nil {
			return nil, fmt.Errorf("fmp4: reading init: %w", err)
		}
		if typ == typMoof {
			// Stash the moof for the first fragment; init is what
			// precedes it.
			s.pend = append([]byte(nil), init[prev:]...)
			init = init[:prev]
			if len(init) == 0 {
				return nil, fmt.Errorf("fmp4: stream begins with moof, no init segment")
			}
			return init, nil
		}
	}
}

// Next reads one complete fragment (moof through its mdat) and returns it.
// Any boxes seen between fragments (e.g. styp) are prepended to this
// fragment's Data. io.EOF means the pipeline's stdout closed.
func (s *Splitter) Next() (*Fragment, error) {
	buf := fragPool.Get().([]byte)[:0]
	moofOff := -1
	pts := int64(0)

	if len(s.pend) > 0 {
		moofOff = 0
		buf = append(buf, s.pend...)
		s.pend = nil
		pts = nowMs()
	}

	for {
		prev := len(buf)
		var typ [4]byte
		var err error
		buf, typ, err = s.appendBox(buf)
		if err != nil {
			releaseBuf(buf)
			return nil, err
		}
		switch typ {
		case typMoof:
			if moofOff < 0 {
				moofOff = prev
				pts = nowMs()
			}
		case typMdat:
			if moofOff < 0 {
				releaseBuf(buf)
				return nil, fmt.Errorf("fmp4: mdat without preceding moof")
			}
			f := &Fragment{PtsMs: pts, Key: moofIsKeyframe(buf[moofOff:prev]), Data: buf, buf: buf}
			if s.lastPts != 0 {
				if d := pts - s.lastPts; d > 0 {
					f.DurMs = uint32(d)
				}
			} else {
				f.DurMs = s.nominalDur
			}
			s.lastPts = pts
			return f, nil
		}
	}
}

// appendBox reads exactly one box (header + body) from the stream and
// appends its raw bytes to dst. Returns the grown dst and the box's 4CC.
func (s *Splitter) appendBox(dst []byte) ([]byte, [4]byte, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
		return dst, [4]byte{}, err // io.EOF / ErrUnexpectedEOF propagate
	}
	size := uint64(binary.BigEndian.Uint32(hdr[:4]))
	typ := [4]byte{hdr[4], hdr[5], hdr[6], hdr[7]}
	headerLen := 8
	if size == 1 { // 64-bit largesize
		var ext [8]byte
		if _, err := io.ReadFull(s.r, ext[:]); err != nil {
			return dst, [4]byte{}, err
		}
		size = binary.BigEndian.Uint64(ext[:])
		headerLen = 16
		dst = append(dst, hdr[:]...)
		dst = append(dst, ext[:]...)
	} else {
		dst = append(dst, hdr[:]...)
	}
	switch {
	case size == 0:
		return dst, typ, fmt.Errorf("fmp4: %s box with size 0 (extends to EOF) cannot be framed", typ)
	case size < uint64(headerLen):
		return dst, typ, fmt.Errorf("fmp4: %s box size %d < header %d", typ, size, headerLen)
	case size > maxBox:
		return dst, typ, fmt.Errorf("fmp4: %s box size %d exceeds cap", typ, size)
	}
	body := int(size) - headerLen
	need := len(dst) + body
	if need > cap(dst) {
		nd := make([]byte, len(dst), need)
		copy(nd, dst)
		dst = nd
	}
	dst = dst[:need]
	if _, err := io.ReadFull(s.r, dst[need-body:]); err != nil {
		return dst, typ, err
	}
	return dst, typ, nil
}

// moofIsKeyframe reports whether the fragment's first sample is a sync
// sample: it walks moof → traf → trun and inspects sample 0's flags, where
// the sample_is_non_sync_sample bit (0x00010000) must be absent. Preference
// order: trun first_sample_flags > trun per-sample flags of sample 0 >
// tfhd default_sample_flags > false.
func moofIsKeyframe(moof []byte) bool {
	flags, ok := firstSampleFlags(moof)
	return ok && flags&0x00010000 == 0
}

// boxChildren calls fn for each top-level box inside b (which must start at
// a box boundary): payload holds that box's bytes after its 8/16-byte
// header. Returning false from fn stops iteration.
func boxChildren(b []byte, fn func(payload []byte, typ [4]byte) bool) {
	for pos := 0; pos+8 <= len(b); {
		size := int(binary.BigEndian.Uint32(b[pos:]))
		typ := [4]byte{b[pos+4], b[pos+5], b[pos+6], b[pos+7]}
		hdr := 8
		if size == 1 {
			if pos+16 > len(b) {
				return
			}
			size64 := binary.BigEndian.Uint64(b[pos+8:])
			if size64 > uint64(len(b)) {
				return
			}
			size = int(size64)
			hdr = 16
		}
		if size < hdr || pos+size > len(b) {
			return
		}
		if !fn(b[pos+hdr:pos+size], typ) {
			return
		}
		pos += size
	}
}

// firstSampleFlags extracts sample 0's flags from the first traf's first
// trun in a moof box. moof must start at the moof box's header; trailing
// bytes past the moof's own size are ignored. ok=false means no flag
// information was present.
func firstSampleFlags(moof []byte) (uint32, bool) {
	// Bound to the moof's own extent — buf may run on into the mdat.
	if len(moof) < 8 {
		return 0, false
	}
	hdr := 8
	size := uint64(binary.BigEndian.Uint32(moof[:4]))
	if size == 1 {
		if len(moof) < 16 {
			return 0, false
		}
		hdr = 16
		size = binary.BigEndian.Uint64(moof[8:])
	}
	if size < uint64(hdr) || size > uint64(len(moof)) {
		return 0, false
	}
	moof = moof[hdr:size]
	var (
		trunFlags uint32
		trunFound bool
		tfhdFlags uint32
		tfhdFound bool
	)
	boxChildren(moof, func(traf []byte, typ [4]byte) bool {
		if typ != typTraf {
			return true
		}
		// Inside traf: find tfhd + first trun.
		boxChildren(traf, func(child []byte, typ2 [4]byte) bool {
			switch typ2 {
			case typTfhd:
				if f, ok := tfhdDefaultSampleFlags(child); ok {
					tfhdFlags, tfhdFound = f, true
				}
			case typTrun:
				if !trunFound {
					trunFlags, trunFound = trunFirstSampleFlags(child)
				}
			}
			return !trunFound // stop once the first trun is parsed
		})
		return false // first traf only — this is a single video track
	})
	if trunFound {
		return trunFlags, true
	}
	return tfhdFlags, tfhdFound
}

// trunFirstSampleFlags parses a trun payload (after the box header).
func trunFirstSampleFlags(b []byte) (uint32, bool) {
	if len(b) < 8 {
		return 0, false
	}
	flags := binary.BigEndian.Uint32(b[:4]) & 0x00ffffff
	samples := binary.BigEndian.Uint32(b[4:])
	if samples == 0 {
		return 0, false
	}
	p := 8
	if flags&0x000001 != 0 { // data_offset
		p += 4
	}
	if flags&0x000004 != 0 { // first_sample_flags covers sample 0
		if p+4 > len(b) {
			return 0, false
		}
		return binary.BigEndian.Uint32(b[p:]), true
	}
	if flags&0x000400 == 0 { // no per-sample flags
		return 0, false
	}
	// Skip optional per-sample fields that precede sample_flags.
	if flags&0x000100 != 0 { // sample_duration
		p += 4
	}
	if flags&0x000200 != 0 { // sample_size
		p += 4
	}
	if p+4 > len(b) {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[p:]), true
}

// tfhdDefaultSampleFlags parses a tfhd payload for default_sample_flags.
func tfhdDefaultSampleFlags(b []byte) (uint32, bool) {
	if len(b) < 8 {
		return 0, false
	}
	flags := binary.BigEndian.Uint32(b[:4]) & 0x00ffffff
	p := 8                   // past version/flags + track_ID
	if flags&0x000001 != 0 { // base_data_offset u64
		p += 8
	}
	if flags&0x000002 != 0 { // sample_description_index
		p += 4
	}
	if flags&0x000008 != 0 { // default_sample_duration
		p += 4
	}
	if flags&0x000010 != 0 { // default_sample_size
		p += 4
	}
	if flags&0x000020 == 0 {
		return 0, false
	}
	if p+4 > len(b) {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[p:]), true
}

func nowMs() int64 { return time.Now().UnixMilli() }

func releaseBuf(b []byte) {
	if cap(b) > 0 && cap(b) <= maxPoolBuf {
		fragPool.Put(b[:0])
	}
}
