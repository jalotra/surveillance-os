package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFull(t *testing.T) {
	p := writeCfg(t, `
cam_id   = "home-a"
hub      = "100.64.0.1:8080"
width    = 1280
height   = 720
fps      = 15
bitrate  = 400_000
spool_dir    = "/tmp/spool"
spool_max_mb = 256
thumbs   = false
software_encode = true
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.CamID != "home-a" || c.Hub != "100.64.0.1:8080" {
		t.Fatalf("identity fields wrong: %+v", c)
	}
	if c.Width != 1280 || c.Height != 720 || c.FPS != 15 || c.Bitrate != 400000 {
		t.Fatalf("capture fields wrong: %+v", c)
	}
	if c.Thumbs || !c.SoftwareEncode {
		t.Fatalf("flags wrong: %+v", c)
	}
	if c.SpoolMaxMB != 256 || c.SegMaxMB != 16 {
		t.Fatalf("spool fields wrong: %+v", c)
	}
}

func TestDefaultsApplied(t *testing.T) {
	p := writeCfg(t, `
cam_id = "home-b"
hub    = "100.64.0.1:8080"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Width != 1920 || c.Height != 1080 || c.FPS != 10 || c.Bitrate != 700000 {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if c.SpoolDir != "/run/surveillance/spool" || c.SpoolMaxMB != 1024 || c.SegMaxMB != 16 {
		t.Fatalf("spool defaults wrong: %+v", c)
	}
	if c.FragDurMs != 1000 || !c.Thumbs {
		t.Fatalf("frag/thumbs defaults wrong: %+v", c)
	}
	if g := c.GOP(); g != 20 {
		t.Fatalf("GOP = %d, want fps*2 = 20", g)
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing cam_id", `hub = "h:1"`},
		{"missing hub", `cam_id = "a"`},
		{"bad hub", `cam_id = "a"
hub = "not-a-hostport"`},
		{"zero fps", `cam_id = "a"
hub = "h:1"
fps = 0`},
		{"fps too high", `cam_id = "a"
hub = "h:1"
fps = 61`},
		{"seg bigger than max", `cam_id = "a"
hub = "h:1"
spool_max_mb = 24
spool_seg_mb = 16`},
		{"cam_id with slash", `cam_id = "a/b"
hub = "h:1"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeCfg(t, tc.body)); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	p := writeCfg(t, `
cam_id = "a"
hub    = "h:1"
typo_field = 1
`)
	if _, err := Load(p); err == nil {
		t.Fatal("unknown TOML field should be rejected")
	}
}

func TestIngestURL(t *testing.T) {
	c := Defaults()
	c.CamID = "a"
	c.Hub = "100.64.0.1:8080"
	if got := c.IngestURL(); got != "ws://100.64.0.1:8080/ingest" {
		t.Fatalf("IngestURL = %q", got)
	}
	c.Hub = "wss://example.com:8443"
	if got := c.IngestURL(); got != "wss://example.com:8443/ingest" {
		t.Fatalf("IngestURL = %q", got)
	}
	c.Hub = "ws://10.0.0.1:9000/"
	if got := c.IngestURL(); got != "ws://10.0.0.1:9000/ingest" {
		t.Fatalf("IngestURL trailing slash = %q", got)
	}
}
