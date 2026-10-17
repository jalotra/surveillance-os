// Package ingest serves the Pi-facing WebSocket at /ingest: tailscale
// whois auth (behind WhoisResolver, dev bypass via INGEST_AUTH=none),
// the hello/hello_ok handshake, per-connection validation, and routing
// into the ring, chunker and catalog.
//
// Validation rules (plans/backend.md §1.1):
//   - pts_ms must be strictly increasing on the live (replay=0) stream
//     within a boot_id — a violation closes the connection and logs a
//     suspect_clock event. Replay frags are exempt (they are by
//     definition in the past) but each stream stays ordered internally.
//   - Absolute clock skew vs hub time NEVER drops data; |now-pts| > 5min
//     on live frags marks the boot suspect → windows persist suspect=true.
//   - ack carries a contiguous watermark; gaps are jumped only when both
//     the live and replay frontiers have passed them (a seq the replay
//     frontier passed is provably absent from the spool — anything else
//     may still be in flight, so we wait rather than trim footage early).
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/sjalotra/surveillance-os/backend/internal/chunker"
	"github.com/sjalotra/surveillance-os/backend/internal/proto"
	"github.com/sjalotra/surveillance-os/backend/internal/ring"
	"github.com/sjalotra/surveillance-os/backend/internal/store"
)

// Tuning constants.
const (
	ackEveryFrags = 20
	ackEvery      = 5 * time.Second
	tickEvery     = 5 * time.Second
	helloTimeout  = 10 * time.Second
	skewSuspectMs = 5 * 60 * 1000 // |now - pts| beyond this → suspect boot
	// gapPatience: how long a pending seq may sit before being declared
	// lost — long enough that a seq legitimately queued in the Pi's spool
	// is never trimmed early, short enough that permanent gaps don't
	// stall the ack watermark (and thus spool trimming) forever.
	gapPatience = 60 * time.Second
)

// WhoisResolver resolves a remote ip:port to a tailnet node name.
// Implemented by LocalAPI (tailscaled unix socket); faked in tests and
// bypassed entirely when INGEST_AUTH=none.
type WhoisResolver interface {
	WhoIs(ctx context.Context, addr string) (nodeName string, err error)
}

// LocalAPI talks to tailscaled's LocalAPI over its unix socket — the same
// endpoint tailscale.com/client/tailscale WhoIs wraps, without the dep.
type LocalAPI struct {
	// SockPath defaults to /run/tailscale/tailscaled.sock.
	SockPath string
}

func (l LocalAPI) client() *http.Client {
	sock := l.SockPath
	if sock == "" {
		sock = "/run/tailscale/tailscaled.sock"
	}
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
		Timeout: 5 * time.Second,
	}
}

// WhoIs returns the tailnet node name owning addr ("ip" or "ip:port").
func (l LocalAPI) WhoIs(ctx context.Context, addr string) (string, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // bare IP
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://local-tailscaled.sock/localapi/v0/whois?addr="+host+":0", nil)
	if err != nil {
		return "", err
	}
	resp, err := l.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("localapi whois: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("localapi whois: status %d", resp.StatusCode)
	}
	var body struct {
		Node struct {
			Name string `json:"Name"`
		} `json:"Node"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Node.Name == "" {
		return "", fmt.Errorf("localapi whois: no node for %s", addr)
	}
	return strings.TrimSuffix(body.Node.Name, "."), nil
}

// Cam is a camera's hub-side runtime state. Fields under mu are written
// by the owner goroutine and read by api/alerts handlers.
type Cam struct {
	ID          string
	Site        string
	TailnetName string
	Ring        *ring.Ring

	mu           sync.Mutex
	online       bool
	conn         *conn // active owner; replaced on reconnect-steal
	bootID       string
	hello        proto.Hello
	lastFragAt   time.Time
	lastSeqAcked uint64 // contiguous watermark, survives reconnects
	haveAck      bool
	stat         proto.Stat
	haveStat     bool
	thumb        []byte
	thumbAt      time.Time
	suspect      bool // current boot flagged clock-skewed
	droppedSTot  float64
	registeredAt time.Time
}

// Snapshot is an unlocked read of cam state for API/status responses.
type Snapshot struct {
	ID           string
	Site         string
	Online       bool
	BootID       string
	LastFragAt   time.Time
	LastSeqAcked uint64
	Stat         proto.Stat
	HaveStat     bool
	Suspect      bool
	DroppedSTot  float64
	Hello        proto.Hello
}

func (c *Cam) Snap() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Snapshot{
		ID: c.ID, Site: c.Site, Online: c.online, BootID: c.bootID,
		LastFragAt: c.lastFragAt, LastSeqAcked: c.lastSeqAcked,
		Stat: c.stat, HaveStat: c.haveStat, Suspect: c.suspect,
		DroppedSTot: c.droppedSTot, Hello: c.hello,
	}
}

// Thumb returns the latest JPEG snapshot.
func (c *Cam) Thumb() ([]byte, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.thumb, c.thumbAt
}

// RegisteredAt is when the camera was added to the registry — used to
// bound "online but never produced" silence checks.
func (c *Cam) RegisteredAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.registeredAt
}

// BroadcastGap notifies live viewers of dropped footage.
func (c *Cam) BroadcastGap(droppedS float64) {
	wire, err := proto.Encode(proto.Gap{Type: proto.TypeGap, DroppedS: droppedS}, nil)
	if err == nil {
		c.Ring.Broadcast(wire)
	}
}

// Registry tracks all cameras, keyed by id and tailnet name.
type Registry struct {
	mu        sync.RWMutex
	byID      map[string]*Cam
	byTailnet map[string]*Cam
	newRing   func() *ring.Ring
}

// NewRegistry builds a Registry; newRing creates each camera's ring.
func NewRegistry(newRing func() *ring.Ring) *Registry {
	return &Registry{
		byID:      map[string]*Cam{},
		byTailnet: map[string]*Cam{},
		newRing:   newRing,
	}
}

// Add registers (or refreshes) a camera.
func (r *Registry) Add(id, site, tailnetName string) *Cam {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.byID[id]
	if c == nil {
		c = &Cam{ID: id, Ring: r.newRing(), registeredAt: time.Now()}
		r.byID[id] = c
	}
	c.Site = site
	c.TailnetName = tailnetName
	if tailnetName != "" {
		r.byTailnet[tailnetName] = c
	}
	return c
}

// Get returns a camera by id.
func (r *Registry) Get(id string) *Cam {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byID[id]
}

// ByTailnet returns the camera bound to a tailnet node name.
func (r *Registry) ByTailnet(nodeName string) *Cam {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byTailnet[nodeName]
}

// All returns a snapshot slice of registered cameras.
func (r *Registry) All() []*Cam {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Cam, 0, len(r.byID))
	for _, c := range r.byID {
		out = append(out, c)
	}
	return out
}

// Session is the per-connection validation state.
type session struct {
	cam      *Cam
	boot     string
	nextWant uint64 // watermark+1: next contiguous seq expected
	seen     map[uint64]struct{}

	lastLiveSeq   uint64
	lastReplaySeq uint64
	lastReplayAt  time.Time
	haveReplay    bool
	stalledAt     time.Time // when the watermark last made progress

	lastLivePts int64
	haveLivePts bool

	fragsSinceAck int
	skewMs        int64 // measured hub−pi clock offset on live frags
	suspect       bool
	gapEvents     []gapRange // declared-lost seq ranges (for tests/events)
}

type gapRange struct{ from, to uint64 }

// Server wires the ingest endpoint.
type Server struct {
	Reg      *Registry
	Store    store.Store
	Chunker  *chunker.Chunker
	Whois    WhoisResolver
	AuthNone bool // INGEST_AUTH=none → skip whois (dev only)
	Logf     func(string, ...any)
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// HandleWS upgrades, authenticates, handshakes, then runs the owner loop.
// Mounted at /ingest — bind this listener to the tailnet interface only.
func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The tailnet is the security boundary; OriginCheck is a browser
		// concern and Pis are not browsers.
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.logf("ingest: accept: %v", err)
		return
	}
	c.SetReadLimit(proto.MaxHeader + proto.MaxPayload + 64)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// --- auth + hello ---
	cam, hello, err := s.authAndHello(ctx, c, r)
	if err != nil {
		s.logf("ingest: %v", err)
		c.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}

	// --- owner registration (new connection steals from any zombie) ---
	co := &conn{ws: c}
	cam.mu.Lock()
	if cam.conn != nil {
		s.logf("ingest: %s reconnect; stealing from old conn", cam.ID)
		cam.conn.ws.Close(websocket.StatusGoingAway, "replaced by new connection")
	}
	// resume_from_seq is the last acked seq *for this boot* — a new boot_id
	// restarts the seq space, so an old boot's watermark must not carry.
	sameBoot := cam.haveAck && cam.bootID == hello.BootID
	cam.conn = co
	cam.online = true
	cam.bootID = hello.BootID
	cam.hello = *hello
	cam.suspect = false
	resume := uint64(0)
	if sameBoot {
		resume = cam.lastSeqAcked
	}
	cam.mu.Unlock()

	s.Chunker.SetFormat(cam.ID, hello.Codec, hello.CodecStr, hello.W, hello.H)
	s.Chunker.SetSuspect(cam.ID, false)
	s.event(cam.ID, store.EventOnline, map[string]any{"fw": hello.FW, "boot_id": hello.BootID})
	s.setStatus(cam, "online", true, hello.FW)

	if err := co.send(proto.HelloOK{Type: proto.TypeHelloOK, ResumeFromSeq: resume}); err != nil {
		s.logf("ingest: %s hello_ok: %v", cam.ID, err)
		return
	}

	// --- owner loop: read → validate → ring + chunker ---
	sess := &session{
		cam: cam, boot: hello.BootID,
		nextWant: wantAfterResume(sameBoot, resume, hello.FirstSeq),
		seen:     map[uint64]struct{}{},
	}
	s.run(ctx, co, sess)

	// --- teardown ---
	cam.mu.Lock()
	if cam.conn == co {
		cam.conn = nil
		cam.online = false
	}
	cam.mu.Unlock()
	s.Chunker.Flush(cam.ID) // quiet-finalize everything still open
	s.setStatus(cam, "offline", false, "")
	s.event(cam.ID, store.EventOffline, nil)
	c.Close(websocket.StatusNormalClosure, "bye")
}

// authAndHello resolves the peer identity and validates the hello.
func (s *Server) authAndHello(ctx context.Context, c *websocket.Conn, r *http.Request) (*Cam, *proto.Hello, error) {
	var nodeName string
	if !s.AuthNone {
		var err error
		nodeName, err = s.Whois.WhoIs(ctx, r.RemoteAddr)
		if err != nil {
			return nil, nil, fmt.Errorf("whois %s: %w", r.RemoteAddr, err)
		}
	}

	hctx, hcancel := context.WithTimeout(ctx, helloTimeout)
	mt, frame, err := c.Read(hctx)
	hcancel()
	if err != nil {
		return nil, nil, fmt.Errorf("read hello: %w", err)
	}
	if mt != websocket.MessageBinary {
		return nil, nil, fmt.Errorf("hello must be binary")
	}
	m, err := proto.DecodeOne(frame)
	if err != nil {
		return nil, nil, fmt.Errorf("bad hello envelope: %w", err)
	}
	if m.Type != proto.TypeHello {
		return nil, nil, fmt.Errorf("first message must be hello, got %q", m.Type)
	}
	var hello proto.Hello
	if err := m.Decode(&hello); err != nil {
		return nil, nil, fmt.Errorf("bad hello header: %w", err)
	}
	if hello.Cam == "" || hello.BootID == "" {
		return nil, nil, fmt.Errorf("hello missing cam/boot_id")
	}

	if s.AuthNone {
		// Dev mode: trust the claimed cam id; auto-register unknown cams.
		cam := s.Reg.Get(hello.Cam)
		if cam == nil {
			cam = s.Reg.Add(hello.Cam, "dev", "")
			s.logf("ingest: dev-registered camera %s", hello.Cam)
		}
		return cam, &hello, nil
	}

	cam := s.Reg.ByTailnet(nodeName)
	if cam == nil {
		return nil, nil, fmt.Errorf("tailnet node %q not a registered camera", nodeName)
	}
	if cam.ID != hello.Cam {
		return nil, nil, fmt.Errorf("hello cam %q does not match node %q (camera %q)",
			hello.Cam, nodeName, cam.ID)
	}
	return cam, &hello, nil
}

// run is the owner loop: message handling + housekeeping on a ticker.
//
// Reads happen on a dedicated goroutine feeding a channel — coder/websocket
// treats a read context's expiry as connection-fatal (it closes the
// socket), so timeouts cannot be used to pace housekeeping. Cap 1 gives
// real backpressure: a slow owner stalls the reader, TCP windows fill,
// and the Pi's writes block instead of unbounded memory growth.
func (s *Server) run(ctx context.Context, co *conn, sess *session) {
	type readResult struct {
		mt    websocket.MessageType
		frame []byte
		err   error
	}
	frames := make(chan readResult, 1)
	go func() {
		for {
			mt, frame, err := co.ws.Read(ctx)
			select {
			case frames <- readResult{mt, frame, err}:
				if err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	lastAck := time.Now()
	tick := time.NewTicker(tickEvery)
	defer tick.Stop()

	// housekeep runs after every message and every tick: watermark
	// advancement, gap emission, and the 20-frags/5s ack cadence.
	// Returns false when the connection must close.
	housekeep := func(now time.Time) bool {
		sess.advance(now) // dormant gaps resolve even while the stream is quiet
		s.drainGaps(sess)
		if sess.fragsSinceAck >= ackEveryFrags || now.Sub(lastAck) >= ackEvery {
			if err := s.ack(co, sess); err != nil {
				s.logf("ingest: %s ack send: %v — closing", sess.cam.ID, err)
				return false
			}
			lastAck = now
		}
		return true
	}

	for {
		select {
		case <-ctx.Done():
			return
		case rr := <-frames:
			if rr.err != nil {
				if websocket.CloseStatus(rr.err) != websocket.StatusNormalClosure &&
					!errors.Is(rr.err, context.Canceled) {
					s.logf("ingest: %s read: %v", sess.cam.ID, rr.err)
				}
				return
			}
			if rr.mt == websocket.MessageBinary {
				m, err := proto.DecodeOne(rr.frame)
				if err != nil {
					s.logf("ingest: %s bad envelope: %v", sess.cam.ID, err)
					return
				}
				if err := s.handle(co, sess, m); err != nil {
					s.logf("ingest: %s: %v", sess.cam.ID, err)
					return // validation violation — close
				}
			}
			if !housekeep(time.Now()) {
				return
			}
		case <-tick.C:
			now := time.Now()
			if !housekeep(now) {
				return
			}
			s.Chunker.Tick(sess.cam.ID)
			co.ping(ctx)
		}
	}
}

// drainGaps emits events/gap-rows/viewer-notices for seq ranges the
// watermark has declared permanently lost.
func (s *Server) drainGaps(sess *session) {
	if len(sess.gapEvents) == 0 {
		return
	}
	ranges := sess.gapEvents
	sess.gapEvents = nil
	cam := sess.cam
	var lost float64
	for _, g := range ranges {
		lost += float64(g.to - g.from + 1)
		s.event(cam.ID, store.EventGap, map[string]any{"from_seq": g.from, "to_seq": g.to, "reason": "seq_gap"})
	}
	now := time.Now()
	s.gap(cam.ID, now.Add(-time.Duration(lost)*time.Second), &now, "spool_overflow")
	cam.BroadcastGap(lost) // ~1s per fragment
}

// handle routes one decoded message. Returning an error closes the conn.
func (s *Server) handle(co *conn, sess *session, m *proto.Message) error {
	cam := sess.cam
	switch m.Type {
	case proto.TypeInit:
		cam.Ring.SetInit(m.Payload)
		s.Chunker.SetInit(cam.ID, m.Payload)

	case proto.TypeFrag:
		f, err := proto.FragFromMessage(m)
		if err != nil {
			return fmt.Errorf("bad frag header: %w", err)
		}
		skip, err := sess.validate(f, time.Now())
		if err != nil {
			s.event(cam.ID, store.EventSuspectClock, map[string]any{"err": err.Error(), "seq": f.Seq})
			return err // close connection on violation
		}
		if susp, skew := sess.suspectBoot(); susp {
			cam.mu.Lock()
			if !cam.suspect {
				cam.suspect = true
				cam.mu.Unlock()
				s.Chunker.SetSuspect(cam.ID, true)
				s.event(cam.ID, store.EventSuspectClock, map[string]any{"skew_ms": skew, "boot_id": sess.boot})
			} else {
				cam.mu.Unlock()
			}
		}
		// Declared-lost seq ranges → gap events + viewer notification.
		s.drainGaps(sess)
		if skip {
			return nil // resend of already-written frag — ack covers it
		}
		// Replay frags go to the chunker only: broadcasting hours-old,
		// out-of-order footage into the live ring would glitch viewers'
		// MSE buffers and pollute the 5-min hot window.
		if !f.Replay {
			cam.Ring.Append(f)
		}
		if err := s.Chunker.Feed(cam.ID, sess.boot, f); err != nil {
			s.logf("ingest: %s chunker feed: %v", cam.ID, err)
		}
		sess.fragsSinceAck++
		cam.mu.Lock()
		cam.lastFragAt = time.Now()
		cam.mu.Unlock()

	case proto.TypeThumb:
		cam.mu.Lock()
		cam.thumb = append([]byte(nil), m.Payload...) // no backing reuse: readers hold slices
		cam.thumbAt = time.Now()
		cam.mu.Unlock()

	case proto.TypeStat:
		var st proto.Stat
		if err := m.Decode(&st); err != nil {
			return fmt.Errorf("bad stat: %w", err)
		}
		cam.mu.Lock()
		delta := st.DroppedSTotal - cam.droppedSTot
		cam.stat = st
		cam.haveStat = true
		cam.droppedSTot = st.DroppedSTotal
		cam.mu.Unlock()
		if delta > 0 {
			cam.BroadcastGap(delta)
			s.Chunker.AddDropped(cam.ID, delta)
			now := time.Now()
			s.event(cam.ID, store.EventGap, map[string]any{"dropped_s": delta, "total": st.DroppedSTotal})
			s.gap(cam.ID, now.Add(-time.Duration(delta)*time.Second), &now, "spool_overflow")
		}

	default:
		s.logf("ingest: %s unexpected message type %q", cam.ID, m.Type)
	}
	return nil
}

// wantAfterResume computes the session's starting watermark+1. On a fresh
// boot the seq space restarts at first_seq — seqs below it can never
// exist, and any older boot's watermark is meaningless.
func wantAfterResume(sameBoot bool, resume, firstSeq uint64) uint64 {
	if !sameBoot {
		if firstSeq > 1 {
			return firstSeq
		}
		return 1
	}
	return resume + 1
}

// ack pushes the contiguous watermark to the Pi.
func (s *Server) ack(co *conn, sess *session) error {
	wm := sess.nextWant - 1
	if sess.nextWant == 0 {
		return nil
	}
	if err := co.send(proto.Ack{Type: proto.TypeAck, LastSeq: wm}); err != nil {
		return err
	}
	sess.fragsSinceAck = 0
	sess.cam.mu.Lock()
	sess.cam.lastSeqAcked = wm
	sess.cam.haveAck = true
	sess.cam.mu.Unlock()
	return nil
}

// validate checks one fragment and updates watermark state. Returns
// skip=true for duplicates/resends, or a fatal error on violation.
func (sess *session) validate(f proto.Fragment, now time.Time) (skip bool, err error) {
	if f.Replay {
		if f.Seq > sess.lastReplaySeq {
			sess.lastReplaySeq = f.Seq
		}
		sess.haveReplay = true
		sess.lastReplayAt = now
		sess.stalledAt = now // replay progress is watermark progress
	} else if f.Seq > sess.lastLiveSeq {
		sess.lastLiveSeq = f.Seq
	}

	// Dedupe before any validation — a resent frag must never trip the
	// monotonic check (the chunker dedupes again by boot+seq anyway).
	if _, ok := sess.seen[f.Seq]; ok || f.Seq < sess.nextWant {
		return true, nil
	}

	if !f.Replay {
		// Monotonic pts on the live stream only.
		if sess.haveLivePts && f.PtsMs <= sess.lastLivePts {
			return false, fmt.Errorf("pts_ms not monotonic in boot %s: %d after %d (seq %d)",
				sess.boot, f.PtsMs, sess.lastLivePts, f.Seq)
		}
		sess.lastLivePts = f.PtsMs
		sess.haveLivePts = true
		// Clock skew: never drop, only mark suspect.
		skew := now.UnixMilli() - f.PtsMs
		if !sess.suspect && (skew > skewSuspectMs || skew < -skewSuspectMs) {
			sess.suspect = true
			sess.skewMs = skew
		}
	}

	sess.seen[f.Seq] = struct{}{}
	sess.advance(now)
	return false, nil
}

// advance raises nextWant over seen seqs and over provably-lost gap seqs.
//
// The seq space is two ordered streams: the live stream (replay=0) and
// the spool-replay stream (replay=1) which continuously chases the ack
// watermark. A pending seq is declared lost when:
//  1. the replay frontier has passed it (provably absent from the spool —
//     the reader sends every un-acked seq in order), or
//  2. the live stream passed it AND the watermark has been stalled for
//     >gapPatience AND the replay stream is dormant — meaning the seq is
//     not sitting in the spool waiting its turn.
//
// Declaring loss early would tell the Pi to trim footage it still holds.
func (sess *session) advance(now time.Time) {
	for {
		if _, ok := sess.seen[sess.nextWant]; ok {
			delete(sess.seen, sess.nextWant)
			sess.nextWant++
			sess.stalledAt = now
			continue
		}
		if sess.stalledAt.IsZero() {
			sess.stalledAt = now
		}
		if !sess.lost(sess.nextWant, now) {
			return
		}
		from := sess.nextWant
		for sess.lost(sess.nextWant, now) && !sess.has(sess.nextWant) {
			sess.nextWant++
		}
		sess.gapEvents = append(sess.gapEvents, gapRange{from, sess.nextWant - 1})
		// stalledAt intentionally kept: the rest of the run waited as long.
	}
}

func (sess *session) has(seq uint64) bool {
	_, ok := sess.seen[seq]
	return ok
}

func (sess *session) lost(seq uint64, now time.Time) bool {
	if seq < sess.lastReplaySeq {
		return true // replay frontier passed it — not in the spool
	}
	if seq < sess.lastLiveSeq &&
		now.Sub(sess.stalledAt) > gapPatience &&
		(!sess.haveReplay || now.Sub(sess.lastReplayAt) > gapPatience) {
		return true // ordered live stream passed it; no replay is coming
	}
	return false
}

// suspectBoot reports whether the boot's clock was flagged skewed; the
// owner wires it to chunker.SetSuspect + a suspect_clock event once.
func (sess *session) suspectBoot() (bool, int64) { return sess.suspect, sess.skewMs }

// event is best-effort: catalog writes never block or fail ingest.
func (s *Server) event(camID, kind string, meta any) {
	if s.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Store.InsertEvent(ctx, camID, kind, meta); err != nil {
		s.logf("ingest: event %s/%s: %v", camID, kind, err)
	}
}

func (s *Server) gap(camID string, start time.Time, end *time.Time, reason string) {
	if s.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Store.InsertGap(ctx, camID, start, end, reason); err != nil {
		s.logf("ingest: gap %s: %v", camID, err)
	}
}

func (s *Server) setStatus(cam *Cam, status string, online bool, fw string) {
	if s.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Store.SetCameraStatus(ctx, cam.ID, status, online, fw); err != nil {
		s.logf("ingest: status %s: %v", cam.ID, err)
	}
}

// conn wraps the camera's websocket; writes only ever happen from the
// owner goroutine so no send lock is needed.
type conn struct {
	ws *websocket.Conn
}

func (c *conn) send(header any) error {
	wire, err := proto.Encode(header, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageBinary, wire)
}

func (c *conn) ping(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = c.ws.Ping(pctx)
}
