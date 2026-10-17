// Package proto implements the agent side of the hub wire protocol
// (plans/backend.md §1.1). Every message is one binary WebSocket frame:
//
//	[u32 header_len][header JSON][payload bytes]
//
// header_len is big-endian (network byte order) and counts only the JSON
// header bytes. The payload may be empty. Field names below are wire-locked:
// they must match the hub's decoder byte-for-byte.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

// Sanity bounds. A fragment at 0.7 Mbps is ~87 KB; thumbs a few tens of KB.
const (
	MaxHeaderLen  = 64 << 10
	MaxPayloadLen = 256 << 20
	MaxFrameLen   = MaxHeaderLen + MaxPayloadLen + 4
)

// Message types.
const (
	TypeHello   = "hello"    // pi→hub
	TypeHelloOK = "hello_ok" // hub→pi
	TypeFrag    = "frag"     // pi→hub
	TypeInit    = "init"     // pi→hub
	TypeThumb   = "thumb"    // pi→hub
	TypeStat    = "stat"     // pi→hub
	TypeAck     = "ack"      // hub→pi
	TypeCfg     = "cfg"      // hub→pi
)

// Hello is the first message after WS connect. first_seq is the seq the
// agent will assign to the next live fragment (seqs are monotonic per
// boot_id).
type Hello struct {
	Type     string `json:"type"`
	Cam      string `json:"cam"`
	BootID   string `json:"boot_id"`
	FW       string `json:"fw"`
	Codec    string `json:"codec"`     // "h264"
	CodecStr string `json:"codec_str"` // "avc1.640028"
	W        int    `json:"w"`
	H        int    `json:"h"`
	FPS      int    `json:"fps"`
	FirstSeq uint64 `json:"first_seq"`
}

// HelloOk is the hub's handshake reply. resume_from_seq is the last seq the
// hub acked for this boot_id; the agent replays spooled frags above it.
type HelloOK struct {
	Type          string `json:"type"`
	ResumeFromSeq uint64 `json:"resume_from_seq"`
}

// Frag carries one moof+mdat pair as payload. key/replay are 0/1 per spec.
type Frag struct {
	Type   string `json:"type"`
	Seq    uint64 `json:"seq"`
	PtsMs  int64  `json:"pts_ms"`
	DurMs  uint32 `json:"dur_ms"`
	Key    int    `json:"key"`
	Replay int    `json:"replay"`
}

// Init carries the ftyp+moov init segment as payload.
type Init struct {
	Type string `json:"type"`
}

// Thumb carries a JPEG snapshot as payload.
type Thumb struct {
	Type  string `json:"type"`
	PtsMs int64  `json:"pts_ms"`
}

// Stat is the 15 s health payload (plans/video-service.md §2.3).
type Stat struct {
	Type          string  `json:"type"`
	UptimeS       int64   `json:"uptime_s"`
	TempC         float64 `json:"temp_c"`
	Throttled     string  `json:"throttled"`
	SpoolBytes    int64   `json:"spool_bytes"`
	DroppedSTotal float64 `json:"dropped_s_total"`
	WifiRSSI      int     `json:"wifi_rssi"`
}

// Ack is the hub's cumulative confirmation; the Pi trims the spool at it.
type Ack struct {
	Type    string `json:"type"`
	LastSeq uint64 `json:"last_seq"`
}

// Cfg is a hub→pi control message; zero values mean "leave unchanged".
// Live is a *int so null/omitted ("don't touch live") is distinct from an
// explicit 0 (pause) — otherwise a bitrate-only cfg would freeze live.
type Cfg struct {
	Type    string `json:"type"`
	Bitrate int    `json:"bitrate"` // bps
	FPS     int    `json:"fps"`
	Live    *int   `json:"live,omitempty"` // 0/1 pause/resume; nil = unchanged
}

// Marshal encodes header+payload into one frame. It allocates a fresh slice;
// callers on the hot path reuse the returned buffer via their own pools.
func Marshal(header any, payload []byte) ([]byte, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return nil, fmt.Errorf("proto: marshal header: %w", err)
	}
	return MarshalRaw(h, payload), nil
}

// MarshalRaw is Marshal for an already-encoded JSON header.
func MarshalRaw(headerJSON, payload []byte) []byte {
	frame := make([]byte, 4+len(headerJSON)+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(headerJSON)))
	copy(frame[4:], headerJSON)
	copy(frame[4+len(headerJSON):], payload)
	return frame
}

// splitFrame validates the envelope and returns the header/payload slices.
func splitFrame(frame []byte) (header, payload []byte, err error) {
	if len(frame) < 4 {
		return nil, nil, fmt.Errorf("proto: frame %d bytes, missing header_len", len(frame))
	}
	hl := binary.BigEndian.Uint32(frame)
	if hl > MaxHeaderLen {
		return nil, nil, fmt.Errorf("proto: header_len %d exceeds cap", hl)
	}
	if uint64(len(frame)) < 4+uint64(hl) {
		return nil, nil, fmt.Errorf("proto: frame %d bytes truncated before %d-byte header", len(frame), hl)
	}
	return frame[4 : 4+hl], frame[4+hl:], nil
}

// Unmarshal decodes a frame into header (a *struct) and returns the payload.
func Unmarshal(frame []byte, header any) ([]byte, error) {
	h, payload, err := splitFrame(frame)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(h, header); err != nil {
		return nil, fmt.Errorf("proto: unmarshal header: %w", err)
	}
	return payload, nil
}

// TypeOf returns the header's "type" field without a full decode.
func TypeOf(frame []byte) (string, error) {
	h, _, err := splitFrame(frame)
	if err != nil {
		return "", err
	}
	var p struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(h, &p); err != nil {
		return "", fmt.Errorf("proto: unmarshal type: %w", err)
	}
	return p.Type, nil
}
