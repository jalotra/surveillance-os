package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func roundTrip(t *testing.T, header any, payload []byte) *Message {
	t.Helper()
	frame, err := Encode(header, payload)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	m, err := ReadMessage(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	return m
}

func TestEnvelopeRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 1000)
	m := roundTrip(t, Hello{
		Type: TypeHello, Cam: "home-a", BootID: "boot-1", FW: "0.1.0",
		Codec: "h264", CodecStr: "avc1.640028", W: 1920, H: 1080, FPS: 10,
		FirstSeq: 0,
	}, payload)

	if m.Type != TypeHello {
		t.Fatalf("type = %q", m.Type)
	}
	var h Hello
	if err := m.Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h.Cam != "home-a" || h.CodecStr != "avc1.640028" || h.W != 1920 || h.FPS != 10 {
		t.Fatalf("bad hello: %+v", h)
	}
	if !bytes.Equal(m.Payload, payload) {
		t.Fatal("payload corrupted")
	}
}

func TestEnvelopeFrameLayout(t *testing.T) {
	payload := []byte{1, 2, 3}
	frame, err := Encode(FragHeader{Type: TypeFrag, Seq: 7, PtsMs: 123, DurMs: 1000, Key: 1}, payload)
	if err != nil {
		t.Fatal(err)
	}
	hlen := binary.BigEndian.Uint32(frame[:4])
	if int(hlen)+4+len(payload) != len(frame) {
		t.Fatalf("frame size mismatch: hlen=%d total=%d", hlen, len(frame))
	}
	hdr := frame[4 : 4+hlen]
	var raw map[string]any
	if err := json.Unmarshal(hdr, &raw); err != nil {
		t.Fatal(err)
	}
	// Wire field names are the contract — assert them literally.
	for _, k := range []string{"type", "seq", "pts_ms", "dur_ms", "key", "replay"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("missing wire field %q in %v", k, raw)
		}
	}
	if raw["key"] != float64(1) || raw["replay"] != float64(0) {
		t.Fatalf("key/replay encoding wrong: %v", raw)
	}
}

func TestFragConversions(t *testing.T) {
	orig := Fragment{Seq: 42, PtsMs: 9999, DurMs: 1000, Key: true, Replay: true, Data: []byte("moofmdat")}
	m := roundTrip(t, orig.Header(), orig.Data)
	f, err := FragFromMessage(m)
	if err != nil {
		t.Fatal(err)
	}
	if f.Seq != 42 || f.PtsMs != 9999 || f.DurMs != 1000 || !f.Key || !f.Replay {
		t.Fatalf("bad frag: %+v", f)
	}
	if string(f.Data) != "moofmdat" {
		t.Fatalf("bad data: %q", f.Data)
	}
}

func TestStatWireShape(t *testing.T) {
	m := roundTrip(t, Stat{
		Type: TypeStat, UptimeS: 1234, TempC: 51.2, Throttled: "0x0",
		SpoolBytes: 1234567, DroppedSTotal: 0, WifiRSSI: -58,
	}, nil)
	var s Stat
	if err := m.Decode(&s); err != nil {
		t.Fatal(err)
	}
	if s.Throttled != "0x0" || s.WifiRSSI != -58 || s.UptimeS != 1234 {
		t.Fatalf("bad stat: %+v", s)
	}
}

func TestRejectsBadFrames(t *testing.T) {
	// zero header_len
	if _, err := ReadMessage(bytes.NewReader([]byte{0, 0, 0, 0})); err == nil {
		t.Fatal("expected error for zero header_len")
	}
	// header_len over cap
	over := make([]byte, 4)
	binary.BigEndian.PutUint32(over, MaxHeader+1)
	if _, err := ReadMessage(bytes.NewReader(over)); err == nil {
		t.Fatal("expected error for oversized header_len")
	}
	// truncated header
	short := make([]byte, 4)
	binary.BigEndian.PutUint32(short, 10)
	if _, err := ReadMessage(bytes.NewReader(short)); err == nil {
		t.Fatal("expected error for truncated header")
	}
	// bad JSON
	bad := append([]byte{}, short...)
	bad = append(bad, []byte("not-json!!")...)
	if _, err := ReadMessage(bytes.NewReader(bad)); err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

func TestPayloadLimit(t *testing.T) {
	big := make([]byte, MaxPayload+1)
	if _, err := Encode(InitHeader{Type: TypeInit}, big); err == nil {
		t.Fatal("expected encode error for oversized payload")
	}
}
