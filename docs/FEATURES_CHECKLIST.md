# Open-Streamer — Feature Checklist

Legend for **Completion**:

| Level | Meaning |
| ------- | --------- |
| **Complete** | Implemented and usable for the described scope |
| **Partial** | Works with known limitations or narrow codec/path support |
| **Stub** | Registered in config/API but no real implementation |
| **Schema only** | Domain / API / persistence fields exist; not wired into live pipeline |
| **Planned** | Documented intent only |

---

## Core Platform

| Feature | Completion | Notes |
| --------- | ------------ | ------- |
| Configuration (`config/`, file + env layered) | Complete | Single `Config` loaded at startup; OPEN_STREAMER_ env prefix |
| Dependency injection container | Complete | All services wired in `cmd/server/main.go` |
| Structured logging (`pkg/logger`) | Complete | `text` / `json` format; configurable level |
| Graceful shutdown (SIGINT/SIGTERM) | Complete | 10 s timeout, all services shut down in reverse order |
| Prometheus metrics (`internal/metrics`) | Complete | Wired in ingestor, manager, transcoder, DVR, coordinator; stream start time, bytes/packets, failovers, restarts, active workers |

---

## Storage & API

| Feature | Completion | Notes |
| --------- | ------------ | ------- |
| Stream repository — JSON file | Complete | Default store; configurable dir |
| Stream repository — SQL (Postgres/MySQL) | Complete | JSONB storage; auto-migrate on startup |
| Stream repository — MongoDB | Complete | BSON+JSON document storage; indexes created on startup |
| Recording repository | Complete | All 3 drivers; DVR writes recording metadata on every segment flush |
| Hook repository | Complete | Full CRUD + test endpoint |
| REST API — streams CRUD / start / stop / status | Complete | Chi router under `/streams` |
| REST API — `PUT /streams/{code}` hot-reload | Complete | Calls `coordinator.Update`; only changed components are restarted (see Coordinator section) |
| REST API — recordings (start / stop / list / get / delete / info) | Complete | Full lifecycle + `info` endpoint (dvr_range, gaps, size) |
| REST API — recordings playlist.m3u8 | Complete | Reads `playlist.m3u8` directly from `SegmentDir` |
| REST API — recordings timeshift.m3u8 | Complete | Dynamic VOD M3U8; `from`, `offset_sec`, `duration` query params |
| REST API — recordings segment serve | Complete | `GET /recordings/{rid}/{file}` — path traversal protected |
| REST API — hooks CRUD + test (HTTP) | Complete | HTTP hook test fires real outbound request |
| REST API — hooks test (Kafka) | Complete | `DeliverTestEvent` routes to `deliverKafka`; brokers via `hooks.kafka_brokers` |
| OpenAPI / Swagger | Complete | Auto-generated spec served at `/swagger/` |
| HTTP static delivery — HLS master + segments | Complete | `/{code}/index.m3u8`, `/{code}/*` |
| HTTP static delivery — DASH MPD + segments | Complete | `/{code}/index.mpd`, `/{code}/*` |
| Health / readiness probes | Complete | `/healthz`, `/readyz` |

---

## Buffer Hub

| Feature | Completion | Notes |
| --------- | ------------ | ------- |
| In-memory ring buffer per stream ID | Complete | Fan-out via independent subscribers; write never blocks |
| Raw ingest buffer (`$raw$<code>`) | Complete | Created when internal transcoding is active |
| Rendition buffers (`$r$<code>$track_N`) | Complete | One buffer per ABR ladder rung |
| Playback buffer selection (`PlaybackBufferID`) | Complete | Returns best track if ABR, logical stream code otherwise |
| Slow consumer packet drop | Complete | `default:` in fan-out; ingestor is never blocked by a slow consumer |

---

## Ingest

| Feature | Completion | Notes |
| --------- | ------------ | ------- |
| Pull — HLS | Complete | M3U8 parser, segment fetch, retry + backoff |
| Pull — HTTP (raw MPEG-TS) | Complete | |
| Pull — RTSP | Complete | RTCP A/V sync, RTP reorder buffer, proper DTS extraction, H.264 + H.265 + AAC |
| Pull — RTMP | Complete | AVCC→Annex-B, ADTS wrapping via TSDemuxPacketReader |
| Pull — SRT (caller) | Partial | Code path complete; HLS/DASH combos not yet verified in manual matrix |
| Pull — UDP / MPEG-TS | Complete | Unicast + multicast; auto-strip RTP header; OS-assigned port for tests |
| Pull — File (`.ts`, `.mp4`, `.flv`) | Complete | Loop mode; paced playback to simulate real-time |
| Pull — S3 | Complete | GetObject stream; S3-compatible via `?endpoint=` |
| Pull — `copy://<code>` (in-process stream copy) | Planned | Subscribes to another stream's `PlaybackBufferID` (best rendition if ABR, raw otherwise). Cycle detection + failover via existing input priority. See [PLAN_COPY_PROTOCOL.md](./PLAN_COPY_PROTOCOL.md). |
| Push — RTMP listen (:1935) | Complete | Shared RTMP relay → loopback pull worker → Buffer Hub |
| Push — SRT listen (:9999) | Complete | streamid `live/<code>` → registry → Buffer Hub |
| Configurable write target (raw vs main buffer) | Complete | `mediaBufferID` passed from coordinator |
| Exponential-backoff reconnect | Complete | Per-input `Net.ReconnectDelaySec`, `ReconnectMaxDelaySec` |

---

## Stream Manager (Failover)

| Feature | Completion | Notes |
| --------- | ------------ | ------- |
| Multi-input registration with priority | Complete | Lower priority value = higher priority |
| Packet timestamp health tracking | Complete | `RecordPacket` called on every ingest packet |
| Input timeout detection | Complete | Configurable via `manager.input_packet_timeout_sec` |
| Degraded state + failback probe | Complete | Probes run in background goroutines with cooldown |
| Seamless failover (Go-level, no FFmpeg restart) | Complete | Old ingestor stops writing; new one starts; no buffer flush |
| Failover events (`input.degraded`, `input.failover`) | Complete | Published to event bus; triggers HLS discontinuity counter |
| All-inputs-exhausted detection | Complete | Stream status → `degraded` in store; auto-recovers to `active` when probe succeeds |
| Live input update (`UpdateInputs`) | Complete | Add/remove/update inputs without stopping the pipeline; active input removal triggers immediate failover |
| Live buffer write target update (`UpdateBufferWriteID`) | Complete | Called by coordinator on transcoder topology change; restarts active ingestor with new target |

---

## Transcoder

| Feature | Completion | Notes |
| --------- | ------------ | ------- |
| FFmpeg subprocess (stdin TS → stdout TS) | Complete | `exec.CommandContext`; killed with context |
| Multiple profiles = multiple FFmpeg processes | Complete | One encoder per `track_N`; shared raw ingest buffer |
| Per-profile independent context (`profileWorker`) | Complete | Each FFmpeg process has its own cancel func; stop/start one profile without affecting others |
| Bounded worker pool | Complete | Semaphore; `transcoder.max_workers` (default 4) |
| Video ladder config (`VideoProfile` slice) | Complete | Stable IDs: `track_1`, `track_2`, … |
| Audio encoding config | Complete | AAC/MP3/Opus/AC3/copy |
| Copy video / copy audio modes | Complete | `video.copy: true` / `audio.copy: true` |
| Extra FFmpeg args passthrough | Complete | `global.extra_args` |
| Hardware acceleration (NVENC / VAAPI / VideoToolbox / QSV) | Complete | `global.hw_accel` maps to encoder + hwaccel flags |
| FFmpeg stderr filtering | Complete | Timestamp discontinuity, frame reorder → debug; real errors → warn/error |
| Passthrough / remux mode (no FFmpeg) | Complete | `transcoder.mode: passthrough` or `remux` skips FFmpeg; ingestor writes raw MPEG-TS directly to publisher buffer |
| FFmpeg crash auto-restart with backoff (infinite) | Complete | Per-profile retry forever: 2 s → 4 s → … → 30 s cap. Pipeline never tears down on FFmpeg failure — streams self-heal once the underlying issue is resolved. |
| Crash log + event spam suppression | Complete | After 3 consecutive identical errors, warn-level logs drop to debug and `transcoder.error` events are emitted only on power-of-2 attempts (1, 2, 4, 8, 16…). Per-profile error history (last 5) and `TranscoderRestartsTotal` metric remain accurate for ops visibility. |
| `StopProfile(streamID, idx)` | Complete | Cancels one `profileWorker` context; only that FFmpeg process exits |
| `StartProfile(streamID, idx, target)` | Complete | Acquires semaphore slot, spawns new `profileWorker` with fresh context |

---

## Publisher — Delivery

| Feature | Completion | Notes |
| --------- | ------------ | ------- |
| HLS — single rendition | Complete | Native TS segmenter + media playlist |
| HLS — ABR (master + `track_N` sub-playlists) | Complete | When transcoding ladder is active |
| HLS — `#EXT-X-DISCONTINUITY` on failover | Complete | Per-variant generation counter; exactly one tag per failover |
| DASH — single representation (fMP4 + dynamic MPD) | Complete | H.264 + H.265 + AAC supported; MP3 skipped |
| DASH — ABR (root MPD + per-track directories) | Complete | Audio packaged only on best track folder |
| RTSP play (H.264 + AAC) | Complete | Shared RTSP server (default :554) configured under `listeners.rtsp.port`; lazy stream mount after codec detection; clients use `rtsp://host:port/live/<code>` |
| RTMP play | Complete | Shared port with ingest (default :1935) via play callback configured under `listeners.rtmp.port`; clients use `rtmp://host:port/live/<code>` |
| SRT listen | Complete | Shared SRT listener (default :9999) configured under `listeners.srt.port`; per-client buffer subscriber; raw MPEG-TS output; clients use `srt://host:port?streamid=live/<code>` |
| RTMP push out (re-stream to platform) | Complete | `rtmp://` (plain TCP) and `rtmps://` (TLS, default :443); built on `q191201771/lal` PushSession (replaced `yapingcat/gomedia` to fix Flussonic AMF panic). lal Push() blocks until publish ack so no separate readyCh / pending queue. Codec adapter is custom (`push_codec.go`): tracks SPS/PPS + AAC ASC, emits FLV sequence headers, builds video tags with proper `composition_time = PTS - DTS` so B-frames render correctly at the receiver (replaces lal's high-level `AvPacket2RtmpRemuxer` which dropped PTS). Per-input discontinuity tear-down handled in publisher `feedLoop`; auto-reconnect with backoff. |
| Per-protocol independent context | Complete | Each output goroutine (`"hls"`, `"dash"`, `"rtsp"`, `"push:<url>"`) has its own cancel func inside `streamState.protocols` |
| `UpdateProtocols(old, new)` | Complete | Only stops/starts protocols whose ON↔OFF state changed; connected RTSP/SRT viewers unaffected |
| `RestartHLSDASH(stream)` | Complete | Restarts only HLS + DASH goroutines when ABR ladder count changes; RTSP/RTMP/SRT unaffected |
| `UpdateABRMasterMeta(code, updates)` | Complete | Rewrites HLS master playlist in-place (no goroutine restart) when a profile's bitrate/resolution changes |
| ABR master playlist override (`SetRepOverride`) | Complete | Override persists across segment flushes; master rewritten within 50 ms |

---

## Coordinator & Lifecycle

| Feature | Completion | Notes |
| --------- | ------------ | ------- |
| Start pipeline (buffer → manager → publisher → transcoder) | Complete | Creates raw + rendition buffers as needed |
| Auto-start DVR when `stream.dvr.enabled` | Complete | Called from `Coordinator.Start` after publisher setup |
| Auto-stop DVR when stream stops | Complete | `Coordinator.Stop` calls `dvr.StopRecording` before teardown |
| Stop pipeline / teardown all buffers | Complete | Main + `$raw$` + `$r$…` rendition buffers all cleaned up |
| Bootstrap persisted streams on startup | Complete | Skips stopped, disabled, and zero-input streams |
| `Update(ctx, old, new)` hot-reload | Complete | Diff-based; routes to the minimal set of service calls; no pipeline disruption for unchanged components |
| Diff engine (`ComputeDiff`) | Complete | 5 independent change categories: inputs, transcoder topology, profiles, protocols/push, DVR |
| Per-profile granular reload | Complete | Changed profile: `StopProfile` + `StartProfile`; added: `buf.Create` + `StartProfile`; removed: `StopProfile` + `buf.Delete` |
| ABR ladder add/remove → `RestartHLSDASH` | Complete | Only HLS + DASH goroutines restart when ladder count changes |
| ABR profile update → `UpdateABRMasterMeta` | Complete | Master playlist rewritten in-place; no FFmpeg restart for unchanged profiles |
| Topology change → `reloadTranscoderFull` | Complete | Full pipeline rebuild when transcoder nil↔non-nil or mode changes |
| DVR hot-reload | Complete | `reloadDVR` toggles recording on/off; restarts with new mediaBuf if best rendition changed |
| Narrow service interfaces (`deps.go`) | Complete | `mgrDep`, `tcDep`, `pubDep`, `dvrDep`; enables spy-based integration testing |

---

## DVR

| Feature | Completion | Notes |
| --------- | ------------ | ------- |
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
| --------- | ------------ | ------- |
| In-process event bus | Complete | Typed events; bounded queue (512); worker pool |
| Event types | Complete | `stream.*`, `input.*`, `recording.*`, `segment.written`, `transcoder.*` — all wired and published |
| HTTP webhook delivery | Complete | Retries, timeout, optional HMAC (`X-OpenStreamer-Signature`) |
| Kafka delivery | Complete | Lazy writer per topic; brokers via `hooks.kafka_brokers` config |
| Event documentation | Complete | `docs/EVENTS.md` — full payload schemas, volume guide, delivery details |

---

## Runtime Status & Observability

All live state is exposed under `runtime.*` in `GET /streams/{code}` so the UI has one root for everything dynamic. Persisted config stays at the top level (`stream.transcoder`, `stream.push`, …); runtime overlay never collides.

| Feature | Completion | Notes |
| --------- | ------------ | ------- |
| `runtime.status` + `runtime.pipeline_active` | Complete | Coordinator-resolved lifecycle state. Always present (even when stream is stopped/idle), populated by API handler. |
| `runtime.inputs[].errors[]` | Complete | Last 5 degradation reasons per input (packet timeout, ingestor error, …) with timestamps; newest at index 0. Persists for the manager registration lifetime; cleared only on Stop. |
| `runtime.transcoder.profiles[]` | Complete | Per-profile FFmpeg state: `restart_count`, `errors[]` (last 5 crashes with stderr-tail context). Updated on every restart attempt. |
| `runtime.publisher.pushes[]` | Complete | Per-destination push state: `status` (starting / active / reconnecting / failed), `attempt`, `connected_at` (only when active), `errors[]` (last 5). On successful re-Active the errors list and `connected_at` reset. |
| FFmpeg stderr tail in transcoder errors | Complete | `runOnce` keeps a ring of the last 8 warn-level stderr lines and embeds them in the returned error so `errors[].message` shows the actual cause ("No such filter: pad_cuda" etc.) instead of just "exit status 8". |
| Build version stamping (`pkg/version`) | Complete | `Version`, `Commit`, `BuiltAt` injected at build time via Makefile ldflags / GitHub release workflow. Exposed via `GET /config.version`. |

---

## Domain Extras (Not in Live Pipeline)

| Feature | Completion | Notes |
| --------- | ------------ | ------- |
| Watermark config on `Stream` | Schema only | Fields exist; not applied in transcoder graph |
| Thumbnail config on `Stream` | Schema only | Fields exist; not generated alongside outputs |
| `TranscodeMode` (passthrough / remux) | Complete | `transcoder.mode: passthrough/remux` bypasses FFmpeg entirely |

---

## Pending / Planned

Tracking what is intentionally NOT done yet. Each row links to a plan file or
captures the current decision context.

| Priority | Feature | Status | Notes |
| -------- | ------- | ------ | ----- |
| High | `copy://<code>` ingest protocol | Planned | Design locked, see [PLAN_COPY_PROTOCOL.md](./PLAN_COPY_PROTOCOL.md). Re-stream another in-process stream's published output (raw or full ABR ladder). Implementation not started. |
| High | RTMP ingest server lal migration | Pending | `internal/publisher/push_rtmp.go` already swapped (Phase 1). `internal/ingestor/push/rtmp_server.go` still on `yapingcat/gomedia` with a per-connection `recover()` guard against AMF panics. Phase 2 swaps it to lal `Server` for symmetric robustness. |
| Medium | Per-rendition push selection | Discussed | `serveRTMPPush` always uses `PlaybackBufferID` (best rendition by resolution × bitrate). To push different rungs to different destinations, add `Rendition` field to `domain.PushDestination` and route via `pickPushBuffer(stream, dest.Rendition)`. ~50 LOC. |
| Medium | Watermark | Schema only | Already in domain; needs FFmpeg `overlay`/`drawtext` injection in `buildVideoFilter`, and dynamic asset upload. |
| Medium | Thumbnail | Schema only | Periodic JPEG snapshot from main buffer; needs ffmpeg `select=eq(pict_type\\,I)` chain or a dedicated TS demux + JPEG encode. |
| Low | HLS / DASH push out | Not started | Only RTMP/RTMPS push exists. HLS push (live HTTP POST chunk upload) would target Akamai-style ingest. |
| Low | WebRTC publish / play | Not started | Would add a Pion-based subsystem; large surface (SDP, ICE, DTLS-SRTP). |
| Low | RTSP / SRT pull manual matrix verification | Untested | Code paths complete; manual playback matrix never run end-to-end. |
| — | Auto-recovery scheduler at coordinator level | Decided NOT | Replaced by infinite per-module retry with backoff cap (transcoder retry forever, manager probe forever). No fatal callback, no `MaxRestarts`. Pipeline never tears down on crash. |

---

## Testing & Quality

| Feature | Completion | Notes |
| --------- | ------------ | ------- |
| Unit tests — protocol detection | Complete | |
| Unit tests — buffer ring / fan-out | Complete | |
| Unit tests — ingestor dispatch + registry | Complete | |
| Unit tests — pull readers (File, HTTP, UDP, RTMP packet parsing) | Complete | |
| Unit tests — manager state machine | Complete | selectBest, collectTimeoutIfNeeded, collectProbeIfNeeded |
| Unit tests — transcoder args construction | Complete | buildScaleFilter, normalizeVideoEncoder, gopFrames, audioEncodeArgs, MaxBitrate/Framerate/CodecProfile, gpuResizeFilter pad-always-CPU-roundtrip |
| Unit tests — error history rings | Complete | `recordInputError` (manager), `recordProfileErrorEntry` (transcoder), `recordPushErrorEntry` (publisher) — ordering, cap, defensive snapshot copy |
| Unit tests — runtime status snapshots | Complete | manager / transcoder / publisher RuntimeStatus shape, defensive copy, sort order, lazy init for push state, Active-clears-errors / leaving-Active-clears-connectedAt |
| Unit tests — manager exhausted recovery regression | Complete | `TestCollectProbeIfNeeded_ProbesActiveWhenExhausted` — guards the bug where single-input streams stayed degraded forever after upstream restart |
| Unit tests — stderr tail + ffmpeg cmdline formatter | Complete | `stderrTail` ring buffer + `formatFFmpegCmd` shell-pasteable rendering with proper quoting |
| Integration tests — ffmpeg filter chain validation (`make test-integration`) | Complete | Build-tagged tests spawn real ffmpeg with the generated `-vf` chain and 1 lavfi frame. Auto-skips when ffmpeg / specific filters / CUDA device are missing. Catches version-specific syntax bugs that pass Go-level checks. |
| Unit tests — publisher HLS segmenter | Complete | windowTailEntries, hlsCodecString, manifest generation, discontinuity, context cancel |
| Unit tests — dvr playlist parsing | Complete | parsePlaylist (single/multi/disc/skip), loadIndex/saveIndex round-trip, atomic write |
| Unit tests — coordinator diff engine | Complete | `TestComputeDiff_*` in `internal/coordinator/diff_test.go`; covers all 5 change categories |
| Integration tests — coordinator.Update routing | Complete | 14 test cases in `internal/coordinator/update_test.go`; spy implementations of all 4 service interfaces |
| CI (GitHub Actions) | Complete | `build`, `test`, `lint` (allow-fail), `govulncheck` jobs |
| golangci-lint (`.golangci.yml`) | Complete | 0 issues |
| gofumpt formatting | Complete | Enforced via CI formatter step |

---

## Manual Test Matrix — Ingest × Publisher

**OK** = playback acceptable in manual test · **—** = not tested · **Issue** = visible stutter / lag

### Without internal transcoding

| Ingest (pull) | HLS | DASH |
| --------------- | ----- | ------ |
| File | OK | OK |
| HLS | OK | OK |
| RTMP | OK | OK |
| SRT | — | — |
| RTSP | — | — |

### With internal transcoding (FFmpeg ABR ladder)

| Ingest (pull) | HLS | DASH |
| --------------- | ----- | ------ |
| File | OK | OK |
| HLS | OK | OK |
| RTMP | OK | OK |
| SRT | — | — |
| RTSP | — | — |

### Push ingest

| Ingest (push) | HLS | DASH |
| --------------- | ----- | ------ |
| RTMP push (OBS/FFmpeg) | OK | OK |
| SRT push | — | — |

---

## Operational Assumptions

- **FFmpeg** must be on `PATH` (or set via `transcoder.ffmpeg_path`) for transcoding. Not required for passthrough / pure ingest.
- **HLS and DASH dirs must differ** when both publishers are active (`publisher.hls.dir` ≠ `publisher.dash.dir`).
- **DVR has no global enable/disable.** Each stream opt-in via `stream.dvr.enabled = true`.
- **Failover timestamp jumps** produce `#EXT-X-DISCONTINUITY` in HLS and are logged at debug level from FFmpeg stderr.
- **`PUT /streams/{code}` is non-disruptive** when the stream is running — only changed components are restarted.
- **Pipeline never tears down on FFmpeg crash.** Each profile retries forever with backoff (2s → 30s cap). After 3 consecutive identical errors, logs drop to debug and events emit only on power-of-2 attempts. Restart count + last 5 errors stay visible via `runtime.transcoder.profiles[].errors`.
- **Push to non-standard RTMP servers (Flussonic etc.)** uses `q191201771/lal` — production-tested against the AMF-extension panic that `yapingcat/gomedia` had. Ingest RTMP server still on gomedia (Phase 2 of lib swap pending) but with `recover()` guard to isolate per-connection panics.
- **Build version** is stamped into the binary at compile time (`make build` runs `git describe --tags --always --dirty`); exposed via `GET /config.version`. Override with `make build VERSION=v1.2.3`.
- **`build/reinstall.sh <tag>`** downloads + verifies + uninstalls + reinstalls a tagged release on Linux/systemd hosts. Data dir (`/var/lib/open-streamer`) preserved across version switches.

---

*Updated 2026-04-23 (revision 2). Adds Runtime Status & Observability section (per-input/profile/push error history with timestamps, build version exposure), Pending/Planned section (consolidated decision context for what's intentionally not done), updated push out notes (lal swap), updated test list (error history rings, runtime snapshots, exhausted-recovery regression, ffmpeg integration filter validation). Previous notes: hot-reload (coordinator.Update + diff engine), per-profile transcoder lifecycle, per-protocol publisher lifecycle, planned `copy://` ingest protocol (see [PLAN_COPY_PROTOCOL.md](./PLAN_COPY_PROTOCOL.md)).*
