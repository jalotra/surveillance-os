package camera

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/vladimirvivien/go4vl/device"
	"github.com/vladimirvivien/go4vl/v4l2"
)

// Frame represents a single captured frame with its metadata.
type Frame struct {
	Data      []byte
	Timestamp time.Time
	Sequence  uint32
	Release   func()
}

// Config holds camera configuration.
type Config struct {
	DevicePath  string
	Width       uint32
	Height      uint32
	PixelFormat v4l2.FourCCType
	FPS         uint32
}

// CaptureService handles the interaction with the physical camera device.
type CaptureService struct {
	config Config
	dev    *device.Device
}

// NewCaptureService creates a new instance of CaptureService.
func NewCaptureService(cfg Config) *CaptureService {
	return &CaptureService{
		config: cfg,
	}
}

// Start initiates the camera capture and returns a channel of frames.
func (s *CaptureService) Start(ctx context.Context) (<-chan Frame, error) {
	var err error
	s.dev, err = device.Open(
		s.config.DevicePath,
		device.WithPixFormat(v4l2.PixFormat{
			PixelFormat: s.config.PixelFormat,
			Width:       s.config.Width,
			Height:      s.config.Height,
		}),
		device.WithFPS(s.config.FPS),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to open device %s: %w", s.config.DevicePath, err)
	}

	if err := s.dev.Start(ctx); err != nil {
		s.dev.Close()
		return nil, fmt.Errorf("failed to start camera stream: %w", err)
	}

	out := make(chan Frame, 10) // Small internal buffer

	go func() {
		defer close(out)
		defer s.dev.Close()

		for {
			select {
			case <-ctx.Done():
				return
			case frame, ok := <-s.dev.GetFrames():
				if !ok {
					return
				}

				// Create a copy or pass the frame with a release callback
				// go4vl uses a frame pool, so we MUST call Release() when done.
				out <- Frame{
					Data:      frame.Data, // Note: This might need to be copied if shared across goroutines
					Timestamp: frame.Timestamp,
					Sequence:  frame.Sequence,
					Release:   frame.Release,
				}
			}
		}
	}()

	return out, nil
}

// Close stops the camera and releases resources.
func (s *CaptureService) Close() error {
	if s.dev != nil {
		return s.dev.Close()
	}
	return nil
}
