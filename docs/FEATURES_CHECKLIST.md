# Open-Streamer — Feature Checklist

Legend for **Completion**:

| Level | Meaning |
|-------|---------|
| **Complete** | Implemented and usable for the described scope |
| **Partial** | Works with known limitations or narrow codec/path support |
| **Stub** | Registered in config/API but no real implementation |
| **Schema only** | Domain / API / persistence fields exist; not wired into live pipeline |
| **Planned** | Documented intent only |

---

## Core Platform

| Feature | Completion | Notes |
|---------|------------|-------|
| Configuration (`config/`, viper/env) | Complete | Single `Config` loaded at startup; OPEN_STREAMER_ env prefix |
| Dependency injection (`samber/do/v2`) | Complete | All services wired in `cmd/server/main.go` |
| Structured logging (`slog`, `pkg/logger`) | Complete | `text` / `json` format; configurable level |
| Graceful shutdown (SIGINT/SIGTERM) | Complete | 10 s timeout, all services shut down in reverse order |
| Prometheus metrics (`internal/metrics`) | Partial | Module present; full coverage of all subsystems not yet verified |

---

## Storage & API

| Feature | Completion | Notes |
|---------|------------|-------|
| Stream repository — JSON file | Complete | Default store; configurable dir |
| Stream repository — SQL (Postgres/MySQL) | Complete | pgx + sqlx; JSONB storage; auto-migrate on startup |
| Stream repository — MongoDB | Complete | mongo-driver v2; BSON+JSON; indexes created on startup |
| Recording repository | Complete | All 3 drivers; DVR writes recording metadata on every segment flush |
| Hook repository | Complete | Full CRUD + test endpoint |
| REST API — streams CRUD / start / stop / status | Complete | Chi router under `/streams` |
| REST API — recordings (start / stop / list / get / delete / info) | Complete | Full lifecycle + `info` endpoint (dvr_range, gaps, size) |
| REST API — recordings playlist.m3u8 | Complete | Reads `playlist.m3u8` directly from `SegmentDir` |
| REST API — recordings timeshift.m3u8 | Complete | Dynamic VOD M3U8; `from`, `offset_sec`, `duration` query params |
| REST API — recordings segment serve | Complete | `GET /recordings/{rid}/{file}` — path traversal protected |
| REST API — hooks CRUD + test (HTTP) | Complete | HTTP hook test fires real outbound request |
| REST API — hooks test (Kafka) | Stub | Returns "not implemented" |
| OpenAPI / Swagger | Complete | Generated via `swag`; served at `/swagger/` |
| HTTP static delivery — HLS master + segments | Complete | `/{code}/index.m3u8`, `/{code}/*` |
| HTTP static delivery — DASH MPD + segments | Complete | `/{code}/index.mpd`, `/{code}/*` |
| Health / readiness probes | Complete | `/healthz`, `/readyz` |

---

## Buffer Hub

| Feature | Completion | Notes |
|---------|------------|-------|
| In-memory ring buffer per stream ID | Complete | Fan-out via independent subscribers; write never blocks |
| Raw ingest buffer (`$raw$<code>`) | Complete | Created when internal transcoding is active |
| Rendition buffers (`$r$<code>$track_N`) | Complete | One buffer per ABR ladder rung |
| Playback buffer selection (`PlaybackBufferID`) | Complete | Returns best track if ABR, logical stream code otherwise |
| Slow consumer packet drop | Complete | `default:` in fan-out; ingestor is never blocked by a slow consumer |

---

## Ingest

| Feature | Completion | Notes |
|---------|------------|-------|
| Pull — HLS | Complete | M3U8 parser, segment fetch, retry + backoff |
| Pull — HTTP (raw MPEG-TS) | Complete | |
| Pull — RTSP | Complete | Rewritten with gortsplib v5: RTCP A/V sync, RTP reorder buffer, proper DTS extraction (DTSExtractor), H.264 + H.265 + AAC |
| Pull — RTMP | Complete | AVCC→Annex-B, ADTS wrapping via TSDemuxPacketReader |
| Pull — SRT (caller) | Partial | Code path complete; HLS/DASH combos not yet verified in manual matrix |
| Pull — UDP / MPEG-TS | Complete | Unicast + multicast; auto-strip RTP header; OS-assigned port for tests |
| Pull — File (`.ts`, `.mp4`, `.flv`) | Complete | Loop mode; paced playback to simulate real-time |
| Pull — S3 | Complete | GetObject stream; S3-compatible via `?endpoint=` |
| Push — RTMP listen (:1935) | Complete | gomedia relay → loopback joy4 pull → Buffer Hub |
| Push — SRT listen (:9999) | Complete | streamid `live/<code>` → registry → Buffer Hub |
| Configurable write target (raw vs main buffer) | Complete | `mediaBufferID` passed from coordinator |
| Exponential-backoff reconnect | Complete | Per-input `Net.ReconnectDelaySec`, `ReconnectMaxDelaySec` |

---

## Stream Manager (Failover)

| Feature | Completion | Notes |
|---------|------------|-------|
| Multi-input registration with priority | Complete | Lower priority value = higher priority |
| Packet timestamp health tracking | Complete | `RecordPacket` called on every ingest packet |
| Input timeout detection | Complete | Configurable via `manager.input_packet_timeout_sec` |
| Degraded state + failback probe | Complete | Probes run in background goroutines with cooldown |
| Seamless failover (Go-level, no FFmpeg restart) | Complete | Old ingestor stops writing; new one starts; no buffer flush |
| Failover events (`input.degraded`, `input.failover`) | Complete | Published to event bus; triggers HLS discontinuity counter |

---

## Transcoder

| Feature | Completion | Notes |
|---------|------------|-------|
| FFmpeg subprocess (stdin TS → stdout TS) | Complete | `exec.CommandContext`; killed with context |
| Multiple profiles = multiple FFmpeg processes | Complete | One encoder per `track_N`; shared raw ingest buffer |
| Bounded worker pool | Complete | Semaphore; `transcoder.max_workers` (default 4) |
| Video ladder config (`VideoProfile` slice) | Complete | Stable IDs: `track_1`, `track_2`, … |
| Audio encoding config | Complete | AAC/MP3/Opus/AC3/copy |
| Copy video / copy audio modes | Complete | `video.copy: true` / `audio.copy: true` |
| Extra FFmpeg args passthrough | Complete | `global.extra_args` |
| Hardware acceleration (NVENC / VAAPI / VideoToolbox / QSV) | Complete | `global.hw_accel` maps to encoder + hwaccel flags |
| FFmpeg stderr filtering | Complete | Timestamp discontinuity, frame reorder → debug; real errors → warn/error |
| Passthrough / remux mode (no FFmpeg) | Complete | `transcoder.mode: passthrough` or `remux` skips FFmpeg; ingestor writes raw MPEG-TS directly to publisher buffer |

---

## Publisher — Delivery

| Feature | Completion | Notes |
|---------|------------|-------|
| HLS — single rendition | Complete | Native TS segmenter + media playlist |
| HLS — ABR (master + `track_N` sub-playlists) | Complete | When transcoding ladder is active |
| HLS — `#EXT-X-DISCONTINUITY` on failover | Complete | Per-variant generation counter; exactly one tag per failover |
| DASH — single representation (fMP4 + dynamic MPD) | Complete | H.264 + H.265 + AAC supported; MP3 skipped |
| DASH — ABR (root MPD + per-track directories) | Complete | Audio packaged only on best track folder |
| RTSP play (H.264 + AAC, gortsplib) | Complete | Shared gortsplib.Server; lazy stream mount after codec detection; clients use `rtsp://host:port_min/live/<code>` |
| RTMP play (gomedia) | Complete | Shared port with ingest (:1935) via `PlayFunc` callback; optional dedicated port via `publisher.rtmp.port`; clients use `rtmp://host:port/live/<code>` |
| SRT listen (gosrt) | Stub | Fields reserved in publisher.Service; no serve goroutine yet |
| RTMP push out (re-stream to platform) | Complete | `rtmp://` destinations; auto-reconnect with backoff; RTMPS/WebRTC return clear error |
| RTS / WebRTC (WHEP) | Stub | Logs "not implemented"; use HLS/DASH for browsers |

---

## Coordinator & Lifecycle

| Feature | Completion | Notes |
|---------|------------|-------|
| Start pipeline (buffer → manager → publisher → transcoder) | Complete | Creates raw + rendition buffers as needed |
| Auto-start DVR when `stream.dvr.enabled` | Complete | Called from `Coordinator.Start` after publisher setup |
| Auto-stop DVR when stream stops | Complete | `Coordinator.Stop` calls `dvr.StopRecording` before teardown |
| Stop pipeline / teardown all buffers | Complete | Main + `$raw$` + `$r$…` rendition buffers all cleaned up |
| Bootstrap persisted streams on startup | Complete | Skips stopped, disabled, and zero-input streams |

---

## DVR

| Feature | Completion | Notes |
|---------|------------|-------|
| Persistent recording (ID = stream code) | Complete | One recording per stream; survives restarts |
| Segment writing (MPEG-TS, configurable duration) | Complete | PTS-based cutting; wall-clock fallback for raw-TS sources |
| Gap detection + `#EXT-X-DISCONTINUITY` | Complete | Gap timer = 2 × segment duration; partial segment flushed on gap |
| Gap recording in `index.json` | Complete | `DVRGap{From, To, Duration}` appended to `idx.Gaps` |
| Resume after restart (playlist parsing) | Complete | `parsePlaylist` rebuilds in-memory segment list from `playlist.m3u8` |
| `#EXT-X-PROGRAM-DATE-TIME` in playlist | Complete | Written before first segment and after every `#EXT-X-DISCONTINUITY` |
| `index.json` (lightweight metadata) | Complete | Atomic write (tmp→rename); no per-segment data |
| Retention by time (`retention_sec`) | Complete | Oldest viewable = `now − RetentionSec`; older segments deleted |
| Retention by size (`max_size_gb`) | Complete | Oldest segments pruned when total size exceeds cap |
| Gap list pruning on retention | Complete | Gaps whose `To` < new oldest segment wall time are removed |
| VOD playlist endpoint | Complete | `GET /recordings/{rid}/playlist.m3u8` — reads file directly |
| Timeshift endpoint (absolute) | Complete | `?from=RFC3339&duration=N` — filters segments by wall time window |
| Timeshift endpoint (relative) | Complete | `?offset_sec=N&duration=N` — anchored to first segment wall time |
| Segment file serving | Complete | `GET /recordings/{rid}/{file}` — path traversal sanitised with `filepath.Base` |
| DVR info endpoint | Complete | `GET /recordings/{rid}/info` — dvr_range, gaps, segment_count, total_size_bytes |

---

## Events & Hooks

| Feature | Completion | Notes |
|---------|------------|-------|
| In-process event bus | Complete | Typed events; bounded queue (512); worker pool |
| Event types | Complete | `stream.*`, `input.*`, `recording.*`, `segment.written` |
| HTTP webhook delivery | Complete | Retries, timeout, optional HMAC (`X-OpenStreamer-Signature`) |
| NATS delivery | Stub | Returns "not implemented" |
| Kafka delivery | Stub | Returns "not implemented" |

---

## Domain Extras (Not in Live Pipeline)

| Feature | Completion | Notes |
|---------|------------|-------|
| Watermark config on `Stream` | Schema only | Fields exist; not applied in transcoder graph |
| Thumbnail config on `Stream` | Schema only | Fields exist; not generated alongside outputs |
| `TranscodeMode` (passthrough / remux) | Complete | `transcoder.mode: passthrough/remux` bypasses FFmpeg entirely |

---

## Testing & Quality

| Feature | Completion | Notes |
|---------|------------|-------|
| Unit tests — protocol detection | Complete | |
| Unit tests — buffer ring / fan-out | Complete | |
| Unit tests — ingestor dispatch + registry | Complete | |
| Unit tests — pull readers (File, HTTP, UDP, RTMP packet parsing) | Complete | |
| Unit tests — manager state machine | Partial | Core cases covered |
| Unit tests — transcoder args construction | Partial | |
| Unit tests — publisher HLS segmenter | Partial | |
| Unit tests — dvr playlist parsing | Partial | |
| CI (GitHub Actions) | Complete | `build`, `test`, `lint` (allow-fail), `govulncheck` jobs |
| golangci-lint (`.golangci.yml`) | Complete | 0 issues |
| gofumpt formatting | Complete | Enforced via CI formatter step |

---

## Manual Test Matrix — Ingest × Publisher

**OK** = playback acceptable in manual test · **—** = not tested · **Issue** = visible stutter / lag

### Without internal transcoding

| Ingest (pull) | HLS | DASH |
|---------------|-----|------|
| File | OK | OK |
| HLS | OK | OK |
| RTMP | OK | OK |
| SRT | — | — |
| RTSP | — | — |

### With internal transcoding (FFmpeg ABR ladder)

| Ingest (pull) | HLS | DASH |
|---------------|-----|------|
| File | OK | OK |
| HLS | OK | OK |
| RTMP | OK | OK |
| SRT | — | — |
| RTSP | — | — |

### Push ingest

| Ingest (push) | HLS | DASH |
|---------------|-----|------|
| RTMP push (OBS/FFmpeg) | OK | OK |
| SRT push | — | — |

---

## Operational Assumptions

- **FFmpeg** must be on `PATH` (or set via `transcoder.ffmpeg_path`) for transcoding. Not required for passthrough / pure ingest.
- **HLS and DASH dirs must differ** when both publishers are active (`publisher.hls.dir` ≠ `publisher.dash.dir`).
- **DVR has no global enable/disable.** Each stream opt-in via `stream.dvr.enabled = true`.
- **Failover timestamp jumps** produce `#EXT-X-DISCONTINUITY` in HLS and are logged at debug level from FFmpeg stderr.

---

*Updated against codebase state 2026-04-07 (RTMP play wired to shared port). Update this file when feature status changes.*
