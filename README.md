# Open Streamer

A high-availability live media server written in Go. It ingests streams from virtually any source, normalises them to an internal MPEG-TS pipeline, transcodes on demand, and publishes to consumers over HLS, RTMP, and SRT — all without spawning a process per stream.

## Table of Contents

- [Features](#features)
- [Architecture](#architecture)
- [Supported Protocols](#supported-protocols)
- [Quick Start](#quick-start)
- [Stream Management](#stream-management)
- [REST API](#rest-api)
- [Server Configuration](#server-configuration)
- [Development](#development)
- [Testing](#testing)
- [Project Layout](#project-layout)

---

## Features

- **URL-driven ingest** — protocol and mode (pull vs. push-listen) are derived automatically from the URL scheme; no extra config needed per stream
- **Zero-subprocess ingest** — all pull protocols (RTMP, RTSP, SRT, UDP, HLS, HTTP, File, S3) use native Go implementations
- **Automatic failover** — each stream accepts multiple prioritised inputs; the Stream Manager switches seamlessly when the active source degrades or drops
- **Exponential-backoff reconnect** — pull workers reconnect automatically with configurable backoff per input
- **Fan-out Buffer Hub** — a single in-memory ring buffer per stream fans out to Transcoder, Publisher, and DVR concurrently
- **ABR transcoding** — FFmpeg worker pool with configurable profiles (resolution, bitrate, codec, hardware acceleration)
- **HLS publishing** — segmenter + playlist generator with configurable CDN base URL
- **Push-to-platform** — server actively re-streams to multiple destinations (YouTube, Facebook, Twitch, CDN relay) per stream
- **DVR recording** — rolling window with configurable retention, per-stream override
- **Webhook hooks** — lifecycle events (`on_stream_start`, `on_stream_stop`, `on_input_fail`, …) with retry and timeout
- **Prometheus metrics** — bitrate, FPS, input-switch count, transcoder restarts, buffer depth
- **Pluggable storage** — JSON flat-file (default), PostgreSQL/MySQL, MongoDB

---

## Architecture

```
                        ┌─────────────────┐
                        │   REST API      │  :8080
                        └────────┬────────┘
                                 │  CRUD streams/recordings/hooks
                        ┌────────▼────────┐
                        │ Stream Manager  │  failover, lifecycle
                        └────────┬────────┘
                                 │
             ┌───────────────────▼─────────────────────┐
             │               Ingestor                  │
             │                                         │
             │  Pull workers (1 goroutine / stream)    │
             │  ┌──────────┬──────────┬─────────────┐  │
             │  │ RTMP/SRT │  HLS/HTTP│ UDP/File/S3 │  │
             │  └──────────┴──────────┴─────────────┘  │
             │  Push servers (shared, 1 port total)    │
             │  ┌──────────────┬─────────────────────┐ │
             │  │  RTMP :1935  │     SRT :9999       │ │
             │  └──────────────┴─────────────────────┘ │
             └─────────────────────────────────────────┘
                                 │  MPEG-TS chunks
                        ┌────────▼────────┐
                        │   Buffer Hub    │  ring buffer / stream
                        └────┬──────┬─────┘
                             │      │
               ┌─────────────▼─┐  ┌─▼───────────┐  ┌────────┐
               │  Transcoder   │  │  Publisher  │  │  DVR   │
               │  FFmpeg pool  │  │  HLS segs   │  │  TS    │
               └───────────────┘  └─────────────┘  └────────┘
```

**Key design principle:** the server only stores operational configuration (ports, storage driver, FFmpeg path…) in environment variables or `config.yaml`. Everything about a stream — its inputs, transcoder settings, output protocols, push destinations, DVR — is created and managed by the user through the REST API and persisted in the chosen data storage.

---

## Supported Protocols

### Pull mode — server connects to the remote source

| URL | Protocol | Notes |
|-----|----------|-------|
| `rtmp://server/app/key` | RTMP | Native Go (yapingcat/gomedia) |
| `rtmps://server/app/key` | RTMPS | TLS wrapper over RTMP |
| `rtsp://camera:554/stream` | RTSP | Native Go RTSP client + RTP→MPEG-TS |
| `srt://relay:9999?streamid=key` | SRT | Native Go (datarhei/gosrt caller mode) |
| `udp://239.1.1.1:5000` | UDP MPEG-TS | Native Go, multicast-ready |
| `http://cdn/live.ts` | HTTP stream | Raw MPEG-TS over HTTP/HTTPS |
| `https://cdn/playlist.m3u8` | HLS | Native M3U8 parser (grafov/m3u8) |
| `file:///recordings/src.ts` | File | Local filesystem; `?loop=true` to loop |
| `/absolute/path/to/src.ts` | File | Bare absolute path also accepted |
| `s3://bucket/key?region=ap-1` | AWS S3 | GetObject stream; S3-compatible via `?endpoint=` |

### Push mode — external encoder connects to Open Streamer

| URL | Protocol | Notes |
|-----|----------|-------|
| `rtmp://0.0.0.0:1935/live/key` | RTMP push | OBS, hardware encoders, `ffmpeg -f flv` |
| `srt://0.0.0.0:9999?streamid=key` | SRT push | StreamID carries the stream key |

Push mode is detected automatically when the URL host is a wildcard address (`0.0.0.0`, `::`) or loopback.

---

## Quick Start

### Docker Compose

```bash
cp .env.example .env
docker compose -f build/docker-compose.yml up -d
```

This starts:

| Service | Ports | Purpose |
|---------|-------|---------|
| `open-streamer` | `8080` (HTTP API), `1935` (RTMP), `9999` (SRT), `9091` (metrics) | Main server |
| `postgres` | `5432` | Stream/recording/hook storage |
| `prometheus` | `9092` | Metrics scraping |
| `grafana` | `3000` | Dashboards (admin / admin) |

### Build from source

```bash
# Requires Go 1.25+ and FFmpeg on PATH
git clone https://github.com/open-streamer/open-streamer
cd open-streamer
go build -o bin/open-streamer ./cmd/server
./bin/open-streamer
```

---

## Stream Management

All stream configuration is managed through the REST API. A **stream** is the central entity — it describes every aspect of how one live channel is ingested, processed, and delivered.

### Core concepts

| Concept | Description |
|---------|-------------|
| **Input** | A source URL. Each stream can have multiple inputs ordered by `priority`. The Stream Manager monitors health and switches to the next input on failure. |
| **Stream key** | Used to authenticate RTMP/SRT push ingest. Specified in the input URL or as `stream_key` in the auth block. |
| **Transcoder config** | Per-stream encoding settings: mode (`passthrough` / `remux` / `transcode`), codec, bitrate ladder, hardware acceleration. |
| **Output protocols** | Which delivery endpoints are opened for the stream: HLS, DASH, RTSP, SRT, WebRTC. |
| **Push destinations** | External platforms the server actively re-streams to (YouTube, Facebook, CDN relay). Per-stream, multiple entries allowed. |
| **DVR config** | Per-stream override of the global DVR settings: retention window, segment duration, storage path, max disk usage. |

### Example: create a stream with failover inputs

```json
POST /streams
{
  "stream_id": "channel-1",
  "name": "Morning Show",
  "inputs": [
    {
      "id": "primary",
      "url": "rtmp://encoder.studio.local/live/mykey",
      "priority": 1,
      "enabled": true,
      "net": {
        "reconnect": true,
        "reconnect_delay_sec": 2,
        "reconnect_max_delay_sec": 30
      }
    },
    {
      "id": "backup",
      "url": "udp://239.1.1.1:5000",
      "priority": 2,
      "enabled": true
    }
  ],
  "transcoder": {
    "mode": "transcode",
    "hw_accel": "nvenc",
    "video_profiles": [
      { "name": "1080p", "width": 1920, "height": 1080, "bitrate": 4000, "codec": "h264", "preset": "p5" },
      { "name": "720p",  "width": 1280, "height": 720,  "bitrate": 2000, "codec": "h264", "preset": "p5" },
      { "name": "480p",  "width": 854,  "height": 480,  "bitrate": 800,  "codec": "h264", "preset": "p5" }
    ],
    "audio": { "codec": "aac", "bitrate": 128, "channels": 2 },
    "segment_align": true
  },
  "protocols": { "hls": true },
  "push": [
    {
      "url": "rtmp://a.rtmp.youtube.com/live2/xxxx-xxxx-xxxx",
      "enabled": true,
      "timeout_sec": 10,
      "retry_timeout_sec": 5,
      "comment": "YouTube Live"
    }
  ],
  "dvr": {
    "enabled": true,
    "retention_hours": 48,
    "segment_duration": 6
  }
}
```

### Example: RTMP push ingest (OBS → Open Streamer)

Configure OBS with:
- **Server**: `rtmp://your-server:1935/live`
- **Stream Key**: `mykey`

Then create the stream with a push-listen input URL:

```json
POST /streams
{
  "stream_id": "obs-channel",
  "name": "OBS Stream",
  "inputs": [
    {
      "id": "obs",
      "url": "rtmp://0.0.0.0:1935/live/mykey",
      "priority": 1,
      "enabled": true
    }
  ],
  "transcoder": { "mode": "passthrough" },
  "protocols": { "hls": true }
}
```

### Example: S3 file ingest

```json
{
  "id": "vod-source",
  "url": "s3://my-bucket/videos/source.ts?region=us-east-1",
  "priority": 1,
  "enabled": true
}
```

For S3-compatible storage (MinIO, Cloudflare R2):

```json
{
  "url": "s3://bucket/key?region=auto&endpoint=https://account.r2.cloudflarestorage.com",
  "auth": { "username": "ACCESS_KEY", "password": "SECRET_KEY" }
}
```

---

## REST API

Base URL: `http://localhost:8080`

### Streams

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/streams` | Create a stream |
| `GET` | `/streams` | List all streams |
| `GET` | `/streams/:id` | Get stream details |
| `PUT` | `/streams/:id` | Update stream configuration |
| `DELETE` | `/streams/:id` | Delete stream |
| `POST` | `/streams/:id/start` | Start ingest + publishing |
| `POST` | `/streams/:id/stop` | Stop stream |

### Recordings

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/recordings` | List recordings |
| `GET` | `/recordings/:id` | Get recording details |
| `DELETE` | `/recordings/:id` | Delete recording |

### Hooks

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/hooks` | Register a hook |
| `GET` | `/hooks` | List hooks |
| `GET` | `/hooks/:id` | Get hook |
| `PUT` | `/hooks/:id` | Update hook |
| `DELETE` | `/hooks/:id` | Delete hook |

Hook events: `on_stream_start`, `on_stream_stop`, `on_input_fail`, `on_input_recover`, `on_recording_complete`

**Register hook body**

```jsonc
{
  "name": "Production alert",
  "type": "http",                          // "http" | "nats" | "kafka"
  "target": "https://hooks.example.com/os",
  "secret": "my-hmac-secret",             // signs payload with X-OpenStreamer-Signature
  "event_types": ["stream.started", "input.failed"],  // omit = all events
  "enabled": true,
  "max_retries": 3,    // attempts before giving up (0 = server default)
  "timeout_sec": 10    // per-attempt timeout (0 = server default)
}
```

---

## Server Configuration

These variables control **server infrastructure** only — ports, storage backend, worker pool sizes, and filesystem paths. Everything about a stream (inputs, timeouts, transcoder profiles, DVR settings…) or a hook (retries, timeout, event filter…) is configured via the REST API and persisted in the data storage.

Configuration is loaded in this order (later sources override earlier ones):

1. Built-in defaults
2. `config.yaml` in the working directory or `/etc/open-streamer/`
3. Environment variables (`OPEN_STREAMER_` prefix, `.` replaced by `_`)

```bash
# HTTP / gRPC
OPEN_STREAMER_SERVER_HTTP_ADDR=:8080
OPEN_STREAMER_SERVER_GRPC_ADDR=:9090

# Storage — driver: "json" | "sql" | "mongo"
OPEN_STREAMER_STORAGE_DRIVER=json
OPEN_STREAMER_STORAGE_JSON_DIR=./data
OPEN_STREAMER_STORAGE_SQL_DSN=postgres://open_streamer:secret@localhost:5432/open_streamer?sslmode=disable
OPEN_STREAMER_STORAGE_MONGO_URI=mongodb://localhost:27017
OPEN_STREAMER_STORAGE_MONGO_DATABASE=open_streamer

# Buffer — ring buffer capacity in MPEG-TS packets per stream
OPEN_STREAMER_BUFFER_CAPACITY=1000

# Ingestor — push server port bindings
OPEN_STREAMER_INGESTOR_RTMP_ENABLED=true
OPEN_STREAMER_INGESTOR_RTMP_ADDR=:1935
OPEN_STREAMER_INGESTOR_SRT_ENABLED=true
OPEN_STREAMER_INGESTOR_SRT_ADDR=:9999
# Memory guard: max HLS segments buffered per stream
OPEN_STREAMER_INGESTOR_HLS_MAX_SEGMENT_BUFFER=8

# Transcoder — FFmpeg worker pool
OPEN_STREAMER_TRANSCODER_MAX_WORKERS=4
OPEN_STREAMER_TRANSCODER_FFMPEG_PATH=ffmpeg

# Publisher — nested keys publisher.hls.*, publisher.dash.*, publisher.rtsp.*, publisher.rtmp.*, publisher.srt.*
OPEN_STREAMER_PUBLISHER_HLS_DIR=./hls
OPEN_STREAMER_PUBLISHER_HLS_BASE_URL=http://localhost:8080/hls

# DVR — global on/off and root directory
# Per-stream: segment duration, retention, storage path → set via the API
OPEN_STREAMER_DVR_ENABLED=false
OPEN_STREAMER_DVR_ROOT_DIR=./dvr

# Hooks — delivery worker pool size
# Per-hook: max retries, timeout → set when registering the hook via the API
OPEN_STREAMER_HOOKS_WORKER_COUNT=4

# Metrics
OPEN_STREAMER_METRICS_ADDR=:9091
OPEN_STREAMER_METRICS_PATH=/metrics

# Logging — level: debug|info|warn|error  format: text|json
OPEN_STREAMER_LOG_LEVEL=info
OPEN_STREAMER_LOG_FORMAT=text
```

---

## Development

### Prerequisites

- Go 1.25+
- FFmpeg (for Transcoder; not needed for ingest)
- Docker (optional, for Postgres + Prometheus + Grafana)

### Run locally

```bash
# Start Postgres (optional — defaults to JSON storage)
docker compose -f build/docker-compose.yml up -d postgres

# Run with default JSON storage
go run ./cmd/server

# Run with Postgres
OPEN_STREAMER_STORAGE_DRIVER=sql \
OPEN_STREAMER_STORAGE_SQL_DSN="postgres://open_streamer:secret@localhost:5432/open_streamer?sslmode=disable" \
go run ./cmd/server
```

### Linting

```bash
golangci-lint run ./...
```

---

## Testing

```bash
# All unit tests
go test ./...

# With race detector (recommended)
go test -race ./...

# Single package, verbose
go test -v ./internal/ingestor/...

# Coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

### Test coverage

| Package | What's tested |
|---------|---------------|
| `pkg/protocol` | URL detection, push-listen detection, MPEG-TS helpers |
| `internal/ingestor` | `NewReader` dispatch, Registry CRUD + concurrent access, worker reconnect logic |
| `internal/ingestor/pull` | File reader, HTTP reader (httptest mock), UDP reader (loopback) |

---

## Project Layout

```
├── cmd/server/         # Binary entry point — DI wiring, graceful shutdown
├── config/             # Server config struct + Viper loader
├── internal/
│   ├── api/            # HTTP server + REST handlers (chi router)
│   ├── buffer/         # Buffer Hub — in-memory ring buffer, fan-out
│   ├── domain/         # Domain types: Stream, Input, TranscoderConfig, …
│   ├── dvr/            # DVR recording service
│   ├── events/         # In-process pub/sub event bus
│   ├── hooks/          # Webhook dispatcher with retry
│   ├── ingestor/       # Ingest orchestrator
│   │   ├── pull/       # Pull readers: RTMP, RTSP, SRT, UDP, HLS, HTTP, File, S3
│   │   └── push/       # Push servers: RTMP (:1935), SRT (:9999)
│   ├── manager/        # Stream Manager — failover, lifecycle
│   ├── metrics/        # Prometheus metrics
│   ├── publisher/      # HLS segmenter + playlist generator
│   ├── store/          # Repository interfaces + JSON / SQL / Mongo drivers
│   └── transcoder/     # FFmpeg transcoding worker pool
├── pkg/
│   ├── ffmpeg/         # FFmpeg subprocess helper
│   ├── logger/         # slog initialisation
│   └── protocol/       # URL → protocol Kind detection
├── build/
│   ├── Dockerfile      # Multi-stage build (Go 1.25 → alpine)
│   └── docker-compose.yml
└── scripts/
    └── prometheus.yml  # Prometheus scrape config
```

---

## License

MIT
