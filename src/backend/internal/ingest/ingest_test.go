package ingest

import (
	"testing"
	"time"

	"github.com/sjalotra/surveillance-os/backend/internal/proto"
)

func newSession() *session {
	return &session{
		boot: "b1", nextWant: 1,
		seen: map[uint64]struct{}{},
	}
}

func frag(seq uint64, pts int64, replay bool) proto.Fragment {
	return proto.Fragment{Seq: seq, PtsMs: pts, DurMs: 1000, Replay: replay}
}

func TestMonotonicLivePts(t *testing.T) {
	s := newSession()
	now := time.Now()
	base := now.UnixMilli()
	if _, err := s.validate(frag(1, base, false), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.validate(frag(2, base+1000, false), now); err != nil {
		t.Fatal(err)
	}
	// Backwards pts on the live stream → fatal violation.
	if _, err := s.validate(frag(3, base-5000, false), now); err == nil {
		t.Fatal("expected monotonic violation")
	}
}

func TestReplayExemptFromMonotonic(t *testing.T) {
	s := newSession()
	now := time.Now()
	base := now.UnixMilli()
	// live establishes a pts frontier
	_, _ = s.validate(frag(100, base, false), now)
	// replay frag far in the past → allowed
	if skip, err := s.validate(frag(50, base-3*3600_000, true), now); err != nil || skip {
		t.Fatalf("replay frag rejected: skip=%v err=%v", skip, err)
	}
}

func TestClockSkewMarksSuspectNeverDrops(t *testing.T) {
	s := newSession()
	now := time.Now()
	// pts 30 minutes behind hub — skewed but kept.
	skip, err := s.validate(frag(1, now.UnixMilli()-30*60_000, false), now)
	if err != nil || skip {
		t.Fatalf("skewed frag dropped: skip=%v err=%v", skip, err)
	}
	if !s.suspect {
		t.Fatal("suspect not set on skewed clock")
	}
	// Future skew also flagged.
	s2 := newSession()
	if _, err := s2.validate(frag(1, now.UnixMilli()+30*60_000, false), now); err != nil {
		t.Fatal(err)
	}
	if !s2.suspect {
		t.Fatal("suspect not set on future-skewed clock")
	}
}

func TestWatermarkContiguous(t *testing.T) {
	s := newSession()
	now := time.Now()
	base := now.UnixMilli()
	for seq := uint64(1); seq <= 10; seq++ {
		s.validate(frag(seq, base+int64(seq)*1000, false), now)
	}
	if s.nextWant != 11 {
		t.Fatalf("nextWant = %d", s.nextWant)
	}
}

func TestGapNotDeclaredLostImmediately(t *testing.T) {
	s := newSession()
	t0 := time.Now()
	base := t0.UnixMilli()
	s.validate(frag(1, base, false), t0)
	s.validate(frag(2, base+1000, false), t0)
	s.validate(frag(4, base+3000, false), t0) // seq 3 pending
	if s.nextWant != 3 {
		t.Fatalf("nextWant = %d, want 3 (gap still pending)", s.nextWant)
	}
	// Still pending shortly after — a spooled 3 must not be trimmed early.
	s.validate(frag(5, base+4000, false), t0.Add(10*time.Second))
	if s.nextWant != 3 {
		t.Fatalf("nextWant = %d, want 3 (premature loss)", s.nextWant)
	}
}

func TestGapDeclaredLostAfterPatience(t *testing.T) {
	s := newSession()
	t0 := time.Now()
	base := t0.UnixMilli()
	s.validate(frag(1, base, false), t0)
	s.validate(frag(2, base+1000, false), t0)
	s.validate(frag(4, base+3000, false), t0)
	// 70 s later, still no 3 and no replay flowing → declared lost.
	s.validate(frag(5, base+4000, false), t0.Add(70*time.Second))
	if s.nextWant != 6 {
		t.Fatalf("nextWant = %d, want 6 (gap jumped)", s.nextWant)
	}
	if len(s.gapEvents) != 1 || s.gapEvents[0].from != 3 || s.gapEvents[0].to != 3 {
		t.Fatalf("gapEvents = %+v", s.gapEvents)
	}
}

func TestGapLostWhenReplayFrontierPasses(t *testing.T) {
	s := newSession()
	t0 := time.Now()
	base := t0.UnixMilli()
	// Live stream is at 100+, replay is working its way up from 1.
	s.validate(frag(100, base, false), t0)
	s.validate(frag(101, base+1000, false), t0)
	for seq := uint64(1); seq <= 50; seq++ {
		s.validate(frag(seq, base-100_000, true), t0)
	}
	if s.nextWant != 51 {
		t.Fatalf("nextWant = %d want 51", s.nextWant)
	}
	// The replay stream skips 51 (spool dropped it) → passing it on the
	// replay frontier declares it lost immediately — no patience wait.
	s.validate(frag(52, base-90_000, true), t0)
	if s.nextWant != 53 {
		t.Fatalf("nextWant = %d want 53 (51 lost below replay frontier)", s.nextWant)
	}
	found := false
	for _, g := range s.gapEvents {
		if g.from <= 51 && g.to >= 51 {
			found = true
		}
	}
	if !found {
		t.Fatalf("seq 51 not declared lost: %+v", s.gapEvents)
	}
}

func TestReplayKeepsWatermarkAdvancing(t *testing.T) {
	s := newSession()
	t0 := time.Now()
	base := t0.UnixMilli()
	// Reconnect: live at 1000, replaying backlog 1.. — pending seqs must
	// resolve via the replay stream, never via the patience path.
	s.validate(frag(1000, base, false), t0)
	for seq := uint64(1); seq <= 30; seq++ {
		s.validate(frag(seq, base-300_000, true), t0.Add(time.Duration(seq)*time.Second))
	}
	if s.nextWant != 31 {
		t.Fatalf("nextWant = %d want 31", s.nextWant)
	}
	if len(s.gapEvents) != 0 {
		t.Fatalf("false loss during replay: %+v", s.gapEvents)
	}
}

func TestDupSeqSkipped(t *testing.T) {
	s := newSession()
	now := time.Now()
	base := now.UnixMilli()
	// Contiguous live stream 1..5 → watermark at 6.
	for seq := uint64(1); seq <= 5; seq++ {
		s.validate(frag(seq, base+int64(seq)*1000, false), now)
	}
	// Resend of an already-seen seq (replay or not) → skip.
	if skip, _ := s.validate(frag(3, base+3000, true), now); !skip {
		t.Fatal("duplicate seq not skipped")
	}
	// Below watermark → skip.
	if skip, _ := s.validate(frag(2, base+2000, true), now); !skip {
		t.Fatal("old seq not skipped")
	}
}

func TestResumeFromSeq(t *testing.T) {
	// After a reconnect, nextWant resumes above the acked watermark.
	s := &session{boot: "b1", nextWant: 41, seen: map[uint64]struct{}{}}
	now := time.Now()
	s.validate(frag(41, now.UnixMilli(), false), now)
	if s.nextWant != 42 {
		t.Fatalf("nextWant = %d", s.nextWant)
	}
}

func TestWantAfterResume(t *testing.T) {
	cases := []struct {
		name          string
		sameBoot      bool
		resume, first uint64
		want          uint64
	}{
		{"fresh cam, first_seq=1", false, 0, 1, 1},
		{"fresh cam, first_seq=500", false, 0, 500, 500},
		{"same-boot reconnect resumes", true, 40, 1, 41},
		{"new boot ignores old watermark", false, 40, 1, 1},
		{"new boot with high first_seq", false, 40, 700, 700},
	}
	for _, tc := range cases {
		if got := wantAfterResume(tc.sameBoot, tc.resume, tc.first); got != tc.want {
			t.Errorf("%s: wantAfterResume = %d, want %d", tc.name, got, tc.want)
		}
	}
}
