// Command agent is the surveillance-os Pi node process: it supervises a
// gst-launch-1.0 child (hardware H.264 → fMP4 on stdout), splits the stream
// into fragments, spools every fragment to tmpfs, and pushes live + replayed
// fragments to the hub over a tailnet WebSocket.
//
// Ordering guarantee (plans/video-service.md §2.2): every fragment hits the
// spool BEFORE the uplink sees it, so the uplink may drop/reconnect freely
// without losing footage.
//
// Runtime knobs (set in deploy/agent.service): GOGC=200, GOMEMLIMIT=256MiB,
// GOMAXPROCS=4 — tuned for a Pi 4's 4 GB alongside the tmpfs spool.
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"surveillance-os/agent/internal/config"
	"surveillance-os/agent/internal/fmp4"
	"surveillance-os/agent/internal/gst"
	"surveillance-os/agent/internal/spool"
	"surveillance-os/agent/internal/uplink"
)

// version is stamped into hello.fw; override with -ldflags -X main.version=.
var version = "0.1.0"

func main() {
	cfgPath := flag.String("config", config.DefaultPath, "path to agent.toml")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[agent] ")

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	bootID := readBootID()
	log.Printf("cam=%s boot=%s hub=%s %dx%d@%dfps bitrate=%d software_encode=%v thumbs=%v spool=%s (%dMB)",
		cfg.CamID, bootID, cfg.Hub, cfg.Width, cfg.Height, cfg.FPS, cfg.Bitrate,
		cfg.SoftwareEncode, cfg.Thumbs, cfg.SpoolDir, cfg.SpoolMaxMB)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	started := time.Now()
	sp, err := spool.Open(cfg.SpoolDir, bootID, cfg.SpoolMaxBytes(), cfg.SegMaxBytes())
	if err != nil {
		log.Fatalf("spool: %v", err)
	}
	defer sp.Close()

	up := uplink.New(cfg, sp, bootID, version, started)

	// seq is monotonic per boot_id; resume across agent restarts from the
	// spool's newest record so we never alias seqs the hub already acked.
	var seq atomic.Uint64
	seq.Store(sp.Latest())
	up.NextSeq = func() uint64 { return seq.Load() + 1 }

	upDone := make(chan struct{})
	go func() { up.Run(ctx); close(upDone) }()

	// Hub cfg messages → live flag + pipeline restart params.
	pipelineCh := make(chan *config.Config, 4)
	go applyCfg(ctx, cfg, up, pipelineCh)

	// Supervisor: one callback invocation per gst-launch lifetime.
	err = gst.Run(ctx, cfg, pipelineCh, func(procCtx context.Context, p *gst.Proc) error {
		split := fmp4.NewSplitter(p.Stdout, uint32(cfg.FragDurMs))
		init, err := split.ReadInit()
		if err != nil {
			return fmt.Errorf("read init: %w", err)
		}
		up.SetInit(init)
		log.Printf("pipeline up: init=%dB", len(init))

		if p.Thumbs != nil {
			go thumbLoop(procCtx, p.Thumbs, up)
		}

		var lastWriteErr time.Time
		for {
			f, err := split.Next()
			if err != nil {
				return err
			}
			f.Seq = seq.Add(1)
			// Spool first — the always-on consumer. A write failure
			// (tmpfs full) is logged but never stalls capture.
			if werr := sp.Write(*f); werr != nil && time.Since(lastWriteErr) > 10*time.Second {
				lastWriteErr = time.Now()
				log.Printf("spool write: %v", werr)
			}
			up.Send(*f) // ownership of the pooled buffer transfers
		}
	})
	if err != nil {
		log.Printf("gst supervisor: %v", err)
	}

	// Drain the uplink session, then persist the final index.
	<-upDone
	if err := sp.Close(); err != nil {
		log.Printf("spool close: %v", err)
	}
	log.Printf("bye")
}

// thumbLoop pumps JPEG snapshots from the pipeline's fd-3 branch to the hub.
func thumbLoop(ctx context.Context, r io.Reader, up *uplink.Uplink) {
	tr := gst.NewThumbReader(r)
	for ctx.Err() == nil {
		jpeg, err := tr.Next()
		if err != nil {
			return // pipe closed with the process
		}
		up.SendThumb(time.Now().UnixMilli(), jpeg)
	}
}

// applyCfg applies hub cfg messages: `live` toggles live sending; bitrate/fps
// changes take effect by restarting the pipeline with a new config.
func applyCfg(ctx context.Context, cur *config.Config, up *uplink.Uplink, ch chan<- *config.Config) {
	for {
		select {
		case <-ctx.Done():
			return
		case c := <-up.CfgEvents():
			next := *cur
			restart := false
			if c.Live != nil {
				up.SetLive(*c.Live == 1)
			}
			if c.Bitrate > 0 && c.Bitrate != cur.Bitrate {
				next.Bitrate = c.Bitrate
				restart = true
			}
			if c.FPS > 0 && c.FPS != cur.FPS {
				next.FPS = c.FPS
				restart = true
			}
			if !restart {
				continue
			}
			if err := next.Validate(); err != nil {
				log.Printf("cfg: rejected %v: %v", c, err)
				continue
			}
			cur = &next
			up.SetVideo(next.Width, next.Height, next.FPS)
			select {
			case ch <- cur:
			default:
			}
		}
	}
}

// readBootID scopes the seq counter to this boot (plan §2.1). Falls back to
// a random UUID on non-Linux systems (dev machines) so the agent still runs.
func readBootID() string {
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Fatalf("boot_id: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // v4
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
