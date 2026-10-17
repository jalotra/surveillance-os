package proto

import (
	"encoding/binary"
	"encoding/json"
	"testing"
)

// marshalToMap marshals a header like Marshal does and returns the JSON
// object — used to pin wire field names against plans/backend.md §1.1.
func marshalToMap(t *testing.T, header any) map[string]any {
	t.Helper()
	h, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(h, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func wantKeys(t *testing.T, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("wire header missing field %q; got %v", k, m)
		}
	}
	if len(m) != len(keys) {
		t.Errorf("header has %d fields, want %d: %v", len(m), len(keys), m)
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	payload := []byte("moof+mdat bytes \x00\x01\x02")
	frame, err := Marshal(&Frag{Type: TypeFrag, Seq: 42, PtsMs: 1700000000000,
		DurMs: 1000, Key: 1, Replay: 0}, payload)
	if err != nil {
		t.Fatal(err)
	}

	// Envelope shape: [u32 BE header_len][header JSON][payload].
	hl := binary.BigEndian.Uint32(frame)
	if int(hl) != len(frame)-4-len(payload) {
		t.Fatalf("header_len %d inconsistent with frame %d payload %d", hl, len(frame), len(payload))
	}

	var got Frag
	gotPayload, err := Unmarshal(frame, &got)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotPayload) != string(payload) {
		t.Fatalf("payload mismatch: %q vs %q", gotPayload, payload)
	}
	if got.Seq != 42 || got.PtsMs != 1700000000000 || got.DurMs != 1000 || got.Key != 1 || got.Replay != 0 {
		t.Fatalf("header mismatch: %+v", got)
	}
	if typ, _ := TypeOf(frame); typ != TypeFrag {
		t.Fatalf("TypeOf = %q, want frag", typ)
	}
}

func TestWireFieldNames(t *testing.T) {
	// These names are the contract with the hub — do not "fix" typos here,
	// fix the hub's decoder too.
	wantKeys(t, marshalToMap(t, Hello{Type: TypeHello, Cam: "home-a", BootID: "b",
		FW: "1.0", Codec: "h264", CodecStr: "avc1.640028", W: 1920, H: 1080, FPS: 10, FirstSeq: 1}),
		"type", "cam", "boot_id", "fw", "codec", "codec_str", "w", "h", "fps", "first_seq")

	wantKeys(t, marshalToMap(t, HelloOK{Type: TypeHelloOK, ResumeFromSeq: 7}),
		"type", "resume_from_seq")

	wantKeys(t, marshalToMap(t, Frag{Type: TypeFrag, Seq: 1, PtsMs: 2, DurMs: 3, Key: 1, Replay: 1}),
		"type", "seq", "pts_ms", "dur_ms", "key", "replay")

	wantKeys(t, marshalToMap(t, Thumb{Type: TypeThumb, PtsMs: 9}),
		"type", "pts_ms")

	wantKeys(t, marshalToMap(t, Stat{Type: TypeStat, UptimeS: 1, TempC: 51.2,
		Throttled: "0x0", SpoolBytes: 9, DroppedSTotal: 0.5, WifiRSSI: -58}),
		"type", "uptime_s", "temp_c", "throttled", "spool_bytes", "dropped_s_total", "wifi_rssi")

	wantKeys(t, marshalToMap(t, Ack{Type: TypeAck, LastSeq: 3}),
		"type", "last_seq")

	one := 1
	wantKeys(t, marshalToMap(t, Cfg{Type: TypeCfg, Bitrate: 1, FPS: 2, Live: &one}),
		"type", "bitrate", "fps", "live")
}

func TestUnmarshalErrors(t *testing.T) {
	var h Frag
	if _, err := Unmarshal([]byte{0, 0}, &h); err == nil {
		t.Fatal("expected error for short frame")
	}
	big := make([]byte, 4)
	binary.BigEndian.PutUint32(big, MaxHeaderLen+1)
	if _, err := Unmarshal(big, &h); err == nil {
		t.Fatal("expected error for oversized header_len")
	}
	// header_len ok but frame truncated before header ends
	trunc := []byte{0, 0, 0, 10, '{'}
	if _, err := Unmarshal(trunc, &h); err == nil {
		t.Fatal("expected error for truncated header")
	}
}

func TestEmptyPayload(t *testing.T) {
	frame, err := Marshal(&Ack{Type: TypeAck, LastSeq: 99}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var a Ack
	payload, err := Unmarshal(frame, &a)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != 0 {
		t.Fatalf("expected empty payload, got %d bytes", len(payload))
	}
	if a.LastSeq != 99 {
		t.Fatalf("last_seq = %d, want 99", a.LastSeq)
	}
}
