// Package config loads and validates /etc/surveillance/agent.toml
// (plans/video-service.md §1.3). Defaults target a Pi 4 + Camera Module 3
// streaming 1080p10 H.264 at 0.7 Mbps to the hub.
package config

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// DefaultPath is where the systemd unit and install script put the config.
const DefaultPath = "/etc/surveillance/agent.toml"

// Config is the agent's full runtime configuration.
type Config struct {
	// Identity / uplink.
	CamID string `toml:"cam_id"` // must equal cameras.id on the hub
	Hub   string `toml:"hub"`    // "100.x.y.z:8080" or ws(s):// URL

	// Capture.
	Source         string `toml:"source"`           // gst source element + props; default libcamerasrc
	Width          int    `toml:"width"`
	Height         int    `toml:"height"`
	FPS            int    `toml:"fps"`
	Bitrate        int    `toml:"bitrate"`          // bits per second (v4l2 video_bitrate unit)
	GOPSize        int    `toml:"gop_size"`         // frames per GOP; 0 = fps*2 (keyframe ~every 2 s)
	FragDurMs      int    `toml:"frag_duration_ms"` // mp4mux fragment-duration; 0 = 1000
	SoftwareEncode bool   `toml:"software_encode"`  // Pi 5 / no hw encoder: x264enc zerolatency
	Thumbs         bool   `toml:"thumbs"`           // tee → jpegenc snapshot branch on fd 3

	// Spool (plans/video-service.md §2.2).
	SpoolDir   string `toml:"spool_dir"`
	SpoolMaxMB int64  `toml:"spool_max_mb"` // total cap, tmpfs
	SegMaxMB   int64  `toml:"spool_seg_mb"` // per-segment rollover target; 0 = 16
}

// Load reads path and returns a validated Config with defaults applied.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := Defaults()
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return cfg, nil
}

// Defaults returns the out-of-the-box config (plan §1.3 values).
func Defaults() *Config {
	return &Config{
		Source:     "libcamerasrc",
		Width:      1920,
		Height:     1080,
		FPS:        10,
		Bitrate:    700_000,
		FragDurMs:  1000,
		SpoolDir:   "/run/surveillance/spool",
		SpoolMaxMB: 1024,
		SegMaxMB:   16,
		Thumbs:     true,
	}
}

// Validate checks that the config is internally consistent.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.CamID) == "" {
		return fmt.Errorf("cam_id is required (must match cameras.id on the hub)")
	}
	if strings.ContainsAny(c.CamID, "/ \t\n") {
		return fmt.Errorf("cam_id %q must not contain whitespace or '/'", c.CamID)
	}
	if c.Hub == "" {
		return fmt.Errorf("hub is required (hub tailnet host:port)")
	}
	if _, _, err := net.SplitHostPort(c.Hub); err != nil &&
		!strings.HasPrefix(c.Hub, "ws://") && !strings.HasPrefix(c.Hub, "wss://") {
		return fmt.Errorf("hub %q: want host:port or ws(s):// URL: %w", c.Hub, err)
	}
	if c.Source == "" {
		return fmt.Errorf("source is required (gst source element, e.g. libcamerasrc)")
	}
	// '!' is the pipeline separator — allowing it would let one config
	// field inject arbitrary elements into the pipeline.
	if strings.Contains(c.Source, "!") {
		return fmt.Errorf("source %q must not contain '!'", c.Source)
	}
	if c.Width <= 0 || c.Height <= 0 {
		return fmt.Errorf("width/height must be > 0, got %dx%d", c.Width, c.Height)
	}
	if c.FPS <= 0 || c.FPS > 60 {
		return fmt.Errorf("fps must be in [1,60], got %d", c.FPS)
	}
	if c.Bitrate <= 0 {
		return fmt.Errorf("bitrate must be > 0, got %d", c.Bitrate)
	}
	if c.GOPSize < 0 {
		return fmt.Errorf("gop_size must be >= 0, got %d", c.GOPSize)
	}
	if c.FragDurMs < 100 || c.FragDurMs > 10_000 {
		return fmt.Errorf("frag_duration_ms must be in [100,10000], got %d", c.FragDurMs)
	}
	if c.SpoolDir == "" {
		return fmt.Errorf("spool_dir is required")
	}
	if c.SpoolMaxMB <= 0 || c.SegMaxMB <= 0 {
		return fmt.Errorf("spool_max_mb and spool_seg_mb must be > 0")
	}
	if c.SpoolMaxMB < 2*c.SegMaxMB {
		return fmt.Errorf("spool_max_mb (%d) must be >= 2x spool_seg_mb (%d): at least one full segment must fit alongside the open one", c.SpoolMaxMB, c.SegMaxMB)
	}
	return nil
}

// GOP returns the GOP length in frames (keyframe interval).
func (c *Config) GOP() int {
	if c.GOPSize > 0 {
		return c.GOPSize
	}
	return c.FPS * 2
}

// SpoolMaxBytes returns the spool cap in bytes.
func (c *Config) SpoolMaxBytes() int64 { return c.SpoolMaxMB << 20 }

// SegMaxBytes returns the segment rollover target in bytes.
func (c *Config) SegMaxBytes() int64 { return c.SegMaxMB << 20 }

// IngestURL resolves the hub address to a WS URL. Bare "host:port" becomes
// ws://host:port/ingest — the tailnet (WireGuard) already encrypts, and the
// hub binds tailnet-only (plans/backend.md §1.1). Use an explicit wss://
// hub value if TLS termination is ever added in front.
func (c *Config) IngestURL() string {
	if strings.HasPrefix(c.Hub, "ws://") || strings.HasPrefix(c.Hub, "wss://") {
		return strings.TrimSuffix(c.Hub, "/") + "/ingest"
	}
	return "ws://" + c.Hub + "/ingest"
}
