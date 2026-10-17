package ring

import (
	"testing"

	"github.com/sjalotra/surveillance-os/backend/internal/proto"
)

func frag(seq uint64, ptsMs int64, key bool) proto.Fragment {
	return proto.Fragment{Seq: seq, PtsMs: ptsMs, DurMs: 1000, Key: key, Data: []byte{byte(seq)}}
}

func TestSubscribeGetsInitAndFromLastKey(t *testing.T) {
	r := New(5*60*1000, 1<<26)
	r.SetInit([]byte("ftypmoov"))

	r.Append(frag(1, 10_000, true))
	r.Append(frag(2, 11_000, false))
	r.Append(frag(3, 12_000, true)) // newest keyframe
	r.Append(frag(4, 13_000, false))
	r.Append(frag(5, 14_000, false))

	init, backlog, s := r.Subscribe()
	defer r.Unsubscribe(s)

	m, err := proto.DecodeOne(init)
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != proto.TypeInit || string(m.Payload) != "ftypmoov" {
		t.Fatalf("bad init wire: %v", m.Type)
	}
	if len(backlog) != 3 { // seqs 3,4,5 — from last keyframe forward
		t.Fatalf("backlog len = %d, want 3", len(backlog))
	}
	first, _ := proto.DecodeOne(backlog[0])
	fh := proto.FragHeader{}
	if err := first.Decode(&fh); err != nil {
		t.Fatal(err)
	}
	if fh.Seq != 3 || fh.Key != 1 {
		t.Fatalf("backlog starts at seq %d key=%d", fh.Seq, fh.Key)
	}
}

func TestLiveAppendReachesSub(t *testing.T) {
	r := New(5*60*1000, 1<<26)
	_, _, s := r.Subscribe()
	defer r.Unsubscribe(s)

	r.Append(frag(1, 1000, true))
	select {
	case wire := <-s.Ch:
		m, _ := proto.DecodeOne(wire)
		if m.Type != proto.TypeFrag {
			t.Fatalf("got %q", m.Type)
		}
	default:
		t.Fatal("nothing received")
	}
}

func TestSlowSubDropsAndResyncsOnKeyframe(t *testing.T) {
	r := New(5*60*1000, 1<<26)
	_, _, s := r.Subscribe()
	defer r.Unsubscribe(s)

	// Fill the sub chan (cap 4) with non-key frags, then overflow it.
	for i := uint64(1); i <= 6; i++ {
		r.Append(frag(i, int64(i*1000), false))
	}
	if !s.needsKey {
		t.Fatal("sub should be marked needsKey after overflow")
	}

	// Non-key frags are withheld while needsKey.
	r.Append(frag(7, 7000, false))
	select {
	case <-s.Ch:
		t.Fatal("stale delta delivered while needsKey")
	default:
	}

	// Next keyframe resyncs: stale backlog dropped, keyframe delivered.
	r.Append(frag(8, 8000, true))
	select {
	case wire := <-s.Ch:
		m, _ := proto.DecodeOne(wire)
		var fh proto.FragHeader
		_ = m.Decode(&fh)
		if fh.Seq != 8 || fh.Key != 1 {
			t.Fatalf("resync delivered seq=%d key=%d", fh.Seq, fh.Key)
		}
	default:
		t.Fatal("keyframe not delivered on resync")
	}
	if s.needsKey {
		t.Fatal("needsKey should clear after resync")
	}

	// Healthy again: subsequent deltas flow.
	r.Append(frag(9, 9000, false))
	select {
	case wire := <-s.Ch:
		m, _ := proto.DecodeOne(wire)
		var fh proto.FragHeader
		_ = m.Decode(&fh)
		if fh.Seq != 9 {
			t.Fatalf("seq=%d", fh.Seq)
		}
	default:
		t.Fatal("post-resync delta missing")
	}
}

func TestSubWithoutKeyframeWaitsDark(t *testing.T) {
	r := New(5*60*1000, 1<<26)
	// Only deltas in the buffer — no keyframe to join from.
	r.Append(frag(1, 1000, false))
	_, backlog, s := r.Subscribe()
	defer r.Unsubscribe(s)
	if len(backlog) != 0 {
		t.Fatalf("backlog without keyframe: %d", len(backlog))
	}
	r.Append(frag(2, 2000, false))
	select {
	case <-s.Ch:
		t.Fatal("delta delivered before any keyframe")
	default:
	}
	r.Append(frag(3, 3000, true))
	select {
	case <-s.Ch:
	default:
		t.Fatal("first keyframe not delivered")
	}
}

func TestTrimDropsOldFragments(t *testing.T) {
	r := New(60_000, 1<<26) // 1 min window
	for i := 0; i < 10; i++ {
		r.Append(frag(uint64(i), int64(i*10_000), i == 0)) // 0..90s, key at 0
	}
	// pts 90_000, window 60s → frags with pts < 30_000 dropped (seqs 0,1,2)
	n, _, _ := r.Stats()
	if n != 7 {
		t.Fatalf("frags = %d, want 7", n)
	}
	// lastKey must track: keyframe seq0 trimmed → lastKey = -1
	if r.lastKey != -1 {
		t.Fatalf("lastKey = %d, want -1", r.lastKey)
	}
	// New keyframe becomes join point again.
	r.Append(frag(100, 95_000, true))
	if r.lastKey != len(r.frags)-1 {
		t.Fatalf("lastKey = %d, want %d", r.lastKey, len(r.frags)-1)
	}
}

func TestByteCapLeavesNewest(t *testing.T) {
	r := New(5*60*1000, 3) // absurdly small cap
	r.Append(frag(1, 1000, true))
	r.Append(frag(2, 2000, false))
	r.Append(frag(3, 3000, false))
	n, _, _ := r.Stats()
	if n != 1 {
		t.Fatalf("frags = %d, want 1 (newest kept)", n)
	}
}

func TestUnsubscribeClosesAndRemoves(t *testing.T) {
	r := New(5*60*1000, 1<<26)
	_, _, s1 := r.Subscribe()
	_, _, s2 := r.Subscribe()
	r.Unsubscribe(s1)
	if _, _, n := r.Stats(); n != 1 {
		t.Fatalf("subs = %d", n)
	}
	if _, ok := <-s1.Ch; ok {
		t.Fatal("sub chan should be closed")
	}
	// Unsubscribe is idempotent.
	r.Unsubscribe(s1)
	r.Unsubscribe(s2)
}

func TestBroadcastControl(t *testing.T) {
	r := New(5*60*1000, 1<<26)
	_, _, s := r.Subscribe()
	defer r.Unsubscribe(s)
	gap, _ := proto.Encode(proto.Gap{Type: proto.TypeGap, DroppedS: 3}, nil)
	r.Broadcast(gap)
	m, _ := proto.DecodeOne(<-s.Ch)
	if m.Type != proto.TypeGap {
		t.Fatalf("got %q", m.Type)
	}
}
