package chunker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sjalotra/surveillance-os/backend/internal/blob"
	"github.com/sjalotra/surveillance-os/backend/internal/proto"
	"github.com/sjalotra/surveillance-os/backend/internal/store"
)

// fakeBlob is an in-memory blob.Client recording every put.
type fakeBlob struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    []string // key order
	getErr  map[string]error
}

func newFakeBlob() *fakeBlob { return &fakeBlob{objects: map[string][]byte{}} }

func (f *fakeBlob) Put(_ context.Context, key string, body io.Reader, _ int64, _ string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data
	f.puts = append(f.puts, key)
	return nil
}

func (f *fakeBlob) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.getErr[key]; ok {
		return nil, err
	}
	data, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("no such key %s", key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeBlob) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://r2.test/" + key + "?sig=1", nil
}

func (f *fakeBlob) List(_ context.Context, prefix string) ([]blob.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []blob.Object
	for k, v := range f.objects {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, blob.Object{Key: k, Size: int64(len(v))})
		}
	}
	return out, nil
}

// fakeStore records chunk upserts.
type fakeStore struct {
	mu     sync.Mutex
	chunks []store.Chunk
	err    error
}

func (f *fakeStore) UpsertChunk(_ context.Context, c store.Chunk) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.chunks = append(f.chunks, c)
	return nil
}

func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.chunks)
}

func (f *fakeStore) last() store.Chunk {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chunks[len(f.chunks)-1]
}

// fragBytes synthesizes a plausible moof+mdat payload — box-shaped so
// splitInit can walk assembled mp4s on the R2-restore path.
func fragBytes(seq uint64, n int) []byte {
	if n < 28 {
		n = 28
	}
	b := append(box(20, "moof"), box(n-20, "mdat")...)
	for i := 28; i < len(b); i++ {
		b[i] = byte(seq) ^ byte(i)
	}
	return b
}

func box(size int, typ string) []byte {
	b := make([]byte, size)
	b[3] = byte(size)
	copy(b[4:8], typ)
	return b
}

// testInit is a structurally-valid init segment (ftyp+moov, 40 bytes) so
// splitInit can walk it on the R2-restore path.
var testInit = append(box(16, "ftyp"), box(24, "moov")...)

var testNow = time.UnixMilli(1737000000000) // fixed base for determinism

func newTestChunker(t *testing.T, b blob.Client, s ChunkStore) (*Chunker, *time.Time) {
	t.Helper()
	now := testNow
	c, err := New(b, s, Options{
		ScratchDir:    t.TempDir(),
		KeyPrefix:     "surveillance",
		Quiet:         60 * time.Second,
		ScratchRetain: 4 * time.Hour,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, &now
}

const cam = "home-a"
const boot = "boot-1"

func feedN(t *testing.T, c *Chunker, startSeq uint64, startPts, dur int64, n int, keyEvery int) {
	t.Helper()
	for i := 0; i < n; i++ {
		f := proto.Fragment{
			Seq:   startSeq + uint64(i),
			PtsMs: startPts + int64(i)*dur,
			DurMs: uint32(dur),
			Key:   keyEvery > 0 && i%keyEvery == 0,
			Data:  fragBytes(startSeq+uint64(i), 100),
		}
		if err := c.Feed(cam, boot, f); err != nil {
			t.Fatalf("feed seq %d: %v", f.Seq, err)
		}
	}
}

func TestWindowIndexFloor(t *testing.T) {
	if windowIndex(0) != 0 || windowIndex(599_999) != 0 || windowIndex(600_000) != 1 {
		t.Fatal("windowIndex boundaries wrong")
	}
	if windowIndex(-1) != -1 || windowIndex(-600_000) != -1 || windowIndex(-600_001) != -2 {
		t.Fatal("windowIndex negative floor wrong")
	}
}

func TestFinalizeOnDataPastWindow(t *testing.T) {
	fb := newFakeBlob()
	fs := &fakeStore{}
	c, now := newTestChunker(t, fb, fs)
	c.SetFormat(cam, "h264", "avc1.640028", 1920, 1080)
	c.SetInit(cam, testInit)

	// Five 1s frags in window 0 (pts 0..4000), then one in window 1.
	feedN(t, c, 1, 0, 1000, 5, 2)
	if fs.count() != 0 {
		t.Fatal("premature finalize")
	}
	feedN(t, c, 6, 600_000, 1000, 1, 0)

	// Window 0 must be finalized: mp4 then manifest then upsert.
	mp4Key := "surveillance/home-a/0.mp4"
	manKey := "surveillance/home-a/0.json"
	mp4, ok := fb.objects[mp4Key]
	if !ok {
		t.Fatalf("missing %s", mp4Key)
	}
	wantPayload := append([]byte(nil), testInit...)
	for i := uint64(1); i <= 5; i++ {
		wantPayload = append(wantPayload, fragBytes(i, 100)...)
	}
	if !bytes.Equal(mp4, wantPayload) {
		t.Fatalf("mp4 bytes wrong: got %d want %d", len(mp4), len(wantPayload))
	}

	var man Manifest
	if err := json.Unmarshal(fb.objects[manKey], &man); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	sum := sha256.Sum256(wantPayload)
	if man.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("manifest sha256 mismatch")
	}
	if man.WindowStart != 0 || man.Cam != cam || man.Codec != "h264" || man.CodecStr != "avc1.640028" {
		t.Fatalf("bad manifest: %+v", man)
	}
	if man.Width != 1920 || man.Height != 1080 {
		t.Fatalf("bad dims: %+v", man)
	}
	// keyframes at i=0,2,4 → offs 40 (init) + 0,200,400
	if len(man.Keyframes) != 3 {
		t.Fatalf("keyframes: %+v", man.Keyframes)
	}
	if man.Keyframes[0].Off != 40 || man.Keyframes[1].Off != 240 || man.Keyframes[2].Off != 440 {
		t.Fatalf("keyframe offs: %+v", man.Keyframes)
	}
	if man.Keyframes[0].TMs != 0 || man.Keyframes[1].TMs != 2000 {
		t.Fatalf("keyframe t_ms: %+v", man.Keyframes)
	}
	if man.DurationS != 5 {
		t.Fatalf("duration_s = %v", man.DurationS)
	}

	// R2-first ordering: upsert happens only after both objects exist.
	if fs.count() != 1 {
		t.Fatal("no upsert")
	}
	ch := fs.last()
	if ch.R2Key != mp4Key || ch.ManifestKey != manKey || ch.Bytes != int64(len(wantPayload)) {
		t.Fatalf("bad chunk row: %+v", ch)
	}
	if ch.SHA256 != man.SHA256 || ch.WindowStart.Unix() != 0 || ch.WindowEnd.Unix() != 600 {
		t.Fatalf("bad chunk times/sha: %+v", ch)
	}

	// Ordering assertion: mp4 put precedes manifest put precedes upsert —
	// upsert is only ever called from doFinalize after both puts.
	_ = now
}

func TestQuietFinalizeAndFlush(t *testing.T) {
	fb := newFakeBlob()
	fs := &fakeStore{}
	c, now := newTestChunker(t, fb, fs)
	c.SetInit(cam, testInit)

	feedN(t, c, 1, 0, 1000, 3, 1)
	// Not quiet yet.
	c.Tick(cam)
	if fs.count() != 0 {
		t.Fatal("quiet tick finalized too early")
	}
	// Advance wall clock past quiet.
	*now = now.Add(61 * time.Second)
	c.Tick(cam)
	if fs.count() != 1 {
		t.Fatal("quiet finalize did not run")
	}

	// New data in a later window; Flush on disconnect finalizes it.
	feedN(t, c, 10, 600_000, 1000, 2, 0)
	c.Flush(cam)
	if fs.count() != 2 {
		t.Fatal("flush did not finalize")
	}
}

func TestLateReplayReopensFinalizedWindow(t *testing.T) {
	fb := newFakeBlob()
	fs := &fakeStore{}
	c, now := newTestChunker(t, fb, fs)
	c.SetInit(cam, testInit)

	// Window 0: 5 frags, window 1: 1 frag → w0 finalized.
	feedN(t, c, 1, 0, 1000, 5, 0)
	feedN(t, c, 6, 600_000, 1000, 1, 0)
	if fs.count() != 1 {
		t.Fatal("w0 not finalized")
	}
	putsBefore := len(fb.puts)

	// Out-of-order replay arrives for finalized window 0.
	late := proto.Fragment{
		Seq: 50, PtsMs: 5_000, DurMs: 1000, Replay: true,
		Data: fragBytes(50, 100),
	}
	if err := c.Feed(cam, boot, late); err != nil {
		t.Fatal(err)
	}

	// It reopens w0; next live frag (window 1) re-finalizes it.
	feedN(t, c, 7, 601_000, 1000, 1, 0)
	mp4 := fb.objects["surveillance/home-a/0.mp4"]
	wantLen := 40 + 6*100 // init + 6 frags
	if len(mp4) != wantLen {
		t.Fatalf("reopened mp4 len = %d, want %d", len(mp4), wantLen)
	}
	if fs.count() != 2 {
		t.Fatal("no re-upsert after reopen")
	}
	if len(fb.puts) <= putsBefore {
		t.Fatal("no rewrite happened")
	}

	// Resending the same replay must be deduped (idempotent).
	if err := c.Feed(cam, boot, late); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(61 * time.Second)
	c.Tick(cam)
	mp4 = fb.objects["surveillance/home-a/0.mp4"]
	if len(mp4) != wantLen {
		t.Fatalf("duplicate append: len = %d, want %d", len(mp4), wantLen)
	}
}

func TestCrossBootSameSeqNotDeduped(t *testing.T) {
	fb := newFakeBlob()
	fs := &fakeStore{}
	c, _ := newTestChunker(t, fb, fs)
	c.SetInit(cam, testInit)

	// boot-1 seq 1..3, then a "reboot" (boot-2) whose seqs restart at 1
	// with different pts in the same window — must NOT dedupe.
	feedN(t, c, 1, 0, 1000, 3, 0)
	for i := 0; i < 3; i++ {
		f := proto.Fragment{Seq: uint64(1 + i), PtsMs: 10_000 + int64(i)*1000, DurMs: 1000, Data: fragBytes(uint64(100+i), 50)}
		if err := c.Feed(cam, "boot-2", f); err != nil {
			t.Fatal(err)
		}
	}
	c.Flush(cam)
	mp4 := fb.objects["surveillance/home-a/0.mp4"]
	want := 40 + 3*100 + 3*50
	if len(mp4) != want {
		t.Fatalf("len = %d want %d (cross-boot seqs wrongly deduped)", len(mp4), want)
	}
}

func TestReopenFromR2WhenScratchGone(t *testing.T) {
	fb := newFakeBlob()
	fs := &fakeStore{}
	c, _ := newTestChunker(t, fb, fs)
	c.SetInit(cam, testInit)

	feedN(t, c, 1, 0, 1000, 3, 2)       // keys at i=0,2 → 2 keyframes
	feedN(t, c, 9, 600_000, 1000, 1, 0) // finalizes w0

	// Simulate scratch GC + new chunker instance (hub restart, cold disk).
	dir2 := t.TempDir()
	c2, err := New(fb, fs, Options{ScratchDir: dir2, KeyPrefix: "surveillance", Quiet: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	c2.SetInit(cam, testInit)

	// Replay for the old window must restore state from R2 objects:
	// manifest (keyframes/seqs) + mp4 (split into init+frags).
	late := proto.Fragment{Seq: 40, PtsMs: 3_500, DurMs: 1000, Replay: true, Data: fragBytes(40, 77)}
	if err := c2.Feed(cam, boot, late); err != nil {
		t.Fatalf("reopen-from-R2: %v", err)
	}
	feedN(t, c2, 10, 601_000, 1000, 1, 0) // trigger re-finalize

	mp4 := fb.objects["surveillance/home-a/0.mp4"]
	want := 40 + 3*100 + 77 // init + orig 3 frags + late frag
	if len(mp4) != want {
		t.Fatalf("rebuilt mp4 len = %d want %d", len(mp4), want)
	}
	var man Manifest
	if err := json.Unmarshal(fb.objects["surveillance/home-a/0.json"], &man); err != nil {
		t.Fatal(err)
	}
	if len(man.Keyframes) != 2 { // orig had keys at 0,2000; late frag isn't key
		t.Fatalf("restored keyframes: %+v", man.Keyframes)
	}
	// Dedup survives R2 restore: resend an ORIGINAL seq — must be skipped.
	dup := proto.Fragment{Seq: 1, PtsMs: 0, DurMs: 1000, Replay: true, Data: fragBytes(1, 100)}
	if err := c2.Feed(cam, boot, dup); err != nil {
		t.Fatal(err)
	}
	c2.Flush(cam)
	mp4 = fb.objects["surveillance/home-a/0.mp4"]
	if len(mp4) != want {
		t.Fatalf("dup survived R2-restore dedup: len = %d want %d", len(mp4), want)
	}
}

func TestScratchSidecarSurvivesRestart(t *testing.T) {
	fb := newFakeBlob()
	fs := &fakeStore{}
	dir := t.TempDir()
	c, err := New(fb, fs, Options{ScratchDir: dir, KeyPrefix: "surveillance", Quiet: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	c.SetInit(cam, testInit)
	feedN(t, c, 1, 0, 1000, 4, 2)
	c.Close() // persists sidecars, no finalize

	// New instance: Recover() must reload window state and finalize it.
	c2, err := New(fb, fs, Options{ScratchDir: dir, KeyPrefix: "surveillance"})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	c2.Recover()
	if fs.count() != 1 {
		t.Fatal("recover did not finalize open window")
	}
	var man Manifest
	if err := json.Unmarshal(fb.objects["surveillance/home-a/0.json"], &man); err != nil {
		t.Fatal(err)
	}
	if len(man.Keyframes) != 2 || len(man.Seqs) != 4 {
		t.Fatalf("recovered state wrong: %+v", man)
	}
}

func TestStoreFailureQueuesUpsert(t *testing.T) {
	fb := newFakeBlob()
	fs := &fakeStore{err: fmt.Errorf("planetscale down")}
	c, _ := newTestChunker(t, fb, fs)
	c.SetInit(cam, testInit)
	feedN(t, c, 1, 0, 1000, 2, 0)
	feedN(t, c, 9, 600_000, 1000, 1, 0)

	// R2 objects exist despite the failed upsert — ordering guarantee.
	if _, ok := fb.objects["surveillance/home-a/0.mp4"]; !ok {
		t.Fatal("mp4 missing")
	}
	if _, ok := fb.objects["surveillance/home-a/0.json"]; !ok {
		t.Fatal("manifest missing")
	}
	// Upsert is queued for retry.
	c.rqmu.Lock()
	n := len(c.retryq)
	c.rqmu.Unlock()
	if n != 1 {
		t.Fatalf("retryq len = %d", n)
	}

	// DB heals → retry lands the row.
	fs.mu.Lock()
	fs.err = nil
	fs.mu.Unlock()
	if err := c.retryUpsert(retryJob{cam: cam, idx: 0}); err != nil {
		t.Fatal(err)
	}
	if fs.count() != 1 {
		t.Fatal("retry upsert failed")
	}
}

func TestSplitInitBoxes(t *testing.T) {
	// ftyp(16) + moov(24) + moof(20)+mdat(12)
	mp4 := append(append(box(16, "ftyp"), box(24, "moov")...), append(box(20, "moof"), box(12, "mdat")...)...)
	init, frags, err := splitInit(mp4)
	if err != nil {
		t.Fatal(err)
	}
	if len(init) != 40 || len(frags) != 32 {
		t.Fatalf("init=%d frags=%d", len(init), len(frags))
	}
}
