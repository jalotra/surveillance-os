package gst

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"surveillance-os/agent/internal/config"
)

func joined(args []string) string { return strings.Join(args, " ") }

func TestPipelineHardwareEncode(t *testing.T) {
	c := config.Defaults()
	c.CamID = "a"
	c.Hub = "h:1"
	c.Thumbs = false

	args := Pipeline(c)
	s := joined(args)

	for _, want := range []string{
		"libcamerasrc",
		"video/x-raw,width=1920,height=1080,framerate=10/1",
		"v4l2h264enc",
		"extra-controls=controls,video_bitrate=700000,video_gop_size=20",
		"h264parse config-interval=1",
		"mp4mux fragment-duration=1000",
		"fdsink fd=1",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("pipeline missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "x264enc") || strings.Contains(s, "jpegenc") || strings.Contains(s, "fd=3") {
		t.Errorf("hw pipeline should not contain sw-encode/thumb tokens:\n%s", s)
	}
	if args[0] != "-q" || args[1] != "-e" {
		t.Errorf("pipeline should start with -q -e (quiet stdout, EOS on shutdown), got %q %q", args[0], args[1])
	}
}

func TestPipelineSoftwareEncode(t *testing.T) {
	c := config.Defaults()
	c.SoftwareEncode = true
	c.Thumbs = false
	c.FPS = 15
	c.Bitrate = 500_000

	s := joined(Pipeline(c))
	for _, want := range []string{
		"x264enc tune=zerolatency speed-preset=ultrafast",
		"bitrate=500", // x264enc takes kbit/s
		"key-int-max=30",
		"framerate=15/1",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("sw pipeline missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "v4l2h264enc") {
		t.Errorf("sw pipeline must not use hardware encoder:\n%s", s)
	}
}

func TestPipelineThumbs(t *testing.T) {
	c := config.Defaults()
	c.Thumbs = true

	s := joined(Pipeline(c))
	for _, want := range []string{
		"tee name=v",
		"v.",
		"videorate",
		"video/x-raw,framerate=1/5",
		"jpegenc",
		"fdsink fd=3",
		"fdsink fd=1",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("thumb pipeline missing %q:\n%s", want, s)
		}
	}
}

func TestThumbReader(t *testing.T) {
	jpeg1 := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{1}, 100)...)
	jpeg1 = append(jpeg1, 0xFF, 0xD9)
	jpeg2 := append([]byte{0xFF, 0xD8, 0xFF, 0xE1}, bytes.Repeat([]byte{2}, 50)...)
	jpeg2 = append(jpeg2, 0xFF, 0xD9)
	stream := append(jpeg1, jpeg2...)

	tr := NewThumbReader(bytes.NewReader(stream))
	f1, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f1, jpeg1) {
		t.Fatalf("frame1 len %d, want %d", len(f1), len(jpeg1))
	}
	f2, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f2, jpeg2) {
		t.Fatal("frame2 mismatch")
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Fatalf("Next = %v, want EOF", err)
	}
}

func TestThumbReaderResync(t *testing.T) {
	// Garbage + a torn SOI before the real frame — reader must resync to
	// the real FF D8.
	junk := []byte{0x12, 0xFF, 0x11, 0xFF, 0x99}
	jpeg := []byte{0xFF, 0xD8, 0x10, 0x20, 0xFF, 0xD9}
	tr := NewThumbReader(bytes.NewReader(append(junk, jpeg...)))
	f, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f, jpeg) {
		t.Fatalf("resync got %x, want %x", f, jpeg)
	}
}
