# Open Streamer

[![Go Reference](https://pkg.go.dev/badge/github.com/ntt0601zcoder/open-streamer.svg)](https://pkg.go.dev/github.com/ntt0601zcoder/open-streamer)
[![CI](https://github.com/ntt0601zcoder/open-streamer/actions/workflows/ci.yml/badge.svg)](https://github.com/ntt0601zcoder/open-streamer/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/ntt0601zcoder/open-streamer)](https://goreportcard.com/report/github.com/ntt0601zcoder/open-streamer)
[![Coverage](https://codecov.io/gh/ntt0601zcoder/open-streamer/branch/main/graph/badge.svg)](https://codecov.io/gh/ntt0601zcoder/open-streamer)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A high-availability live media server written in pure Go. Open Streamer ingests streams from virtually any source, normalises them to an internal MPEG-TS pipeline, transcodes on demand, and publishes to consumers over HLS, DASH, RTMP, RTSP, and SRT — all without spawning a process per stream.

---

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
- [Contributing](#contributing)
- [License](#license)

---

## Features

- **URL-driven ingest** — protocol and mode (pull vs. push-listen) detected automatically from the URL; no extra config per stream
- **Zero-subprocess ingest** — all pull protocols (RTMP, RTSP, SRT, UDP, HLS, HTTP, File, S3) implemented in native Go, no external processes
- **RTMP / SRT push ingest** — shared listener on `:1935` / `:9999`; loopback relay architecture ensures the same stable codec path as pull mode
- **Automatic failover** — each stream accepts multiple prioritised inputs; the Stream Manager switches seamlessly when the active source degrades, without restarting FFmpeg
- **Exponential-backoff reconnect** — pull workers reconnect automatically with configurable per-input backoff parameters
- **Fan-out Buffer Hub** — single in-memory ring buffer per stream fans out to Transcoder, Publisher, and DVR concurrently; slow consumers drop packets, never block the writer
- **ABR transcoding** — bounded FFmpeg worker pool with configurable profiles (resolution, bitrate, codec) and hardware acceleration (NVENC / VAAPI / VideoToolbox / QSV)
- **FFmpeg crash recovery** — per-profile exponential-backoff restart (2 s → 30 s cap), retried indefinitely so streams self-heal once the underlying issue resolves; per-profile last-5-errors history exposed via `runtime.transcoder.profiles[].errors[]`
- **HLS publishing** — MPEG-TS segmenter + playlist generator; ABR master playlist with `#EXT-X-DISCONTINUITY` on failover
- **DASH publishing** — fMP4 packager + dynamic MPD; ABR per-track sharding
- **RTSP / RTMP / SRT serve** — shared listeners; streams selected by path (`/live/<code>`), RTMP app, or SRT streamid
- **DVR recording** — persistent per-stream recording (ID = stream code); resumes after restart with `#EXT-X-DISCONTINUITY` markers; configurable segment duration, retention window, and max disk size
- **Timeshift** — dynamic VOD M3U8 from `playlist.m3u8` by absolute time (`from=RFC3339`) or relative offset (`offset_sec=N`)
- **Hot-reload stream config** — `PUT /streams/{code}` applies only the diff; adding a push destination does not interrupt HLS viewers; changing one ABR profile restarts only that FFmpeg process; toggling DASH does not affect RTSP subscribers
- **Push-to-platform** — re-streams to multiple destinations (YouTube, Facebook, Twitch, CDN relay) per stream
- **Webhook & Kafka hooks** — lifecycle events with retry, timeout, and optional HMAC signing
- **Prometheus metrics** — bitrate, FPS, failover count, transcoder restarts, buffer depth, stream uptime
- **Pluggable storage** — JSON flat-file (default), PostgreSQL/MySQL (JSONB), MongoDB

---

## Architecture

```text
                        ┌──────────────────┐
                        │    REST API      │  :8080
                        └────────┬─────────┘
                                 │  CRUD streams / recordings / hooks
                        ┌────────▼─────────┐
                        │   Coordinator    │  pipeline wiring
                        └────────┬─────────┘
                                 │
             ┌───────────────────▼──────────────────────┐
             │                Ingestor                  │
             │   Pull workers  (1 goroutine / stream)   │
             │  ┌──────────┬───────────┬─────────────┐  │
             │  │ RTMP/SRT │  HLS/HTTP │ UDP/File/S3 │  │
             │  └──────────┴───────────┴─────────────┘  │
             │   Push servers (shared, 1 port total)    │
             │  ┌───────────────┬──────────────────────┐ │
             │  │  RTMP  :1935  │      SRT  :9999      │ │
             │  └───────────────┴──────────────────────┘ │
             └──────────────────────────────────────────┘
                                 │  MPEG-TS chunks
                        ┌────────▼─────────┐
                        │   Buffer Hub     │  ring buffer / stream
                        └────┬───────┬─────┘
                             │       │
          ┌──────────────────┼───────┼──────────────────┐
          │                  │       │                  │
   ┌──────▼──────┐   ┌───────▼──┐   └──▼─────────┐  ┌──▼─────┐
   │  Transcoder │   │Publisher │     │    DVR    │  │Manager │
   │  FFmpeg pool│   │HLS  DASH │     │ TS+index  │  │failover│
   │  ABR ladder │   │RTSP RTMP │     │ playlist  │  │ health │
   └─────────────┘   │SRT  push │     └───────────┘  └────────┘
                     └──────────┘
                                          │
                              ┌───────────▼──────────┐
                              │      Event Bus       │
                              └───────────┬──────────┘
                                          │
                              ┌───────────▼──────────┐
                              │  Hooks dispatcher    │
                              │   HTTP  ·  Kafka     │
                              └──────────────────────┘
```

**Core data flow:** every MPEG-TS packet written by the Ingestor flows through the Buffer Hub exactly once. Publisher, Transcoder, DVR, and the Stream Manager health sampler are independent subscribers reading from the same in-memory ring buffer. No packet is ever re-fetched from the network.

**When ABR transcoding is active**, two buffer namespaces are used per stream:

```text
Ingestor → $raw$<code> → Transcoder → $r$<code>$track_1 → Publisher (rendition 1)
                                    → $r$<code>$track_2 → Publisher (rendition 2)
           $raw$<code> → DVR        (records raw stream, not transcoded output)
```

---

## Supported Protocols

### Pull mode — server connects to the remote source

| URL | Protocol | Notes |
| --- | -------- | ----- |
| `rtmp://server/app/key` | RTMP | Native Go pull; AVCC→Annex-B and ADTS wrapping handled internally |
| `rtmps://server/app/key` | RTMPS | Currently *not* supported on pull; RTMPS works on push-out |
| `rtsp://camera:554/stream` | RTSP | Native Go pull, RTCP A/V sync, H.264 + H.265 + AAC |
| `srt://relay:9999?streamid=key` | SRT | Native Go pull, caller mode |
| `udp://239.1.1.1:5000` | UDP MPEG-TS | Unicast + multicast; RTP header auto-stripped |
| `http://cdn/live.ts` | HTTP stream | Raw MPEG-TS over HTTP/HTTPS |
| `https://cdn/playlist.m3u8` | HLS | Native M3U8 parser, segment fetch with retry |
| `file:///recordings/src.ts` | File | Local filesystem; `?loop=true` for looping |
| `/absolute/path/to/src.ts` | File | Bare absolute path also accepted |
| `s3://bucket/key?region=ap-1` | AWS S3 | GetObject stream; S3-compatible via `?endpoint=` |

### Push mode — external encoder connects to Open Streamer

| URL                              | Protocol  | Notes                                     |
|----------------------------------|-----------|-------------------------------------------|
| `rtmp://0.0.0.0:1935/live/key`   | RTMP push | OBS, hardware encoders, `ffmpeg -f flv`   |
| `srt://0.0.0.0:9999?streamid=key`| SRT push  | StreamID carries the stream key           |

Push mode is detected automatically when the URL host is a wildcard address (`0.0.0.0`, `::`).

**RTMP push relay architecture**: the encoder pushes to a shared RTMP relay; an internal pull worker connects loopback to the same relay. All codec conversion (AVCC→Annex-B, ADTS wrapping) is handled by the same RTMP reader as pull mode — no code duplication.

### Output (serve & push-out)

| Protocol      | Clients                           | URL pattern                                                                 |
|---------------|-----------------------------------|-----------------------------------------------------------------------------|
| HLS           | Browsers, iOS, Android, Smart TVs | `GET /{code}/index.m3u8`                                                    |
| DASH          | MPEG-DASH players, DRM            | `GET /{code}/index.mpd`                                                     |
| RTSP          | VLC, broadcast tools, IP cameras  | `rtsp://host:port/live/<code>`                                              |
| RTMP play     | Legacy players, CDN relays        | `rtmp://host:port/live/<code>`                                              |
| SRT listen    | Contribution-quality pull         | `srt://host:port?streamid=live/<code>`                                      |
| RTMP push-out | YouTube, Facebook, Twitch, CDN    | `rtmp://` and `rtmps://` (TLS) supported; configured per-stream in `push[]` |

---

## Quick Start

### Build from source

```bash
# Requires Go 1.25.9+ and FFmpeg on PATH (only needed for transcoding)
git clone https://github.com/ntt0601zcoder/open-streamer
cd open-streamer
make build       # → bin/open-streamer
make run         # run without building binary
```

Once running, the Swagger UI is available at `http://localhost:8080/swagger/`.

Default ports: `8080` (HTTP API + HLS/DASH), `1935` (RTMP), `8554` (RTSP), `9999` (SRT). All ports
are configurable at runtime via `POST /config`.

### Production install (Linux + systemd)

Production servers should not have the Go toolchain installed. Pre-built archives
(`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`) are
produced by the [Release workflow](.github/workflows/release.yml) and attached to every
GitHub Release.

**Trigger a release build** (one of):

- Push a tag: `git tag v0.1.0 && git push origin v0.1.0` → builds all targets and
  publishes a GitHub Release with archives + `SHA256SUMS`.
- Manual: GitHub → Actions → *Release* → *Run workflow* → optional version label →
  archives appear as workflow artifacts (no Release created).

**Install on the server** (Linux example):

```bash
# Download the linux/amd64 archive from the Release page, then:
tar -xzf open-streamer-v0.1.0-linux-amd64.tar.gz
cd open-streamer-linux-amd64
sudo build/install.sh

systemctl status open-streamer
journalctl -u open-streamer -f       # follow logs
```

To upgrade: download the new archive, extract over the previous folder, re-run
`sudo build/install.sh` — idempotent, won't touch `/var/lib/open-streamer/`.

To uninstall (data dir preserved):

```bash
sudo build/install.sh uninstall      # from extracted archive
# OR from a git checkout:
sudo make uninstall-service
```

**Local install from a git checkout** (when Go is available on the same machine):

```bash
sudo make install-service            # builds + installs + enables in one step
```

The installer creates a system user `open-streamer`, copies the binary to `/usr/local/bin/`,
installs the unit at `/etc/systemd/system/open-streamer.service`, and uses
`/var/lib/open-streamer/` as the JSON store. To customize storage backend, log level, or
service user, edit the unit with `sudo systemctl edit open-streamer`.

GPU transcoding (NVENC / VAAPI / QSV) works out of the box — the binary uses the host's
ffmpeg and GPU drivers directly. Make sure `ffmpeg -hwaccels` shows the backend you want
before starting the service.

### First stream (30 seconds)

```bash
# 1. Create a stream that ingests from an RTMP push (OBS / FFmpeg)
curl -X PUT http://localhost:8080/streams/demo \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "Demo",
    "inputs": [{"url": "rtmp://0.0.0.0:1935/live/demo", "priority": 0}],
    "protocols": {"hls": true, "dash": true}
  }'

# 2. Start the stream
curl -X POST http://localhost:8080/streams/demo/start

# 3. Push from FFmpeg
ffmpeg -re -f lavfi -i testsrc=size=1280x720:rate=25 \
       -f lavfi -i sine=frequency=440 \
       -c:v libx264 -preset veryfast -c:a aac \
       -f flv rtmp://localhost:1935/live/demo

# 4. Play HLS
open http://localhost:8080/demo/index.m3u8
```

---

## Stream Management

All stream configuration is managed through the REST API. A **stream** is the central entity — it describes every aspect of how one live channel is ingested, processed, and delivered.

### Core concepts

| Concept | Description |
|---------|-------------|
| **StreamCode** | Unique identifier (`a-zA-Z0-9_-`, max 128 chars). Used in API paths, filesystem paths, and buffer IDs. |
| **Input** | A source URL. Each stream can have multiple inputs ordered by `priority` (lower = higher). The Stream Manager monitors health and switches on failure. |
| **Transcoder config** | Per-stream encoding: video profiles (resolution, bitrate, codec, HW accel), audio encoding, copy/passthrough modes. |
| **Output protocols** | Which delivery endpoints are opened: `hls`, `dash`, `rtsp`, `rtmp`, `srt`. |
| **Push destinations** | External platforms the server actively re-streams to (YouTube, Facebook, CDN relay). |
| **DVR config** | Per-stream: `enabled`, `segment_duration`, `retention_sec`, `storage_path`, `max_size_gb`. |

### Example: ABR transcoding with failover inputs

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
  "protocols": { "hls": true, "dash": true, "rtsp": true },
  "push": [
    { "url": "rtmp://a.rtmp.youtube.com/live2/xxxx", "enabled": true, "comment": "YouTube Live" }
  ],
  "dvr": {
    "enabled": true,
    "segment_duration": 6,
    "retention_sec": 172800
  }
}
```

### Example: OBS push (RTMP)

Configure OBS → **Server**: `rtmp://your-server:1935/live` · **Stream Key**: `mykey`

```json
PUT /streams/obs-channel
{
  "code": "obs-channel",
  "name": "OBS Stream",
  "inputs": [{ "url": "rtmp://0.0.0.0:1935/live/mykey", "priority": 0 }],
  "protocols": { "hls": true, "dash": true }
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

### Hot-reload — non-disruptive config updates

When a stream is running, `PUT /streams/{code}` applies only the parts that changed. The server computes a diff between the old and new config and routes each change to the appropriate service:

| What changed | Effect | What is NOT disrupted |
|---|---|---|
| Input URL / priority added or removed | Manager updates routing; active input may failover | Transcoder, Publisher, DVR |
| One ABR profile bitrate/resolution | Only that FFmpeg process is restarted | All other profiles, HLS/DASH/RTSP viewers |
| ABR profile added | New FFmpeg process started; HLS+DASH restart to update master playlist | RTSP/RTMP/SRT viewers |
| ABR profile removed | That FFmpeg process stopped; HLS+DASH restart | RTSP/RTMP/SRT viewers |
| HLS / DASH / RTSP toggled | Only that protocol goroutine started or stopped | All other protocols |
| Push destination added/removed | Only that push goroutine started or stopped | HLS, DASH, RTSP, other push destinations |
| DVR enabled/disabled | Recording started or stopped | Ingest, transcoder, publisher |
| Transcoder nil → non-nil (or mode change) | Full pipeline rebuild (unavoidable — buffer topology changes) | — |
| `disabled: true` | Full pipeline stop | — |

**Example — update one ABR profile without stopping the stream:**

```bash
# Change track_2 from 720p/2Mbps to 480p/800kbps — only that FFmpeg process restarts
curl -X PUT http://localhost:8080/streams/channel-1 \
  -H 'Content-Type: application/json' \
  -d '{
    "transcoder": {
      "video": {
        "profiles": [
          { "width": 1920, "height": 1080, "bitrate": 4000, "codec": "h264" },
          { "width": 854,  "height": 480,  "bitrate": 800,  "codec": "h264" }
        ]
      }
    }
  }'
```

### Hardware acceleration

Set `transcoder.global.hw_accel` to one of:

| Value | FFmpeg backend | Typical hardware |
| ----- | -------------- | ---------------- |
| `none` | `libx264` (software) | Any CPU |
| `nvenc` | `h264_nvenc` | NVIDIA GPU |
| `vaapi` | `h264_vaapi` | Intel / AMD GPU (Linux) |
| `videotoolbox` | `h264_videotoolbox` | Apple Silicon / macOS |
| `qsv` | `h264_qsv` | Intel Quick Sync |

---

## DVR & Timeshift

DVR records every stream as a single persistent recording (ID = stream code). Recording resumes transparently after server restart or signal loss, using `#EXT-X-DISCONTINUITY` markers for gaps. Segment numbering continues from where it left off.

### Storage layout

```
./dvr/{streamCode}/
  index.json       # lightweight metadata: segment count, total bytes, gap list
  playlist.m3u8    # HLS EVENT/VOD with #EXT-X-PROGRAM-DATE-TIME per segment
  000000.ts
  000001.ts
  ...
```

### Recording lifecycle

```bash
POST /streams/{code}/recordings/start    # start recording
POST /streams/{code}/recordings/stop     # stop (playlist becomes VOD)
GET  /recordings/{code}                  # lifecycle metadata
GET  /recordings/{code}/info             # dvr_range, gaps, segment_count, disk_usage
```

### Playback & timeshift

```bash
# Full VOD playlist
GET /recordings/{code}/playlist.m3u8

# Serve individual segment
GET /recordings/{code}/000042.ts

# Timeshift — absolute wall time window
GET /recordings/{code}/timeshift.m3u8?from=2026-04-06T14:30:00Z&duration=3600

# Timeshift — relative to recording start
GET /recordings/{code}/timeshift.m3u8?offset_sec=1800&duration=3600
```

The `timeshift.m3u8` response is computed on every request from the on-disk `playlist.m3u8` — no additional storage required.

---

## REST API

Base URL: `http://localhost:8080`  
Interactive docs: `http://localhost:8080/swagger/`

### Streams

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/streams` | List all streams |
| `GET` | `/streams/{code}` | Get stream |
| `PUT` | `/streams/{code}` | Create or update stream |
| `DELETE` | `/streams/{code}` | Delete stream |
| `POST` | `/streams/{code}/start` | Start ingest + publishing |
| `POST` | `/streams/{code}/stop` | Stop stream |
| `GET` | `/streams/{code}/status` | Live runtime status |

### Recordings

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/streams/{code}/recordings/start` | Start DVR recording |
| `POST` | `/streams/{code}/recordings/stop` | Stop DVR recording |
| `GET` | `/streams/{code}/recordings` | List recordings for stream |
| `GET` | `/recordings/{rid}` | Recording lifecycle metadata |
| `DELETE` | `/recordings/{rid}` | Delete recording metadata |
| `GET` | `/recordings/{rid}/info` | DVR range, gaps, segment count, disk usage |
| `GET` | `/recordings/{rid}/playlist.m3u8` | Full VOD playlist |
| `GET` | `/recordings/{rid}/timeshift.m3u8` | Dynamic timeshift playlist |
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

**Event types**: `stream.started`, `stream.stopped`, `stream.created`, `stream.deleted`, `input.degraded`, `input.failover`, `recording.started`, `recording.stopped`, `transcoder.error`

**Hook example:**

```json
POST /hooks
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

Kafka hook example (type `kafka`):

```json
{
  "name": "Kafka events",
  "type": "kafka",
  "target": "my-events-topic",
  "event_types": ["stream.started", "recording.started"],
  "enabled": true
}
```

Configure Kafka brokers in `config.yaml`:

```yaml
hooks:
  kafka_brokers: ["kafka:9092"]
```

### Media delivery

| Path | Description |
|------|-------------|
| `GET /{code}/index.m3u8` | HLS master playlist (or single-rendition media playlist) |
| `GET /{code}/index.mpd` | DASH manifest |
| `GET /{code}/*` | HLS segments, DASH init + segments |

### Health

| Path | Description |
| ---- | ----------- |
| `GET /healthz` | Liveness probe |
| `GET /readyz` | Readiness probe |

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

# Buffer — ring buffer capacity per stream (MPEG-TS packets)
OPEN_STREAMER_BUFFER_CAPACITY=1000

# Transcoder
OPEN_STREAMER_TRANSCODER_MAX_WORKERS=4
OPEN_STREAMER_TRANSCODER_FFMPEG_PATH=ffmpeg
OPEN_STREAMER_TRANSCODER_MAX_RESTARTS=5

# Publisher — filesystem packaging only (HLS, DASH).
# RTMP/RTSP/SRT ports are configured under `listeners.*` below because the
# same port serves both ingest (push) and play (pull) traffic.
OPEN_STREAMER_PUBLISHER_HLS_DIR=./hls
OPEN_STREAMER_PUBLISHER_HLS_BASE_URL=http://localhost:8080
OPEN_STREAMER_PUBLISHER_DASH_DIR=./dash

# Listeners — one shared port per protocol for both ingest and play
OPEN_STREAMER_LISTENERS_RTMP_ENABLED=true
OPEN_STREAMER_LISTENERS_RTMP_LISTEN_HOST=0.0.0.0
OPEN_STREAMER_LISTENERS_RTMP_PORT=1935
OPEN_STREAMER_LISTENERS_RTSP_ENABLED=true
OPEN_STREAMER_LISTENERS_RTSP_LISTEN_HOST=0.0.0.0
OPEN_STREAMER_LISTENERS_RTSP_PORT=554
OPEN_STREAMER_LISTENERS_RTSP_TRANSPORT=tcp
OPEN_STREAMER_LISTENERS_SRT_ENABLED=true
OPEN_STREAMER_LISTENERS_SRT_LISTEN_HOST=0.0.0.0
OPEN_STREAMER_LISTENERS_SRT_PORT=9999
OPEN_STREAMER_LISTENERS_SRT_LATENCY_MS=120

# Manager — input health timeout
OPEN_STREAMER_MANAGER_INPUT_PACKET_TIMEOUT_SEC=30

# Hooks — delivery worker pool and Kafka brokers
OPEN_STREAMER_HOOKS_WORKER_COUNT=4
OPEN_STREAMER_HOOKS_KAFKA_BROKERS=kafka:9092

# Metrics
OPEN_STREAMER_METRICS_ADDR=:9091
OPEN_STREAMER_METRICS_PATH=/metrics

# Logging
OPEN_STREAMER_LOG_LEVEL=info     # debug | info | warn | error
OPEN_STREAMER_LOG_FORMAT=text    # text | json
```

> **DVR has no global config.** Each stream opts in via `stream.dvr.enabled = true`.
>
> **HLS and DASH dirs must be different** when both are active.

---

## Development

### Prerequisites

| Requirement | Version | Notes |
| ----------- | ------- | ----- |
| Go | 1.25.9+ | |
| FFmpeg | any recent | Only needed for transcoding; not needed for passthrough/ingest-only |
| systemd | any | Linux production deploy via `make install-service` |
| Docker | any | Optional — only needed for testcontainer-based integration tests (Postgres / MongoDB / RTMP) |

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
make install-service   # build + install as systemd service (Linux, sudo)
make uninstall-service # stop and remove systemd service (sudo)
```

Run a single test:

```bash
go test -run TestRTMPReader ./internal/ingestor/pull/
go test -run TestRegistry   ./internal/ingestor/
```

### CI

GitHub Actions runs on every push / PR to `main`:

| Job | What it checks |
| --- | -------------- |
| `go mod tidy` | `go.mod` / `go.sum` are up to date |
| `test (go 1.25.9)` | `go test -race -shuffle=on` on the minimum supported version |
| `test (go stable)` | Same on the latest stable Go |
| `swagger docs` | Regenerates OpenAPI spec; commits if changed |
| `golangci-lint` | Static analysis (allow-fail) |
| `govulncheck` | Known vulnerability scan |

---

## Testing

### Strategy

- **Unit tests** — pure logic, no I/O; fast and fully deterministic
- **Integration tests** — SQL store (Postgres), Mongo store use real containers; skipped automatically when Docker is unavailable
- **RTMP integration test** — spins up an RTMP server container and an FFmpeg publisher; skipped without Docker

### Coverage by package

| Package | What's covered |
| ------- | -------------- |
| `pkg/protocol` | URL detection, push-listen detection, MPEG-TS helpers |
| `internal/ingestor` | `NewReader` dispatch, Registry CRUD + concurrent access, worker reconnect logic |
| `internal/ingestor/pull` | File reader, HTTP mock, UDP loopback, RTMP packet parsing, RTP header stripping, RTMP integration (Docker) |
| `internal/manager` | `selectBest`, `collectTimeoutIfNeeded`, `collectProbeIfNeeded` |
| `internal/transcoder` | FFmpeg args construction, scale filter, codec normalisation, audio encoding |
| `internal/publisher` | HLS segmenter, codec string, manifest generation, discontinuity, context cancel |
| `internal/dvr` | Playlist parse, index read/write round-trip, atomic write |
| `internal/store/json` | Full CRUD + concurrent read-modify-write safety (race detector) |
| `internal/store/sql` | Full CRUD + concurrent access (containerised Postgres) |
| `internal/store/mongo` | Full CRUD + concurrent access (containerised MongoDB) |

---

## Project Layout

```
├── cmd/server/         # Binary entry point — DI wiring, graceful shutdown
├── config/             # Server config struct + Viper loader
├── data/               # Runtime data (default JSON store dir)
├── docs/
│   ├── DESIGN.md           # Detailed design notes per subsystem
│   ├── EVENTS.md           # Event payload schemas and delivery guide
│   └── FEATURES_CHECKLIST.md
├── api/
│   └── docs/           # Auto-generated OpenAPI/Swagger spec
├── internal/
│   ├── api/            # HTTP server + REST handlers
│   │   └── handler/    # StreamHandler, RecordingHandler, HookHandler
│   ├── buffer/         # Buffer Hub — ring buffer, fan-out subscriptions
│   ├── coordinator/    # Pipeline wiring (buffer → manager → transcoder → publisher → DVR)
│   ├── domain/         # Domain types: Stream, Input, Recording, Event, Hook, …
│   ├── dvr/            # DVR recording service (TS muxer, playlist, index, retention)
│   ├── events/         # In-process pub/sub event bus
│   ├── hooks/          # Webhook + Kafka dispatcher with retry and HMAC
│   ├── ingestor/       # Ingest orchestrator
│   │   ├── pull/       # Pull readers: RTMP, RTSP, SRT, UDP, HLS, HTTP, File, S3
│   │   └── push/       # Push servers: RTMP (:1935), SRT (:9999)
│   ├── manager/        # Stream Manager — health monitoring, failover
│   ├── mediaserve/     # Static HTTP file serving for HLS/DASH segments
│   ├── metrics/        # Prometheus collectors
│   ├── publisher/      # Output delivery: HLS, DASH, RTSP, RTMP serve, SRT listen, RTMP push-out
│   ├── store/          # Repository interfaces + JSON / SQL / MongoDB drivers
│   ├── transcoder/     # FFmpeg transcoding worker pool
│   └── tsmux/          # MPEG-TS muxing utilities (AVPacket → 188-byte TS)
├── pkg/
│   ├── ffmpeg/         # FFmpeg subprocess helper
│   ├── logger/         # structured logger initialisation
│   └── protocol/       # URL → protocol Kind detection
└── build/
    ├── open-streamer.service  # systemd unit for production deploy
    └── install.sh             # build + install/uninstall helper (Linux)
```

---

## Contributing

Contributions are welcome. Please follow these steps:

1. **Fork** the repository and create a feature branch (`git checkout -b feat/my-feature`).
2. **Code style** — run `make fmt` (gofumpt) and `make lint` before committing.
3. **Tests** — add or update tests for any changed behaviour; ensure `make test` passes.
4. **Commit messages** — use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`).
5. **Pull request** — open a PR against `main`; describe *what* changed and *why*.

### Key design constraints (please read before submitting)

- **Buffer Hub is the only data source** for consumers — never read from network directly in Publisher, DVR, or Transcoder.
- **Failover is Go-level** (Stream Manager), never by restarting FFmpeg.
- **Ingestor uses goroutines**, not one process per stream.
- **`internal/store/` is the only package** allowed to import database drivers.
- **Modules communicate through interfaces** — never import sibling `internal/` packages directly.
- **Write never blocks** — any consumer that can't keep up drops packets silently.

### Reporting issues

Please open a [GitHub issue](https://github.com/ntt0601zcoder/open-streamer/issues) and include:

- Go version (`go version`)
- Open Streamer version or commit hash
- Minimal reproduction steps or config

---

## License

[MIT](LICENSE) © ntt0601zcoder
