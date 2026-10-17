// Package live is the viewer-facing WebSocket at /live/{cam}: it sends
// the init segment, then every fragment from the most recent keyframe
// forward, then live frames as they arrive — the join shape MSE needs to
// start decoding without stalling a full GOP.
//
// Backpressure is owned by the ring: each subscriber's channel holds 4
// frames; a viewer too slow for that is dropped to needs_key and resynced
// at the next keyframe — one slow socket can never stall a camera.
//
// TCP note: coder/websocket does not expose the underlying net.Conn for
// an accepted connection, so TCP_NODELAY can't be set explicitly — but it
// doesn't need to be: Go's net stack enables TCP_NODELAY on all TCP
// connections (accepted and dialed) by default.
package live

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/sjalotra/surveillance-os/backend/internal/ingest"
)

const (
	writeTimeout = 10 * time.Second
	pingEvery    = 30 * time.Second // keeps tunnel/proxy idle timeouts away
)

// Server fans a camera's ring out to browser viewers.
type Server struct {
	Reg  *ingest.Registry
	Logf func(string, ...any)
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// HandleWS serves one viewer. camID comes from the route wildcard.
// Mount behind Cloudflare Tunnel + Access (auth lives there).
func (s *Server) HandleWS(camID string, w http.ResponseWriter, r *http.Request) {
	cam := s.Reg.Get(camID)
	if cam == nil {
		http.Error(w, "unknown camera", http.StatusNotFound)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // auth boundary is Cloudflare Access
	})
	if err != nil {
		s.logf("live: accept: %v", err)
		return
	}
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// coder/websocket needs a Reader to surface pongs and peer closes —
	// viewers never send us anything, so drain and discard. When the
	// client goes away the read fails and we tear down via ctx.
	go func() {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				cancel()
				return
			}
		}
	}()

	init, backlog, sub := cam.Ring.Subscribe()
	defer cam.Ring.Unsubscribe(sub)

	send := func(wire []byte) bool {
		wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
		defer wcancel()
		return c.Write(wctx, websocket.MessageBinary, wire) == nil
	}

	if len(init) > 0 && !send(init) {
		return
	}
	for _, wire := range backlog {
		if !send(wire) {
			return
		}
	}

	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case wire, ok := <-sub.Ch:
			if !ok {
				return // unsubscribed
			}
			if !send(wire) {
				return
			}
		case <-ping.C:
			pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
			err := c.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
