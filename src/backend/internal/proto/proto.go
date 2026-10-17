// Package proto implements the hub<->Pi wire protocol.
//
// Every message is a single binary WebSocket frame with the envelope:
//
//	[u32 header_len][header JSON][payload bytes]
//
// header_len is big-endian (network order, matching MP4 box convention).
// The header JSON always carries a "type" field; the rest is per-type.
// Field names below are the wire contract — see plans/backend.md §1.1.
package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Message types.
const (
	TypeHello   = "hello"    // pi->hub
	TypeHelloOK = "hello_ok" // hub->pi
	TypeFrag    = "frag"     // pi->hub / hub->viewer
	TypeInit    = "init"     // pi->hub / hub->viewer
	TypeThumb   = "thumb"    // pi->hub
	TypeStat    = "stat"     // pi->hub
	TypeAck     = "ack"      // hub->pi
	TypeCfg     = "cfg"      // hub->pi
	TypeGap     = "gap"      // hub->viewer control message
)

// Framing limits.
const (
	MaxHeader  = 64 << 10   // 64 KiB is far more than any header needs
	MaxPayload = 32 << 20   // 32 MiB covers init segments and pathological fragments
	lenSize    = 4          // u32
	maxLen     = 0xFFFFFFFF // largest u32
)

// Hello is sent by the Pi immediately after connect.
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

// HelloOK is the hub's reply to hello.
type HelloOK struct {
	Type          string `json:"type"`
	ResumeFromSeq uint64 `json:"resume_from_seq"` // hub's last acked seq for this boot
}

// FragHeader rides with each fragment (payload = moof+mdat).
type FragHeader struct {
	Type   string `json:"type"`
	Seq    uint64 `json:"seq"`
	PtsMs  int64  `json:"pts_ms"`
	DurMs  uint32 `json:"dur_ms"`
	Key    int    `json:"key"`    // 0/1
	Replay int    `json:"replay"` // 0/1
}

// ThumbHeader rides with a JPEG snapshot payload.
type ThumbHeader struct {
	Type  string `json:"type"`
	PtsMs int64  `json:"pts_ms"`
}

// Stat is the Pi's periodic health report.
type Stat struct {
	Type          string  `json:"type"`
	UptimeS       int64   `json:"uptime_s"`
	TempC         float64 `json:"temp_c"`
	Throttled     string  `json:"throttled"` // vcgencmd hex string, e.g. "0x0"
	SpoolBytes    int64   `json:"spool_bytes"`
	DroppedSTotal float64 `json:"dropped_s_total"`
	WifiRSSI      int     `json:"wifi_rssi"`
}

// Ack tells the Pi the hub's contiguous seen watermark (spool trim point).
type Ack struct {
	Type    string `json:"type"`
	LastSeq uint64 `json:"last_seq"`
}

// Cfg is a hub->Pi runtime knob message. Live is a *int so "unset"
// (null/omitted) is distinguishable from an explicit pause — a cfg that
// only changes bitrate must not freeze live streaming.
type Cfg struct {
	Type    string `json:"type"`
	Bitrate int    `json:"bitrate"`
	FPS     int    `json:"fps"`
	Live    *int   `json:"live,omitempty"` // 0/1; nil = unchanged
}

// Gap is a hub->viewer control message marking lost footage.
type Gap struct {
	Type     string  `json:"type"`
	DroppedS float64 `json:"dropped_s"`
}

// InitHeader is an empty header for init messages.
type InitHeader struct {
	Type string `json:"type"`
}

// Fragment is the decoded in-memory shape shared (deliberately, ~30 lines)
// with the Pi agent. Data is the moof+mdat payload.
type Fragment struct {
	Seq    uint64
	PtsMs  int64
	DurMs  uint32
	Key    bool
	Replay bool
	Data   []byte
}

// Header returns the wire header for this fragment.
func (f Fragment) Header() FragHeader {
	h := FragHeader{Type: TypeFrag, Seq: f.Seq, PtsMs: f.PtsMs, DurMs: f.DurMs}
	if f.Key {
		h.Key = 1
	}
	if f.Replay {
		h.Replay = 1
	}
	return h
}

// FragFromMessage builds a Fragment from a decoded frag Message.
func FragFromMessage(m *Message) (Fragment, error) {
	var h FragHeader
	if err := m.Decode(&h); err != nil {
		return Fragment{}, err
	}
	return Fragment{
		Seq:    h.Seq,
		PtsMs:  h.PtsMs,
		DurMs:  h.DurMs,
		Key:    h.Key == 1,
		Replay: h.Replay == 1,
		Data:   m.Payload,
	}, nil
}

// Message is a decoded envelope: peeked type, raw header, payload.
type Message struct {
	Type    string
	Header  json.RawMessage
	Payload []byte
}

// Decode unmarshals the message header into v.
func (m *Message) Decode(v any) error {
	return json.Unmarshal(m.Header, v)
}

// Encode serializes header (a struct whose "type" field is set) + payload
// into one envelope frame. The returned slice is safe to share between
// subscribers.
func Encode(header any, payload []byte) ([]byte, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return nil, fmt.Errorf("proto: marshal header: %w", err)
	}
	if len(h) > MaxHeader {
		return nil, fmt.Errorf("proto: header too large: %d", len(h))
	}
	if len(payload) > MaxPayload {
		return nil, fmt.Errorf("proto: payload too large: %d", len(payload))
	}
	buf := make([]byte, lenSize+len(h)+len(payload))
	binary.BigEndian.PutUint32(buf[:lenSize], uint32(len(h)))
	copy(buf[lenSize:], h)
	copy(buf[lenSize+len(h):], payload)
	return buf, nil
}

// ReadMessage reads one envelope from r. r must yield exactly one frame's
// worth of bytes (a WS binary message satisfies this).
func ReadMessage(r io.Reader) (*Message, error) {
	var lb [lenSize]byte
	if _, err := io.ReadFull(r, lb[:]); err != nil {
		return nil, err
	}
	hlen := binary.BigEndian.Uint32(lb[:])
	if hlen == 0 || hlen > MaxHeader {
		return nil, fmt.Errorf("proto: bad header_len %d", hlen)
	}
	hdr := make([]byte, hlen)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, fmt.Errorf("proto: short header: %w", err)
	}
	var peek struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(hdr, &peek); err != nil {
		return nil, fmt.Errorf("proto: bad header JSON: %w", err)
	}
	payload, err := io.ReadAll(io.LimitReader(r, MaxPayload+1))
	if err != nil {
		return nil, fmt.Errorf("proto: payload read: %w", err)
	}
	if len(payload) > MaxPayload {
		return nil, fmt.Errorf("proto: payload too large: %d", len(payload))
	}
	return &Message{Type: peek.Type, Header: hdr, Payload: payload}, nil
}

// DecodeOne is a convenience: encode-side counterpart of ReadMessage for
// buffers already in memory (e.g. a stored wire frame).
func DecodeOne(frame []byte) (*Message, error) {
	m, err := ReadMessage(bytes.NewReader(frame))
	if err != nil {
		return nil, err
	}
	return m, nil
}
