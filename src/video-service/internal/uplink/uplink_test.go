package uplink

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"surveillance-os/agent/internal/config"
	"surveillance-os/agent/internal/fmp4"
	"surveillance-os/agent/internal/proto"
	"surveillance-os/agent/internal/spool"
)

type gotMsg struct {
	typ     string
	frag    *proto.Frag
	hello   *proto.Hello
	payload []byte
}

// fakeHub accepts one ingest connection and drives the protocol.
func fakeHub(t *testing.T, resume uint64, seen chan<- gotMsg, ackAfterInit uint64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		read := func() (string, []byte, error) {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return "", nil, err
			}
			typ, err := proto.TypeOf(data)
			if err != nil {
				return "", nil, err
			}
			var payload []byte
			switch typ {
			case proto.TypeFrag:
				var f proto.Frag
				if payload, err = proto.Unmarshal(data, &f); err == nil {
					seen <- gotMsg{typ: typ, frag: &f, payload: payload}
				}
			case proto.TypeHello:
				var h proto.Hello
				proto.Unmarshal(data, &h)
				seen <- gotMsg{typ: typ, hello: &h}
			default:
				proto.Unmarshal(data, &struct{}{})
				_, payload, _ = splitForTest(data)
				seen <- gotMsg{typ: typ, payload: payload}
			}
			return typ, payload, err
		}

		// hello → hello_ok.
		typ, _, err := read()
		if err != nil || typ != proto.TypeHello {
			t.Errorf("want hello first, got %q err %v", typ, err)
			return
		}
		ok, _ := proto.Marshal(&proto.HelloOK{Type: proto.TypeHelloOK, ResumeFromSeq: resume}, nil)
		if err := conn.Write(r.Context(), websocket.MessageBinary, ok); err != nil {
			t.Errorf("write hello_ok: %v", err)
			return
		}

		// Drain whatever the agent sends until the live frag arrives; send
		// ack once init has passed.
		ackSent := false
		for {
			typ, _, err := read()
			if err != nil {
				return // session closed
			}
			if typ == proto.TypeInit && !ackSent && ackAfterInit > 0 {
				ack, _ := proto.Marshal(&proto.Ack{Type: proto.TypeAck, LastSeq: ackAfterInit}, nil)
				conn.Write(r.Context(), websocket.MessageBinary, ack)
				ackSent = true
			}
		}
	}))
}

// splitForTest mirrors proto.splitFrame for the default decode branch.
func splitForTest(frame []byte) ([]byte, []byte, error) {
	var v struct {
		Type string `json:"type"`
	}
	p, err := proto.Unmarshal(frame, &v)
	return nil, p, err
}

func TestSessionHandshakeReplayLive(t *testing.T) {
	dir := t.TempDir()
	sp, err := spool.Open(dir, "boot-1", 16<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	for i := uint64(1); i <= 6; i++ {
		sp.Write(fmp4.Fragment{Seq: i, PtsMs: int64(i * 1000), DurMs: 1000,
			Key: i%2 == 1, Data: []byte(fmt.Sprintf("payload-%d", i))})
	}

	seen := make(chan gotMsg, 32)
	srv := fakeHub(t, 3, seen, 6)
	defer srv.Close()

	cfg := config.Defaults()
	cfg.CamID = "cam-x"
	cfg.Hub = "ws://" + srv.Listener.Addr().String()
	cfg.Thumbs = false

	up := New(cfg, sp, "boot-xyz", "1.2.3", time.Now())
	up.NextSeq = func() uint64 { return 7 }
	up.SetInit([]byte("ftyp+moov"))
	up.tailscaleUp = func(ctx context.Context) bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := realDial(ctx, cfg.IngestURL())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	sessErr := make(chan error, 1)
	go func() { sessErr <- up.session(ctx, conn) }()

	// Expected order: hello, frag(4..6 replay=1), init, then we inject a
	// live frag.
	expectType := func(want string) gotMsg {
		t.Helper()
		select {
		case m := <-seen:
			if m.typ != want {
				t.Fatalf("got msg type %q, want %q", m.typ, want)
			}
			return m
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for %q", want)
		}
		return gotMsg{}
	}

	m := expectType(proto.TypeHello)
	if m.hello.Cam != "cam-x" || m.hello.BootID != "boot-xyz" || m.hello.FW != "1.2.3" ||
		m.hello.Codec != "h264" || m.hello.CodecStr != "avc1.640028" ||
		m.hello.W != 1920 || m.hello.H != 1080 || m.hello.FPS != 10 || m.hello.FirstSeq != 7 {
		t.Fatalf("hello fields wrong: %+v", m.hello)
	}

	for _, seq := range []uint64{4, 5, 6} {
		m := expectType(proto.TypeFrag)
		if m.frag.Seq != seq || m.frag.Replay != 1 {
			t.Fatalf("replay frag = %+v, want seq=%d replay=1", m.frag, seq)
		}
		if string(m.payload) != fmt.Sprintf("payload-%d", seq) {
			t.Fatalf("replay payload = %q", m.payload)
		}
		if m.frag.Key != 1 && seq%2 == 1 {
			t.Fatalf("seq %d should be key", seq)
		}
	}

	m = expectType(proto.TypeInit)
	if string(m.payload) != "ftyp+moov" {
		t.Fatalf("init payload = %q", m.payload)
	}

	// Now a live fragment — the session is in the live loop.
	up.Send(fmp4.Fragment{Seq: 7, PtsMs: 7000, DurMs: 1000, Key: true, Data: []byte("live-7")})
	m = expectType(proto.TypeFrag)
	if m.frag.Seq != 7 || m.frag.Replay != 0 {
		t.Fatalf("live frag = %+v, want seq=7 replay=0", m.frag)
	}
	if m.frag.Key != 1 {
		t.Fatal("live frag should be key=1")
	}

	// The hub acked 6 after init; wait for it to land.
	deadline := time.Now().Add(3 * time.Second)
	for up.LastAck() != 6 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if up.LastAck() != 6 {
		t.Fatalf("LastAck = %d, want 6", up.LastAck())
	}

	cancel()
	if err := <-sessErr; err == nil {
		t.Fatal("session should end on ctx cancel")
	}
	conn.Close(websocket.StatusNormalClosure, "")
}

func TestSendNeverBlocks(t *testing.T) {
	dir := t.TempDir()
	sp, err := spool.Open(dir, "boot-1", 16<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	cfg := config.Defaults()
	cfg.CamID = "cam-x"
	cfg.Hub = "ws://127.0.0.1:1"
	up := New(cfg, sp, "boot", "fw", time.Now())

	// No session is running; liveQ fills then drops — must not block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < liveQueueLen*3; i++ {
			up.Send(fmp4.Fragment{Seq: uint64(i), Data: []byte("x")})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Send blocked with no consumer")
	}
	if d := up.dropped.Load(); d == 0 {
		t.Fatal("expected drops to be counted")
	}
}
