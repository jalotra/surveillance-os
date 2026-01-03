package streamer

import (
	"context"
	"fmt"
	"log"

	"github.com/segmentio/kafka-go"
	"github.com/sjalotra/surveillance-os/backend/internal/camera"
)

// Streamer defines the interface for pushing data to a destination.
type Streamer interface {
	Stream(ctx context.Context, frames <-chan camera.Frame) error
}

// KafkaStreamer implements the Streamer interface for Kafka.
type KafkaStreamer struct {
	writer *kafka.Writer
}

// NewKafkaStreamer creates a new KafkaStreamer.
func NewKafkaStreamer(brokers []string, topic string) *KafkaStreamer {
	return &KafkaStreamer{
		writer: &kafka.Writer{
			Addr:     kafka.TCP(brokers...),
			Topic:    topic,
			Balancer: &kafka.LeastBytes{},
			// Async writing for higher throughput
			Async: true,
		},
	}
}

// Stream consumes frames from the channel and writes them to Kafka.
func (s *KafkaStreamer) Stream(ctx context.Context, frames <-chan camera.Frame) error {
	defer s.writer.Close()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame, ok := <-frames:
			if !ok {
				return nil
			}

			err := s.writer.WriteMessages(ctx, kafka.Message{
				Key:   []byte(fmt.Sprintf("%d", frame.Sequence)),
				Value: frame.Data,
				Time:  frame.Timestamp,
			})

			// Crucial: Release the frame back to go4vl's pool after sending/queueing
			frame.Release()

			if err != nil {
				log.Printf("Error writing to Kafka: %v", err)
				// Depending on requirements, we might want to continue or return
			}
		}
	}
}
