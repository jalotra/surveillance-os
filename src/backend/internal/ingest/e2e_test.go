package ingest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sjalotra/surveillance-os/backend/internal/blob"
	"github.com/sjalotra/surveillance-os/backend/internal/chunker"
	"github.com/sjalotra/surveillance-os/backend/internal/proto"
	"github.com/sjalotra/surveillance-os/backend/internal/ring"
	"github.com/sjalotra/surveillance-os/backend/internal/store"
)

// TestE2EReconnectResume drives a real websocket through HandleWS:
// handshake → 21 frags → ack → reconnect same boot → resume_from_seq →
// deduped resends → next ack. Guards the coder/websocket semantics that
// a read-context expiry is connection-fatal (housekeeping must not pace
// reads with deadlines).
func TestE2EReconnectResume(t *testing.T) {
	reg := NewRegistry(func() *ring.Ring { return ring.New(5*60*1000, 64<<20) })
	chk, err := chunker.New(&fakeBlobT{}, &fakeStoreT{}, chunker.Options{ScratchDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer chk.Close()
	srv := &Server{Reg: reg, Chunker: chk, AuthNone: true, Store: &fakeStoreT{}}
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleWS))
	defer ts.Close()
	url := "ws" + ts.URL[len("http"):]

	// Conn 1: boot b1, send 21 frags, expect ack.
	c1, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	sendT(t, c1, proto.Hello{Type: proto.TypeHello, Cam: "cam1", BootID: "b1", FW: "x", Codec: "h264", CodecStr: "avc1", W: 640, H: 480, FPS: 15, FirstSeq: 1}, nil)
	m := recvT(t, c1)
	var ok proto.HelloOK
	m.Decode(&ok)
	t.Logf("hello_ok1: %+v", ok)

	sendT(t, c1, proto.InitHeader{Type: proto.TypeInit}, []byte("INITINIT"))
	base := time.Now().UnixMilli()
	for seq := uint64(1); seq <= 21; seq++ {
		sendT(t, c1, proto.Fragment{Seq: seq, PtsMs: base + int64(seq)*1000, DurMs: 1000, Key: seq == 1}.Header(), []byte(fmt.Sprintf("frag-%d", seq)))
	}
	m = recvT(t, c1)
	var ack proto.Ack
	m.Decode(&ack)
	t.Logf("ack1: %+v", ack)
	c1.Close(websocket.StatusNormalClosure, "done")

	// Conn 2, SAME boot: expect resume_from_seq=20.
	c2, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	sendT(t, c2, proto.Hello{Type: proto.TypeHello, Cam: "cam1", BootID: "b1", FW: "x", Codec: "h264", CodecStr: "avc1", W: 640, H: 480, FPS: 15, FirstSeq: 1}, nil)
	m = recvT(t, c2)
	m.Decode(&ok)
	t.Logf("hello_ok2: %+v", ok)
	if ok.ResumeFromSeq != 20 {
		t.Fatalf("resume = %d want 20", ok.ResumeFromSeq)
	}
	// Push 21 frags again (1..20 skipped, 21 appended) — conn must NOT die.
	sendT(t, c2, proto.InitHeader{Type: proto.TypeInit}, []byte("INITINIT"))
	for seq := uint64(1); seq <= 21; seq++ {
		sendT(t, c2, proto.Fragment{Seq: seq, PtsMs: base + int64(seq)*1000, DurMs: 1000, Key: seq == 1}.Header(), []byte(fmt.Sprintf("frag-%d", seq)))
	}
	// Within ~6s we should get ack last_seq=21.
	deadline := time.Now().Add(8 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
		_, frame, err := c2.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("conn2 read: %v", err)
		}
		m, err := proto.DecodeOne(frame)
		if err != nil {
			t.Fatal(err)
		}
		if m.Type != proto.TypeAck {
			continue
		}
		m.Decode(&ack)
		t.Logf("ack2: %+v", ack)
		if ack.LastSeq != 21 {
			t.Fatalf("ack2 last_seq = %d want 21", ack.LastSeq)
		}
		break
	}
	c2.Close(websocket.StatusNormalClosure, "done")
}

func sendT(t *testing.T, c *websocket.Conn, h any, payload []byte) {
	t.Helper()
	wire, err := proto.Encode(h, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(context.Background(), websocket.MessageBinary, wire); err != nil {
		t.Fatal(err)
	}
}

func recvT(t *testing.T, c *websocket.Conn) *proto.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, frame, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m, err := proto.DecodeOne(frame)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// fakes (distinct names to avoid colliding with existing test fakes).
type fakeBlobT struct{}

func (fakeBlobT) Put(context.Context, string, io.Reader, int64, string) error { return nil }
func (fakeBlobT) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("none")
}
func (fakeBlobT) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "", fmt.Errorf("none")
}
func (fakeBlobT) List(context.Context, string) ([]blob.Object, error) { return nil, nil }

type fakeStoreT struct{}

func (fakeStoreT) UpsertCamera(context.Context, store.Camera) error    { return nil }
func (fakeStoreT) EnsureCamera(context.Context, store.Camera) error    { return nil }
func (fakeStoreT) ListCameras(context.Context) ([]store.Camera, error) { return nil, nil }
func (fakeStoreT) SetCameraStatus(context.Context, string, string, bool, string) error {
	return nil
}
func (fakeStoreT) UpsertChunk(context.Context, store.Chunk) error { return nil }
func (fakeStoreT) GetChunk(context.Context, string, time.Time) (*store.Chunk, error) {
	return nil, nil
}
func (fakeStoreT) ListChunks(context.Context, string, time.Time, time.Time) ([]store.Chunk, error) {
	return nil, nil
}
func (fakeStoreT) InsertEvent(context.Context, string, string, any) error { return nil }
func (fakeStoreT) ListEvents(context.Context, string, time.Time, time.Time, int) ([]store.Event, error) {
	return nil, nil
}
func (fakeStoreT) InsertGap(context.Context, string, time.Time, *time.Time, string) error {
	return nil
}
func (fakeStoreT) Close() {}
