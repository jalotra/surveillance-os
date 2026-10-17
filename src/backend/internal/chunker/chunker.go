// Package chunker assembles 10-minute fMP4 windows per camera and lands
// them durably: scratch files on hub disk → R2 PutObject (mp4, then
// manifest json) → Postgres upsert, strictly in that order.
//
// Window index = floor(pts_ms / 600_000); window_start unix = idx*600.
// A window finalizes when data arrives past it, or after 60s quiet, or on
// Flush (camera disconnect). A late fragment (replay=1) into a finalized
// window reopens it and rewrites both objects — keys are stable so the
// rewrite is idempotent.
//
// Durability: per-window sidecar JSON + .init/.frags scratch files make
// every window reconstructable across hub restarts (Recover); once
// finalized, the R2 manifest alone rebuilds the catalog row. Ordering is
// the load-bearing rule: the Postgres upsert happens only after both R2
// objects are stored, and failed puts/upserts queue for retry — a
// PlanetScale or R2 blip can never lose footage, only delay it.
//
// Concurrency: a camera's owner goroutine makes all per-camera calls.
// c.mu guards the cams/windows maps (never held across IO or w.mu);
// w.mu guards one Window's fields and may be held across file/R2 IO;
// rqmu guards only the retry queue. w.finalized / w.lastWrite are
// atomics so map scans never need w.mu. Lock order: c.mu → w.mu → rqmu.
package chunker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjalotra/surveillance-os/backend/internal/blob"
	"github.com/sjalotra/surveillance-os/backend/internal/proto"
	"github.com/sjalotra/surveillance-os/backend/internal/store"
)

// WindowMs is the chunk length in the pts domain: 10 minutes.
const WindowMs = 600_000

// Defaults.
const (
	DefaultQuiet         = 60 * time.Second // finalize after this much silence
	DefaultScratchRetain = 4 * time.Hour    // keep finalized scratch for replays
	putAttempts          = 2                // inline tries before queueing
	retryEvery           = 10 * time.Second
	maxRetryQueue        = 1024
)

// ChunkStore is the Postgres surface the chunker needs.
type ChunkStore interface {
	UpsertChunk(ctx context.Context, c store.Chunk) error
}

// KeyOff is one manifest keyframe entry: t_ms relative to window start,
// off = absolute byte offset of the fragment's moof in the final mp4.
type KeyOff struct {
	TMs int64 `json:"t_ms"`
	Off int64 `json:"off"`
}

// Manifest is the {window}.json object stored next to the mp4.
// It is a superset of plans/backend.md §2.2: "seqs" (boot:seq of every
// written fragment) and "last_seq" make reopen exactly dedupable.
type Manifest struct {
	Cam         string   `json:"cam"`
	WindowStart int64    `json:"window_start"`
	DurationS   float64  `json:"duration_s"`
	Bytes       int64    `json:"bytes"`
	Codec       string   `json:"codec"`
	CodecStr    string   `json:"codec_str"`
	Width       int      `json:"width"`
	Height      int      `json:"height"`
	Keyframes   []KeyOff `json:"keyframes"`
	Seqs        []string `json:"seqs"`
	SHA256      string   `json:"sha256"`
	DroppedS    float64  `json:"dropped_s"`
	Suspect     bool     `json:"suspect"`
	LastSeq     uint64   `json:"last_seq"`
}

// Chunker manages all cameras' windows.
type Chunker struct {
	dir    string
	blob   blob.Client
	store  ChunkStore
	prefix string // R2 key prefix, e.g. "surveillance"
	quiet  time.Duration
	retain time.Duration
	logf   func(format string, args ...any)
	now    func() time.Time

	mu   sync.Mutex
	cams map[string]*camState

	rqmu   sync.Mutex
	retryq []retryJob
	wake   chan struct{}
	done   chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
}

// camState fields are guarded by c.mu (mutated by SetFormat/SetInit/
// SetSuspect on the owner goroutine, read by the retry worker too).
// Feed reads them only via the Window copies made under c.mu.
type camState struct {
	name     string
	windows  map[int64]*Window
	init     []byte
	codec    string
	codecStr string
	w, h     int
	suspect  bool // current boot's clock skewed > 5min → mark windows
}

type retryJob struct {
	cam       string
	idx       int64
	stage     string // "put" (redo whole finalize) or "upsert" (R2 done, DB owed)
	attempts  int
	notBefore time.Time
}

// seqKey dedupes fragments per (boot_id, seq) — seq resets every boot, so
// the pair is the only correct identity.
type seqKey struct {
	boot string
	seq  uint64
}

// Window is one open or finalized-but-retained chunk window.
type Window struct {
	mu       sync.Mutex
	camID    string
	idx      int64
	path     string // scratch base: {dir}/{cam}/{idx}
	frags    *os.File
	initLen  int64
	fragsLen int64

	loaded    atomic.Bool  // restore/open completed; read lock-free
	removed   atomic.Bool  // scratch GC'd; object truth lives in R2
	finalized atomic.Bool  // written under w.mu, read lock-free
	lastWrite atomic.Int64 // unixnano of last appended frag

	codec, codecStr string
	w, h            int

	keyframes []keyOffRel
	seqs      map[seqKey]struct{}
	lastSeq   uint64
	lastEndMs int64
	droppedS  float64
	suspect   bool
}

type keyOffRel struct {
	tMs int64
	off int64 // relative to start of .frags (manifest adds initLen)
}

// sidecar is the persisted window state ({idx}.json in scratch).
type sidecar struct {
	Cam       string   `json:"cam"`
	Idx       int64    `json:"idx"`
	Codec     string   `json:"codec"`
	CodecStr  string   `json:"codec_str"`
	W         int      `json:"w"`
	H         int      `json:"h"`
	Keyframes []KeyOff `json:"keyframes"` // off RELATIVE to .frags here
	Seqs      []string `json:"seqs"`      // "boot:seq"
	LastSeq   uint64   `json:"last_seq"`
	LastEndMs int64    `json:"last_end_ms"`
	DroppedS  float64  `json:"dropped_s"`
	Suspect   bool     `json:"suspect"`
	Finalized bool     `json:"finalized"`
}

// Options tunes a Chunker; zero values take defaults.
type Options struct {
	ScratchDir    string
	KeyPrefix     string        // R2 key prefix, default "surveillance"
	Quiet         time.Duration // default 60s
	ScratchRetain time.Duration // default 4h
	Logf          func(string, ...any)
	Now           func() time.Time // test injection
}

// New creates a Chunker. ScratchDir holds open window files.
func New(b blob.Client, st ChunkStore, o Options) (*Chunker, error) {
	if o.ScratchDir == "" {
		return nil, fmt.Errorf("chunker: ScratchDir required")
	}
	if err := os.MkdirAll(o.ScratchDir, 0o755); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Chunker{
		dir:    o.ScratchDir,
		blob:   b,
		store:  st,
		prefix: o.KeyPrefix,
		quiet:  o.Quiet,
		retain: o.ScratchRetain,
		logf:   o.Logf,
		now:    o.Now,
		cams:   map[string]*camState{},
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
		ctx:    ctx,
		cancel: cancel,
	}
	if c.prefix == "" {
		c.prefix = "surveillance"
	}
	if c.quiet <= 0 {
		c.quiet = DefaultQuiet
	}
	if c.retain <= 0 {
		c.retain = DefaultScratchRetain
	}
	if c.logf == nil {
		c.logf = log.Printf
	}
	if c.now == nil {
		c.now = time.Now
	}
	go c.retryLoop()
	return c, nil
}

// SetFormat records the camera's codec/geometry from hello.
// Owner-goroutine only.
func (c *Chunker) SetFormat(cam, codec, codecStr string, w, h int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cs := c.cam(cam)
	cs.codec, cs.codecStr, cs.w, cs.h = codec, codecStr, w, h
}

// SetInit records the camera's init segment (ftyp+moov). Open windows get
// it too — fragments after a reconnect belong to the new segment.
// Owner-goroutine only.
func (c *Chunker) SetInit(cam string, init []byte) {
	c.mu.Lock()
	cs := c.cam(cam)
	cs.init = append([]byte(nil), init...)
	var ws []*Window
	for _, w := range cs.windows {
		ws = append(ws, w)
	}
	c.mu.Unlock()
	for _, w := range ws {
		w.mu.Lock()
		w.writeInitLocked(init)
		w.mu.Unlock()
	}
}

// SetSuspect marks the camera's current boot as clock-skewed; windows fed
// while set persist suspect=true. Never affects ingest. Owner-goroutine.
func (c *Chunker) SetSuspect(cam string, suspect bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cam(cam).suspect = suspect
}

// AddDropped attributes Pi-reported dropped seconds to the camera's
// newest window (best-effort — the Pi reports only a cumulative counter).
// Owner-goroutine only.
func (c *Chunker) AddDropped(cam string, secs float64) {
	c.mu.Lock()
	cs := c.cams[cam]
	var best *Window
	if cs != nil {
		for _, w := range cs.windows {
			if best == nil || w.idx > best.idx {
				best = w
			}
		}
	}
	c.mu.Unlock()
	if best == nil {
		return
	}
	best.mu.Lock()
	best.droppedS += secs
	best.mu.Unlock()
}

// Feed routes f into its window by floor(pts_ms/600_000), finalizing any
// unfinalized window the fragment proves is complete (data-past-window).
// Duplicate fragments (same boot_id+seq already written) are skipped —
// this is what makes replay idempotent. Owner-goroutine only.
func (c *Chunker) Feed(cam, bootID string, f proto.Fragment) error {
	idx := windowIndex(f.PtsMs)

	c.mu.Lock()
	cs := c.cam(cam)
	suspect := cs.suspect
	var stale []*Window
	for widx, w := range cs.windows {
		if widx < idx && !w.finalized.Load() {
			stale = append(stale, w)
		}
	}
	c.mu.Unlock()

	// Data past a window → it's complete. Outside c.mu, before appending.
	for _, w := range stale {
		c.finalize(w)
	}

	w, err := c.window(cs, idx)
	if err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	k := seqKey{bootID, f.Seq}
	if _, dup := w.seqs[k]; dup {
		return nil // already written — replay resend
	}
	off := w.fragsLen
	if _, err := w.frags.Write(f.Data); err != nil {
		return fmt.Errorf("chunker %s/%d: write: %w", cam, idx, err)
	}
	if f.Key {
		w.keyframes = append(w.keyframes, keyOffRel{tMs: f.PtsMs - idx*WindowMs, off: off})
	}
	w.fragsLen += int64(len(f.Data))
	w.seqs[k] = struct{}{}
	if f.Seq > w.lastSeq {
		w.lastSeq = f.Seq
	}
	if end := f.PtsMs + int64(f.DurMs); end > w.lastEndMs {
		w.lastEndMs = end
	}
	w.lastWrite.Store(c.now().UnixNano())
	if w.suspect || suspect {
		w.suspect = true
	}
	w.finalized.Store(false)
	return w.writeSidecarLocked()
}

// Tick finalizes windows quiet for >quiet, and GCs finalized scratch past
// the retention horizon. Call every few seconds while a camera is
// connected; also safe to call periodically for all cameras.
func (c *Chunker) Tick(cam string) {
	c.TickAll(func(name string) bool { return name == cam })
}

// TickAll runs Tick semantics over cameras matching pred (nil = all).
func (c *Chunker) TickAll(pred func(cam string) bool) {
	now := c.now()
	c.mu.Lock()
	var cams []*camState
	for name, cs := range c.cams {
		if pred == nil || pred(name) {
			cams = append(cams, cs)
		}
	}
	type gcRef struct {
		cs  *camState
		idx int64
		w   *Window
	}
	var gcList []gcRef
	var todo []*Window
	for _, cs := range cams {
		for idx, w := range cs.windows {
			if !w.loaded.Load() {
				continue // mid-restore; owner will finish it
			}
			if !w.finalized.Load() && now.Sub(time.Unix(0, w.lastWrite.Load())) > c.quiet {
				todo = append(todo, w)
				continue
			}
			endMs := idx*WindowMs + WindowMs
			if w.finalized.Load() && !w.removed.Load() && endMs < now.UnixMilli()-c.retain.Milliseconds() {
				gcList = append(gcList, gcRef{cs, idx, w})
			}
		}
	}
	c.mu.Unlock()

	for _, w := range todo {
		c.finalize(w)
	}
	// GC scratch: CAS the tombstone, drop the map entry, then unlink the
	// files under w.mu. Feed and Tick for a cam share one goroutine, so
	// the only interleaving actor is the retry worker — a window it
	// resurrects just falls back to the R2 objects, which always exist
	// for a finalized window.
	for _, g := range gcList {
		if !g.w.removed.CompareAndSwap(false, true) {
			continue
		}
		c.mu.Lock()
		if g.cs.windows[g.idx] == g.w {
			delete(g.cs.windows, g.idx)
		}
		c.mu.Unlock()
		g.w.mu.Lock()
		g.w.gcLocked()
		g.w.mu.Unlock()
	}
}

// Flush finalizes every unfinalized window for cam — called when the
// ingest connection drops.
func (c *Chunker) Flush(cam string) {
	c.mu.Lock()
	cs := c.cams[cam]
	var todo []*Window
	if cs != nil {
		for _, w := range cs.windows {
			if !w.finalized.Load() && w.loaded.Load() {
				todo = append(todo, w)
			}
		}
	}
	c.mu.Unlock()
	for _, w := range todo {
		c.finalize(w)
	}
}

// Recover scans scratch dirs at startup: windows with sidecars are
// reloaded, and any unfinalized window is finalized immediately — the
// hub-restart-mid-window path from the kill test.
func (c *Chunker) Recover() {
	cams, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, ce := range cams {
		if !ce.IsDir() {
			continue
		}
		cam := ce.Name()
		files, _ := os.ReadDir(filepath.Join(c.dir, cam))
		for _, fe := range files {
			name := fe.Name()
			if !strings.HasSuffix(name, ".json") {
				continue // skips .init, .frags, .json.tmp, .corrupt
			}
			var idx int64
			if n, _ := fmt.Sscanf(name, "%d.json", &idx); n != 1 {
				continue
			}
			c.mu.Lock()
			cs := c.cam(cam)
			c.mu.Unlock()
			w, err := c.window(cs, idx)
			if err != nil {
				c.logf("chunker recover %s/%d: %v", cam, idx, err)
				continue
			}
			if !w.finalized.Load() {
				c.finalize(w)
			}
		}
	}
}

// Close stops the retry worker and persists all window state. It does not
// finalize — scratch + sidecars let the next start finish the job.
func (c *Chunker) Close() {
	c.cancel()
	<-c.done
	c.mu.Lock()
	var ws []*Window
	for _, cs := range c.cams {
		for _, w := range cs.windows {
			ws = append(ws, w)
		}
	}
	c.mu.Unlock()
	for _, w := range ws {
		w.mu.Lock()
		_ = w.writeSidecarLocked()
		if w.frags != nil {
			_ = w.frags.Sync()
		}
		w.mu.Unlock()
	}
}

// ---- internals ----

func (c *Chunker) cam(name string) *camState {
	cs := c.cams[name]
	if cs == nil {
		cs = &camState{name: name, windows: map[int64]*Window{}}
		c.cams[name] = cs
	}
	return cs
}

// window returns the loaded window for idx, creating it if needed.
// Insertion happens under c.mu; the (slow, IO) load happens under w.mu.
func (c *Chunker) window(cs *camState, idx int64) (*Window, error) {
	c.mu.Lock()
	w := cs.windows[idx]
	if w != nil && w.removed.Load() {
		w = nil // GC'd tombstone — replace it
	}
	if w == nil {
		w = &Window{
			camID: cs.name,
			idx:   idx,
			path:  filepath.Join(c.dir, cs.name, fmt.Sprint(idx)),
			seqs:  map[seqKey]struct{}{},
			codec: cs.codec, codecStr: cs.codecStr, w: cs.w, h: cs.h,
			suspect: cs.suspect,
		}
		cs.windows[idx] = w
	}
	initSnap := append([]byte(nil), cs.init...)
	c.mu.Unlock()

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.loaded.Load() {
		return w, nil
	}
	if err := c.load(w, initSnap); err != nil {
		return nil, err
	}
	w.loaded.Store(true)
	return w, nil
}

// load restores window state: scratch sidecar → R2 objects → fresh.
// Caller holds w.mu; may run for a while (R2 fetch). initSnap is the
// caller's under-c.mu snapshot of cs.init — a later SetInit also rewrites
// this window's .init file directly, so staleness self-heals.
func (c *Chunker) load(w *Window, initSnap []byte) error {
	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return err
	}
	if data, err := os.ReadFile(w.path + ".json"); err == nil {
		var sc sidecar
		if err := json.Unmarshal(data, &sc); err == nil && sc.Cam == w.camID && sc.Idx == w.idx {
			if err := w.openFilesLocked(); err != nil {
				return err
			}
			w.loadSidecarLocked(&sc)
			return nil
		}
		c.logf("chunker: bad sidecar %s, rebuilding from objects", w.path)
	} else if fileExists(w.path + ".frags") {
		// Scratch data without state can't be trusted — quarantine it.
		c.logf("chunker: orphan scratch %s, quarantining", w.path)
		for _, ext := range []string{".init", ".frags"} {
			os.Rename(w.path+ext, w.path+ext+".corrupt")
		}
	}

	// Fall back to the durable objects in R2.
	mkey := c.manifestKey(w.camID, w.idx)
	mr, err := c.blob.Get(c.ctx, mkey)
	if err != nil {
		return c.fresh(w, initSnap) // no manifest → genuinely new window
	}
	defer mr.Close()
	var man Manifest
	if err := json.NewDecoder(mr).Decode(&man); err != nil {
		return fmt.Errorf("manifest %s decode: %w", mkey, err)
	}
	mp4, err := c.blob.Get(c.ctx, c.mp4Key(w.camID, w.idx))
	if err != nil {
		return fmt.Errorf("manifest exists but mp4 missing: %w", err)
	}
	defer mp4.Close()
	all, err := io.ReadAll(mp4)
	if err != nil {
		return err
	}
	init, frags, err := splitInit(all)
	if err != nil {
		return fmt.Errorf("split %s: %w", mkey, err)
	}
	if err := os.WriteFile(w.path+".init", init, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(w.path+".frags", frags, 0o644); err != nil {
		return err
	}
	if err := w.openFilesLocked(); err != nil {
		return err
	}
	w.loadManifestLocked(&man)
	c.logf("chunker: %s/%d restored from R2 (%d frag bytes)", w.camID, w.idx, w.fragsLen)
	return nil
}

// fresh opens empty scratch files and seeds the window with init.
// Caller holds w.mu.
func (c *Chunker) fresh(w *Window, init []byte) error {
	if err := w.openFilesLocked(); err != nil {
		return err
	}
	w.writeInitLocked(init)
	return nil
}

// finalize runs the durable sequence for one window.
func (c *Chunker) finalize(w *Window) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c.finalizeLocked(w)
}

// finalizeLocked runs under w.mu.
func (c *Chunker) finalizeLocked(w *Window) {
	if w.finalized.Load() || !w.loaded.Load() || w.removed.Load() {
		return
	}
	if w.fragsLen == 0 {
		return // nothing stored — no playable data
	}
	if err := c.doFinalize(w); err != nil {
		c.logf("chunker: finalize %s/%d failed (%v) — queued for retry", w.camID, w.idx, err)
		c.enqueue(retryJob{cam: w.camID, idx: w.idx, stage: "put"})
		return
	}
	w.finalized.Store(true)
	_ = w.writeSidecarLocked()
}

// doFinalize performs the R2-puts-then-DB-upsert sequence. Caller holds
// w.mu. mp4 lands first, manifest second, Postgres last.
func (c *Chunker) doFinalize(w *Window) error {
	init, err := os.ReadFile(w.path + ".init")
	if err != nil {
		return fmt.Errorf("read init: %w", err)
	}
	if w.frags != nil {
		if err := w.frags.Sync(); err != nil {
			return err
		}
	}
	fragsFile, err := os.Open(w.path + ".frags")
	if err != nil {
		return err
	}
	defer fragsFile.Close()

	total := int64(len(init)) + w.fragsLen
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Minute)
	defer cancel()

	freshBody := func() (io.Reader, hash.Hash) {
		h := sha256.New()
		fragsFile.Seek(0, io.SeekStart)
		return io.MultiReader(
			io.TeeReader(bytes.NewReader(init), h),
			io.TeeReader(fragsFile, h),
		), h
	}

	var lastErr error
	var sum string
	for i := 0; i < putAttempts; i++ {
		body, h := freshBody()
		if lastErr = c.blob.Put(ctx, c.mp4Key(w.camID, w.idx), body, total, "video/mp4"); lastErr == nil {
			sum = hex.EncodeToString(h.Sum(nil))
			break
		}
	}
	if lastErr != nil {
		return lastErr
	}

	man := w.manifestLocked(sum, int64(len(init)), total)
	mdata, err := json.Marshal(man)
	if err != nil {
		return err
	}
	if err := c.blob.Put(ctx, c.manifestKey(w.camID, w.idx), bytes.NewReader(mdata), int64(len(mdata)), "application/json"); err != nil {
		return err
	}

	// R2 is durable — only now may Postgres hear about it.
	chunk := store.Chunk{
		CamID:       w.camID,
		WindowStart: time.Unix(w.idx*600, 0).UTC(),
		WindowEnd:   time.Unix(w.idx*600+600, 0).UTC(),
		R2Key:       c.mp4Key(w.camID, w.idx),
		ManifestKey: c.manifestKey(w.camID, w.idx),
		Bytes:       total,
		DurationS:   man.DurationS,
		SHA256:      sum,
		DroppedS:    w.droppedS,
		Suspect:     w.suspect,
	}
	if err := c.store.UpsertChunk(ctx, chunk); err != nil {
		c.logf("chunker: upsert %s/%d failed (%v) — queued", w.camID, w.idx, err)
		c.enqueue(retryJob{cam: w.camID, idx: w.idx, stage: "upsert"})
	}
	return nil
}

func (c *Chunker) mp4Key(cam string, idx int64) string {
	return fmt.Sprintf("%s/%s/%d.mp4", c.prefix, cam, idx*600)
}

func (c *Chunker) manifestKey(cam string, idx int64) string {
	return fmt.Sprintf("%s/%s/%d.json", c.prefix, cam, idx*600)
}

func (c *Chunker) enqueue(j retryJob) {
	c.rqmu.Lock()
	defer c.rqmu.Unlock()
	if len(c.retryq) >= maxRetryQueue {
		c.logf("chunker: retry queue full, dropping %s %s/%d", j.stage, j.cam, j.idx)
		return
	}
	j.notBefore = c.now().Add(retryEvery)
	c.retryq = append(c.retryq, j)
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// retryLoop re-runs queued puts/upserts with backoff until they succeed.
func (c *Chunker) retryLoop() {
	defer close(c.done)
	t := time.NewTicker(retryEvery)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.wake:
		case <-t.C:
		}
		now := c.now()
		c.rqmu.Lock()
		var keep, run []retryJob
		for _, j := range c.retryq {
			if now.Before(j.notBefore) {
				keep = append(keep, j)
			} else {
				run = append(run, j)
			}
		}
		c.retryq = keep
		c.rqmu.Unlock()

		for _, j := range run {
			j.attempts++
			var err error
			switch j.stage {
			case "upsert":
				err = c.retryUpsert(j)
			case "put":
				err = c.retryFinalize(j)
			}
			if err != nil {
				c.logf("chunker: retry %s %s/%d (attempt %d): %v", j.stage, j.cam, j.idx, j.attempts, err)
				j.notBefore = now.Add(retryBackoff(j.attempts))
				c.rqmu.Lock()
				if len(c.retryq) < maxRetryQueue {
					c.retryq = append(c.retryq, j)
				}
				c.rqmu.Unlock()
			}
		}
	}
}

// retryFinalize re-runs a failed finalize — reloading the window from
// scratch/R2 if it has been GC'd from memory.
func (c *Chunker) retryFinalize(j retryJob) error {
	c.mu.Lock()
	cs := c.cam(j.cam)
	c.mu.Unlock()
	w, err := c.window(cs, j.idx)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.finalized.Store(false)
	if err := c.doFinalize(w); err != nil {
		return err
	}
	w.finalized.Store(true)
	return w.writeSidecarLocked()
}

// retryUpsert rebuilds the catalog row from the manifest in R2 — the row
// is always derivable from durable objects.
func (c *Chunker) retryUpsert(j retryJob) error {
	mr, err := c.blob.Get(c.ctx, c.manifestKey(j.cam, j.idx))
	if err != nil {
		return err
	}
	defer mr.Close()
	var man Manifest
	if err := json.NewDecoder(mr).Decode(&man); err != nil {
		return err
	}
	return c.store.UpsertChunk(c.ctx, store.Chunk{
		CamID:       j.cam,
		WindowStart: time.Unix(man.WindowStart, 0).UTC(),
		WindowEnd:   time.Unix(man.WindowStart+600, 0).UTC(),
		R2Key:       c.mp4Key(j.cam, j.idx),
		ManifestKey: c.manifestKey(j.cam, j.idx),
		Bytes:       man.Bytes,
		DurationS:   man.DurationS,
		SHA256:      man.SHA256,
		DroppedS:    man.DroppedS,
		Suspect:     man.Suspect,
	})
}

func retryBackoff(attempts int) time.Duration {
	d := retryEvery << min(attempts, 5)
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

// ---- Window methods (w.mu held by caller) ----

func (w *Window) openFilesLocked() error {
	f, err := os.OpenFile(w.path+".frags", os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	w.frags = f
	if st, err := f.Stat(); err == nil {
		w.fragsLen = st.Size()
	}
	if init, err := os.ReadFile(w.path + ".init"); err == nil {
		w.initLen = int64(len(init))
	}
	return nil
}

func (w *Window) writeInitLocked(init []byte) {
	if len(init) == 0 {
		return
	}
	if err := os.WriteFile(w.path+".init", init, 0o644); err == nil {
		w.initLen = int64(len(init))
	}
}

// gcLocked removes scratch files after the retention horizon. The R2
// objects + manifest remain the durable truth. Caller holds w.mu AND
// has already unlinked the window from its camState.
func (w *Window) gcLocked() {
	if w.frags != nil {
		w.frags.Close()
		w.frags = nil
	}
	for _, ext := range []string{".init", ".frags", ".json", ".json.tmp"} {
		os.Remove(w.path + ext)
	}
	w.removed.Store(true)
}

func (w *Window) manifestLocked(sum string, initLen, total int64) Manifest {
	kf := make([]KeyOff, len(w.keyframes))
	for i, k := range w.keyframes {
		kf[i] = KeyOff{TMs: k.tMs, Off: k.off + initLen}
	}
	dur := float64(w.lastEndMs-w.idx*WindowMs) / 1000
	return Manifest{
		Cam:         w.camID,
		WindowStart: w.idx * 600,
		DurationS:   dur,
		Bytes:       total,
		Codec:       w.codec,
		CodecStr:    w.codecStr,
		Width:       w.w,
		Height:      w.h,
		Keyframes:   kf,
		Seqs:        w.seqList(),
		SHA256:      sum,
		DroppedS:    w.droppedS,
		Suspect:     w.suspect,
		LastSeq:     w.lastSeq,
	}
}

// seqList returns sorted "boot:seq" strings — the full dedup set.
func (w *Window) seqList() []string {
	out := make([]string, 0, len(w.seqs))
	for k := range w.seqs {
		out = append(out, k.boot+":"+strconv.FormatUint(k.seq, 10))
	}
	sort.Strings(out)
	return out
}

func parseSeq(s string) (seqKey, bool) {
	i := strings.LastIndex(s, ":")
	if i <= 0 {
		return seqKey{}, false
	}
	n, err := strconv.ParseUint(s[i+1:], 10, 64)
	if err != nil {
		return seqKey{}, false
	}
	return seqKey{s[:i], n}, true
}

func (w *Window) writeSidecarLocked() error {
	sc := sidecar{
		Cam: w.camID, Idx: w.idx, Codec: w.codec, CodecStr: w.codecStr,
		W: w.w, H: w.h,
		Seqs: w.seqList(), LastSeq: w.lastSeq,
		LastEndMs: w.lastEndMs, DroppedS: w.droppedS, Suspect: w.suspect,
		Finalized: w.finalized.Load(),
	}
	kf := make([]KeyOff, len(w.keyframes))
	for i, k := range w.keyframes {
		kf[i] = KeyOff{TMs: k.tMs, Off: k.off} // frags-relative
	}
	sc.Keyframes = kf
	data, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	tmp := w.path + ".json.tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, w.path+".json")
}

func (w *Window) loadSidecarLocked(sc *sidecar) {
	for _, k := range sc.Keyframes {
		w.keyframes = append(w.keyframes, keyOffRel{tMs: k.TMs, off: k.Off})
	}
	for _, s := range sc.Seqs {
		if k, ok := parseSeq(s); ok {
			w.seqs[k] = struct{}{}
		}
	}
	w.lastSeq = sc.LastSeq
	w.lastEndMs = sc.LastEndMs
	w.droppedS = sc.DroppedS
	w.suspect = sc.Suspect
	w.finalized.Store(sc.Finalized)
	w.lastWrite.Store(time.Now().UnixNano())
	if sc.Codec != "" {
		w.codec, w.codecStr, w.w, w.h = sc.Codec, sc.CodecStr, sc.W, sc.H
	}
}

func (w *Window) loadManifestLocked(m *Manifest) {
	initLen := w.initLen
	for _, k := range m.Keyframes {
		w.keyframes = append(w.keyframes, keyOffRel{tMs: k.TMs, off: k.Off - initLen})
	}
	for _, s := range m.Seqs {
		if k, ok := parseSeq(s); ok {
			w.seqs[k] = struct{}{}
		}
	}
	w.lastSeq = m.LastSeq
	w.lastEndMs = w.idx*WindowMs + int64(m.DurationS*1000)
	w.droppedS = m.DroppedS
	w.suspect = m.Suspect
	w.finalized.Store(true)
	w.lastWrite.Store(time.Now().UnixNano())
}

// windowIndex maps pts_ms → window index, floor-dividing for negatives.
func windowIndex(ptsMs int64) int64 {
	idx := ptsMs / WindowMs
	if ptsMs < 0 && ptsMs%WindowMs != 0 {
		idx--
	}
	return idx
}

// splitInit separates the init segment (ftyp+moov, i.e. everything before
// the first moof) from fragment data by walking MP4 box headers.
func splitInit(mp4 []byte) (init, frags []byte, err error) {
	off := 0
	for off+8 <= len(mp4) {
		size := int(binary.BigEndian.Uint32(mp4[off:]))
		typ := string(mp4[off+4 : off+8])
		if size == 1 { // extended u64 size
			if off+16 > len(mp4) {
				break
			}
			size = int(binary.BigEndian.Uint64(mp4[off+8:]))
		} else if size == 0 { // box extends to EOF
			size = len(mp4) - off
		}
		if size < 8 || off+size > len(mp4) {
			return nil, nil, fmt.Errorf("bad box size %d at %d", size, off)
		}
		if typ == "moof" {
			return mp4[:off], mp4[off:], nil
		}
		off += size
	}
	return nil, nil, fmt.Errorf("no moof found (%d bytes scanned)", off)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
