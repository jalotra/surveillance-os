// Package config loads hub configuration from the environment. Nothing
// here reaches out to the network — main decides which backends are real.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// CamSeed is one statically-declared camera (CAMERAS env).
type CamSeed struct {
	ID      string
	Site    string
	Tailnet string // expected whois node name
}

type Config struct {
	// ListenAddr is the single HTTP listener (ingest + live + REST).
	// Bind it to the tailnet interface (e.g. "100.x.y.z:8080") in prod —
	// Cloudflare Tunnel reaches it on the same address.
	ListenAddr string // LISTEN_ADDR, default ":8080"

	// IngestAuth: "tailscale" (LocalAPI whois) or "none" (dev bypass).
	IngestAuth    string // INGEST_AUTH, default "tailscale"
	TailscaleSock string // TAILSCALE_SOCK, default /run/tailscale/tailscaled.sock

	DatabaseURL string // DATABASE_URL (empty → store.Null dev mode)

	R2Endpoint  string // R2_ENDPOINT e.g. https://<acct>.r2.cloudflarestorage.com
	R2Bucket    string // R2_BUCKET
	R2Prefix    string // R2_PREFIX, default "surveillance"
	R2AccessKey string // R2_ACCESS_KEY_ID
	R2SecretKey string // R2_SECRET_ACCESS_KEY

	ScratchDir string // CHUNK_SCRATCH_DIR, default /var/lib/surveillance-hub/chunks

	Cameras []CamSeed // CAMERAS="id:site:tailnet_name;id2:site2:name2;..."

	TelegramToken  string // TELEGRAM_BOT_TOKEN
	TelegramChatID string // TELEGRAM_CHAT_ID

	RingMaxAge   time.Duration // RING_MAX_AGE, default 5m
	RingMaxBytes int64         // RING_MAX_BYTES, default 64MiB

	ChunkQuiet time.Duration // CHUNK_QUIET, default 60s — window finalize silence
}

// FromEnv loads configuration; absent optional values get defaults.
func FromEnv() (Config, error) {
	cfg := Config{
		ListenAddr:     envOr("LISTEN_ADDR", ":8080"),
		IngestAuth:     envOr("INGEST_AUTH", "tailscale"),
		TailscaleSock:  envOr("TAILSCALE_SOCK", "/run/tailscale/tailscaled.sock"),
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		R2Endpoint:     os.Getenv("R2_ENDPOINT"),
		R2Bucket:       envOr("R2_BUCKET", "surveillance"),
		R2Prefix:       envOr("R2_PREFIX", "surveillance"),
		R2AccessKey:    os.Getenv("R2_ACCESS_KEY_ID"),
		R2SecretKey:    os.Getenv("R2_SECRET_ACCESS_KEY"),
		ScratchDir:     envOr("CHUNK_SCRATCH_DIR", "/var/lib/surveillance-hub/chunks"),
		TelegramToken:  os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramChatID: os.Getenv("TELEGRAM_CHAT_ID"),
		RingMaxAge:     5 * time.Minute,
		RingMaxBytes:   64 << 20,
	}
	if v := os.Getenv("RING_MAX_AGE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("RING_MAX_AGE: %w", err)
		}
		cfg.RingMaxAge = d
	}
	if v := os.Getenv("RING_MAX_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return cfg, fmt.Errorf("RING_MAX_BYTES: %w", err)
		}
		cfg.RingMaxBytes = n
	}
	cfg.ChunkQuiet = 60 * time.Second
	if v := os.Getenv("CHUNK_QUIET"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("CHUNK_QUIET: %w", err)
		}
		cfg.ChunkQuiet = d
	}
	switch cfg.IngestAuth {
	case "tailscale", "none":
	default:
		return cfg, fmt.Errorf("INGEST_AUTH must be 'tailscale' or 'none', got %q", cfg.IngestAuth)
	}
	cams, err := parseCameras(os.Getenv("CAMERAS"))
	if err != nil {
		return cfg, err
	}
	cfg.Cameras = cams
	return cfg, nil
}

// R2Configured reports whether R2 credentials are present; if not, the
// hub falls back to blob.Discard so dev still works end-to-end.
func (c Config) R2Configured() bool {
	return c.R2Endpoint != "" && c.R2AccessKey != "" && c.R2SecretKey != ""
}

// parseCameras parses "id:site:tailnet;id2:site2:tailnet2".
func parseCameras(v string) ([]CamSeed, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	var out []CamSeed
	for _, entry := range strings.Split(v, ";") {
		parts := strings.SplitN(entry, ":", 3)
		if len(parts) != 3 || parts[0] == "" {
			return nil, fmt.Errorf("CAMERAS entry %q: want id:site:tailnet_name", entry)
		}
		out = append(out, CamSeed{ID: parts[0], Site: parts[1], Tailnet: parts[2]})
	}
	return out, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
