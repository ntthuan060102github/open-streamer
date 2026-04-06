# Open Streamer — Design Document

This document describes the working principles of each major subsystem in detail. It is intended for contributors and operators who need to understand *why* things work the way they do, not just *what* they do.

---

## Table of Contents

1. [Overall Data Flow](#1-overall-data-flow)
2. [Buffer Hub](#2-buffer-hub)
3. [Coordinator](#3-coordinator)
4. [Ingestor](#4-ingestor)
5. [Stream Manager (Failover)](#5-stream-manager-failover)
6. [Transcoder](#6-transcoder)
7. [Publisher](#7-publisher)
8. [DVR & Timeshift](#8-dvr--timeshift)
9. [Event Bus & Hooks](#9-event-bus--hooks)
10. [Storage Layer](#10-storage-layer)
11. [API Server](#11-api-server)
12. [Configuration & Dependency Injection](#12-configuration--dependency-injection)

---

## 1. Overall Data Flow

```
┌─────────────┐     MPEG-TS      ┌──────────────┐     fan-out     ┌─────────────┐
│  Ingestor   │ ───────────────► │  Buffer Hub  │ ──────────────► │  Publisher  │
│  (goroutine)│                  │  (ring buf)  │                 │  HLS/DASH/  │
└─────────────┘                  └──────┬───────┘                 │  RTSP/RTMP  │
                                        │                         └─────────────┘
                                        │ fan-out
                              ┌─────────┼──────────┐
                              ▼         ▼          ▼
                        ┌──────────┐ ┌─────┐ ┌─────────┐
                        │Transcoder│ │ DVR │ │ Manager │
                        │FFmpeg    │ │ TS  │ │ health  │
                        └──────────┘ └─────┘ └─────────┘
```

Every MPEG-TS packet written by the Ingestor passes through the Buffer Hub exactly once. All consumers — Publisher, Transcoder, DVR, Stream Manager health sampling — are independent subscribers reading from the same in-memory ring buffer. No packet is ever copied across the network again after ingest.

**When ABR transcoding is active**, two buffer namespaces exist:

```
Ingestor → $raw$<code>  →  Transcoder → $r$<code>$track_1 → Publisher (rendition 1)
                                      → $r$<code>$track_2 → Publisher (rendition 2)
                                      → $r$<code>$track_N → Publisher (rendition N)
           $raw$<code>  →  DVR (records the raw stream, not the transcoded output)
```

---

## 2. Buffer Hub

### Purpose

Single source of truth for live stream data. Decouples the ingest goroutine from all consumers so that any consumer stalling cannot block the ingestor.

### Ring Buffer

Each stream gets an independent `ringBuffer` with a fixed capacity (default 1000 MPEG-TS packets, configurable via `buffer.capacity`). The buffer stores `Packet` values — a union of a raw 188-byte TS slice and an optional decoded `AVPacket` (from protocol readers like RTMP/RTSP).

### Fan-Out

When the ingestor calls `buf.Write(streamID, pkt)`, the hub iterates all registered `*Subscriber` objects for that stream and attempts a non-blocking send:

```go
select {
case sub.ch <- pkt:
default:
    // drop — subscriber is too slow
}
```

**Write never blocks.** If a subscriber's channel is full (consumer too slow), the packet is silently dropped for that consumer only. The ingestor is never stalled.

### Buffer Lifecycle

- Created by `Coordinator.Start` before ingest begins.
- Raw ingest buffer (`$raw$<code>`) created only when transcoding profiles are configured.
- Rendition buffers (`$r$<code>$track_N`) created per-profile.
- All buffers deleted by `Coordinator.Stop` after all consumers unsubscribe.

### Subscriber

Each consumer calls `buf.Subscribe(bufferID)` to get a `*Subscriber` with a private channel. The consumer reads via `sub.Recv()`. When done, it calls `buf.Unsubscribe(bufferID, sub)`.

---

## 3. Coordinator

### Purpose

Wires the full per-stream pipeline in the correct order on `Start()` and tears it down cleanly on `Stop()`.

### Start Sequence

1. Create buffer(s): main buffer `<code>`, plus `$raw$<code>` and `$r$<code>$track_N` if transcoding.
2. Register stream with Stream Manager (spawns ingest workers).
3. Start Publisher for the stream.
4. Start Transcoder workers if profiles configured.
5. If `stream.DVR.Enabled`, call `dvr.StartRecording(ctx, code, mediaBufID, dvrCfg)`.

### Stop Sequence

1. If recording active, call `dvr.StopRecording` (writes final VOD playlist).
2. Unregister from Stream Manager (stops ingest workers).
3. Stop Publisher workers.
4. Delete all buffers (main + raw + renditions).

### Media Buffer Selection

The `mediaBufID` passed to Publisher and DVR depends on whether transcoding is active:

- **No transcoding**: `mediaBufID = code` (main buffer, ingestor writes directly here)
- **With transcoding**: `mediaBufID = PlaybackBufferID(code, transcoder)` = best rendition buffer (e.g. `$r$<code>$track_1`)

DVR always records from `mediaBufID` — the best available quality after transcoding, not the raw ingest.

---

## 4. Ingestor

### Purpose

Ingests live video from any source protocol and writes MPEG-TS packets into the Buffer Hub. One goroutine per stream. No FFmpeg.

### Protocol Detection

`protocol.Detect(url)` inspects the URL scheme and host:

| Scheme | Host | Result |
|--------|------|--------|
| `rtmp://`, `rtmps://` | non-wildcard | `KindRTMP` (pull) |
| `rtsp://` | any | `KindRTSP` (pull) |
| `srt://` | non-wildcard | `KindSRT` (pull) |
| `udp://` | any | `KindUDP` (pull) |
| `http://`, `https://` | any | `KindHLS` (.m3u8) or raw HTTP |
| `file://`, bare path | — | `KindFile` |
| `s3://` | any | `KindS3` |
| `publish://` | any | `KindPublish` (push) |
| wildcard host (`0.0.0.0`, `::`) | any | `KindPublish` (push) |

### Pull Workers

A `Worker` goroutine is created per active input. It:

1. Calls `NewPacketReader(input, cfg)` to get a protocol-specific reader.
2. Opens the connection.
3. Calls `reader.ReadPacket(ctx)` in a loop, writing each `Packet` to the buffer.
4. On error: closes reader, waits exponential backoff (`ReconnectDelaySec` → `ReconnectMaxDelaySec`), reopens.
5. On context cancel: exits cleanly.

Each packet written also calls `manager.RecordPacket(streamCode, inputPriority)` to update health tracking.

### Push Servers (RTMP/SRT)

Push servers (`push.RTMPServer`, `push.SRTServer`) are shared singletons — one TCP port serves all streams simultaneously.

**RTMP push relay architecture:**

```
OBS/FFmpeg ──RTMP publish──► gomedia RTMPServer
                                    │ OnPublish(key):
                                    │  1. registry.Acquire(key) → streamID
                                    │  2. start loopback pull worker:
                                    │     input.URL = "rtmp://127.0.0.1:1935/live/<key>"
                                    ▼
                            joy4 RTMPReader (pull/rtmp.go)
                                    │ same AVCC→Annex-B, ADTS path as normal pull
                                    ▼
                               Buffer Hub
```

When the encoder disconnects, `OnClose` calls `registry.Release(key)`. The loopback joy4 reader receives a natural EOF from the relay and the pull worker exits cleanly. No crash, no goroutine leak.

### Packet Types

The `Packet` written to the buffer is a union:

```go
type Packet struct {
    TS []byte      // raw 188-byte MPEG-TS slice (from file/UDP/HTTP/SRT readers)
    AV *AVPacket   // decoded access unit (from RTMP/RTSP readers via TSDemuxPacketReader)
}
```

Consumers that need 188-byte TS (Publisher HLS segmenter, DVR TS muxer) use the `TS` field. Consumers that need decoded AV (Transcoder stdin feeder via `tsmux.FromAV`) use the `AV` field. Both are populated for RTMP/RTSP sources; only `TS` is populated for file/UDP/HLS sources.

---

## 5. Stream Manager (Failover)

### Purpose

Monitors the health of all ingest inputs for each stream and switches to the next-priority healthy input when the active one fails — without restarting FFmpeg or interrupting consumers.

### Health Tracking

The manager maintains a `streamState` per stream containing:

- `inputs map[priority]*InputHealth` — per-input health record
- `active int` — currently active input priority
- `dead bool` — set when all inputs are exhausted

`InputHealth` tracks:

```go
type InputHealth struct {
    Input        domain.Input
    Status       domain.InputStatus  // idle | active | degraded | failed
    LastPacketAt time.Time
    Bitrate      float64
    PacketLoss   float64
}
```

### Monitor Loop

A `monitor` goroutine runs per stream, ticking every 2 seconds. On each tick, `checkHealth` is called:

1. For the **active input**: if `now - LastPacketAt > packetTimeout` → publish `EventInputDegraded` → call `tryFailover`.
2. For **degraded inputs**: if `now - degradedAt > failbackProbeCooldown` → spawn `runProbe` goroutine.

### Failover

`tryFailover` selects the best healthy (idle or active) input by lowest priority number. If it differs from the current active:

1. Stops the old ingest worker.
2. Starts a new ingest worker on the new input.
3. Increments `hlsFailoverGen` counter in Publisher — HLS segmenter detects the generation change and emits exactly one `#EXT-X-DISCONTINUITY` tag on the next segment flush.
4. Publishes `EventInputFailover`.

FFmpeg is never restarted. The transcoder continues reading from `$raw$<code>` uninterrupted; the buffer simply starts receiving packets from the new source.

### Failback Probe

When an input enters `StatusDegraded`, the manager periodically runs `runProbe(input)` — a lightweight connectivity check. If it succeeds, the input is promoted back to `StatusIdle` and `tryFailover` may switch back to it if it has higher priority than the current active input (after a minimum `failbackMinActiveDuration` to prevent thrashing).

---

## 6. Transcoder

### Purpose

Converts raw ingest into ABR (Adaptive Bitrate) renditions using a bounded pool of FFmpeg subprocesses. One FFmpeg process per rendition profile.

### Worker Pool

A semaphore caps the total number of concurrent FFmpeg processes across all streams (`transcoder.max_workers`, default 4). Each ABR profile for each stream acquires one slot before starting.

### Pipeline Per Rendition

```
$raw$<code> subscriber
      │ Packet.TS or Packet.AV
      ▼
tsmux.FromAV (AV path) or passthrough (TS path)
      │ 188-byte TS
      ▼
FFmpeg stdin  ──── ffmpeg -i pipe:0 [video/audio flags] -f mpegts pipe:1
      │ 188-byte TS
      ▼
stdout reader goroutine → TSDemuxPacketReader → $r$<code>$track_N
```

Two goroutines per FFmpeg process: one reads stdout (transcoded TS), one drains stderr (for log filtering). Both must run simultaneously to prevent pipe deadlock.

### Hardware Acceleration

The `global.hw_accel` field maps to FFmpeg flags:

| Value | FFmpeg flags |
|-------|-------------|
| `none` | `-c:v libx264` (software) |
| `nvenc` | `-hwaccel cuda -c:v h264_nvenc` |
| `vaapi` | `-hwaccel vaapi -c:v h264_vaapi` |
| `videotoolbox` | `-hwaccel videotoolbox -c:v h264_videotoolbox` |
| `qsv` | `-hwaccel qsv -c:v h264_qsv` |

### Profile ID Stability

Rendition IDs are assigned by position: `track_1`, `track_2`, … regardless of profile name or codec. This ensures buffer IDs and HLS sub-playlist paths remain stable even if a profile's content changes.

### FFmpeg Restart Policy

If FFmpeg exits unexpectedly (non-zero exit), the worker restarts with exponential backoff (1s min, 30s max). The buffer subscriber is re-created on each restart so historical packets are not replayed.

---

## 7. Publisher

### Purpose

Reads from Buffer Hub and delivers live video to clients via HLS, DASH, RTSP, RTMP-serve, and SRT-listen. All output goroutines are independent — one format failing does not affect others.

### HLS Segmenter

The HLS segmenter maintains an `hlsSegmenter` struct per stream:

1. Receives `Packet` from buffer subscriber on every tick.
2. Feeds raw TS bytes into an accumulation buffer (`segBuf`).
3. **Segment cut triggers**:
   - **AV path** (decoded AVPacket): cut on keyframe (`KeyFrame == true`) after minimum segment duration elapsed.
   - **TS path** (raw TS): time-based cut via `tickFlush` every 50ms, flush at `maxDur`.
   - **Failover**: generation counter mismatch → flush immediately + set `discNext = true` for next segment.
4. On cut: write `000001.ts` to `{hlsDir}/{code}/`, update `media.m3u8`, optionally update master `index.m3u8`.

`#EXT-X-DISCONTINUITY` is emitted when `discNext == true`. The failover generation counter is a `uint64` incremented by the Stream Manager on every input switch — allowing multiple simultaneous variants to each detect the change exactly once.

### DASH Packager (fMP4)

The DASH packager uses a `tsBuffer` (buffered pipe) feeding into a `mpeg2.TSDemuxer`:

1. Raw TS from buffer subscriber → `tsBuffer.Write` (never blocks; replaces `io.Pipe` which caused packet loss under transcoding load).
2. `TSDemuxer` extracts H.264/AAC elementary streams.
3. eyevinn/mp4ff boxes them into fMP4: `init.mp4` + `seg_N.m4s`.
4. Dynamic MPD updated after each segment.

H.265 in the TS path is currently not supported by the DASH packager and logged as a warning.

### ABR Master Playlist

For streams with multiple transcoding profiles, the HLS master playlist (`index.m3u8`) lists each rendition's sub-playlist (`track_1/media.m3u8`, `track_2/media.m3u8`, …) with bandwidth and resolution hints. Players select the appropriate rendition based on available bandwidth.

### RTSP / RTMP / SRT Serve

These use **shared listeners** — one TCP/UDP port serves all streams:

- **RTSP** (`gortsplib`): clients connect to `rtsp://host:port/live/<code>`
- **RTMP play** (`gomedia`): clients connect to `rtmp://host:port/live/<code>` with app `live`
- **SRT listen** (`gosrt`): clients connect with `streamid=live/<code>`

Each serves the `PlaybackBufferID` for the requested stream.

---

## 8. DVR & Timeshift

### Purpose

Record every stream as a persistent on-disk MPEG-TS archive with wall-time metadata, enabling VOD playback and arbitrary timeshift seeks without additional transcoding.

### Segment Cutting — PTS-Based

HLS sources deliver content faster than real-time (they dump an entire 6-second segment instantly). Wall-clock measurement would produce incorrect segment durations.

Solution: track `segStartPTSms` from the first packet's `PTSms` field. Cut condition:

```
(pktPTSms - segStartPTSms) >= segDurMS
```

Fallback to wall-clock for raw-TS-only sources where `PTSms == 0`.

### Gap Detection

A `gapTimer` is reset on every received packet. Timer duration = `2 × segmentDuration`. When the timer fires (no packets received):

1. Flush any accumulated bytes as a partial segment.
2. Reset the TS muxer (`avMux = nil`) — start fresh TS streams.
3. Set `pendingDiscontinuity = true` for the next segment.
4. Record `gapStartWall` timestamp.

When the stream resumes, append a `DVRGap{From: gapStartWall, To: now}` to `sess.index.Gaps`.

### Resume After Restart

On `StartRecording`, before spawning the record goroutine:

1. `loadIndex(segDir)` — reads `index.json`; nil if fresh.
2. `parsePlaylist(segDir)` — reconstructs `[]segmentMeta` from `playlist.m3u8`.
3. `nextIdx = idx.SegmentCount` — new segments continue numbering from where we left off.
4. `startWithDiscontinuity = true` — first new segment tagged `#EXT-X-DISCONTINUITY`.

`parsePlaylist` extracts:
- `#EXT-X-PROGRAM-DATE-TIME` → `wallTime` for next segment
- `#EXT-X-DISCONTINUITY` → `discontinuity = true`
- `#EXTINF:X.XXX` → `duration`
- `000042.ts` → `index = 42`
- File size from `os.Stat` → `size`

### Playlist Write

`writePlaylist` iterates `sess.segments` in order and emits:

```
#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:<max_seg_sec>
#EXT-X-PLAYLIST-TYPE:EVENT   (or VOD when stopped)

#EXT-X-PROGRAM-DATE-TIME:2026-04-06T14:30:00.000Z   ← before first segment
#EXTINF:6.000,
000000.ts

#EXT-X-DISCONTINUITY                                 ← on gap
#EXT-X-PROGRAM-DATE-TIME:2026-04-06T14:35:42.000Z   ← updated wall time after gap
#EXTINF:6.000,
000001.ts
...
#EXT-X-ENDLIST   ← appended when recording stopped
```

`#EXT-X-PROGRAM-DATE-TIME` is written before the first segment and reset after every `#EXT-X-DISCONTINUITY`. This gives HLS players an absolute wall-time anchor for every contiguous run of segments.

### Retention

`applyRetention` is called after every segment flush. It iterates from the oldest segment:

```
expired  = retentionDur > 0 && time.Since(oldest.wallTime) > retentionDur
overSize = maxSizeBytes > 0 && sess.index.TotalSizeBytes > maxSizeBytes
```

If either condition is true, the oldest `.ts` file is deleted, `TotalSizeBytes` and `SegmentCount` are decremented, and the segment removed from the in-memory list. Gap entries whose `To` timestamp is before the new oldest segment's wall time are also pruned.

### index.json Atomicity

```
write data → index.json.tmp
rename index.json.tmp → index.json
```

Rename is atomic on POSIX filesystems. A crash between write and rename leaves the `.tmp` file orphaned (harmless; it is overwritten on the next flush).

### Timeshift Playlist Generation

`Timeshift` handler in `RecordingHandler`:

1. Parse `playlist.m3u8` via `dvr.ParsePlaylist` → `[]SegmentMeta` with wall times.
2. Resolve `startTime` from `from` (RFC3339) or `offset_sec` (relative to `segments[0].WallTime`).
3. Compute `endTime = startTime + windowDur` (0 = no end).
4. Filter: include segment if `segEnd > startTime` AND (`windowDur == 0` OR `seg.WallTime <= endTime`).
5. Build VOD M3U8 in-memory with `#EXT-X-PLAYLIST-TYPE:VOD` and `#EXT-X-ENDLIST`.
6. Segment URLs point to `GET /recordings/{rid}/{file}` — served from `rec.SegmentDir`.

The timeshift playlist is never stored to disk — it is computed on every request.

---

## 9. Event Bus & Hooks

### Purpose

Decouple domain state changes from external integrations. Services publish events; hooks are passive consumers.

### Event Bus

The in-process bus uses a bounded channel (capacity 512) and a fixed worker pool (default 4 workers).

`Publish(ctx, event)` is non-blocking:

```go
select {
case bus.queue <- delivery{...}:
default:
    slog.Warn("event bus: queue full, dropping event", "type", event.Type)
}
```

Handlers registered with `Subscribe(eventType, handler)` are called by worker goroutines. A handler that panics does not kill the bus — `recover()` is called in the worker loop.

### Domain Events

| Event Type | Trigger | Payload |
|------------|---------|---------|
| `stream.created` | Stream PUT (new) | `stream_code` |
| `stream.started` | Coordinator.Start success | `stream_code` |
| `stream.stopped` | Coordinator.Stop | `stream_code` |
| `stream.deleted` | Stream DELETE | `stream_code` |
| `input.degraded` | Manager: packet timeout | `stream_code`, `input_priority` |
| `input.failover` | Manager: switched input | `stream_code`, `from_priority`, `to_priority` |
| `recording.started` | DVR.StartRecording | `stream_code`, `recording_id` |
| `recording.stopped` | DVR.StopRecording | `stream_code`, `recording_id` |
| `segment.written` | DVR: segment flushed | `stream_code`, `segment_index`, `size_bytes` |

### Hook Delivery

The HTTP hook dispatcher sends a POST to the registered `target` URL with:

- Body: `{"event": {...}, "stream_code": "..."}`
- Header `X-OpenStreamer-Signature: sha256=<hmac>` (if `secret` configured)
- Timeout: `hook.TimeoutSec`
- Retries: `hook.MaxRetries` with exponential backoff

NATS and Kafka deliverers are stubs that return `ErrNotImplemented`.

---

## 10. Storage Layer

### Repository Interfaces

```go
type StreamRepository interface {
    Save(ctx, *Stream) error
    FindByCode(ctx, StreamCode) (*Stream, error)
    List(ctx, StreamFilter) ([]*Stream, error)
    Delete(ctx, StreamCode) error
}

type RecordingRepository interface {
    Save(ctx, *Recording) error
    FindByID(ctx, RecordingID) (*Recording, error)
    ListByStream(ctx, StreamCode) ([]*Recording, error)
    Delete(ctx, RecordingID) error
}

type HookRepository interface {
    Save(ctx, *Hook) error
    FindByID(ctx, HookID) (*Hook, error)
    List(ctx) ([]*Hook, error)
    Delete(ctx, HookID) error
}
```

The `store/` package is the **only** package allowed to import database drivers. All other packages depend on these interfaces only.

### JSON Driver

Stores each entity type as a single JSON file:

```
./data/
  streams.json      { "<code>": Stream, ... }
  recordings.json   { "<id>": Recording, ... }
  hooks.json        { "<id>": Hook, ... }
```

Writes are atomic: marshal → write tmp file → rename. Reads deserialise the entire file. Suitable for development and single-node deployments.

### SQL Driver

Postgres/MySQL via pgx/sqlx. Tables: `streams`, `recordings`, `hooks`. Each row stores the entity as a JSONB column plus indexed lookup fields (`code`, `status`, `stream_code`).

### MongoDB Driver

Each entity stored as a BSON document in its own collection. `_id` = domain ID field. Indexed on `code`/`stream_code` for lookups.

---

## 11. API Server

### Design

The HTTP server is **stateless** — it delegates every operation to the service layer and returns the result. No goroutines or in-memory caches live inside the API handlers.

### Router (chi)

Middleware stack: `RequestID` → `RealIP` → `Recoverer` → `Logger` → `Timeout(120s)`.

Route groups:
- `/streams/{code}` — stream CRUD + lifecycle + recording sub-routes
- `/recordings/{rid}` — recording metadata + media serving
- `/hooks/{hid}` — hook CRUD + test
- `/{code}/*` — HLS/DASH static file serving (via `mediaserve.Mount`)

### Response Envelope

All JSON responses use a consistent structure:

```json
{ "data": { ... } }            // single resource (GET, POST, PUT)
{ "data": [...], "total": N }  // list (GET collection)
{ "error": { "code": "SCREAMING_SNAKE_CODE", "message": "..." } }  // error
```

Go error strings are **never** exposed in API responses — they are logged server-side only.

### Media Serving

`mediaserve.Mount` registers:
- `GET /{code}/index.m3u8` → `{hlsDir}/{code}/index.m3u8` (Cache-Control: no-cache)
- `GET /{code}/index.mpd` → `{dashDir}/{code}/index.mpd` (Cache-Control: no-store)
- `GET /{code}/*` → dispatch by extension: `.ts` → HLS segments, `.m4s`/`.mpd` → DASH

Path traversal protection: rejects any path containing `..` or starting with `/`.

---

## 12. Configuration & Dependency Injection

### Config Loading

Viper loads configuration in three layers (each overrides the previous):

1. Built-in defaults (`setDefaults()` in `config/config.go`)
2. `config.yaml` in the current working directory or `/etc/open-streamer/`
3. Environment variables with prefix `OPEN_STREAMER_` (dots replaced by underscores)

The root `Config` struct is populated once at startup and passed to each service as its relevant sub-config only (e.g. `cfg.Publisher` for Publisher, `cfg.Ingestor` for Ingestor). Services never receive the full `Config`.

### Dependency Injection (samber/do/v2)

All services are registered with a `do.Injector` and resolved lazily on first use. Each service constructor follows the pattern:

```go
func New(i do.Injector) (*Service, error) {
    dep := do.MustInvoke[*dep.Service](i)
    cfg := do.MustInvoke[*config.Config](i)
    return &Service{dep: dep, cfg: cfg.SubConfig}, nil
}
```

Registration order in `cmd/server/main.go`:
1. Config
2. Storage repositories (stream, recording, hook)
3. Buffer service
4. Event bus
5. Ingestor, Manager, Transcoder, Publisher, DVR (order matters for dependency resolution)
6. Coordinator, Hooks dispatcher
7. API handlers → API server
8. Metrics server

Graceful shutdown calls `injector.Shutdown()`, which invokes `HealthCheck` / `Shutdown` on each registered service in reverse registration order.

---

*Updated 2026-04-06. Keep this document in sync when changing subsystem behaviour.*
