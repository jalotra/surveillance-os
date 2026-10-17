package spool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"surveillance-os/agent/internal/fmp4"
)

func frag(seq uint64, ptsMs int64, durMs uint32, key bool, size int) fmp4.Fragment {
	data := []byte(fmt.Sprintf("frag-%04d-", seq))
	for len(data) < size {
		data = append(data, 'x')
	}
	return fmp4.Fragment{
		Seq:   seq,
		PtsMs: ptsMs,
		DurMs: durMs,
		Key:   key,
		Data:  data[:size],
	}
}

// collect drains an iterator into a slice.
func collect(it func(func(fmp4.Fragment) bool)) []fmp4.Fragment {
	var out []fmp4.Fragment
	for f := range it {
		out = append(out, f)
	}
	return out
}

func TestWriteAndReadFrom(t *testing.T) {
	dir := t.TempDir()
	sp, err := Open(dir, "boot-1", 16<<20, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	for i := uint64(1); i <= 5; i++ {
		if err := sp.Write(frag(i, 1000*int64(i), 1000, i%2 == 1, 100)); err != nil {
			t.Fatal(err)
		}
	}

	got := collect(sp.ReadFrom(1))
	if len(got) != 5 {
		t.Fatalf("ReadFrom(1) = %d frags, want 5", len(got))
	}
	for i, f := range got {
		wantSeq := uint64(i + 1)
		if f.Seq != wantSeq {
			t.Fatalf("frag[%d].Seq = %d, want %d", i, f.Seq, wantSeq)
		}
		if f.PtsMs != 1000*int64(wantSeq) || f.DurMs != 1000 {
			t.Fatalf("frag[%d] fields wrong: %+v", i, f)
		}
		if f.Key != (wantSeq%2 == 1) {
			t.Fatalf("frag[%d].Key = %v", i, f.Key)
		}
		if len(f.Data) != 100 {
			t.Fatalf("frag[%d].Data len = %d, want 100", i, len(f.Data))
		}
	}

	// Inclusive lower bound.
	got = collect(sp.ReadFrom(3))
	if len(got) != 3 || got[0].Seq != 3 {
		t.Fatalf("ReadFrom(3) = %v seqs, want 3,4,5", seqs(got))
	}
	if o := sp.Oldest(); o != 1 {
		t.Fatalf("Oldest = %d, want 1", o)
	}
	if l := sp.Latest(); l != 5 {
		t.Fatalf("Latest = %d, want 5", l)
	}
}

func seqs(fs []fmp4.Fragment) []uint64 {
	var out []uint64
	for _, f := range fs {
		out = append(out, f.Seq)
	}
	return out
}

func TestSegmentRolloverAndTrim(t *testing.T) {
	dir := t.TempDir()
	// ~46 B/record (32 hdr + 10 payload + 4 crc) → segBytes 100 ⇒ ~2 per seg.
	sp, err := Open(dir, "boot-1", 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	for i := uint64(1); i <= 9; i++ {
		if err := sp.Write(frag(i, int64(i), 10, false, 10)); err != nil {
			t.Fatal(err)
		}
	}

	// 9 records × ~46 B over 100 B segments → 3 segments of 3 records.
	segs, _ := filepath.Glob(filepath.Join(dir, "seg-*.spl"))
	if len(segs) != 3 {
		t.Fatalf("expected 3 segments after rollover, got %d", len(segs))
	}

	// Trim everything at/below seq 7: every segment whose newest record is
	// ≤7 is deleted; the segment holding seq 8+ survives, as does the write
	// segment.
	removed := sp.Trim(7)
	if removed == 0 {
		t.Fatal("Trim(7) removed nothing")
	}
	if l := sp.Latest(); l != 9 {
		t.Fatalf("Latest after trim = %d, want 9 (write seg survives)", l)
	}
	// Segment granularity: seq 7 rides along in the segment holding 7,8,9 —
	// it can't be evicted mid-segment, and the hub dedups by seq anyway.
	got := collect(sp.ReadFrom(1))
	want := []uint64{7, 8, 9}
	if len(got) != len(want) {
		t.Fatalf("post-trim reads = %v, want %v", seqs(got), want)
	}
	for i, f := range got {
		if f.Seq != want[i] {
			t.Fatalf("post-trim reads = %v, want %v", seqs(got), want)
		}
	}
}

func TestEvictionAccumulatesDroppedSeconds(t *testing.T) {
	dir := t.TempDir()
	// cap ~5 records (46 B each → 230); segBytes 100 → ~2/seg.
	sp, err := Open(dir, "boot-1", 230, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	for i := uint64(1); i <= 12; i++ {
		if err := sp.Write(frag(i, int64(i), 500, false, 10)); err != nil {
			t.Fatal(err)
		}
	}

	if d := sp.DroppedS(); d <= 0 {
		t.Fatalf("dropped_s = %v, want > 0 after evictions", d)
	}
	if sp.Bytes() > 230+64 { // cap + one open segment's slack
		t.Fatalf("Bytes = %d over cap slack", sp.Bytes())
	}
	// Oldest retained must be > 1 (early frags were evicted).
	if o := sp.Oldest(); o <= 1 {
		t.Fatalf("Oldest = %d, want evicted past 1", o)
	}
	// Newest frags still fully readable.
	got := collect(sp.ReadFrom(12))
	if len(got) != 1 || got[0].Seq != 12 {
		t.Fatalf("ReadFrom(12) = %v, want [12]", seqs(got))
	}
}

func TestCRCDetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	sp, err := Open(dir, "boot-1", 16<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= 6; i++ {
		if err := sp.Write(frag(i, int64(i), 10, false, 50)); err != nil {
			t.Fatal(err)
		}
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}

	// Corrupt a payload byte in the middle of the single segment.
	path := filepath.Join(dir, "seg-000001.spl")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	// record size = 32 + 50 + 4 = 86; record 3's payload starts at 4 + 2*86 + 32.
	if _, err := f.WriteAt([]byte{0xFF}, 4+2*86+32); err != nil {
		t.Fatal(err)
	}
	f.Close()

	sp2, err := Open(dir, "boot-1", 16<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer sp2.Close()

	got := collect(sp2.ReadFrom(1))
	// Iterator yields records until the corrupt one, then stops the segment.
	if len(got) != 2 {
		t.Fatalf("expected 2 good records before corrupt one, got %v", seqs(got))
	}
}

func TestReopenPersistsAndResumes(t *testing.T) {
	dir := t.TempDir()
	sp, err := Open(dir, "boot-1", 16<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= 4; i++ {
		sp.Write(frag(i, int64(i), 10, false, 20))
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}

	sp2, err := Open(dir, "boot-1", 16<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer sp2.Close()
	if l := sp2.Latest(); l != 4 {
		t.Fatalf("reopened Latest = %d, want 4", l)
	}
	got := collect(sp2.ReadFrom(1))
	if len(got) != 4 {
		t.Fatalf("reopened spool has %d frags, want 4", len(got))
	}
}

func TestBootIDMismatchWipes(t *testing.T) {
	dir := t.TempDir()
	sp, err := Open(dir, "boot-1", 16<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= 3; i++ {
		sp.Write(frag(i, int64(i), 10, false, 20))
	}
	sp.Close()

	sp2, err := Open(dir, "boot-2", 16<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer sp2.Close()
	if got := collect(sp2.ReadFrom(1)); len(got) != 0 {
		t.Fatalf("boot_id change should wipe spool, read %d frags", len(got))
	}
	if l := sp2.Latest(); l != 0 {
		t.Fatalf("Latest after wipe = %d, want 0", l)
	}
}

func TestIndexFileWritten(t *testing.T) {
	dir := t.TempDir()
	sp, err := Open(dir, "boot-x", 16<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sp.Write(frag(1, 1, 10, false, 20))
	sp.Close()

	data, err := os.ReadFile(filepath.Join(dir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	var idx indexFile
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	if idx.BootID != "boot-x" || idx.WriteSeg != 1 || idx.ReadSeg != 1 || idx.ReadOff != 4 {
		t.Fatalf("index = %+v", idx)
	}
}

func TestTornTailTruncatedOnOpen(t *testing.T) {
	dir := t.TempDir()
	sp, err := Open(dir, "boot-1", 16<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= 3; i++ {
		sp.Write(frag(i, int64(i), 10, false, 20))
	}
	sp.Close()

	// Append a half-written record — simulates a kill mid-write.
	path := filepath.Join(dir, "seg-000001.spl")
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte{0x53, 0x46, 0x52, 0x47, 0x00}) // SFRG then nothing
	f.Close()

	sp2, err := Open(dir, "boot-1", 16<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer sp2.Close()
	got := collect(sp2.ReadFrom(1))
	if len(got) != 3 {
		t.Fatalf("torn tail should be truncated; read %v", seqs(got))
	}
	st, _ := os.Stat(path)
	if st.Size() != 4+3*(32+20+4) {
		t.Fatalf("file not truncated to last good record: %d", st.Size())
	}
}
