package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vladimirvivien/go4vl/v4l2"
	"github.com/sjalotra/surveillance-os/backend/internal/api"
	"github.com/sjalotra/surveillance-os/backend/internal/camera"
	"github.com/sjalotra/surveillance-os/backend/internal/streamer"
)

func main() {
	// 1. Setup Configuration
	camCfg := camera.Config{
		DevicePath:  "/dev/video0",
		Width:       1280,
		Height:      720,
		PixelFormat: v4l2.PixelFmtMJPEG,
		FPS:         30,
	}

	kafkaBrokers := []string{"localhost:9092"}
	kafkaTopic := "camera-stream"
	apiAddr := ":8080"

	// 2. Initialize Services
	captureSvc := camera.NewCaptureService(camCfg)
	kafkaStreamer := streamer.NewKafkaStreamer(kafkaBrokers, kafkaTopic)
	apiServer := api.NewServer()

	// 3. Setup Context for Graceful Shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		log.Printf("Received signal %v, shutting down...", sig)
		cancel()
	}()

	// 4. Start API Server in background
	go func() {
		if err := apiServer.Start(apiAddr); err != nil {
			log.Fatalf("API Server failed: %v", err)
		}
	}()

	// 5. Orchestrate the Pipeline
	log.Printf("Starting Surveillance OS Backend...")
	log.Printf("Camera: %s (%dx%d @ %d FPS)", camCfg.DevicePath, camCfg.Width, camCfg.Height, camCfg.FPS)
	log.Printf("Kafka: %v, Topic: %s", kafkaBrokers, kafkaTopic)
	log.Printf("Admin API: http://localhost%s", apiAddr)

	frames, err := captureSvc.Start(ctx)
	if err != nil {
		log.Fatalf("Critical error starting camera: %v", err)
	}

	// This blocks until context is cancelled or streamer finishes
	if err := kafkaStreamer.Stream(ctx, frames); err != nil {
		if err != context.Canceled {
			log.Printf("Streamer exited with error: %v", err)
		}
	}

	log.Println("Backend shutdown complete.")
}
