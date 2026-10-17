// hub is the central surveillance server: one Go process that ingests
// camera fragments from Pis over the tailnet, fans live video to
// browsers, writes 10-minute fMP4 chunks to R2, and keeps the catalog in
// Postgres. All state is disposable — PlanetScale + R2 are the truth.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sjalotra/surveillance-os/backend/internal/alerts"
	"github.com/sjalotra/surveillance-os/backend/internal/api"
	"github.com/sjalotra/surveillance-os/backend/internal/blob"
	"github.com/sjalotra/surveillance-os/backend/internal/chunker"
	"github.com/sjalotra/surveillance-os/backend/internal/config"
	"github.com/sjalotra/surveillance-os/backend/internal/ingest"
	"github.com/sjalotra/surveillance-os/backend/internal/live"
	"github.com/sjalotra/surveillance-os/backend/internal/ring"
	"github.com/sjalotra/surveillance-os/backend/internal/store"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ---- Postgres catalog (degraded-ok: never on the ingest hot path) ----
	var st store.Store = store.Null{}
	if cfg.DatabaseURL != "" {
		db, err := store.Connect(ctx, cfg.DatabaseURL)
		if err != nil {
			log.Fatalf("store connect: %v", err)
		}
		pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
		if err := db.Ping(pctx); err != nil {
			log.Printf("WARNING: postgres ping failed (%v) — running degraded; catalog writes retry", err)
		}
		pcancel()
		st = db
		defer db.Close()
	} else {
		log.Printf("DATABASE_URL unset — catalog writes are no-ops (dev mode)")
	}

	// ---- R2 (degraded-ok in dev) ----
	var blobs blob.Client = blob.Discard{}
	if cfg.R2Configured() {
		blobs = blob.NewR2(cfg.R2Endpoint, cfg.R2Bucket, cfg.R2AccessKey, cfg.R2SecretKey)
	} else {
		log.Printf("R2 credentials unset — blob writes are logged-and-dropped (dev mode)")
	}

	// ---- camera registry + per-camera rings ----
	reg := ingest.NewRegistry(func() *ring.Ring {
		return ring.New(cfg.RingMaxAge.Milliseconds(), int(cfg.RingMaxBytes))
	})
	for _, seed := range cfg.Cameras {
		reg.Add(seed.ID, seed.Site, seed.Tailnet)
		cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
		if err := st.UpsertCamera(cctx, store.Camera{
			ID: seed.ID, Site: seed.Site, TailnetName: seed.Tailnet,
		}); err != nil {
			log.Printf("camera seed %s upsert: %v (continuing)", seed.ID, err)
		}
		ccancel()
	}
	if len(cfg.Cameras) == 0 {
		log.Printf("CAMERAS unset — no pre-registered cameras (dev mode auto-registers with INGEST_AUTH=none)")
	}

	// ---- chunker ----
	chk, err := chunker.New(blobs, st, chunker.Options{
		ScratchDir: cfg.ScratchDir,
		KeyPrefix:  cfg.R2Prefix,
		Quiet:      cfg.ChunkQuiet,
	})
	if err != nil {
		log.Fatalf("chunker: %v", err)
	}
	chk.Recover() // finalize windows orphaned by a previous shutdown

	// ---- ingest ----
	var whois ingest.WhoisResolver = ingest.LocalAPI{SockPath: cfg.TailscaleSock}
	authNone := cfg.IngestAuth == "none"
	if authNone {
		log.Printf("INGEST_AUTH=none — DEV MODE, whois checks disabled")
	}
	ing := &ingest.Server{
		Reg: reg, Store: st, Chunker: chk,
		Whois: whois, AuthNone: authNone,
	}

	// ---- live + REST ----
	lv := &live.Server{Reg: reg}
	rest := api.New(reg, st, blobs, cfg.R2Prefix, cfg.ScratchDir)

	// ---- HTTP: one listener; bind to the tailnet interface in prod ----
	mux := http.NewServeMux()
	mux.HandleFunc("/ingest", ing.HandleWS)
	mux.HandleFunc("/live/{cam}", func(w http.ResponseWriter, r *http.Request) {
		lv.HandleWS(r.PathValue("cam"), w, r)
	})
	mux.Handle("/", rest.Handler())

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("hub listening on %s (ingest ws /ingest, live /live/{cam}, api /api/*)", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	// ---- background loops ----
	var sender alerts.Sender = alerts.LogSender{}
	if cfg.TelegramToken != "" && cfg.TelegramChatID != "" {
		sender = alerts.Telegram{Token: cfg.TelegramToken, ChatID: cfg.TelegramChatID}
	}
	go alerts.New(reg, sender, st).Run(ctx)

	// Global chunker sweeper — covers windows left open by dead cams.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				chk.TickAll(nil)
			}
		}
	}()

	// ---- shutdown ----
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down…")
	cancel()

	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	_ = srv.Shutdown(sctx)
	chk.Close() // persist sidecars; Recover finalizes on next start
	log.Printf("bye")
}
