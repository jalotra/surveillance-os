// Package spool persists capture fragments to append-only segment files on
// tmpfs (plans/video-service.md §2.2). It is the always-on consumer of the
// capture path: writes never block on the uplink, and when the spool is over
// cap the oldest segment is deleted and its duration is accumulated into
// dropped_s (shipped in stat, stored on the hub as a gap event).
//
// Layout:
//
//	<spool_dir>/
//	  index            json: {boot_id, write_seg, read_seg, read_off, dropped_s}
//	  seg-000042.spl   [u32 magic 'SPLF'][record]*
//
// Record layout — 32-byte header, then payload, then a 4-byte crc32 trailer:
//
//	off  field
//	0    u32 magic 'SFRG'
//	4    u16 ver (=1)
//	6    u16 flags (bit0=keyframe)
//	8    u64 seq
//	16   i64 pts_ms
//	24   u32 dur_ms
//	28   u32 len           payload length
//	32   payload[len]
//	32+len u32 crc32 (IEEE, over payload only)
//
// All integers are big-endian. The plan's field list totals 36 bytes if the
// crc is inlined in the header, so "fixed 32 B header + payload" is
// implemented as header(32) + payload + crc trailer — all field widths and
// the 32-byte requirement are honored; only the crc's position moves.
//
// Recovery: segments are scanned (headers only) at Open; a torn or corrupt
// tail — e.g. killed mid-write — is truncated. An index whose boot_id does
// not match wipes the spool, since seq numbers are scoped per boot_id.
package spool

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"iter"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"surveillance-os/agent/internal/fmp4"
)

const (
	segMagic       = 0x53504c46 // 'SPLF'
	recMagic       = 0x53465247 // 'SFRG'
	recVer         = 1
	recHeaderSize  = 32
	recTrailerSize = 4
	flagKey        = 1 << 0

	indexName = "index"
	maxRecLen = 64 << 20 // sanity bound on a record payload
)

// indexFile is the on-disk index (§2.2). read_seg/read_off mark the oldest
// retained record (segment deletion is the only trim unit, so read_off is
// always the first record's offset within read_seg).
type indexFile struct {
	BootID   string  `json:"boot_id"`
	WriteSeg uint64  `json:"write_seg"`
	ReadSeg  uint64  `json:"read_seg"`
	ReadOff  int64   `json:"read_off"`
	DroppedS float64 `json:"dropped_s"`
}

// segMeta tracks one segment file.
type segMeta struct {
	id       uint64
	firstSeq uint64 // seq of first record (0 if empty)
	lastSeq  uint64 // seq of last complete record
	durMs    uint64 // sum of dur_ms — becomes dropped_s on eviction
	bytes    int64  // file size
}

// Spool is safe for concurrent use: Write from the capture goroutine,
// ReadFrom/Trim from the uplink goroutines.
type Spool struct {
	dir      string
	bootID   string
	maxBytes int64
	segBytes int64

	mu       sync.Mutex
	segs     []segMeta // ascending id; last is the open write segment
	w        *os.File  // write file for segs[len-1]
	droppedS float64
	scratch  []byte // record encode buffer, reused under mu
}

// Open loads (or creates) the spool in dir. A stored index whose boot_id
// differs — only possible across reboots on non-tmpfs dirs — wipes the
// directory so stale seq numbers never alias the new boot's.
func Open(dir, bootID string, maxBytes, segBytes int64) (*Spool, error) {
	if maxBytes <= 0 || segBytes <= 0 {
		return nil, fmt.Errorf("spool: nonpositive max/seg size")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("spool: mkdir %s: %w", dir, err)
	}
	s := &Spool{dir: dir, bootID: bootID, maxBytes: maxBytes, segBytes: segBytes}

	var idx indexFile
	if data, err := os.ReadFile(filepath.Join(dir, indexName)); err == nil {
		_ = json.Unmarshal(data, &idx)
	}
	if idx.BootID != "" && idx.BootID != bootID {
		log.Printf("spool: boot_id changed (%s → %s), wiping stale segments", idx.BootID, bootID)
		if err := s.wipe(); err != nil {
			return nil, err
		}
		idx = indexFile{}
	}
	s.droppedS = idx.DroppedS

	segs, err := s.scan()
	if err != nil {
		return nil, err
	}
	s.segs = segs

	// Pick the write segment: newest existing, or a fresh id.
	writeID := uint64(1)
	if idx.WriteSeg > 0 {
		writeID = idx.WriteSeg
	}
	if n := len(segs); n > 0 {
		last := segs[n-1]
		if last.id >= writeID {
			writeID = last.id
			if last.bytes >= segBytes {
				writeID = last.id + 1 // roll past the already-full tail
			}
		}
	}
	if n := len(s.segs); n == 0 || s.segs[n-1].id != writeID {
		s.segs = append(s.segs, segMeta{id: writeID, firstSeq: 0})
	}
	if err := s.openWriteSegLocked(); err != nil {
		return nil, err
	}
	if err := s.saveIndexLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// wipe deletes every segment and index (boot_id change).
func (s *Spool) wipe() error {
	names, _ := filepath.Glob(filepath.Join(s.dir, "seg-*.spl"))
	for _, n := range names {
		if err := os.Remove(n); err != nil {
			return fmt.Errorf("spool: wipe %s: %w", n, err)
		}
	}
	return nil
}

// scan reads every seg-*.spl and builds metadata, truncating torn tails.
func (s *Spool) scan() ([]segMeta, error) {
	names, err := filepath.Glob(filepath.Join(s.dir, "seg-*.spl"))
	if err != nil {
		return nil, err
	}
	var ids []uint64
	for _, n := range names {
		var id uint64
		if _, err := fmt.Sscanf(filepath.Base(n), "seg-%d.spl", &id); err == nil {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var segs []segMeta
	for _, id := range ids {
		m, ok, err := scanSeg(filepath.Join(s.dir, segName(id)))
		if err != nil {
			return nil, err
		}
		if !ok { // unreadable/corrupt file: drop it entirely
			log.Printf("spool: dropping unreadable segment %06d", id)
			_ = os.Remove(filepath.Join(s.dir, segName(id)))
			continue
		}
		m.id = id
		segs = append(segs, m)
	}
	return segs, nil
}

// scanSeg validates a segment and truncates any torn tail. ok=false means
// the file is unusable (bad magic).
func scanSeg(path string) (m segMeta, ok bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return m, false, err
	}
	defer f.Close()

	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil || binary.BigEndian.Uint32(magic[:]) != segMagic {
		return m, false, nil
	}
	off := int64(4)
	var hdr [recHeaderSize]byte
	var trailer [recTrailerSize]byte
	for {
		if _, err := f.ReadAt(hdr[:], off); err != nil {
			break // torn tail: fewer than header bytes remain
		}
		plen, ok := validHeader(hdr[:])
		if !ok {
			break
		}
		recEnd := off + recHeaderSize + int64(plen) + recTrailerSize
		if _, err := f.ReadAt(trailer[:], recEnd-recTrailerSize); err != nil {
			break // payload or crc truncated
		}
		seq := binary.BigEndian.Uint64(hdr[8:])
		if m.firstSeq == 0 {
			m.firstSeq = seq
		}
		m.lastSeq = seq
		m.durMs += uint64(binary.BigEndian.Uint32(hdr[24:]))
		off = recEnd
	}
	// Truncate any torn tail — including a magic-only file with junk past
	// offset 4, which would otherwise corrupt every record appended after.
	if st, err := f.Stat(); err == nil && st.Size() != off {
		log.Printf("spool: truncating torn tail of %s at %d", path, off)
		_ = f.Truncate(off)
	}
	m.bytes = off
	return m, true, nil
}

// validHeader checks a record header and returns the payload length.
func validHeader(hdr []byte) (plen uint32, ok bool) {
	if binary.BigEndian.Uint32(hdr[0:]) != recMagic ||
		binary.BigEndian.Uint16(hdr[4:]) != recVer {
		return 0, false
	}
	plen = binary.BigEndian.Uint32(hdr[28:])
	if plen == 0 || plen > maxRecLen {
		return 0, false
	}
	return plen, true
}

func segName(id uint64) string { return fmt.Sprintf("seg-%06d.spl", id) }

// openWriteSegLocked opens the current (last) segment for append,
// creating it with the SPLF magic if new. Caller holds s.mu.
func (s *Spool) openWriteSegLocked() error {
	id := s.segs[len(s.segs)-1].id
	path := filepath.Join(s.dir, segName(id))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("spool: open %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	switch {
	case st.Size() == 0:
		var m [4]byte
		binary.BigEndian.PutUint32(m[:], segMagic)
		if _, err := f.WriteAt(m[:], 0); err != nil {
			f.Close()
			return err
		}
		s.segs[len(s.segs)-1].bytes = 4
	case st.Size() < 4:
		f.Close()
		return fmt.Errorf("spool: %s too small (%d bytes)", path, st.Size())
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return err
	}
	s.w = f
	return nil
}

// Write appends f to the spool, rolling the segment at ~segBytes and
// evicting oldest segments while over cap. Write errors (e.g. tmpfs full)
// are returned but never block the caller's loop — the caller logs and
// keeps capturing.
func (s *Spool) Write(f fmp4.Fragment) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur := &s.segs[len(s.segs)-1]
	if cur.bytes >= s.segBytes {
		if err := s.rotateLocked(); err != nil {
			return err
		}
		cur = &s.segs[len(s.segs)-1]
	}

	if cap(s.scratch) < recHeaderSize+len(f.Data)+recTrailerSize {
		s.scratch = make([]byte, recHeaderSize+len(f.Data)+recTrailerSize)
	}
	rec := s.scratch[:recHeaderSize+len(f.Data)+recTrailerSize]
	binary.BigEndian.PutUint32(rec[0:], recMagic)
	binary.BigEndian.PutUint16(rec[4:], recVer)
	var flags uint16
	if f.Key {
		flags |= flagKey
	}
	binary.BigEndian.PutUint16(rec[6:], flags)
	binary.BigEndian.PutUint64(rec[8:], f.Seq)
	binary.BigEndian.PutUint64(rec[16:], uint64(f.PtsMs))
	binary.BigEndian.PutUint32(rec[24:], f.DurMs)
	binary.BigEndian.PutUint32(rec[28:], uint32(len(f.Data)))
	copy(rec[recHeaderSize:], f.Data)
	binary.BigEndian.PutUint32(rec[recHeaderSize+len(f.Data):], crc32.ChecksumIEEE(f.Data))

	if _, err := s.w.Write(rec); err != nil {
		return fmt.Errorf("spool: write: %w", err)
	}
	if cur.firstSeq == 0 {
		cur.firstSeq = f.Seq
	}
	cur.lastSeq = f.Seq
	cur.durMs += uint64(f.DurMs)
	cur.bytes += int64(len(rec))

	return s.evictLocked()
}

// totalLocked sums segment file sizes. Caller holds s.mu.
func (s *Spool) totalLocked() int64 {
	var n int64
	for _, m := range s.segs {
		n += m.bytes
	}
	return n
}

// rotateLocked closes the write segment and opens a fresh one.
func (s *Spool) rotateLocked() error {
	if err := s.w.Close(); err != nil {
		return err
	}
	s.w = nil
	next := s.segs[len(s.segs)-1].id + 1
	s.segs = append(s.segs, segMeta{id: next})
	if err := s.openWriteSegLocked(); err != nil {
		return err
	}
	return s.saveIndexLocked()
}

// evictLocked deletes oldest segments while over cap, accounting their
// durations as dropped_s. The open write segment is never evicted, so a
// single segment may exceed the cap when maxBytes < segment size.
func (s *Spool) evictLocked() error {
	for s.totalLocked() > s.maxBytes && len(s.segs) > 1 {
		old := s.segs[0]
		if err := os.Remove(filepath.Join(s.dir, segName(old.id))); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("spool: evict %06d: %w", old.id, err)
		}
		s.droppedS += float64(old.durMs) / 1000
		s.segs = s.segs[1:]
		if err := s.saveIndexLocked(); err != nil {
			return err
		}
	}
	return nil
}

// Trim deletes whole segments whose newest record has seq <= lastSeq
// (the hub's cumulative ack point). The open write segment is exempt.
// Returns the number of segments removed.
func (s *Spool) Trim(lastSeq uint64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for len(s.segs) > 1 && s.segs[0].lastSeq != 0 && s.segs[0].lastSeq <= lastSeq {
		old := s.segs[0]
		if err := os.Remove(filepath.Join(s.dir, segName(old.id))); err == nil || os.IsNotExist(err) {
			s.segs = s.segs[1:]
			removed++
		} else {
			break
		}
	}
	if removed > 0 {
		if err := s.saveIndexLocked(); err != nil {
			log.Printf("spool: index save after trim: %v", err)
		}
	}
	return removed
}

// ReadFrom returns an iterator over all retained fragments with seq >= the
// given seq — pass the hub's resume_from_seq+1. If seq is older than the
// oldest retained fragment, iteration starts at the oldest retained (the
// seq jump signals the gap to the hub). Corrupt records end their segment's
// iteration; iteration then continues at the next segment. A segment evicted
// mid-iteration is skipped.
func (s *Spool) ReadFrom(seq uint64) iter.Seq[fmp4.Fragment] {
	s.mu.Lock()
	ids := make([]uint64, len(s.segs))
	for i, m := range s.segs {
		ids[i] = m.id
	}
	s.mu.Unlock()

	return func(yield func(fmp4.Fragment) bool) {
		var hdr [recHeaderSize]byte
		for _, id := range ids {
			f, err := os.Open(filepath.Join(s.dir, segName(id)))
			if err != nil {
				continue // evicted between snapshot and open
			}
			if _, err := f.Seek(4, io.SeekStart); err != nil { // past SPLF magic
				f.Close()
				continue
			}
			for {
				if _, err := io.ReadFull(f, hdr[:]); err != nil {
					break
				}
				plen, ok := validHeader(hdr[:])
				if !ok {
					break
				}
				payload := make([]byte, int(plen)+recTrailerSize)
				if _, err := io.ReadFull(f, payload); err != nil {
					break
				}
				stored := binary.BigEndian.Uint32(payload[plen:])
				if crc32.ChecksumIEEE(payload[:plen]) != stored {
					break // corrupt record; rest of segment is unreachable
				}
				fseq := binary.BigEndian.Uint64(hdr[8:])
				if fseq < seq {
					continue
				}
				frag := fmp4.Fragment{
					Seq:   fseq,
					PtsMs: int64(binary.BigEndian.Uint64(hdr[16:])),
					DurMs: binary.BigEndian.Uint32(hdr[24:]),
					Key:   binary.BigEndian.Uint16(hdr[6:])&flagKey != 0,
					Data:  payload[:plen],
				}
				if !yield(frag) {
					f.Close()
					return
				}
			}
			f.Close()
		}
	}
}

// saveIndexLocked persists the index atomically (tmp+rename). Caller holds mu.
func (s *Spool) saveIndexLocked() error {
	idx := indexFile{
		BootID:   s.bootID,
		WriteSeg: s.segs[len(s.segs)-1].id,
		ReadSeg:  s.segs[0].id,
		ReadOff:  4, // first record sits right after the SPLF magic
		DroppedS: s.droppedS,
	}
	data, err := json.Marshal(&idx)
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, ".index.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("spool: index write: %w", err)
	}
	return os.Rename(tmp, filepath.Join(s.dir, indexName))
}

// Bytes returns total bytes retained.
func (s *Spool) Bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalLocked()
}

// DroppedS returns cumulative seconds of footage evicted over cap.
func (s *Spool) DroppedS() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.droppedS
}

// Latest returns the newest retained seq — used to resume the per-boot seq
// counter across an agent restart (0 if the spool is empty).
func (s *Spool) Latest() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest uint64
	for _, m := range s.segs {
		if m.lastSeq > latest {
			latest = m.lastSeq
		}
	}
	return latest
}

// Oldest returns the oldest retained seq (0 if the spool is empty).
func (s *Spool) Oldest() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.segs {
		if m.firstSeq != 0 {
			return m.firstSeq
		}
	}
	return 0
}

// Close flushes the index and closes the write segment.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	if err := s.saveIndexLocked(); err != nil {
		firstErr = err
	}
	if s.w != nil {
		if err := s.w.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		s.w = nil
	}
	return firstErr
}
