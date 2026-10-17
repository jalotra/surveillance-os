// Package uplink is the hub WebSocket client (plans/video-service.md §1.2,
// plans/backend.md §1.1). One long-lived WS to wss://<hub>:8080/ingest
// carrying proto-envelope binary frames.
//
// Connect order: hello → hello_ok{resume_from_seq} → spool replay
// (interleaved ~4 spool : 1 live so live stays fresh) → init once → live
// frags + thumbs + stat every 15 s. Reconnect backs off 1 s → 30 s, checking
// `tailscale status` first so we don't burn dials while the tailnet is down.
//
// THE CAPTURE PATH NEVER BLOCKS ON THE UPLINK. The spool is the always-on
// consumer; liveQ overflows are dropped because the spool replays them on
// reconnect anyway.
package uplink

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"surveillance-os/agent/internal/config"
	"surveillance-os/agent/internal/fmp4"
	"surveillance-os/agent/internal/health"
	"surveillance-os/agent/internal/proto"
	"surveillance-os/agent/internal/spool"
)

const (
	liveQueueLen  = 128 // ~2 min of 1 fps fragments; spool covers drops
	thumbQueueLen = 4
	statInterval  = 15 * time.Second
	pingInterval  = 30 * time.Second
	writeTimeout  = 15 * time.Second
	dialTimeout   = 10 * time.Second
	replayRatio   = 4 // spool frags sent per one interleaved live frag
)

// Uplink owns the hub connection and everything sent over it.
type Uplink struct {
	url   string
	spool *spool.Spool
	hello proto.Hello
	start time.Time

	// NextSeq returns the seq the capture loop will assign to the next
	// fragment — wired by main, read at each handshake for first_seq and
	// the replay upper bound.
	NextSeq func() uint64

	liveQ  chan fmp4.Fragment
	thumbQ chan thumbMsg
	initCh chan []byte
	cfgCh  chan proto.Cfg

	init    atomic.Pointer[[]byte]
	video   atomic.Value // videoDims — refreshed by SetVideo on hub cfg
	liveOn  atomic.Bool
	lastAck atomic.Uint64
	dropped atomic.Uint64 // liveQ overflows (still safe in the spool)

	// Injectable seams for tests / dev machines.
	dial        func(ctx context.Context, url string) (*websocket.Conn, error)
	tailscaleUp func(ctx context.Context) bool
}

type thumbMsg struct {
	ptsMs int64
	data  []byte
}

// videoDims carries the hello's w/h/fps; updated by SetVideo when the hub
// pushes a cfg change that takes effect on pipeline restart.
type videoDims struct {
	w, h, fps int
}

// New builds the uplink; Run dials. hello fields are taken from cfg/boot;
// startedAt is the uptime base for stat.
func New(cfg *config.Config, sp *spool.Spool, bootID, fw string, startedAt time.Time) *Uplink {
	u := &Uplink{
		url:   cfg.IngestURL(),
		spool: sp,
		hello: proto.Hello{
			Type:     proto.TypeHello,
			Cam:      cfg.CamID,
			BootID:   bootID,
			FW:       fw,
			Codec:    "h264",
			CodecStr: "avc1.640028",
			W:        cfg.Width,
			H:        cfg.Height,
			FPS:      cfg.FPS,
		},
		start:  startedAt,
		liveQ:  make(chan fmp4.Fragment, liveQueueLen),
		thumbQ: make(chan thumbMsg, thumbQueueLen),
		initCh: make(chan []byte, 1),
		cfgCh:  make(chan proto.Cfg, 8),
	}
	u.liveOn.Store(true)
	u.video.Store(videoDims{cfg.Width, cfg.Height, cfg.FPS})
	u.dial = realDial
	u.tailscaleUp = tailscaleUp
	return u
}

// SetVideo updates the w/h/fps fields advertised in future hello messages.
func (u *Uplink) SetVideo(w, h, fps int) { u.video.Store(videoDims{w, h, fps}) }

// SetInit stores the current init segment (called at every pipeline start)
// and resends it mid-session if one is live — codec params can change on a
// cfg restart, and the hub must see the matching moov.
func (u *Uplink) SetInit(init []byte) {
	u.init.Store(&init)
	select {
	case u.initCh <- init:
	default:
	}
}

// Send hands a live fragment to the uplink. It never blocks the capture
// path: a full queue drops the fragment (it stays in the spool regardless).
// Ownership transfers: the caller must not touch f afterwards.
func (u *Uplink) Send(f fmp4.Fragment) {
	select {
	case u.liveQ <- f:
	default:
		f.Release()
		if n := u.dropped.Add(1); n%100 == 1 {
			log.Printf("uplink: live queue full, dropped %d fragments total (spooled, will replay)", n)
		}
	}
}

// SendThumb queues a JPEG snapshot; on a full queue the oldest is dropped
// so thumbs stay fresh.
func (u *Uplink) SendThumb(ptsMs int64, jpeg []byte) {
	t := thumbMsg{ptsMs: ptsMs, data: jpeg}
	select {
	case u.thumbQ <- t:
		return
	default:
	}
	select {
	case <-u.thumbQ: // evict oldest
	default:
	}
	select {
	case u.thumbQ <- t:
	default:
	}
}

// SetLive pauses/resumes live frag sending (hub cfg.live). The capture and
// spool paths are unaffected.
func (u *Uplink) SetLive(on bool) { u.liveOn.Store(on) }

// CfgEvents delivers cfg messages from the hub.
func (u *Uplink) CfgEvents() <-chan proto.Cfg { return u.cfgCh }

// LastAck returns the hub's last cumulative acked seq.
func (u *Uplink) LastAck() uint64 { return u.lastAck.Load() }

// Run is the connect/backoff loop. Returns when ctx is done.
func (u *Uplink) Run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for ctx.Err() == nil {
		if !u.tailscaleUp(ctx) {
			log.Printf("uplink: tailnet not up; retry in %s", backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = grow(backoff, maxBackoff)
			continue
		}
		start := time.Now()
		conn, err := u.dial(ctx, u.url)
		if err != nil {
			log.Printf("uplink: dial %s: %v (retry in %s)", u.url, err, backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = grow(backoff, maxBackoff)
			continue
		}
		err = u.session(ctx, conn)
		_ = conn.Close(websocket.StatusNormalClosure, "")
		if ctx.Err() != nil {
			return
		}
		log.Printf("uplink: session ended: %v (retry in %s)", err, backoff)
		if time.Since(start) > 30*time.Second {
			backoff = time.Second // stable session: reset backoff
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = grow(backoff, maxBackoff)
	}
}

// session runs one connected lifetime: hello → replay → init → live.
func (u *Uplink) session(ctx context.Context, conn *websocket.Conn) error {
	conn.SetReadLimit(proto.MaxFrameLen)
	defer u.drainLive()

	// Drop any init resend queued before this session — step 3 always
	// sends the freshest initVal anyway.
	select {
	case <-u.initCh:
	default:
	}

	// 1. hello → hello_ok.
	h := u.hello
	if d, ok := u.video.Load().(videoDims); ok {
		h.W, h.H, h.FPS = d.w, d.h, d.fps
	}
	if u.NextSeq != nil {
		h.FirstSeq = u.NextSeq()
	}
	if err := u.writeFrame(ctx, conn, &h, nil); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	var ok proto.HelloOK
	rctx, rcancel := context.WithTimeout(ctx, dialTimeout)
	typ, data, err := conn.Read(rctx)
	rcancel()
	if err != nil {
		return fmt.Errorf("hello_ok: %w", err)
	}
	if typ != websocket.MessageBinary {
		return fmt.Errorf("hello_ok: unexpected message type %v", typ)
	}
	if _, err := proto.Unmarshal(data, &ok); err != nil {
		return fmt.Errorf("hello_ok: %w", err)
	}
	if ok.Type != proto.TypeHelloOK {
		return fmt.Errorf("expected hello_ok, got %q", ok.Type)
	}
	u.lastAck.Store(ok.ResumeFromSeq)

	// 2. Replay spooled frags above the hub's resume point, interleaving
	// live traffic 4:1 so a long backlog doesn't freeze the live view.
	// hi bounds the replay to this boot generation: anything seq'd after
	// the handshake arrives via liveQ as replay=0.
	hi := uint64(1<<63 - 1)
	if u.NextSeq != nil {
		hi = u.NextSeq()
	}
	n := 0
	for f := range u.spool.ReadFrom(ok.ResumeFromSeq + 1) {
		if f.Seq >= hi {
			break
		}
		if err := u.writeFrag(ctx, conn, f, 1); err != nil {
			return err
		}
		n++
		if n%replayRatio == 0 {
			u.drainOneLive(ctx, conn)
		}
	}

	// 3. init once.
	if ip := u.init.Load(); ip != nil {
		if err := u.writeFrame(ctx, conn, &proto.Init{Type: proto.TypeInit}, *ip); err != nil {
			return fmt.Errorf("init: %w", err)
		}
	}

	// 4. Live loop.
	readErr := make(chan error, 1)
	go u.readPump(ctx, conn, readErr)
	statT := time.NewTicker(statInterval)
	defer statT.Stop()
	pingT := time.NewTicker(pingInterval)
	defer pingT.Stop()
	for {
		select {
		case f := <-u.liveQ:
			if u.liveOn.Load() {
				if err := u.writeFrag(ctx, conn, f, 0); err != nil {
					f.Release()
					return err
				}
			}
			f.Release()
		case t := <-u.thumbQ:
			if err := u.writeFrame(ctx, conn,
				&proto.Thumb{Type: proto.TypeThumb, PtsMs: t.ptsMs}, t.data); err != nil {
				return err
			}
		case init := <-u.initCh: // pipeline restarted mid-session
			if err := u.writeFrame(ctx, conn, &proto.Init{Type: proto.TypeInit}, init); err != nil {
				return err
			}
		case <-statT.C:
			if err := u.writeFrame(ctx, conn, u.stat(), nil); err != nil {
				return err
			}
		case <-pingT.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return fmt.Errorf("ping: %w", err)
			}
		case err := <-readErr:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// writeFrag sends one moof+mdat fragment; replay is 0/1 per spec.
func (u *Uplink) writeFrag(ctx context.Context, conn *websocket.Conn, f fmp4.Fragment, replay int) error {
	key := 0
	if f.Key {
		key = 1
	}
	return u.writeFrame(ctx, conn, &proto.Frag{
		Type:   proto.TypeFrag,
		Seq:    f.Seq,
		PtsMs:  f.PtsMs,
		DurMs:  f.DurMs,
		Key:    key,
		Replay: replay,
	}, f.Data)
}

// drainOneLive emits at most one queued live fragment (replay interleave).
func (u *Uplink) drainOneLive(ctx context.Context, conn *websocket.Conn) {
	select {
	case f := <-u.liveQ:
		if u.liveOn.Load() {
			_ = u.writeFrag(ctx, conn, f, 0)
		}
		f.Release()
	default:
	}
}

// drainLive releases fragments and thumbs queued behind a dead session —
// every frag is in the spool anyway, so dropping here loses nothing.
func (u *Uplink) drainLive() {
	for {
		select {
		case f := <-u.liveQ:
			f.Release()
		case <-u.thumbQ:
		default:
			return
		}
	}
}

// readPump owns conn reads: ack trims the spool, cfg goes to main.
func (u *Uplink) readPump(ctx context.Context, conn *websocket.Conn, errCh chan<- error) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
			return
		}
		typ, err := proto.TypeOf(data)
		if err != nil {
			continue
		}
		switch typ {
		case proto.TypeAck:
			var a proto.Ack
			if _, err := proto.Unmarshal(data, &a); err == nil {
				if a.LastSeq > u.lastAck.Load() {
					u.lastAck.Store(a.LastSeq)
					u.spool.Trim(a.LastSeq)
				}
			}
		case proto.TypeCfg:
			var c proto.Cfg
			if _, err := proto.Unmarshal(data, &c); err == nil {
				select {
				case u.cfgCh <- c:
				default:
				}
			}
		}
	}
}

func (u *Uplink) writeFrame(ctx context.Context, conn *websocket.Conn, header any, payload []byte) error {
	frame, err := proto.Marshal(header, payload)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageBinary, frame)
}

func (u *Uplink) stat() *proto.Stat {
	h := health.Read(context.Background())
	return &proto.Stat{
		Type:          proto.TypeStat,
		UptimeS:       int64(time.Since(u.start).Seconds()),
		TempC:         h.TempC,
		Throttled:     h.Throttled,
		SpoolBytes:    u.spool.Bytes(),
		DroppedSTotal: u.spool.DroppedS(),
		WifiRSSI:      h.WifiRSSI,
	}
}

// realDial dials with the default options: no compression (the payload is
// already-encoded video), 10 s timeout.
func realDial(ctx context.Context, url string) (*websocket.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, url, nil)
	return conn, err
}

// tailscaleUp reports whether the tailnet looks usable. When the tailscale
// binary is absent (dev machine) we can't tell, so we return true and let
// the dial decide; when present we require BackendState Running so we don't
// burn dial attempts while the daemon is down — that's what the spool is for.
func tailscaleUp(ctx context.Context) bool {
	if _, err := exec.LookPath("tailscale"); err != nil {
		return true
	}
	tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(tctx, "tailscale", "status", "--json").Output()
	return strings.Contains(string(out), `"BackendState":"Running"`)
}

func grow(d, max time.Duration) time.Duration {
	d *= 2
	if d > max {
		return max
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
