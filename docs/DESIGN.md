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

```text
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

```text
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

### Responsibility

Wires the full per-stream pipeline in the correct order on `Start()` and tears it down cleanly on `Stop()`. Also orchestrates surgical hot-reload via `Update()` — only the components that changed are restarted.

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

### Hot-Reload: `Update(ctx, old, new)`

`Update` computes a `StreamDiff` between the old and new configuration, then applies only the changes:

```
1. NowDisabled?               → coordinator.Stop(), return

2. TranscoderTopologyChanged? → reloadTranscoderFull()
   (nil↔non-nil, mode change)   stops DVR + publisher + transcoder,
                                  rebuilds buffers, restarts all
                                  return

3. TranscoderChanged + ProfilesDiff?
   → per-profile granular reload (see Transcoder section)
   → if profiles added/removed: publisher.RestartHLSDASH()
   → if profiles updated only:  publisher.UpdateABRMasterMeta()

4. InputsChanged?  → mgr.UpdateInputs(added, removed, updated)

5. ProtocolsChanged || PushChanged? → publisher.UpdateProtocols(old, new)

6. DVRChanged?     → reloadDVR()
```

Each category is independent: updating inputs does not touch the transcoder; toggling a protocol does not restart RTSP viewers already connected.

### Diff Engine (`coordinator/diff.go`)

`ComputeDiff(old, new *domain.Stream) StreamDiff` classifies changes into five independent categories:

| Category | Flag | Trigger |
|----------|------|---------|
| Inputs | `InputsChanged` | URL/priority/config change, add, or remove |
| Transcoder topology | `TranscoderTopologyChanged` | nil↔non-nil, mode change (`passthrough`↔full), `video.copy` toggle |
| Transcoder profiles | `ProfilesDiff` | Per-profile resolution/bitrate/codec change, add, remove |
| Protocols / Push | `ProtocolsChanged`, `PushChanged` | HLS/DASH/RTSP toggle, push destination add/remove/edit |
| DVR | `DVRChanged` | `enabled` flag, retention, segment duration |

`TranscoderTopologyChanged` takes priority over `ProfilesDiff` — when topology changes the entire pipeline must be rebuilt because the buffer namespace changes.

### Dependency Interfaces (`coordinator/deps.go`)

The coordinator talks to each service through a narrow interface:

```go
type mgrDep interface { IsRegistered, Register, Unregister, UpdateInputs, UpdateBufferWriteID, ... }
type tcDep  interface { Start, Stop, StopProfile, StartProfile, SetFatalCallback }
type pubDep interface { Start, Stop, UpdateProtocols, RestartHLSDASH, UpdateABRMasterMeta }
type dvrDep interface { IsRecording, StartRecording, StopRecording }
```

This enables full unit-test coverage via spy implementations without starting real ingestors, FFmpeg processes, or RTSP servers (see `internal/coordinator/update_test.go`).

---

## 4. Ingestor

### Overview

Ingests live video from any source protocol and writes MPEG-TS packets into the Buffer Hub. One goroutine per stream. No FFmpeg.

### Protocol Detection

`protocol.Detect(url)` inspects the URL scheme and host:

| Scheme | Host | Result |
| ------ | ---- | ------ |
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

```text
OBS/FFmpeg ──RTMP publish──► Shared RTMP server
                                    │ OnPublish(key):
                                    │  1. registry.Acquire(key) → streamID
                                    │  2. start loopback pull worker:
                                    │     input.URL = "rtmp://127.0.0.1:1935/live/<key>"
                                    ▼
                            RTMP pull reader (pull/rtmp.go)
                                    │ same AVCC→Annex-B, ADTS path as normal pull
                                    ▼
                               Buffer Hub
```

When the encoder disconnects, `OnClose` calls `registry.Release(key)`. The loopback reader receives a natural EOF from the relay and the pull worker exits cleanly. No crash, no goroutine leak.

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

### Live Input Updates (`UpdateInputs`)

`UpdateInputs(streamID, added, removed, updated []domain.Input)` applies live changes without stopping the pipeline:

1. **Removed** inputs: deleted from the health map; if the removed input was active, `tryFailover` is called immediately.
2. **Added** inputs: inserted as `StatusIdle`; `tryFailover` runs so a higher-priority new input can take over.
3. **Updated** inputs: the `Input` config is patched in-place; if the active input was updated, its ingestor is restarted with the new config.

---

## 6. Transcoder

### Purpose

Converts raw ingest into ABR (Adaptive Bitrate) renditions using a bounded pool of FFmpeg subprocesses. One FFmpeg process per rendition profile.

### Worker Pool

A semaphore caps the total number of concurrent FFmpeg processes across all streams (`transcoder.max_workers`, default 4). Each ABR profile for each stream acquires one slot before starting.

### Pipeline Per Rendition

```text
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

### Internal Data Structures

Each active stream is represented by a `streamWorker`:

```go
type streamWorker struct {
    baseCtx    context.Context
    baseCancel context.CancelFunc      // cancelling stops ALL profiles
    profiles   map[int]*profileWorker  // key = profile index (0-based)
    rawIngest  domain.StreamCode
    tc         *domain.TranscoderConfig
}

type profileWorker struct {
    cancel  context.CancelFunc
    profile domain.VideoProfile
    target  RenditionTarget
}
```

Each `profileWorker` has its own context derived from `baseCtx`. This allows individual profiles to be stopped and restarted independently without affecting other profiles or the ingestor.

### Per-Profile Lifecycle

`StopProfile(streamID, profileIndex)` cancels the context of a single `profileWorker`, sending SIGTERM to just that FFmpeg process.

`StartProfile(streamID, profileIndex, target)` acquires a semaphore slot, creates a new `profileWorker` with a fresh child context, and starts the FFmpeg goroutine.

These two methods are used by the coordinator for surgical per-profile updates — when only one rendition's resolution or bitrate changes, only that FFmpeg process is restarted.

### Hardware Acceleration

The `global.hw_accel` field maps to FFmpeg flags:

| Value | FFmpeg flags |
| ----- | ------------ |
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

### Per-Protocol Lifecycle

Each protocol goroutine has its own child context derived from a per-stream `baseCtx`:

```go
type streamState struct {
    baseCtx    context.Context
    baseCancel context.CancelFunc      // cancels ALL protocols when stream stops
    mediaBuf   domain.StreamCode       // buffer the protocols read from
    protocols  map[string]context.CancelFunc  // "hls", "dash", "rtsp", "push:<url>"
    hlsMaster  *hlsABRMaster           // non-nil when ABR HLS is running
}
```

Protocol keys:
- `"hls"` — HLS segmenter (single-rendition or ABR)
- `"dash"` — DASH fMP4 packager
- `"rtsp"` — RTSP server mount
- `"push:<url>"` — RTMP push-out goroutine per destination

`stopProtocol(streamID, key)` cancels exactly one goroutine. `spawnProtocolLocked(ss, key, fn)` starts a new goroutine for the key, cancelling any existing one first.

### `UpdateProtocols(ctx, old, new)`

Called when protocol flags or push destinations change. Only acts on ON↔OFF transitions:

- `old.HLS && !new.HLS` → `stopProtocol("hls")`
- `!old.HLS && new.HLS` → `spawnProtocolLocked("hls", ...)`
- Same logic for DASH and RTSP.
- Push destinations: diff by URL — stop removed/disabled, start new/re-enabled.

RTSP/RTMP/SRT viewers already connected are **not** affected when unrelated protocols toggle.

### `RestartHLSDASH(ctx, stream)`

Called when the ABR ladder count changes (profile added or removed). Restarts only the `"hls"` and `"dash"` goroutines so the master playlist and per-shard segmenters reflect the new rendition set. RTSP, RTMP play, and SRT goroutines are unaffected.

### `UpdateABRMasterMeta(streamCode, updates []ABRRepMeta)`

Called when a profile's bitrate or resolution changes but the ladder count stays the same. Routes to the live `hlsABRMaster` instance via `ss.hlsMaster` and calls `SetRepOverride` for each changed shard. The master playlist is rewritten within 50ms with the new metadata — no goroutine restart required.

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

### ABR Master Playlist (`hlsABRMaster`)

`hlsABRMaster` coordinates writing the root `index.m3u8`:

- Each per-rendition segmenter calls `onShardUpdated(slug, bwBps, width, height)` after every segment flush.
- The master debounces and rewrites `index.m3u8` at most every 100ms.
- `overrides sync.Map` holds externally-set metadata (from `SetRepOverride`). Override values take precedence over what segmenters report, so the master reflects the new profile immediately even before the first new segment arrives from the updated FFmpeg process.

### DASH Packager (fMP4)

The DASH packager uses a `tsBuffer` (buffered pipe) feeding into an MPEG-TS demuxer:

1. Raw TS from buffer subscriber → `tsBuffer.Write` (never blocks; replaces `io.Pipe` which caused packet loss under transcoding load).
2. The demuxer extracts H.264/AAC elementary streams.
3. An fMP4 muxer boxes them into `init.mp4` + `seg_N.m4s`.
4. Dynamic MPD updated after each segment.

H.265 in the TS path is currently not supported by the DASH packager and logged as a warning.

### RTSP / RTMP / SRT Serve

These use **shared listeners** — one TCP/UDP port serves all streams:

- **RTSP**: clients connect to `rtsp://host:port/live/<code>`
- **RTMP play**: clients connect to `rtmp://host:port/live/<code>` with app `live`
- **SRT listen**: clients connect with `streamid=live/<code>`

Each serves the `PlaybackBufferID` for the requested stream.

### RTMP Push-Out

`push_rtmp.go` re-streams a stream's media buffer out to a remote RTMP/RTMPS endpoint
(YouTube, Facebook, Twitch, generic CDN). Each push destination runs as its own
`"push:<url>"` goroutine inside the per-stream `streamState.protocols` map.

**Transport:** the publisher dials the remote itself — plain TCP for `rtmp://` and TLS
for `rtmps://` (default port 443). The RTMP client reads and writes through the
(optionally TLS-wrapped) `net.Conn` the publisher already opened. Inbound bytes are
pumped into the client from a single reader goroutine; outbound frames are written
through the client's frame API.

**Codec config:** the underlying H.264 muxer extracts SPS/PPS from each new stream
itself and emits the `AVCDecoderConfigurationRecord` on the first VCL frame; the
AAC muxer accepts ADTS-prefixed frames and emits the AAC sequence header on the
first audio frame. The publisher does **not** carry codec config across reconnects —
the muxer rebuilds it from the next keyframe.

**Handshake — FMLE flow:** when the connection state advances to
`RTMP_CONNECTING`, the client sends `releaseStream` and `FCPublish` before
`createStream` and `publish` — the legacy FMLE order that strict ingest pops
(YouTube/Twitch) require. The publisher then waits for the publish-start state
(and the corresponding `NetStream.Publish.Start` status callback) before sending
any media. If a publish-failed state arrives instead, the connection is torn down
and the outer reconnect loop backs off.

**Pre-publish queue + keyframe-gated drain:** AV packets pulled from the buffer
arrive while the handshake is still in progress. They are queued; once the ready
signal fires, the drain step drops everything older than the first H.264 keyframe
and then begins writing in order. This guarantees the remote starts with a
decodable GOP.

**Per-input discontinuity:** discontinuity is handled in the publisher's
`feedLoop`, **not** in the ingestor (per project rule "ingestor must not know
about source switch"). When a `Discontinuity` AV packet arrives:

- if the session is already ready → tear it down so the next loop iteration
  reconnects with fresh codec probing on the new source;
- if not ready yet → absorb silently (the queue + keyframe-gate already handles it).

**Reconnect:** any error from the read loop, write path, or status handler signals
the failure channel; the outer goroutine cancels the session, sleeps
`RetryTimeoutSec` (per-destination, default backoff), and retries. The destination
URL is parsed once per connect attempt so DNS rotates correctly across retries.

---

## 8. DVR & Timeshift

### Purpose

Record every stream as a persistent on-disk MPEG-TS archive with wall-time metadata, enabling VOD playback and arbitrary timeshift seeks without additional transcoding.

### Segment Cutting — PTS-Based

HLS sources deliver content faster than real-time (they dump an entire 6-second segment instantly). Wall-clock measurement would produce incorrect segment durations.

Solution: track `segStartPTSms` from the first packet's `PTSms` field. Cut condition:

```text
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

```text
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
    log.Warn("event bus: queue full, dropping event", "type", event.Type)
}
```

Handlers registered with `Subscribe(eventType, handler)` are called by worker goroutines. A handler that panics does not kill the bus — `recover()` is called in the worker loop.

### Domain Events

| Event Type | Trigger | Payload |
| ---------- | ------- | ------- |
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

The Kafka deliverer uses a lazy writer per topic; brokers are configured via `hooks.kafka_brokers`. NATS is not implemented.

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

```text
./data/
  streams.json      { "<code>": Stream, ... }
  recordings.json   { "<id>": Recording, ... }
  hooks.json        { "<id>": Hook, ... }
```

Writes are atomic: marshal → write tmp file → rename. Reads deserialise the entire file. Suitable for development and single-node deployments.

### SQL Driver

Postgres/MySQL with three tables: `streams`, `recordings`, `hooks`. Each row stores the entity as a JSONB column plus indexed lookup fields (`code`, `status`, `stream_code`).

### MongoDB Driver

Each entity stored as a BSON document in its own collection. `_id` = domain ID field. Indexed on `code`/`stream_code` for lookups.

---

## 11. API Server

### Design

The HTTP server is **stateless** — it delegates every operation to the service layer and returns the result. No goroutines or in-memory caches live inside the API handlers.

### Router

Middleware stack: `RequestID` → `RealIP` → `Recoverer` → `Logger` → `Timeout(120s)`.

Route groups:
- `/streams/{code}` — stream CRUD + lifecycle + recording sub-routes
- `/recordings/{rid}` — recording metadata + media serving
- `/hooks/{hid}` — hook CRUD + test
- `/{code}/*` — HLS/DASH static file serving (via `mediaserve.Mount`)

### `PUT /streams/{code}` — Hot-Reload

When a stream is already running, `PUT /streams/{code}` performs a **hot-reload** rather than a full stop/start:

1. Persist the new config to the store first (pipeline keeps running with old config if save fails).
2. Call `coordinator.Update(ctx, old, new)` — which applies only the diff.

This means changing a push destination does not interrupt HLS viewers; toggling DASH does not restart the RTSP server; updating one transcoding profile does not restart FFmpeg processes for other profiles.

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

Configuration is loaded in three layers (each overrides the previous):

1. Built-in defaults (`setDefaults()` in `config/config.go`)
2. `config.yaml` in the current working directory or `/etc/open-streamer/`
3. Environment variables with prefix `OPEN_STREAMER_` (dots replaced by underscores)

The root `Config` struct is populated once at startup and passed to each service as its relevant sub-config only (e.g. `cfg.Publisher` for Publisher, `cfg.Ingestor` for Ingestor). Services never receive the full `Config`.

### Dependency Injection

All services are registered with a DI container and resolved lazily on first use. Each service constructor follows the pattern:

```go
func New(i Injector) (*Service, error) {
    dep := MustInvoke[*dep.Service](i)
    cfg := MustInvoke[*config.Config](i)
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

Graceful shutdown calls the container's `Shutdown()`, which invokes `HealthCheck` / `Shutdown` on each registered service in reverse registration order.

---

*Updated 2026-04-21. Keep this document in sync when changing subsystem behaviour.*
