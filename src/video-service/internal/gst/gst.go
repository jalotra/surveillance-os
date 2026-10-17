// Package gst builds and supervises the gst-launch-1.0 capture pipeline
// (plans/video-service.md §1.1). The agent never links GStreamer — it execs
// gst-launch-1.0 and reads fragmented MP4 from the child's stdout (fd 1) and
// optional JPEG thumbnails from fd 3.
//
// Nothing here runs at import time; Start is called by the supervisor only.
// On a dev machine without gst-launch-1.0 the watchdog simply retries.
package gst

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"surveillance-os/agent/internal/config"
)

// Binary is the gst-launch executable name (overridable for tests).
var Binary = "gst-launch-1.0"

// Pipeline returns the gst-launch-1.0 argument vector for cfg (one element
// per gst-launch token; "!" separates elements). The source element comes
// from cfg.Source ("libcamerasrc" on a Pi; "videotestsrc is-live=true"
// fakes a camera on any machine with gst installed). Pi 4 default is the
// hardware encoder; software_encode swaps in x264enc — required whenever
// the host lacks v4l2h264enc, i.e. every non-Pi host. With thumbs on, a
// tee taps the raw video before the encoder and jpegenc snapshots land
// on fd 3.
func Pipeline(c *config.Config) []string {
	caps := fmt.Sprintf("video/x-raw,width=%d,height=%d,framerate=%d/1",
		c.Width, c.Height, c.FPS)
	gop := c.GOP()

	src := strings.Fields(c.Source)
	if len(src) == 0 {
		src = []string{"libcamerasrc"} // configs built by hand, not via Load
	}
	// -q is load-bearing: gst-launch otherwise prints "Setting pipeline to
	// PAUSED…" etc. to stdout, which contaminates the fMP4 box stream the
	// splitter reads from fd 1.
	args := append([]string{"-q", "-e"}, src...)
	args = append(args, "!", caps)
	if c.Thumbs {
		args = append(args, "!", "tee", "name=v", "!", "queue")
	}
	if c.SoftwareEncode {
		args = append(args, "!",
			"x264enc", "tune=zerolatency", "speed-preset=ultrafast",
			fmt.Sprintf("bitrate=%d", c.Bitrate/1000), // x264enc unit is kbit/s
			fmt.Sprintf("key-int-max=%d", gop))
	} else {
		args = append(args, "!",
			"v4l2h264enc",
			fmt.Sprintf("extra-controls=controls,video_bitrate=%d,video_gop_size=%d",
				c.Bitrate, gop))
	}
	args = append(args, "!",
		"h264parse", "config-interval=1",
		"!", "mp4mux", fmt.Sprintf("fragment-duration=%d", c.FragDurMs),
		"!", "fdsink", "fd=1")
	if c.Thumbs {
		args = append(args,
			"v.", "!", "queue",
			"!", "videorate",
			"!", "video/x-raw,framerate=1/5",
			"!", "jpegenc",
			"!", "fdsink", "fd=3")
	}
	return args
}

// Proc is one running gst-launch instance. Stdout carries fMP4 bytes;
// Thumbs carries an unbroken JFIF stream of JPEG frames (nil unless
// cfg.Thumbs). The caller owns both readers and must Wait after they EOF.
type Proc struct {
	Stdout io.ReadCloser
	Thumbs io.ReadCloser
	cmd    *exec.Cmd
	thumbs *os.File // write end kept open in the parent only until start
}

// Start launches one pipeline instance. ctx kill → SIGKILL via exec;
// caller may also Kill explicitly.
func Start(ctx context.Context, c *config.Config) (*Proc, error) {
	args := Pipeline(c)
	cmd := exec.CommandContext(ctx, Binary, args...)
	cmd.Stderr = os.Stderr // gst diagnostics belong in the journal

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("gst: stdout pipe: %w", err)
	}

	p := &Proc{cmd: cmd, Stdout: stdout}
	if c.Thumbs {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("gst: thumb pipe: %w", err)
		}
		cmd.ExtraFiles = []*os.File{w} // child fd 3
		p.Thumbs = r
		p.thumbs = w
	}
	if err := cmd.Start(); err != nil {
		if p.thumbs != nil {
			p.thumbs.Close()
			p.Thumbs.Close()
		}
		return nil, fmt.Errorf("gst: start %s: %w", Binary, err)
	}
	// Parent doesn't write thumbs; closing our copy lets the reader see EOF.
	if p.thumbs != nil {
		p.thumbs.Close()
		p.thumbs = nil
	}
	return p, nil
}

// Kill hard-stops the pipeline (SIGKILL). Safe to call repeatedly.
func (p *Proc) Kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// Wait reaps the child. Always call after the readers return EOF.
func (p *Proc) Wait() error { return p.cmd.Wait() }

// Run is the restart watchdog: it invokes fn once per pipeline instance and
// restarts with exponential backoff whenever fn returns (process exit,
// splitter error, whatever). fn receives procCtx which is cancelled when a
// config change arrives on cfgCh — that restart is immediate, no backoff.
// Run returns nil when ctx is done.
func Run(ctx context.Context, cfg *config.Config, cfgCh <-chan *config.Config,
	fn func(procCtx context.Context, p *Proc) error) error {

	const (
		minBackoff = 500 * time.Millisecond
		maxBackoff = 30 * time.Second
		resetAfter = 30 * time.Second // run this long → backoff resets
	)
	backoff := minBackoff
	cur := cfg
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		start := time.Now()
		proc, err := Start(ctx, cur)
		if err != nil {
			log.Printf("gst: start failed: %v (retry in %s)", err, backoff)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			backoff = grow(backoff, maxBackoff)
			continue
		}

		// Watch for mid-run config changes; cancel procCtx to force a
		// restart with the new pipeline.
		procCtx, cancel := context.WithCancel(ctx)
		newCfgCh := make(chan *config.Config, 1)
		go func() {
			select {
			case c := <-cfgCh:
				newCfgCh <- c
				cancel()
			case <-procCtx.Done():
			}
		}()

		runErr := fn(procCtx, proc)
		cancel()
		proc.Kill()
		_ = proc.Wait()

		select {
		case c := <-newCfgCh:
			cur = c
			backoff = minBackoff
			log.Printf("gst: config changed, restarting pipeline")
			continue
		default:
		}
		if ctx.Err() != nil {
			return nil
		}
		if runErr != nil && runErr != io.EOF {
			log.Printf("gst: capture ended: %v", runErr)
		} else {
			log.Printf("gst: process exited")
		}
		if time.Since(start) > resetAfter {
			backoff = minBackoff
		}
		if !sleepCtx(ctx, backoff) {
			return nil
		}
		backoff = grow(backoff, maxBackoff)
	}
}

func grow(d, max time.Duration) time.Duration {
	d *= 2
	if d > max {
		d = max
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

// ThumbReader splits the fd-3 JFIF stream into individual JPEG frames.
// fdsink writes each GstBuffer unframed, so frames are delimited by the
// JPEG EOI marker (FF D9); FFD9 cannot appear inside entropy-coded data
// (0xFF bytes there are stuffed as FF 00).
type ThumbReader struct {
	r *bufio.Reader
}

// NewThumbReader wraps the proc's thumb pipe.
func NewThumbReader(r io.Reader) *ThumbReader {
	return &ThumbReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// Next returns the next complete JPEG (SOI..EOI inclusive). Bytes before
// the first SOI are skipped (resync after a torn write).
func (t *ThumbReader) Next() ([]byte, error) {
	var buf []byte
	for {
		b, err := t.r.ReadByte()
		if err != nil {
			return nil, err
		}
		// Resync: only start buffering at a SOI marker.
		if len(buf) == 0 && !(b == 0xFF) {
			continue
		}
		buf = append(buf, b)
		n := len(buf)
		if n == 2 && buf[1] != 0xD8 { // second byte wasn't SOI marker → rescan
			buf = buf[:0]
			if b == 0xFF {
				buf = append(buf, b)
			}
			continue
		}
		if n >= 4 && buf[n-2] == 0xFF && buf[n-1] == 0xD9 {
			return buf, nil
		}
		if len(buf) > 8<<20 { // runaway frame guard
			return nil, fmt.Errorf("gst: jpeg frame exceeds 8MB")
		}
	}
}
