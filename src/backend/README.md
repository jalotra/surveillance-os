# Surveillance OS Backend (Go)

This component handles high-frequency camera capture from a Raspberry Pi using FFmpeg native libraries and streams the frames to a Kafka cluster.

## Prerequisites

To build and run this on a Raspberry Pi, you need:

1.  **Go**: Install the latest version of Go.
2.  **FFmpeg Development Headers**:
    ```bash
    sudo apt-get update
    sudo apt-get install -y libavformat-dev libavcodec-dev libavdevice-dev libavutil-dev pkg-config
    ```
3.  **Kafka**: A running Kafka broker (local or remote).

## Architecture

1.  **Capture Loop**: Uses `CGO` to interface with FFmpeg (`libavdevice`). It captures frames from `/dev/video0` (V4L2) at 30 FPS.
2.  **Local Buffer**: A buffered Go channel (`chan Frame`) decoupling the capture logic from network latency.
3.  **Kafka Producer**: A background goroutine that pulls frames from the buffer and writes them to a Kafka topic.

## Configuration

Update the constants in `main.go` to match your environment:
- `KafkaBroker`: Address of your Kafka cluster.
- `KafkaTopic`: The topic where frames will be published.
- `BufferSize`: How many frames to keep in memory before dropping (to handle spikes).

## Building

```bash
cd src/backend
go build -o camera-streamer
```

## Kafka Retention (Time Travel)

To support the requirement of viewing history (e.g., 4 hours), ensure your Kafka topic is configured with a retention policy:

```bash
kafka-configs --bootstrap-server localhost:9092 --alter --entity-type topics --entity-name camera-stream --add-config retention.ms=14400000
```
(14,400,000 ms = 4 hours)
