# Open-Streamer — Feature Checklist

Snapshot of what's implemented today, organised by subsystem. For
end-to-end pipeline flow see [APP_FLOW.md](./APP_FLOW.md); for design
rationale see [ARCHITECTURE.md](./ARCHITECTURE.md); for operator-facing
config see [CONFIG.md](./CONFIG.md).

Legend:

| Level | Meaning |
|---|---|
| **Complete** | Implemented and usable for the described scope |
| **Partial** | Works with known limitations or narrow codec/path support |
| **Schema only** | Domain / API / persistence fields exist; not wired into live pipeline |
| **Planned** | Documented intent only |

---

## Core Platform

| Feature | Status | Notes |
|---|---|---|
| Layered configuration (file + env) | Complete | `config.yaml` + `OPEN_STREAMER_*` env vars; only StorageConfig from file, rest from store |
| Dependency injection | Complete | `samber/do/v2`; all services in `cmd/server/main.go` |
| Structured logging | Complete | `slog` with text / json format; level configurable |
| Graceful shutdown | Complete | SIGINT/SIGTERM with 10s timeout; reverse-order teardown |
| Prometheus metrics | Complete | Per-stream uptime, bytes/packets, failovers, restarts, active workers, buffer depth |
| Hardware detection | Complete | `internal/hwdetect` probes /dev for NVIDIA / DRI / Intel — listed in `/config.hw_accels` |
| FFmpeg compatibility probe | Complete | `internal/transcoder.Probe` runs at boot (fail-fast on missing required encoders) + `POST /config/transcoder/probe` for UI test + save-time validation |
| Build version stamping | Complete | `pkg/version` injected at compile via Makefile ldflags / Release workflow |

---

## Storage & API

| Feature | Status | Notes |
|---|---|---|
| Stream repository — JSON | Complete | Default; flat-file under `storage.json_dir` |
| Stream repository — YAML | Complete | Single `open_streamer.yaml` per data dir |
| Recording / Hook / VOD repositories | Complete | Both backends |
| REST API — streams CRUD + start/stop/restart | Complete | `chi/v5` router under `/streams` |
| REST API — `PUT /streams/{code}` hot-reload | Complete | Diff-based; only changed components restart |
| REST API — input switch | Complete | `POST /streams/{code}/inputs/switch` forces active priority |
| REST API — recordings | Complete | CRUD + unified `/{file}` dispatch (playlist.m3u8 vs timeshift via `?from=` / `?offset_sec=` query) + segment serve + info |
| REST API — hooks CRUD + test (HTTP & File) | Complete | `DeliverTestEvent` routes per hook type |
| REST API — config GET/POST | Complete | `/config` static enums + GlobalConfig; POST hot-applies |
| REST API — config defaults | Complete | `GET /config/defaults` returns implicit values for UI placeholders (incl. encoder routing table per HW) |
| REST API — config YAML editor | Complete | `GET/PUT /config/yaml` round-trips entire system state |
| REST API — VOD mounts | Complete | Browse on-disk recordings outside DVR scope |
| REST API — Watermark assets | Complete | `/watermarks` library: list / upload / get / raw / delete (mirrors VOD UX) |
| REST API — Play sessions | Complete | `/sessions`, `/streams/{code}/sessions`, `/sessions/{id}` (kick), filters by proto / status / limit |
| OpenAPI / Swagger | Complete | Spec served at `/swagger/`; `make swagger` regenerates |
| Static delivery — HLS / DASH | Complete | `/{code}/index.m3u8`, `/{code}/index.mpd`, `/{code}/*` (wrapped with sessions middleware when tracker enabled) |
| Health probes | Complete | `/healthz`, `/readyz` |
| CORS | Complete | Configurable origins/methods/headers/credentials |

---

## Buffer Hub

| Feature | Status | Notes |
|---|---|---|
| In-memory ring buffer per stream | Complete | Fan-out via independent `Subscriber`; write never blocks |
| Raw ingest buffer (`$raw$<code>`) | Complete | Created when transcoder is active |
| Rendition buffers (`$r$<code>$track_N`) | Complete | One per ABR ladder rung |
| Slow-consumer packet drop | Complete | `default:` in fan-out — ingestor never blocked |
| `PlaybackBufferID` resolver | Complete | Picks best rendition for ABR, else logical stream code |
| Capacity tunable | Complete | `buffer.capacity` (default 1024) |
| `Delete` closes subscriber channels | Complete | Subscribers see `ok=false` on `<-Recv()` so mixer/copy taps observe upstream tear-down and reconnect — fix for the "downstream silently dies after upstream restart" class of bugs |

---

## Ingest

| Feature | Status | Notes |
|---|---|---|
| Pull — HLS / HLS-LL | Complete | `grafov/m3u8`; max-segment-buffer guard; per-input headers/auth |
| Pull — HTTP raw MPEG-TS | Complete | |
| Pull — RTSP | Complete | `gortsplib/v5`; H.264/H.265/AAC; RTCP A/V sync |
| Pull — RTMP | Complete | `q191201771/lal` PullSession; AVCC→Annex-B; ADTS wrap |
| Pull — SRT (caller) | Complete | `datarhei/gosrt` |
| Pull — UDP / MPEG-TS | Complete | Unicast + multicast; auto-strip RTP header |
| Pull — File (`.ts`, `.mp4`, `.flv`) | Complete | Loop mode; paced playback |
| Pull — S3 | Complete | GetObject stream; S3-compatible via `?endpoint=` |
| Pull — `copy://<code>` | Complete | In-process subscribe to another stream's published output (raw or per-rendition) |
| Pull — `mixer://<videoCode>?audio=<audioCode>` | Complete | In-process video+audio mix from two upstream streams |
| Push — RTMP listen | Complete | Shared `:1935` (default); RTMP relay → loopback pull |
| Push — SRT listen | Complete | Shared `:9999`; streamid `live/<code>` dispatch |
| Multi-input registration with priority | Complete | Lower value = higher priority |
| Per-input `Net` config | Complete | `timeout_sec` (per-protocol op budget) and `insecure_tls`. Reconnect/silence-detection knobs were dropped — pull workers use a hardcoded backoff and stream-level liveness lives in `manager.input_packet_timeout_sec`. |
| HLS pull tuning | Complete | Per-stream `net.timeout_sec` (sets playlist GET budget; segment timeout auto-derives ×4 floored at server default); server-wide `hls_max_segment_buffer` |

---

## Stream Manager (Failover)

| Feature | Status | Notes |
|---|---|---|
| Multi-input failover (Go-level, no FFmpeg restart) | Complete | Old ingestor stops, new one starts; buffer continuity preserved |
| Packet timeout detection | Complete | `manager.input_packet_timeout_sec` (default 30); hot-reload via `SetConfig` (atomic.Int64) — change applies on next health-check tick without pipeline restart |
| Background failback probe | Complete | Cooldown 8s probe / 12s switch |
| Bypass-probe recovery | Complete | When ingestor reader auto-reconnects faster than probe cycle, `RecordPacket` clears exhausted state + records recovery switch |
| Switch history (last 20) | Complete | `runtime.switches[]` per stream with reason: `initial`, `error`, `timeout`, `manual`, `failback`, `recovery`, `input_added`, `input_removed`; from/to/at/detail |
| Per-input error history (last 5) | Complete | `runtime.inputs[].errors[]` — degradation reasons + timestamps |
| Live input update (`UpdateInputs`) | Complete | Add/remove/update without pipeline stop; active removal triggers failover with `input_removed` reason |
| Live buffer write-target update | Complete | `UpdateBufferWriteID` — restart active ingestor with new target |
| Manual switch API | Complete | `POST /streams/{code}/inputs/switch { priority }` records `manual` reason |
| Exhausted callback → coordinator | Complete | `setStatus(degraded)`; auto-recover via probe success |

---

## Transcoder

| Feature | Status | Notes |
|---|---|---|
| FFmpeg subprocess (stdin TS → stdout TS) | Complete | `exec.CommandContext`; killed via context cancel |
| Per-profile encoder pool | Complete | Each `track_N` is independent `profileWorker`; hot start/stop one without affecting others |
| Transcoder mode (per-stream) | Complete | `Stream.TranscoderMode`: `multi_output` (default) runs ONE FFmpeg per stream emitting N rendition pipes — single decode + multi encode → ~50% NVDEC + ~40% RAM saved per ABR stream. `per_profile` runs one FFmpeg per ladder rung. Hot-switch restarts the affected stream only |
| Shadow profile workers (multi-output) | Complete | All N ladder rungs appear in `RuntimeStatus.Profiles[]` even though one process drives them — error history accurate per rung |
| ABR profile config | Complete | Resolution, bitrate, codec, preset, profile, level, framerate, GOP, B-frames, refs, SAR, resize_mode |
| Encoder codec routing | Complete | `domain.ResolveVideoEncoder` maps user alias (`""`/`h264`/`h265`/`vp9`/`av1`) + HW backend → FFmpeg encoder name; explicit names (`h264_nvenc`, `h264_qsv`) preserved |
| Preset normalization | Complete | Translates between encoder families (`veryfast` ↔ `p2`, `medium` ↔ `p4`); drops invalid values for backends without `-preset` (VAAPI, VideoToolbox) so cross-family preset choices remain valid |
| Audio encoding | Complete | AAC / MP3 / Opus / AC3 / copy |
| Copy video / copy audio modes | Complete | `video.copy=true` + `audio.copy=true` skips FFmpeg entirely (passthrough) |
| Hardware acceleration | Complete | NVENC, VAAPI, VideoToolbox, QSV; full-GPU pipeline (decode→scale_cuda→encode) when HW matches encoder family |
| Resize modes (pure GPU) | Complete | `pad`, `crop`, `stretch`, `fit` — all stay on GPU (no CPU round-trip via hwdownload) for NVENC; `pad`/`crop` degrade to aspect-preserving fit on GPU |
| Deinterlace | Complete | yadif (CPU) / yadif_cuda (GPU); auto-detect parity or operator-specified tff/bff |
| Watermark — text overlay | Complete | drawtext-based; per-position presets + custom (raw FFmpeg expressions for X/Y); strftime fields supported in text |
| Watermark — image overlay | Complete | `movie=`-source overlay (no second `-i` needed → uniform with multi-output); PNG / JPG / GIF; opacity; CPU + GPU pipelines (GPU round-trip via hwdownload/hwupload_cuda) |
| Watermark asset library | Complete | `/watermarks` REST API + on-disk store under `watermarks.dir`; ID-keyed files + JSON sidecar; resolved by coordinator before transcoder.Start |
| Thumbnail | Schema only | Domain fields exist; not yet generated |
| Extra FFmpeg args passthrough | Complete | `extra_args` per stream |
| FFmpeg crash auto-restart | Complete | Per-profile exponential backoff: 2s → 30s cap; retries forever |
| Crash log spam suppression | Complete | After 3 consecutive identical errors, warn drops to debug; events fire only on power-of-2 attempts |
| Per-profile error history (last 5) | Complete | `runtime.transcoder.profiles[].errors[]` — stderr-tail context embedded ("No such filter X") |
| Stderr filtering | Complete | Timestamp resync, packet-corrupt, MMCO chatter → debug; real errors → warn |
| Health detection → coordinator | Complete | After 3 consecutive crashes (sub-30s) fires `onUnhealthy` → status Degraded; sustained run (>30s) fires `onHealthy` → status Active. Hot-restart (Update path) clears flag via `dropHealthState` callback |
| Hot-swap config (`SetConfig`) | Complete | runtime updates `FFmpegPath`; per-stream `TranscoderMode` swap is handled by stream-level diff (restarts only the affected stream) |
| `StopProfile` / `StartProfile` | Complete | Granular ladder control; multi-output mode loses granularity (must full-restart) |

---

## Coordinator & Lifecycle

| Feature | Status | Notes |
|---|---|---|
| Start pipeline | Complete | Buffers → manager → publisher → transcoder; raw + rendition buffers per topology |
| Stop pipeline | Complete | Reverse-order teardown; buffer cleanup |
| Bootstrap persisted streams on boot | Complete | Skips disabled / zero-input streams |
| Hot-reload (`Update`) | Complete | Diff engine: 5 categories — inputs, transcoder topology, profiles, protocols/push, DVR |
| Per-profile granular reload | Complete | Add/remove/update one profile without touching others |
| ABR ladder add/remove → `RestartHLSDASH` | Complete | Only HLS+DASH goroutines restart; RTSP/RTMP/SRT viewers preserved |
| ABR profile metadata update | Complete | `UpdateABRMasterMeta` rewrites HLS master playlist in-place (no FFmpeg restart) |
| Topology change → `reloadTranscoderFull` | Complete | Full pipeline rebuild when transcoder nil↔non-nil or mode changes |
| ABR-copy pipeline (`copy://` upstream with ladder) | Complete | N tap goroutines re-publish each upstream rendition; bypasses ingest worker + transcoder; reconnects on upstream restart (relies on `buffer.Delete` channel-close signal) |
| ABR-mixer pipeline | Complete | Mirror video ladder + audio fan-out from two upstream streams; reconnects on upstream restart; **PTS/DTS rebased per-source against shared wall-clock anchor** so video (upstream A) and audio (upstream B) collapse onto a common timeline — without this, divergent PCR bases between unrelated sources caused players to render black + silent |
| Stream-level health reconciliation | Complete | `streamDegradation` flags (`inputsExhausted`, `transcoderUnhealthy`) — Degraded if either set, Active when all clear |
| DVR hot-reload | Complete | Toggle on/off; restarts with new mediaBuf when best rendition shifts |
| Narrow service interfaces (`deps.go`) | Complete | `mgrDep`, `tcDep`, `pubDep`, `dvrDep` — spy-based testing |

---

## Publisher — Delivery

| Feature | Status | Notes |
|---|---|---|
| HLS — single rendition | Complete | Native TS segmenter + media playlist |
| HLS — ABR (master + per-track sub-playlists) | Complete | Auto-active when transcoder ladder present |
| HLS — `#EXT-X-DISCONTINUITY` on failover | Complete | Per-variant generation counter |
| DASH — single representation (fMP4 + dynamic MPD) | Complete | H.264 / H.265 / AAC; MP3 skipped |
| DASH — ABR (root MPD + per-track dirs) | Complete | Audio packaged on best track only |
| RTSP play | Complete | Shared listener (default `:554`); `gortsplib/v5`; `rtsp://host:port/live/<code>` |
| RTMP play | Complete | Shared port with ingest (`:1935`); `rtmp://host:port/live/<code>` |
| SRT play | Complete | Shared listener (`:9999`); `srt://host:port?streamid=live/<code>`; default latency 120ms |
| RTMP push out | Complete | `q191201771/lal` PushSession; `rtmp://` + `rtmps://`; custom codec adapter for proper PTS/DTS composition_time (B-frame friendly) |
| Per-protocol independent context | Complete | Each output (`hls`, `dash`, `rtsp`, `push:<url>`) has its own cancel func |
| `UpdateProtocols(old, new)` | Complete | Only changed protocols stop/start; live viewers preserved |
| Per-push state tracking | Complete | `runtime.publisher.pushes[]` — status (`starting`/`active`/`reconnecting`/`failed`), attempt, connected_at, last 5 errors |
| Listener hot-reload (RTMP / SRT / RTSP) | Complete | `publisher.SetListeners` + `ingestor.SetListeners` + `manager.SetConfig` swap `atomic.Pointer` snapshots before `diffService` restarts the affected goroutine — port / latency changes pick up on next restart cycle without losing other live viewers |

---

## DVR & Timeshift

| Feature | Status | Notes |
|---|---|---|
| Persistent recording (ID = stream code) | Complete | One recording per stream; survives restarts |
| Segment writing (MPEG-TS) | Complete | PTS-based cutting; wall-clock fallback |
| Gap detection + `#EXT-X-DISCONTINUITY` | Complete | Gap timer = 2× segment duration |
| Gap recording in `index.json` | Complete | `DVRGap{From, To, Duration}` |
| Resume after restart | Complete | Playlist parsing rebuilds in-memory segment list |
| `#EXT-X-PROGRAM-DATE-TIME` | Complete | Written before first segment + after every discontinuity |
| Retention by time + size | Complete | Both `retention_sec` (0=forever) and `max_size_gb` (0=unlimited) |
| VOD playlist + timeshift + segments | Complete | Unified `GET /recordings/{rid}/{file}` — `.m3u8` serves `playlist.m3u8` from disk by default, dispatches to dynamic timeshift slice when `?from=RFC3339` / `?offset_sec=N` (+ optional `?duration=N`) is present; `.ts` serves segment as-is. Path traversal sanitised |
| Info endpoint | Complete | `GET /recordings/{rid}/info` — range, gaps, count, total bytes |
| Configurable storage path | Complete | Per-stream `storage_path` overrides `./out/dvr/{streamCode}` default |

---

## Events & Hooks

| Feature | Status | Notes |
|---|---|---|
| In-process event bus | Complete | Typed events, bounded queue (512), worker pool sized via `hooks.worker_count` (default 4; rarely needs tuning after batching) |
| HTTP webhook delivery (batched) | Complete | Per-hook batcher; flushes on `BatchMaxItems` OR `BatchFlushIntervalSec`; POST body is a JSON array; `X-OpenStreamer-Batch-Size` header reports cardinality; HMAC `X-OpenStreamer-Signature` covers the array body |
| HTTP retry + re-queue on failure | Complete | Up to `max_retries` retries within a flush (1s/5s/30s backoff); failed batches re-queue at the front for next flush; queue capped by `BatchMaxQueueItems` (drops oldest on overflow) |
| File delivery | Complete | Appends one JSON line per event to an absolute target path; per-target mutex serialises concurrent writes; line-atomic at filesystem level via O_APPEND. **Not batched** to keep the JSON-lines contract intact |
| Per-hook event filter | Complete | `event_types[]` whitelist |
| Per-hook stream filter | Complete | `stream_codes.only[]` / `.except[]` |
| Per-hook metadata injection | Complete | Merged into payload as `metadata.*` |
| Per-hook MaxRetries / TimeoutSec | Complete | Defaults: 3 retries, 10s timeout (from `domain.Default*`) |
| Per-hook batch overrides | Complete | `batch_max_items` / `batch_flush_interval_sec` / `batch_max_queue_items` on Hook record override `hooks.batch_*` global defaults |
| Graceful drain on shutdown | Complete | Service.Start exits → each HTTP batcher gets a final best-effort flush before goroutine returns |
| Test endpoint (HTTP + File) | Complete | `POST /hooks/{id}/test` — for HTTP, signals an immediate flush so the test response is visible in seconds rather than waiting a full flush interval |
| Event documentation | Complete | See [APP_FLOW.md](./APP_FLOW.md#events-reference) |

---

## Runtime Status & Observability

All live state is exposed under `runtime.*` in `GET /streams/{code}` so the UI has one root for everything dynamic. Persisted config stays at the top level — runtime overlay never collides.

| Feature | Status | Notes |
|---|---|---|
| `runtime.status` + `pipeline_active` | Complete | Coordinator-resolved lifecycle: `active` / `degraded` / `stopped` / `idle` |
| `runtime.exhausted` | Complete | True when all inputs are degraded with no failover candidate |
| `runtime.active_input_priority` + `override_input_priority` | Complete | Manager state |
| `runtime.inputs[]` | Complete | Per-input snapshot: status, last_packet_at, bitrate_kbps, errors[] |
| `runtime.switches[]` | Complete | Last 20 active-input switches with reason + detail |
| `runtime.transcoder.profiles[]` | Complete | Per-rung restart_count + errors[]; FFmpeg stderr-tail embedded |
| `runtime.publisher.pushes[]` | Complete | Per-destination status + attempts + errors[]; resets on Active |
| Defensive snapshot copies | Complete | Caller-side mutation cannot leak back into service state |

---

## Configuration Defaults

Single source of truth: [internal/domain/defaults.go](../internal/domain/defaults.go). Exposed via `GET /config/defaults` for frontend placeholder rendering.

| Group | Constants |
|---|---|
| Buffer | `DefaultBufferCapacity=1024` |
| Manager | `DefaultInputPacketTimeoutSec=30` |
| Publisher HLS/DASH | `DefaultLiveSegmentSec=2`, `DefaultLiveWindow=12`, `DefaultLiveHistory=0` |
| DVR | `DefaultDVRSegmentDuration=4`, `DefaultDVRRoot="./out/dvr"` |
| Push | `DefaultPushTimeoutSec=10`, `DefaultPushRetryTimeoutSec=5` |
| Hook | `DefaultHookMaxRetries=3`, `DefaultHookTimeoutSec=10` |
| Video | `DefaultVideoBitrateK=2500`, `DefaultVideoResizeMode=pad` |
| Audio | `DefaultAudioBitrateK=128` |
| Listeners | `DefaultListenHost="0.0.0.0"`, `DefaultRTMPTimeoutSec=10`, `DefaultRTSPTimeoutSec=10`, `DefaultSRTLatencyMS=120` |
| Ingestor | `DefaultHLSPlaylistTimeoutSec=15`, `DefaultHLSSegmentTimeoutSec=60`, `DefaultHLSMaxSegmentBuffer=8` |
| Transcoder | `DefaultFFmpegPath="ffmpeg"` |

---

## Play Sessions (`internal/sessions`)

Tracks every active player so operators can answer "who is watching
this stream right now?". State is in-memory only — restart loses
records, viewers reconnect into fresh sessions.

| Feature | Status | Notes |
|---|---|---|
| HLS / DASH session tracking | Complete | `mediaserve.Mount` wrapped with `sessions.HTTPMiddleware`; each segment GET extends the session record; bytes counted from the `ResponseWriter` |
| RTMP session tracking | Complete | `push.PlayFunc` extended with `PlayInfo{RemoteAddr, FlashVer}`; bytes counted by wrapping `writeFrame` |
| SRT session tracking | Complete | `srtHandleSubscribe` opens a tracker session; bytes accumulated on every successful `conn.Write` |
| RTSP session tracking | Complete | gortsplib `OnPlay` / `OnSessionClose` hooks; bytes default to 0 (gortsplib mux is internal) |
| Fingerprint session ID (HLS / DASH) | Complete | `sha256(stream + ip + ua + token)[0..16]` so repeated segment GETs collapse onto one record within the idle window |
| UUID session ID (RTMP / SRT / RTSP) | Complete | Connection-bound, generated at handshake; closed on TCP teardown |
| Idle reaper | Complete | Default 30s without activity → close + emit `EventSessionClosed`; tunable via `sessions.idle_timeout_sec` |
| Max-lifetime cap | Complete | Optional `sessions.max_lifetime_sec` hard-closes any session older than the cap |
| Hot-reload config | Complete | `sessions.UpdateConfig` swaps an `atomic.Pointer[runtimeConfig]` — toggling `enabled` / changing `idle_timeout_sec` takes effect on the next reaper tick without restart |
| Kick (force-close) | Complete | `DELETE /sessions/{id}` → reason=`kicked`; idempotent (404 on already-closed) |
| Filter / list | Complete | `GET /sessions?proto=…&status=…&limit=…` + per-stream `/streams/{code}/sessions` |
| Stats counters | Complete | `active`, `opened_total`, `closed_total`, `idle_closed_total`, `kicked_total` exposed in every list response |
| Event bus emit | Complete | `EventSessionOpened` / `EventSessionClosed` published — hooks can persist analytics or notify ops |
| GeoIP resolver interface | Schema only | `GeoIPResolver` interface + `NullGeoIP` default; `geoip_db_path` config field reserved. MaxMind / IP2Location reader not wired. |

---

## Watermarks (`internal/watermarks` + transcoder filter graph)

| Feature | Status | Notes |
|---|---|---|
| Text overlay (drawtext) | Complete | `text` supports `%{localtime}` and friends; opacity folded into `fontcolor=…@α` |
| Image overlay (overlay+movie) | Complete | `movie=` source filter avoids second `-i`; opacity via `colorchannelmixer=aa` |
| GPU round-trip on NVENC | Complete | `hwdownload,format=nv12 → drawtext/overlay → hwupload_cuda`; portable across distros without `--enable-cuda-nvcc` |
| Multi-output mode support | Complete | Same filter chain emitted on every `-vf:v:0` so each rendition draws the watermark independently |
| Position presets | Complete | `top_left` / `top_right` / `bottom_left` / `bottom_right` / `center`; `offset_x` / `offset_y` act as edge padding |
| Custom position | Complete | `position=custom` + raw FFmpeg expressions in `x` / `y` ("100", "main_w-overlay_w-50", "if(gt(t,5),10,-100)") |
| Asset library upload | Complete | `POST /watermarks` multipart; PNG / JPG / GIF sniffed via `http.DetectContentType`; cap 8 MiB image / 16 MiB request |
| Asset library list / get / raw / delete | Complete | Mirrors VOD UX; `/raw` serves with `Cache-Control: immutable` |
| Sidecar metadata | Complete | One `<id>.json` per asset → `os.ReadDir` rebuilds registry on restart, no DB |
| Asset-id reference from streams | Complete | `Stream.Watermark.AssetID` resolved by coordinator into `ImagePath` before each `tc.Start` (transcoder stays asset-agnostic) |
| Validation at API boundary | Complete | mutually exclusive `image_path` / `asset_id`; opacity 0..1; position-custom requires non-empty x/y; image / font path absoluteness + readability |

---

## Pending / Planned

Tracking what is intentionally NOT done. Each row is a deliberate scope decision.

| Priority | Feature | Status | Notes |
|---|---|---|---|
| Mid | Thumbnail | Schema only | Periodic JPEG snapshot from main buffer; needs ffmpeg `select=eq(pict_type\\,I)` chain |
| Mid | GeoIP MaxMind reader | Schema only | Interface + `NullGeoIP` default exist; concrete MaxMind/IP2Location reader not wired. `sessions.geoip_db_path` config field reserved |
| Mid | Sessions persistence (history beyond active set) | Not started | In-memory only — closed sessions disappear after the event is published. Hooks can persist downstream |
| Mid | Local-packager error tracking (HLS/DASH) | Not started | Currently slog-only; analogous to push state pattern but per-stream-per-format |
| Low | HLS / DASH push out | Not started | Only RTMP/RTMPS push exists |
| Low | RTSP per-session bytes accuracy | Schema only | gortsplib mux is internal; current `bytes=0` for RTSP. Would need a custom `WritePacketRTP` wrapper or fork |
| Low | RTMP `flashVer` capture | Schema only | gomedia doesn't expose `flashVer` from the connect command publicly; left empty in `PlayInfo.FlashVer` |
| Low | Sessions token-based auth | Not started | Token field reserved on PlaySession; no resolver / signed-URL verifier wired yet |
| Low | WebRTC publish / play | Not started | Pion-based subsystem; large surface (SDP, ICE, DTLS-SRTP) |

### Decided NOT (locked)

| Feature | Reason |
|---|---|
| Auto-recovery scheduler at coordinator level | Replaced by infinite per-module retry with backoff (transcoder retry forever, manager probe forever). No `MaxRestarts`. Pipeline never tears down on crash |
| RTMP ingest server lal migration | Current gomedia-based push server is stable with per-connection `recover()`. Cost-benefit doesn't justify refactor + retest matrix |
| Per-rendition push selection | Push always sends best rendition. Multi-tier publishing → run separate streams |
| Full `gomedia` → `lal` swap | TS infrastructure (`gomedia/go-mpeg2`) has no equivalent in lal. Hybrid stack is intentional |

---

## Testing & Quality

| Feature | Status | Notes |
|---|---|---|
| Unit tests — protocol detection | Complete | |
| Unit tests — buffer ring / fan-out | Complete | |
| Unit tests — manager state machine + bypass-recovery + switch history | Complete | |
| Unit tests — transcoder args, encoder routing, preset normalization, multi-output args | Complete | |
| Unit tests — transcoder health detection (3-fail edge, sustain recovery, multi-profile aggregation) | Complete | |
| Unit tests — coordinator diff engine + degradation reconciliation | Complete | |
| Unit tests — publisher HLS/DASH segmenters, push state | Complete | |
| Unit tests — DVR playlist parsing, gap recording | Complete | |
| Unit tests — error history rings (manager / transcoder / push) | Complete | |
| Unit tests — runtime status snapshots (defensive copy, sort order) | Complete | |
| Unit tests — FFmpeg probe (parsers, integration on PATH, missing binary, non-FFmpeg) | Complete | |
| Unit tests — config defaults endpoint (shape, codec routing table, determinism) | Complete | |
| Unit tests — sessions tracker (HTTP + conn paths, idle reaper, kick, filter, hot-reload) | Complete | |
| Unit tests — sessions HTTP middleware (proto detection, byte counting, error path) | Complete | |
| Unit tests — sessions handler (list / get / kick / filter validation) | Complete | |
| Unit tests — watermark filter graph (text + image, CPU + GPU, custom position, position presets, escaping) | Complete | |
| Unit tests — watermark domain validation (mutual-exclusion, asset-id charset, opacity range, custom requires X/Y) | Complete | |
| Unit tests — watermarks asset service (save/list/get/delete, content-type sniff, rebuild from disk) | Complete | |
| Integration tests — coordinator.Update routing | Complete | 14 cases, spy implementations of all service interfaces |
| Integration tests — ffmpeg filter chain | Complete | Build-tagged; spawns real ffmpeg with generated `-vf` |
| CI (GitHub Actions) | Complete | `mod-tidy`, `test` (matrix Go 1.25.9 + stable), `lint` (allow-fail), `govulncheck` |
| Pre-commit hook (auto-regen swagger) | Complete | `make hooks-install` symlinks `scripts/git-hooks/pre-commit` |
| golangci-lint | Complete | 0 issues |
| gofumpt formatting | Complete | Enforced via lint |

### Benchmarking (`bench/`)

Operator-facing capacity tooling — runs sweeps across passthrough,
ABR, multi-output, libx264 and HLS+DASH multi-protocol phases. See
[`bench/README.md`](../bench/README.md) for the full sweep plan.

| Tool | Purpose |
|---|---|
| `bench/scripts/sample.sh` | 2s-cadence CSV of CPU% (jiffies-based, intersection of PID set across ticks) + RSS / GPU enc/dec / VRAM / network for the open-streamer process tree |
| `bench/scripts/run-bench.sh` | One-case driver: spin up N FFmpeg publishers → wait warmup → sample steady window → tear down |
| `bench/scripts/run-all.sh` | Full sweep — Phases A/B/C/F/H/D, auto-stops a phase on first SATURATED, full-stop on first FAIL |
| `bench/scripts/summarize.sh` | Per-run `summary.md` + auto-classify `PASS` / `SATURATED` / `FAIL` against thresholds |
| `bench/scripts/aggregate.sh` | Master report at `bench/reports/<sweep>/report.md` |
| `bench/scripts/notify.sh` | Optional Telegram webhook integration |

---

## Operational Notes

- **FFmpeg required for transcoding.** Boot probes `transcoder.ffmpeg_path` (or `$PATH`) — REQUIRED encoders missing → server exits non-zero with a clear error. Optional encoders missing → boot warns but continues.
- **HLS and DASH dirs must differ** when both publishers are active.
- **DVR is per-stream opt-in.** No global enable.
- **Failover timestamp jumps** produce `#EXT-X-DISCONTINUITY` in HLS and are logged at debug level from FFmpeg stderr.
- **`PUT /streams/{code}` is non-disruptive** when the stream is running — only changed components restart.
- **Pipeline never tears down on FFmpeg crash.** Each profile retries forever with backoff. Status flips to `degraded` after 3 consecutive crashes; flips back to `active` after a sustained run (>30s) or hot-restart.
- **Multi-output toggle restarts running streams** — operator confirmation expected via UI before enabling on a busy server (~2-3s downtime per stream).
- **Build version** stamped at compile time (`make build` runs `git describe --tags --always --dirty`); exposed via `GET /config.version`.
- **`build/reinstall.sh <tag>`** downloads + verifies + uninstalls + reinstalls a tagged release on Linux/systemd hosts. Data dir preserved.
