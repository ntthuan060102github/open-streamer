# Open Streamer

A high-availability live media server written in Go. It ingests streams from virtually any source, normalises them to an internal MPEG-TS pipeline, transcodes on demand, and publishes to consumers over HLS, DASH, RTMP, RTSP, and SRT — all without spawning a process per stream.

## Table of Contents

- [Features](#features)
- [Architecture](#architecture)
- [Supported Protocols](#supported-protocols)
- [Quick Start](#quick-start)
- [Stream Management](#stream-management)
- [DVR & Timeshift](#dvr--timeshift)
- [REST API](#rest-api)
- [Server Configuration](#server-configuration)
- [Development](#development)
- [Testing](#testing)
- [Project Layout](#project-layout)

---

## Features

- **URL-driven ingest** — protocol and mode (pull vs. push-listen) derived automatically from the URL; no extra config per stream
- **Zero-subprocess ingest** — all pull protocols (RTMP, RTSP, SRT, UDP, HLS, HTTP, File, S3) implemented in native Go
- **RTMP / SRT push ingest** — shared listener on `:1935`/`:9999`; loopback relay architecture ensures the same stable codec path as pull mode
- **Automatic failover** — each stream accepts multiple prioritised inputs; the Stream Manager switches seamlessly when the active source degrades
- **Exponential-backoff reconnect** — pull workers reconnect automatically with configurable per-input backoff
- **Fan-out Buffer Hub** — single in-memory ring buffer per stream fans out to Transcoder, Publisher, and DVR concurrently; slow consumers drop packets, never block the writer
- **ABR transcoding** — FFmpeg worker pool with configurable profiles (resolution, bitrate, codec, hardware acceleration: NVENC / VAAPI / VideoToolbox / QSV)
- **HLS publishing** — MPEG-TS segmenter + playlist generator; ABR master playlist with EXT-X-DISCONTINUITY on failover
- **DASH publishing** — fMP4 packager + dynamic MPD (eyevinn/mp4ff); ABR per-track sharding
- **RTSP / RTMP / SRT serve** — shared listeners; streams selected by path (`/live/<code>`), RTMP app, or SRT streamid
- **DVR recording** — persistent per-stream recording (ID = stream code); resumes after restart with `#EXT-X-DISCONTINUITY` markers; configurable segment duration, retention window, and max disk size
- **Timeshift** — dynamic VOD M3U8 from `playlist.m3u8` using absolute time (`from=RFC3339`) or relative offset (`offset_sec=N`)
- **Push-to-platform** — re-streams to multiple destinations (YouTube, Facebook, Twitch, CDN relay) per stream
- **Webhook hooks** — lifecycle events with retry, timeout, and optional HMAC signing
- **Prometheus metrics** — bitrate, FPS, input-switch count, transcoder restarts, buffer depth
- **Pluggable storage** — JSON flat-file (default), PostgreSQL/MySQL, MongoDB

---

## Architecture

```
                        ┌─────────────────┐
                        │   REST API      │  :8080
                        └────────┬────────┘
                                 │  CRUD streams / recordings / hooks
                        ┌────────▼────────┐
                        │  Coordinator   │  pipeline wiring
                        └────────┬────────┘
                                 │
             ┌───────────────────▼─────────────────────┐
             │               Ingestor                  │
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
          ┌──────────────────┼──────┼──────────────────┐
          │                  │      │                  │
   ┌──────▼──────┐   ┌───────▼──┐  └──▼────────┐  ┌───▼────┐
   │  Transcoder │   │Publisher │    │   DVR    │  │Manager │
   │  FFmpeg pool│   │HLS/DASH/ │    │TS + index│  │failover│
   │  ABR ladder │   │RTSP/RTMP │    │.m3u8     │  │health  │
   └─────────────┘   └──────────┘    └──────────┘  └────────┘
                                          │
                              ┌───────────▼──────────┐
                              │     Event Bus        │
                              └───────────┬──────────┘
                                          │
                              ┌───────────▼──────────┐
                              │   Hooks dispatcher   │
                              │  HTTP / NATS / Kafka │
                              └──────────────────────┘
```

**Key design principle:** the server stores only operational configuration (ports, storage driver, FFmpeg path) in `config.yaml` or environment variables. Everything about a stream — inputs, transcoder settings, output protocols, push destinations, DVR — is managed through the REST API and persisted in the chosen data store.

---

## Supported Protocols

### Pull mode — server connects to the remote source

| URL | Protocol | Notes |
|-----|----------|-------|
| `rtmp://server/app/key` | RTMP | Native Go (yapingcat/gomedia) |
| `rtmps://server/app/key` | RTMPS | TLS wrapper over RTMP |
| `rtsp://camera:554/stream` | RTSP | Native Go RTSP client + RTP→MPEG-TS |
| `srt://relay:9999?streamid=key` | SRT | Native Go (datarhei/gosrt, caller mode) |
| `udp://239.1.1.1:5000` | UDP MPEG-TS | Native Go, multicast-ready |
| `http://cdn/live.ts` | HTTP stream | Raw MPEG-TS over HTTP/HTTPS |
| `https://cdn/playlist.m3u8` | HLS | Native M3U8 parser, segment fetch with retry |
| `file:///recordings/src.ts` | File | Local filesystem; `?loop=true` to loop |
| `/absolute/path/to/src.ts` | File | Bare absolute path also accepted |
| `s3://bucket/key?region=ap-1` | AWS S3 | GetObject stream; S3-compatible via `?endpoint=` |

### Push mode — external encoder connects to Open Streamer

| URL | Protocol | Notes |
|-----|----------|-------|
| `publish://live/<key>` | RTMP / SRT | Canonical push-listen URL; protocol inferred from running server |
| `rtmp://0.0.0.0:1935/live/key` | RTMP push | OBS, hardware encoders, `ffmpeg -f flv` |
| `srt://0.0.0.0:9999?streamid=key` | SRT push | StreamID carries the stream key |

Push mode is detected automatically when the URL host is a wildcard address (`0.0.0.0`, `::`) or uses the `publish://` scheme.

**RTMP push architecture**: client pushes to the gomedia relay server; an internal joy4 pull worker connects loopback to the same relay. All codec conversion (AVCC→Annex-B, ADTS wrapping) is handled by the existing RTMPReader — identical to pull mode, no code duplication.

---

## Quick Start

### Docker Compose

```bash
cp .env.example .env
docker compose -f build/docker-compose.yml up -d
```

| Service | Ports | Purpose |
|---------|-------|---------|
| `open-streamer` | `8080` (API), `1935` (RTMP push), `9999` (SRT push), `9091` (metrics) | Main server |
| `postgres` | `5432` | Stream / recording / hook storage |
| `prometheus` | `9092` | Metrics scraping |
| `grafana` | `3000` | Dashboards (admin / admin) |

### Build from source

```bash
# Requires Go 1.25+ and FFmpeg on PATH
git clone https://github.com/ntthuan060102github/open-streamer
cd open-streamer
make build       # → bin/open-streamer
make run         # run without building binary
```

---

## Stream Management

All stream configuration is managed through the REST API. A **stream** is the central entity — it describes every aspect of how one live channel is ingested, processed, and delivered.

### Core concepts

| Concept | Description |
|---------|-------------|
| **StreamCode** | Unique identifier (`a-zA-Z0-9_`, max 128 chars). Used in API paths, file system paths, and buffer IDs. |
| **Input** | A source URL. Each stream can have multiple inputs ordered by `priority` (lower = higher). The Stream Manager monitors health and switches on failure. |
| **Transcoder config** | Per-stream encoding: video profiles (resolution, bitrate, codec, HW accel), audio encoding, copy/passthrough modes. |
| **Output protocols** | Which delivery endpoints are opened: `hls`, `dash`. RTSP/RTMP/SRT serve are always available if enabled in server config. |
| **Push destinations** | External platforms the server actively re-streams to (YouTube, Facebook, CDN relay). |
| **DVR config** | Per-stream: `enabled`, `segment_duration` (seconds), `retention_sec`, `storage_path`, `max_size_gb`. No global DVR config — each stream controls its own. |

### Example: stream with ABR transcoding and failover inputs

```json
PUT /streams/channel-1
{
  "code": "channel-1",
  "name": "Morning Show",
  "inputs": [
    {
      "url": "rtmp://encoder.studio.local/live/mykey",
      "priority": 0,
      "net": { "reconnect": true, "reconnect_delay_sec": 2, "reconnect_max_delay_sec": 30 }
    },
    {
      "url": "udp://239.1.1.1:5000",
      "priority": 1
    }
  ],
  "transcoder": {
    "video": {
      "profiles": [
        { "width": 1920, "height": 1080, "bitrate": 4000, "codec": "h264", "preset": "fast" },
        { "width": 1280, "height": 720,  "bitrate": 2000, "codec": "h264", "preset": "fast" },
        { "width": 854,  "height": 480,  "bitrate": 800,  "codec": "h264", "preset": "fast" }
      ]
    },
    "audio": { "codec": "aac", "bitrate": 128, "channels": 2 },
    "global": { "hw_accel": "nvenc" }
  },
  "protocols": { "hls": true, "dash": true },
  "push": [
    {
      "url": "rtmp://a.rtmp.youtube.com/live2/xxxx",
      "enabled": true,
      "comment": "YouTube Live"
    }
  ],
  "dvr": {
    "enabled": true,
    "segment_duration": 6,
    "retention_sec": 172800
  }
}
```

### Example: OBS push ingest (RTMP)

Configure OBS:
- **Server**: `rtmp://your-server:1935/live`
- **Stream Key**: `mykey`

```json
PUT /streams/obs-channel
{
  "code": "obs-channel",
  "name": "OBS Stream",
  "inputs": [{ "url": "rtmp://0.0.0.0:1935/live/mykey", "priority": 0 }],
  "protocols": { "hls": true }
}
```

### Example: S3 file ingest (looping)

```json
{
  "url": "s3://my-bucket/videos/source.ts?region=us-east-1&loop=true",
  "priority": 0
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

## DVR & Timeshift

DVR records every stream as a single persistent recording (ID = stream code). Recording resumes transparently after server restart or signal loss, using `#EXT-X-DISCONTINUITY` markers for gaps.

### Storage layout

```
./dvr/{streamCode}/
  index.json       # stream info, segment count, total bytes, gap list
  playlist.m3u8    # HLS EVENT/VOD with #EXT-X-PROGRAM-DATE-TIME per segment
  000000.ts
  000001.ts
  ...
```

### Start / stop recording

```bash
# Start
POST /streams/{code}/recordings/start

# Stop
POST /streams/{code}/recordings/stop

# Get metadata
GET /recordings/{code}

# Get detailed info (dvr_range, gaps, segment count, disk usage)
GET /recordings/{code}/info
```

### Playback

```bash
# Full VOD playlist
GET /recordings/{code}/playlist.m3u8

# Serve individual segment
GET /recordings/{code}/000042.ts

# Timeshift — watch from an absolute wall time
GET /recordings/{code}/timeshift.m3u8?from=2026-04-06T14:30:00Z&duration=3600

# Timeshift — watch from N seconds after recording start
GET /recordings/{code}/timeshift.m3u8?offset_sec=1800&duration=3600
```

The `timeshift.m3u8` response is a dynamically-built VOD playlist filtered to the requested window. Segment URLs point back to `/recordings/{code}/{file}` served directly from disk.

---

## REST API

Base URL: `http://localhost:8080`

### Streams

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/streams` | List all streams |
| `GET` | `/streams/{code}` | Get stream details |
| `PUT` | `/streams/{code}` | Create or update stream |
| `DELETE` | `/streams/{code}` | Delete stream |
| `POST` | `/streams/{code}/start` | Start ingest + publishing |
| `POST` | `/streams/{code}/stop` | Stop stream |
| `GET` | `/streams/{code}/status` | Get live runtime status |

### Recordings

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/streams/{code}/recordings/start` | Start DVR recording |
| `POST` | `/streams/{code}/recordings/stop` | Stop DVR recording |
| `GET` | `/streams/{code}/recordings` | List recordings for stream |
| `GET` | `/recordings/{rid}` | Get recording lifecycle metadata |
| `DELETE` | `/recordings/{rid}` | Delete recording metadata |
| `GET` | `/recordings/{rid}/info` | DVR range, gaps, segment count, disk usage |
| `GET` | `/recordings/{rid}/playlist.m3u8` | Full VOD playlist |
| `GET` | `/recordings/{rid}/timeshift.m3u8` | Dynamic timeshift playlist (see query params above) |
| `GET` | `/recordings/{rid}/{file}` | Serve TS segment file |

### Hooks

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/hooks` | List hooks |
| `POST` | `/hooks` | Register a hook |
| `GET` | `/hooks/{hid}` | Get hook |
| `PUT` | `/hooks/{hid}` | Update hook |
| `DELETE` | `/hooks/{hid}` | Delete hook |
| `POST` | `/hooks/{hid}/test` | Send test event |

**Hook event types**: `stream.started`, `stream.stopped`, `stream.created`, `stream.deleted`, `input.degraded`, `input.failover`, `recording.started`, `recording.stopped`

**Hook body example:**
```json
{
  "name": "Production alert",
  "type": "http",
  "target": "https://hooks.example.com/os",
  "secret": "my-hmac-secret",
  "event_types": ["stream.started", "input.degraded"],
  "enabled": true,
  "max_retries": 3,
  "timeout_sec": 10
}
```

### Media delivery

| Path | Description |
|------|-------------|
| `GET /{code}/index.m3u8` | HLS master playlist (or single-rendition media playlist) |
| `GET /{code}/index.mpd` | DASH manifest |
| `GET /{code}/*` | HLS segments, DASH init/segments |

---

## Server Configuration

Configuration is loaded in order (later overrides earlier):

1. Built-in defaults
2. `config.yaml` in working directory or `/etc/open-streamer/`
3. Environment variables (`OPEN_STREAMER_` prefix, `.` → `_`)

```bash
# HTTP
OPEN_STREAMER_SERVER_HTTP_ADDR=:8080

# Storage — driver: "json" | "sql" | "mongo"
OPEN_STREAMER_STORAGE_DRIVER=json
OPEN_STREAMER_STORAGE_JSON_DIR=./data
OPEN_STREAMER_STORAGE_SQL_DSN=postgres://open_streamer:secret@localhost:5432/open_streamer?sslmode=disable
OPEN_STREAMER_STORAGE_MONGO_URI=mongodb://localhost:27017
OPEN_STREAMER_STORAGE_MONGO_DATABASE=open_streamer

# Buffer — ring buffer capacity in MPEG-TS packets per stream
OPEN_STREAMER_BUFFER_CAPACITY=1000

# Ingestor — push server bindings
OPEN_STREAMER_INGESTOR_RTMP_ENABLED=true
OPEN_STREAMER_INGESTOR_RTMP_ADDR=:1935
OPEN_STREAMER_INGESTOR_SRT_ENABLED=true
OPEN_STREAMER_INGESTOR_SRT_ADDR=:9999
OPEN_STREAMER_INGESTOR_HLS_MAX_SEGMENT_BUFFER=8

# Transcoder
OPEN_STREAMER_TRANSCODER_MAX_WORKERS=4
OPEN_STREAMER_TRANSCODER_FFMPEG_PATH=ffmpeg

# Publisher output directories
OPEN_STREAMER_PUBLISHER_HLS_DIR=./hls
OPEN_STREAMER_PUBLISHER_HLS_BASE_URL=http://localhost:8080
OPEN_STREAMER_PUBLISHER_DASH_DIR=./dash

# Manager — input health timeout
OPEN_STREAMER_MANAGER_INPUT_PACKET_TIMEOUT_SEC=30

# Hooks — delivery worker pool
OPEN_STREAMER_HOOKS_WORKER_COUNT=4

# Metrics
OPEN_STREAMER_METRICS_ADDR=:9091
OPEN_STREAMER_METRICS_PATH=/metrics

# Logging
OPEN_STREAMER_LOG_LEVEL=info
OPEN_STREAMER_LOG_FORMAT=text
```

> **DVR has no global config.** Each stream controls its own DVR via the `dvr` field in the stream configuration.

---

## Development

### Prerequisites

- Go 1.25+
- FFmpeg (for Transcoder; not needed for pure ingest / passthrough)
- Docker (optional, for Postgres + Prometheus + Grafana)

### Commands

```bash
make build          # compile → bin/open-streamer
make run            # run without building binary
make test           # go test -race -shuffle=on -count=1 -timeout=5m ./...
make lint           # golangci-lint run ./...
make vet            # go vet ./...
make fmt            # gofumpt
make tidy           # go mod tidy
make check          # tidy + vet + lint + test (full local CI)
make cover          # generate coverage.out
make cover-html     # open HTML coverage report
make docker-build   # build open-streamer:local Docker image
make compose-up     # docker compose up
```

Single test: `go test -run TestName ./internal/ingestor/...`

---

## Testing

### Test coverage

| Package | What's tested |
|---------|---------------|
| `pkg/protocol` | URL detection, push-listen detection, MPEG-TS helpers |
| `internal/buffer` | Ring buffer capacity, fan-out, slow consumer drop |
| `internal/ingestor` | `NewReader` dispatch, Registry CRUD + concurrent access, worker reconnect logic |
| `internal/ingestor/pull` | File reader, HTTP reader (httptest mock), UDP reader (loopback), RTMP packet parsing |
| `internal/manager` | Health state machine, failover trigger |
| `internal/transcoder` | Worker pool, FFmpeg args construction |
| `internal/publisher` | HLS segmenter, DASH packager |
| `internal/dvr` | Playlist parsing, index read/write |

---

## Project Layout

```
├── cmd/server/         # Binary entry point — DI wiring, graceful shutdown
├── config/             # Server config struct + Viper loader
├── data/               # Runtime data (default JSON store)
├── docs/               # Documentation
│   ├── FEATURES_CHECKLIST.md
│   └── DESIGN.md       # Detailed design principles per feature
├── internal/
│   ├── api/            # HTTP server + REST handlers (chi router)
│   │   └── handler/    # StreamHandler, RecordingHandler, HookHandler
│   ├── buffer/         # Buffer Hub — ring buffer, fan-out subscriptions
│   ├── coordinator/    # Pipeline wiring (buffer → manager → transcoder → publisher → DVR)
│   ├── domain/         # Domain types: Stream, Input, Recording, Event, Hook, …
│   ├── dvr/            # DVR recording service (TS muxer, playlist, index, retention)
│   ├── events/         # In-process pub/sub event bus
│   ├── hooks/          # Webhook dispatcher with retry + HMAC
│   ├── ingestor/       # Ingest orchestrator
│   │   ├── pull/       # Pull readers: RTMP, RTSP, SRT, UDP, HLS, HTTP, File, S3
│   │   └── push/       # Push servers: RTMP (:1935), SRT (:9999)
│   ├── manager/        # Stream Manager — health monitoring, failover
│   ├── mediaserve/     # Static HTTP file serving for HLS/DASH
│   ├── metrics/        # Prometheus collectors
│   ├── publisher/      # Output delivery: HLS, DASH, RTSP, RTMP, SRT
│   ├── store/          # Repository interfaces + JSON / SQL / Mongo drivers
│   ├── transcoder/     # FFmpeg transcoding worker pool
│   └── tsmux/          # MPEG-TS muxing utilities (AVPacket → 188-byte TS)
├── pkg/
│   ├── ffmpeg/         # FFmpeg subprocess helper
│   ├── logger/         # slog initialisation
│   └── protocol/       # URL → protocol Kind detection
├── build/
│   ├── Dockerfile      # Multi-stage build (Go 1.25 → alpine)
│   └── docker-compose.yml
└── .cursor/
    ├── rules/          # Cursor IDE project rules
    └── docs/           # Architecture and technical plan notes
```

---

## License

MIT
