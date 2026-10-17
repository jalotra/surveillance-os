package fmp4

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// --- synthetic fMP4 stream builders ----------------------------------------

// box builds one MP4 box: [u32 size][4cc type][payload].
func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	copy(b[4:], typ)
	copy(b[8:], payload)
	return b
}

// largeBox builds a box using the 64-bit largesize form.
func largeBox(typ string, payload []byte) []byte {
	b := make([]byte, 16+len(payload))
	binary.BigEndian.PutUint32(b, 1)
	copy(b[4:], typ)
	binary.BigEndian.PutUint64(b[8:], uint64(len(b)))
	copy(b[16:], payload)
	return b
}

// fullBox payload prefix: version (u8) + flags (u24) = one u32.
func fullBoxHdr(versionFlags uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], versionFlags)
	return b[:]
}

func mfhd(seq uint32) []byte {
	var p [8]byte
	binary.BigEndian.PutUint32(p[4:], seq)
	return box("mfhd", p[:])
}

func tfdt(base uint64) []byte {
	p := fullBoxHdr(0x01000000) // version 1
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], base)
	return box("tfdt", append(p, t[:]...))
}

// tfhd builds a track fragment header; defFlags adds default_sample_flags
// (0x000020) carrying sf.
func tfhd(trackID uint32, defFlags *uint32) []byte {
	p := fullBoxHdr(0x020000) // default-base-is-moof
	var id [4]byte
	binary.BigEndian.PutUint32(id[:], trackID)
	p = append(p, id[:]...)
	if defFlags != nil {
		p[0] = 0 // version
		binary.BigEndian.PutUint32(p[:4], binary.BigEndian.Uint32(p[:4])|0x000020)
		var f [4]byte
		binary.BigEndian.PutUint32(f[:], *defFlags)
		p = append(p, f[:]...)
	}
	return box("tfhd", p)
}

// trun builds a track run box.
//
//	dataOff: include a data_offset field (flag 0x000001)
//	firstSF: include first_sample_flags (flag 0x000004) — covers sample 0
//	samples: per-sample [dur,size,flags,cto] tuples, gated by flags
func trun(sampleCount uint32, dataOff, firstSF *uint32, samples [][4]uint32) []byte {
	flags := uint32(0)
	if dataOff != nil {
		flags |= 0x000001
	}
	if firstSF != nil {
		flags |= 0x000004
	}
	if samples != nil {
		flags |= 0x000100 | 0x000200 | 0x000400 | 0x000800
	}
	p := fullBoxHdr(flags)
	var v [4]byte
	binary.BigEndian.PutUint32(v[:], sampleCount)
	p = append(p, v[:]...)
	if dataOff != nil {
		binary.BigEndian.PutUint32(v[:], *dataOff)
		p = append(p, v[:]...)
	}
	if firstSF != nil {
		binary.BigEndian.PutUint32(v[:], *firstSF)
		p = append(p, v[:]...)
	}
	for _, s := range samples {
		for _, f := range s {
			binary.BigEndian.PutUint32(v[:], f)
			p = append(p, v[:]...)
		}
	}
	return box("trun", p)
}

func moof(seq uint32, trafBoxes ...[]byte) []byte {
	traf := []byte{}
	for _, b := range trafBoxes {
		traf = append(traf, b...)
	}
	return box("moof", append(mfhd(seq), box("traf", traf)...))
}

const (
	flagNonSync   = 0x00010000   // sample_is_non_sync_sample
	flagDepends2  = 0x02000000   // sample_depends_on = 2 ("not depended"... I-frame)
	flagKeySample = flagDepends2 // non_sync absent
)

func u32p(v uint32) *uint32 { return &v }

// --- tests -------------------------------------------------------------------

func TestSplitterInitAndFragments(t *testing.T) {
	ftyp := box("ftyp", []byte("isom\x00\x00\x02\x00isomiso6mp41"))
	moov := box("moov", []byte("minimal-moov"))
	m1 := moof(1, tfhd(1, nil), tfdt(0), trun(2, u32p(40), u32p(flagKeySample), nil))
	d1 := box("mdat", bytes.Repeat([]byte{0xAA}, 1000))
	m2 := moof(2, tfhd(1, nil), tfdt(1000), trun(2, u32p(40), u32p(flagNonSync), nil))
	d2 := box("mdat", bytes.Repeat([]byte{0xBB}, 900))

	stream := append(append(append(append(append(append([]byte{}, ftyp...), moov...), m1...), d1...), m2...), d2...)
	s := NewSplitter(bytes.NewReader(stream), 1000)

	init, err := s.ReadInit()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(init, append(ftyp, moov...)) {
		t.Fatalf("init = %d bytes, want ftyp+moov", len(init))
	}

	f1, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	defer f1.Release()
	if !f1.Key {
		t.Error("frag1 should be a keyframe (first_sample_flags non-sync absent)")
	}
	want1 := append(m1, d1...)
	if !bytes.Equal(f1.Data, want1) {
		t.Fatalf("frag1 data = %d bytes, want %d (moof+mdat)", len(f1.Data), len(want1))
	}
	if f1.DurMs != 1000 {
		t.Errorf("first frag DurMs = %d, want nominal 1000", f1.DurMs)
	}
	if f1.PtsMs <= 0 {
		t.Error("PtsMs not stamped")
	}

	f2, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Release()
	if f2.Key {
		t.Error("frag2 must not be keyframe (non_sync set)")
	}
	if !bytes.Equal(f2.Data, append(m2, d2...)) {
		t.Fatal("frag2 data mismatch")
	}

	if _, err := s.Next(); err != io.EOF {
		t.Fatalf("Next after stream end = %v, want EOF", err)
	}
}

func TestKeyframePerSampleFlags(t *testing.T) {
	// No first_sample_flags; flags live in the per-sample record:
	// [dur][size][flags][cto].
	syncSamples := [][4]uint32{{100, 500, flagKeySample, 0}, {100, 400, flagNonSync, 0}}
	nonSyncSamples := [][4]uint32{{100, 500, flagNonSync, 0}}

	mk := moof(1, tfhd(1, nil), trun(2, u32p(32), nil, syncSamples))
	mn := moof(2, tfhd(1, nil), trun(1, u32p(32), nil, nonSyncSamples))

	stream := bytes.Join([][]byte{
		box("ftyp", []byte("x")), box("moov", []byte("m")),
		mk, box("mdat", []byte("k")),
		mn, box("mdat", []byte("n")),
	}, nil)

	s := NewSplitter(bytes.NewReader(stream), 1000)
	if _, err := s.ReadInit(); err != nil {
		t.Fatal(err)
	}
	fk, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	defer fk.Release()
	if !fk.Key {
		t.Error("per-sample flags: sample0 non_sync absent → keyframe expected")
	}
	fn, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	defer fn.Release()
	if fn.Key {
		t.Error("per-sample flags: sample0 non_sync set → not keyframe")
	}
}

func TestKeyframeFromTfhdDefault(t *testing.T) {
	// trun carries neither first_sample_flags nor per-sample flags → the
	// tfhd default_sample_flags decides.
	m := moof(1, tfhd(1, u32p(flagKeySample)), trun(2, u32p(20), nil, nil))
	stream := bytes.Join([][]byte{
		box("ftyp", []byte("x")), box("moov", []byte("m")),
		m, box("mdat", []byte("k")),
	}, nil)
	s := NewSplitter(bytes.NewReader(stream), 1000)
	if _, err := s.ReadInit(); err != nil {
		t.Fatal(err)
	}
	f, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if !f.Key {
		t.Error("tfhd default_sample_flags non_sync absent → keyframe expected")
	}
}

func TestNoFlagsMeansNotKey(t *testing.T) {
	m := moof(1, tfhd(1, nil), trun(2, u32p(20), nil, nil))
	stream := bytes.Join([][]byte{
		box("ftyp", []byte("x")), box("moov", []byte("m")),
		m, box("mdat", []byte("k")),
	}, nil)
	s := NewSplitter(bytes.NewReader(stream), 1000)
	if _, err := s.ReadInit(); err != nil {
		t.Fatal(err)
	}
	f, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if f.Key {
		t.Error("no flag info → key=false (safe default)")
	}
}

func TestStrayBoxFoldsIntoNextFragment(t *testing.T) {
	styp := box("styp", []byte("segment-type"))
	m1 := moof(1, tfhd(1, nil), trun(1, nil, u32p(flagKeySample), nil))
	stream := bytes.Join([][]byte{
		box("ftyp", []byte("x")), box("moov", []byte("m")),
		m1, box("mdat", []byte("1")),
		styp, moof(2, tfhd(1, nil), trun(1, nil, u32p(flagKeySample), nil)), box("mdat", []byte("2")),
	}, nil)
	s := NewSplitter(bytes.NewReader(stream), 1000)
	if _, err := s.ReadInit(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	f, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if !bytes.HasPrefix(f.Data, styp) {
		t.Error("styp between fragments should fold into the next fragment")
	}
	if !f.Key {
		t.Error("frag should still detect keyframe past the stray box")
	}
}

func TestLargeSizeBox(t *testing.T) {
	stream := bytes.Join([][]byte{
		box("ftyp", []byte("x")), box("moov", []byte("m")),
		moof(1, tfhd(1, nil), trun(1, nil, u32p(flagKeySample), nil)),
		largeBox("mdat", []byte("payload")),
	}, nil)
	s := NewSplitter(bytes.NewReader(stream), 1000)
	if _, err := s.ReadInit(); err != nil {
		t.Fatal(err)
	}
	f, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if !bytes.HasSuffix(f.Data, []byte("payload")) {
		t.Error("64-bit largesize mdat not read")
	}
}

func TestTruncatedStream(t *testing.T) {
	stream := bytes.Join([][]byte{
		box("ftyp", []byte("x")), box("moov", []byte("m")),
		moof(1, tfhd(1, nil), trun(1, nil, u32p(flagKeySample), nil)),
		box("mdat", []byte("0123456789"))[:14], // cut inside mdat body
	}, nil)
	s := NewSplitter(bytes.NewReader(stream), 1000)
	if _, err := s.ReadInit(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(); err == nil {
		t.Fatal("expected error on truncated mdat")
	}
}

func TestMdatWithoutMoof(t *testing.T) {
	stream := bytes.Join([][]byte{
		box("ftyp", []byte("x")), box("moov", []byte("m")),
		box("mdat", []byte("orphan")),
	}, nil)
	s := NewSplitter(bytes.NewReader(stream), 1000)
	if _, err := s.ReadInit(); err == nil {
		t.Fatal("expected ReadInit to hit EOF, got nil")
	}
	// An mdat arriving with no moof before it in the fragment phase is
	// corrupt — the splitter errors and the supervisor restarts the pipe.
	stream2 := bytes.Join([][]byte{
		box("ftyp", []byte("x")), box("moov", []byte("m")),
		moof(1, tfhd(1, nil), trun(1, nil, u32p(flagKeySample), nil)), box("mdat", []byte("a")),
		box("mdat", []byte("orphan")),
	}, nil)
	s2 := NewSplitter(bytes.NewReader(stream2), 1000)
	if _, err := s2.ReadInit(); err != nil {
		t.Fatal(err)
	}
	f, err := s2.Next()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if _, err := s2.Next(); err == nil {
		t.Fatal("orphan mdat should error, not produce a fragment")
	}
}

// trickleReader feeds the splitter a few bytes per Read to exercise the
// bufio refill path.
type trickleReader struct{ r io.Reader }

func (t trickleReader) Read(p []byte) (int, error) {
	if len(p) > 3 {
		p = p[:3]
	}
	return t.r.Read(p)
}

func TestTrickleReads(t *testing.T) {
	stream := bytes.Join([][]byte{
		box("ftyp", []byte("x")), box("moov", bytes.Repeat([]byte{1}, 100)),
		moof(1, tfhd(1, nil), trun(1, nil, u32p(flagKeySample), nil)),
		box("mdat", bytes.Repeat([]byte{2}, 500)),
	}, nil)
	s := NewSplitter(trickleReader{bytes.NewReader(stream)}, 1000)
	init, err := s.ReadInit()
	if err != nil {
		t.Fatal(err)
	}
	if len(init) != len(box("ftyp", []byte("x")))+len(box("moov", bytes.Repeat([]byte{1}, 100))) {
		t.Fatal("init wrong length under trickle reads")
	}
	f, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if !f.Key || len(f.Data) == 0 {
		t.Fatal("bad fragment under trickle reads")
	}
}
