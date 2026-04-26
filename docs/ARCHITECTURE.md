# Open Streamer — Architecture

How the system is wired and **why**. For the operator-facing config see
[CONFIG.md](./CONFIG.md); for end-to-end pipeline traces see
[APP_FLOW.md](./APP_FLOW.md).

---

## 1. Design mindset

Five non-negotiable rules drive every component:

1. **Buffer Hub is the only data source.** No consumer (publisher,
   transcoder, DVR, manager probe) ever reads from the network or a
   sibling module directly. This makes the data path testable end-to-end
   and decouples ingest topology from output topology.

2. **Failover is a Go-level operation.** The Stream Manager swaps the
   active input by stopping the old ingestor goroutine and starting a
   new one — **FFmpeg is never restarted for failover**. Buffer continuity
   means downstream HLS playlists just emit a `#EXT-X-DISCONTINUITY`
   marker; players resume immediately.

3. **One goroutine per ingest stream, not one process.** Pull workers
   live in the same address space, share connections to the storage
   layer, and tear down with `context.Cancel`. No process supervision.
   The single FFmpeg subprocess we spawn is for transcoding only.

4. **Write never blocks.** The Buffer Hub's fan-out uses non-blocking
   sends (`select { case ch <- pkt: default: }`). Slow consumers
   silently drop packets — the ingestor and the upstream connection are
   shielded from any single laggard. This is the most important
   invariant in the codebase: violating it would let a stuck DVR writer
   freeze every viewer.

5. **Hot-reload by diff, never by restart.** `PUT /streams/{code}`
   computes a structured diff of the persisted record and routes each
   change to the minimal set of service calls. Adding a push
   destination doesn't disturb HLS viewers; toggling DASH doesn't drop
   RTMP push sessions; changing one ABR rung restarts only that
   FFmpeg.

Two derived rules:

- **Modules talk through interfaces.** No sibling-module imports — the
  coordinator is the only place that knows about ingestor + manager +
  transcoder + publisher + DVR together.
- **`internal/store/` owns all persistence.** Other modules never
  import database drivers.

---

## 2. High-level topology

```mermaid
flowchart TB
    API["REST API<br/>(chi/v5)"]:::infra
    Coord["Coordinator<br/>(pipeline + diff engine)"]:::infra

    subgraph Sources["Sources"]
        Mgr["Stream Manager<br/>(failover state machine)"]:::svc
        Ing["Ingestor<br/>(1 goroutine / stream)"]:::svc
    end

    Hub(("Buffer Hub<br/>ring buffer / stream<br/>write never blocks")):::data

    Tx["Transcoder<br/>(FFmpeg pool)"]:::svc
    DVR["DVR<br/>(TS segmenter + retention)"]:::svc
    Pub["Publisher<br/>(HLS · DASH · RTSP<br/>RTMP · SRT · Push)"]:::svc

    API -->|CRUD / start / stop / reload| Coord
    Coord --> Mgr
    Coord --> Ing
    Coord --> Pub
    Coord --> Tx
    Coord --> DVR

    Mgr -.health checks.-> Ing
    Ing -->|MPEG-TS packets| Hub
    Hub -.fan-out.-> Tx
    Hub -.fan-out.-> DVR
    Hub -.fan-out.-> Pub
    Tx -->|per-rendition packets| Hub

    classDef infra fill:#1f3a5f,stroke:#5b8def,color:#fff
    classDef svc   fill:#2d4a3e,stroke:#5fc88f,color:#fff
    classDef data  fill:#5a3a1f,stroke:#e0a060,color:#fff
```

**Two buffer namespaces** when transcoder is active:

```mermaid
flowchart LR
    Source[Source URL] --> Ing
    Ing[Ingestor] -->|writes| Raw[("$raw$<code>")]:::data
    Raw -->|reads| Tx[Transcoder]
    Raw -->|reads| DVR[DVR]
    Tx -->|writes| R1[("$r$<code>$track_1")]:::data
    Tx -->|writes| R2[("$r$<code>$track_2")]:::data
    R1 --> P1[HLS / DASH variant 1]
    R2 --> P2[HLS / DASH variant 2]

    classDef data fill:#5a3a1f,stroke:#e0a060,color:#fff
```

Without transcoder, the layout collapses to a single `<code>` buffer
written by the ingestor and read by publishers + DVR.

---

## 3. Subsystems

### Coordinator (`internal/coordinator`)

Wires the per-stream pipeline on `Start` / `Stop` / `Update`. Owns no
data path — pure orchestration.

**`Start`** sequence:
1. Detect topology (legacy ABR vs ABR-copy vs ABR-mixer) from inputs
2. Create raw + rendition buffers as needed
3. Register stream with Manager (which spawns ingest worker)
4. Start Publisher goroutines for enabled protocols + push destinations
5. Optionally start Transcoder workers (one per profile, OR one
   multi-output for the whole stream)
6. Optionally start DVR

**`Update(old, new)`** runs the **diff engine** — 5 independent change
categories:
- **inputs** — Manager.UpdateInputs (add/remove/update without stopping
  the active worker)
- **transcoder topology** — nil↔non-nil or mode change → full pipeline
  rebuild (`reloadTranscoderFull`)
- **profiles** — Add/remove individual rungs:
  - changed: `StopProfile + StartProfile`
  - added: `buf.Create + StartProfile`
  - removed: `StopProfile + buf.Delete`
- **protocols / push** — Publisher.UpdateProtocols (only changed
  protocols cycle; live RTSP viewers preserved)
- **DVR** — toggle on/off; restart with new mediaBuf if best rendition
  shifted

**Status reconciliation** — coordinator tracks per-stream
`streamDegradation { inputsExhausted, transcoderUnhealthy }`. Stream is
`Active` iff every flag is clear; `Degraded` if any flag is set;
`Stopped` when not registered. Manager and Transcoder push their flag
state via callbacks; coordinator never polls them.

### Buffer Hub (`internal/buffer`)

The single source of truth for stream data. One in-memory ring buffer
per stream code. Each consumer (Publisher, Transcoder, DVR) gets an
independent `*Subscriber` with its own bounded channel.

```mermaid
flowchart LR
    W[Ingestor<br/>writer] -->|TS packet| RB(("ring buffer<br/>(per stream)"))
    RB -->|sub.ch <br/>(non-blocking)| S1[Subscriber 1<br/>HLS publisher]
    RB -->|sub.ch <br/>(non-blocking)| S2[Subscriber 2<br/>DASH publisher]
    RB -->|sub.ch <br/>(non-blocking)| S3[Subscriber 3<br/>Transcoder]
    RB -->|sub.ch <br/>(non-blocking)| S4[Subscriber 4<br/>DVR]
    RB -.full channel<br/>= drop packet.-> X((dropped))

    classDef warn fill:#5a3a3a,stroke:#e06060,color:#fff,stroke-dasharray: 3 3
    class X warn
```

```go
// Write never blocks.
func (rb *ringBuffer) write(pkt TSPacket) {
    rb.mu.RLock()
    for _, sub := range rb.subs {
        select {
        case sub.ch <- pkt:
        default:                  // ← packet dropped silently
        }
    }
    rb.mu.RUnlock()
}
```

**Capacity**: subscriber channel is `buffer.capacity` packets (default
1024 ≈ 1MB ≈ 1.5s of 1080p60 @ 5Mbps). HLS pull bursts (one segment
per Read) need this headroom; RTMP/SRT trickle is fine on smaller
sizes.

When ABR is active, ingest writes to `$raw$<code>` (transcoder reads
that), and transcoder writes per-rendition to `$r$<code>$track_N`
(publishers read those). The split keeps DVR recording the original
source rather than a transcoded variant.

### Ingestor (`internal/ingestor`)

One goroutine per stream — never one process. URL scheme drives
protocol selection via `pull.PacketReader` factory:

| Scheme | Reader | Backing lib |
|---|---|---|
| `rtmp://` | RTMPReader | `q191201771/lal` PullSession + AVCC→Annex-B + ADTS wrap |
| `rtsp://` | RTSPReader | `bluenviron/gortsplib/v5` |
| `srt://` | SRTReader | `datarhei/gosrt` |
| `udp://` | UDPReader | stdlib net.UDPConn + RTP-strip |
| `http(s)://...m3u8` | HLSReader | `grafov/m3u8` parser |
| `http(s)://...ts` | HTTPReader | stdlib net.Client |
| `file://` | FileReader | stdlib os.File + paced playback |
| `s3://` | S3Reader | `aws-sdk-go-v2` |
| `copy://` | CopyReader | in-process buffer subscription |
| `mixer://` | MixerReader | in-process video+audio mix |

**Push ingest** (RTMP listen `:1935`, SRT listen `:9999`) shares a
single server per protocol. Incoming connection → registry lookup
(streamid / RTMP app+key) → dispatch to a loopback PullSession that
feeds the Buffer Hub through the same code path as pull mode. The
loopback architecture means RTMP/SRT push streams use the same stable
codec normalisation as pulls, no special-casing.

**Reconnect** is automatic — pull readers retry with their own internal
strategies (`gortsplib`, `lal`, etc.). The manager's per-input
`packet_timeout` is the safety net.

### Stream Manager (`internal/manager`)

Owns failover. Each stream registered with N inputs gets a `streamState`
with per-input `InputHealth` tracking:

- `lastPacketAt` — updated by `RecordPacket` on every ingest packet
- `Status` — `Idle` / `Active` / `Degraded` / `Stopped`
- `Errors[]` — last 5 degradation reasons with timestamps

Per-input health follows a small state machine:

```mermaid
stateDiagram-v2
    [*] --> Idle: Register
    Idle --> Active: First packet (RecordPacket)
    Active --> Degraded: Timeout OR ingestor error
    Degraded --> Idle: Background probe success
    Active --> Idle: Failover (this input demoted)
    Idle --> Active: Failover (this input promoted)
    Degraded --> Active: Auto-reconnect (packets resume)
    Active --> [*]: Unregister
    Idle --> [*]: Unregister
    Degraded --> [*]: Unregister
```

Health check loop (every `monitorInterval=2s`):

- Active input silent > `input_packet_timeout_sec` → mark `Degraded` +
  fire failover
- Degraded inputs probed in background after `failbackProbeCooldown=8s`
- Probe success on a higher-priority input → `failback` switch
  (cooldown `failbackSwitchCooldown=12s`)

Failover commit:
1. `selectBest()` picks lowest-priority `Idle`/`Active` input (or
   override priority if set via manual switch)
2. `ingestor.Start(newInput)` — new goroutine spawns
3. `commitSwitch()` — atomically updates `state.active` + records the
   `SwitchEvent` in rolling history (last 20)

**Switch reasons** tracked in `runtime.switches[]`:

| reason | Trigger |
|---|---|
| `initial` | Register's first activation (`from=-1`) |
| `error` | `ReportInputError` from ingestor |
| `timeout` | Packet timeout in checkHealth |
| `manual` | Operator's `POST /inputs/switch` |
| `failback` | Higher-priority input recovered via probe |
| `recovery` | Exhausted state cleared (active died → probe / packet flow brought it back) |
| `input_added` | UpdateInputs added higher-priority entry |
| `input_removed` | UpdateInputs deleted active entry |

**Auto-reconnect recovery**: pull readers (HLS, RTMP, etc.) handle
their own transient reconnects at the library layer. When packets
resume on a degraded active input ahead of the manager's probe cycle,
`RecordPacket` clears the exhausted flag and records a `recovery`
switch so coordinator status flips back to Active.

### Transcoder (`internal/transcoder`)

Each ABR rung is one FFmpeg subprocess (legacy mode) OR all rungs share
one FFmpeg with N output pipes (multi-output mode).

```mermaid
flowchart LR
    subgraph Legacy["Legacy mode (default) — N processes"]
        Raw1[("$raw$<code>")] --> F1[FFmpeg #1<br/>1080p]
        Raw1 --> F2[FFmpeg #2<br/>720p]
        Raw1 --> F3[FFmpeg #3<br/>480p]
        F1 --> R1a[("$r$<code>$track_1")]
        F2 --> R2a[("$r$<code>$track_2")]
        F3 --> R3a[("$r$<code>$track_3")]
    end

    subgraph Multi["Multi-output mode — 1 process, N pipes"]
        Raw2[("$raw$<code>")] --> FM[FFmpeg<br/>decode 1× + encode N×]
        FM -->|pipe:3| R1b[("$r$<code>$track_1")]
        FM -->|pipe:4| R2b[("$r$<code>$track_2")]
        FM -->|pipe:5| R3b[("$r$<code>$track_3")]
    end
```

**Legacy mode** (default):

- N FFmpeg processes per stream (1 per profile)
- Each subscribes to `$raw$<code>` independently
- Each writes to its own `$r$<code>$track_N` rendition buffer
- One profile crash → just that rung restarts

**Multi-output mode** (`transcoder.multi_output=true`):

- 1 FFmpeg process per stream
- Single decode → N video filter chains → N encoders → N output pipes
  (`pipe:3`, `pipe:4`, ... via `cmd.ExtraFiles`)
- Parent reads each pipe in its own goroutine → fans out to rendition
  buffer
- Saves ~50% NVDEC sessions + ~40% RAM per ABR stream
- Trade-off: one input glitch interrupts all profiles together
  (~2-3s) instead of just one rendition

The `streamWorker` map tracks profile workers under per-(stream,
profile-index) keys. Multi-output uses **shadow `profileWorker`
entries** for indices ≥ 1 — same shape so the existing
`recordProfileError` / `RuntimeStatus` paths see N rungs uniformly,
even though only index 0 owns the real goroutine.

**Encoder routing** (`domain.ResolveVideoEncoder`):
- `codec=""` + `hw=nvenc` → `h264_nvenc`
- `codec=""` + `hw=none` → `libx264`
- `codec="h265"` + `hw=nvenc` → `hevc_nvenc`
- explicit names (`h264_nvenc`, `h264_qsv`) preserved verbatim

**Preset normalization** (`normalizePreset`):
- `veryfast` + NVENC → `p2` (translate)
- `medium` + libx264 → `medium` (passthrough)
- `p4` + libx264 → `medium` (translate)
- garbage value → `""` (drop, encoder uses default — never crash on
  invalid syntax)

**Crash auto-restart**: per-profile loop with exponential backoff (2s →
30s cap). Retries forever — pipeline never tears down on FFmpeg
failure. Spam suppression: after 3 consecutive identical errors, warn
drops to debug; events fire only on power-of-2 attempts. `restart_count`
+ last 5 errors stay visible via `runtime.transcoder.profiles[]`.

**Health detection**: each profile loop tracks consecutive fast
crashes (under 30s). Crossing 3 fires `onUnhealthy` to coordinator →
status Degraded. A sustained run (≥30s) fires `onHealthy` → status
back to Active. Stop / hot-restart paths also fire `onHealthy` so a
freshly-started transcoder always begins from a healthy baseline.

**Pure-GPU pipeline** (NVENC): `decode → scale_cuda → encode` with no
CPU round-trip via hwdownload. All resize modes (`pad`/`crop`/`stretch`/
`fit`) execute on GPU; `pad`/`crop` degrade to aspect-preserving fit
(no server-side letterbox) since the cuda filter graph has no native
crop/pad primitives.

### Publisher (`internal/publisher`)

Reads from Buffer Hub subscriber, segments into output formats:

- **HLS** ([hls.go](../internal/publisher/hls.go)): processes
  `sub.Recv()` directly in main loop — no intermediate goroutine, no
  blocking, packets never dropped at this layer
- **DASH** ([dash_fmp4.go](../internal/publisher/dash_fmp4.go)): uses
  `tsBuffer` (buffered pipe) → `mpeg2.TSDemuxer` → fMP4 segments.
  `tsBuffer.Write` never blocks (replaced `io.Pipe` which caused packet
  loss under transcoding load)
- **RTMP / SRT / RTSP** ([listen.go](../internal/publisher/listen.go)):
  shared listeners; per-client subscribes to the playback buffer

**ABR-aware** segmenters detect when transcoder ladder is active and
auto-emit:
- HLS master playlist (`/{code}/index.m3u8`) with one variant per rung
- DASH root MPD (`/{code}/index.mpd`) with per-track AdaptationSets
- `#EXT-X-DISCONTINUITY` per variant on failover (per-variant
  generation counter — exactly one tag per failover, not one per
  segment)

**Push out** (`push_rtmp.go`, `push_codec.go`): separate goroutine per
destination. Built on `q191201771/lal` PushSession with a custom codec
adapter that emits proper `composition_time = PTS - DTS` so B-frames
render correctly at the receiver. Per-destination state in
`runtime.publisher.pushes[]`: `status` (`starting` / `active` /
`reconnecting` / `failed`), attempt counter, `connected_at` timestamp,
last 5 errors.

### DVR (`internal/dvr`)

Subscribes to the playback buffer (best rendition for ABR, raw
otherwise). Native MPEG-TS segmenter with PTS-based cutting +
wall-clock fallback. Atomic `index.json` writes (tmp→rename) for
metadata; full `playlist.m3u8` for segment timeline.

`#EXT-X-DISCONTINUITY` on every gap (signal loss + server restart).
Resume after restart: `parsePlaylist` rebuilds in-memory segment list
from the on-disk playlist.

Retention: by time (`retention_sec`) + by size (`max_size_gb`). Older
segments pruned + corresponding gap entries removed.

Timeshift VOD: dynamic playlist generated from segment list filtered by
absolute time (`from=RFC3339&duration=N`) or relative offset
(`offset_sec=N`).

### Event Bus & Hooks (`internal/events`, `internal/hooks`)

Typed in-process event bus with bounded queue (512) and worker pool.
Every domain state change emits an Event:

```go
type Event struct {
    ID         string
    Type       EventType
    StreamCode StreamCode
    OccurredAt time.Time
    Payload    map[string]any
}
```

Hooks subscribe via API. Per-hook filters (event types, stream codes
only/except). Per-hook delivery config (max retries, timeout, HMAC
signing for HTTP). Kafka delivery uses lazy per-topic writers.

### API Server (`internal/api`)

`chi/v5` router. Routes by resource:

```
/api/v1/streams/{code}                 — CRUD + start/stop/restart
/api/v1/streams/{code}/inputs/switch   — manual failover
/api/v1/recordings/{rid}               — DVR
/api/v1/recordings/{rid}/playlist.m3u8 — VOD
/api/v1/recordings/{rid}/timeshift.m3u8— time-window VOD
/api/v1/hooks/{id}                     — webhook CRUD
/api/v1/hooks/{id}/test                — synthetic event delivery
/api/v1/config                         — GlobalConfig get/post
/api/v1/config/defaults                — implicit values for UI
/api/v1/config/transcoder/probe        — FFmpeg capability check
/api/v1/config/yaml                    — full system state YAML editor
/api/v1/vod                            — on-disk VOD browse
/healthz, /readyz, /metrics, /swagger  — ops
/{code}/index.m3u8, /{code}/index.mpd  — static delivery
```

### Storage Layer (`internal/store/`)

Repository pattern. Drivers: JSON, YAML, Postgres/MySQL (JSONB),
MongoDB. Selected via `storage.driver`.

```go
type StreamRepository interface {
    List(ctx context.Context, filter StreamFilter) ([]*domain.Stream, error)
    FindByCode(ctx context.Context, code StreamCode) (*domain.Stream, error)
    Save(ctx context.Context, s *domain.Stream) error
    Delete(ctx context.Context, code StreamCode) error
}
```

Same shape for `RecordingRepository`, `HookRepository`,
`GlobalConfigRepository`, `VODMountRepository`.

`internal/store` is the **only package** allowed to import database
drivers — services consume the repository interface.

### Runtime Manager (`internal/runtime`)

Lifecycle wrapper around the long-running services. On boot it loads
GlobalConfig from the store and calls `applyAll(cfg)` — starting each
configured service. On `POST /config` it diffs old vs new and
hot-starts/stops services to match.

Probes FFmpeg at boot via `transcoder.Probe` — fail-fast on missing
required encoders. Hot-swaps `transcoder.multi_output` toggle by
calling `Transcoder.SetConfig` + restarting running streams.

---

## 4. Cross-cutting concerns

### Error handling

- Wrap with context: `fmt.Errorf("module: operation: %w", err)`
- Use `samber/oops` for rich service-layer errors with stack frames
- **Never log AND return an error.** Handle at one site only — duplicate
  logs make ops grep meaningless
- Error history rings (`recordInputError`, `recordProfileErrorEntry`,
  `recordPushErrorEntry`) for UI visibility — newest at index 0, cap 5

### Concurrency

- Lock order: `Service.mu` (broad) → `state.mu` (per-stream). Never
  reverse.
- Hot paths (`RecordPacket`, fan-out write) take RLock first to find
  state pointer, then per-stream Lock to mutate. At packet rates < 100/s
  per stream the per-packet mutex overhead is negligible.
- **Write-never-blocks** invariant: every fan-out uses non-blocking send
  with default branch.
- Goroutine ownership is explicit — every spawn has a clear cancel
  path via `context.WithCancel`. Defer `<-done` channels for clean
  teardown.

### Dependency injection (`samber/do/v2`)

All services registered in `cmd/server/main.go`. Each service
constructor:

```go
func New(i do.Injector) (*Service, error) {
    dep := do.MustInvoke[*dep.Service](i)
    return &Service{dep: dep}, nil
}
```

Sub-configs are extracted from GlobalConfig + provided to DI so each
service sees only its own config type:

```go
do.ProvideValue(i, deref(gcfg.Buffer))
do.ProvideValue(i, deref(gcfg.Transcoder))
// ...
```

Circular deps are broken via post-construction setters
(`ConfigHandler.SetRuntimeManager(rtm)`).

### Testing

- Narrow service interfaces (`coordinator/deps.go`) enable spy-based
  testing without spinning up real ingestors / FFmpeg / RTSP servers
- Build-tagged integration tests (`make test-integration`) spawn real
  ffmpeg with the generated `-vf` chain to catch version-specific
  syntax bugs that pass Go-level checks
- Per-package fixtures avoid cross-package coupling
- Race detector + shuffled order in CI: `-race -shuffle=on -count=1`

### Hot-reload guarantees

- `PUT /streams/{code}` merges JSON onto existing record → diff →
  minimal restart
- Adding a push destination: 0 viewer impact
- Toggling DASH: HLS viewers unaffected
- Changing one ABR profile: only that FFmpeg restarts (legacy mode)
- Multi-output toggle: restarts every running stream's transcoder
  (~2-3s downtime per stream)
- Server config change: runtime manager diffs services, only changed
  ones cycle (no app restart)

---

## 5. Key invariants summary

| Invariant | Where enforced | Why |
|---|---|---|
| Write never blocks | Buffer Hub fan-out | Slow consumer must never freeze ingest |
| Failover doesn't restart FFmpeg | Manager + Coordinator | Transcoder warm-up is expensive (~1-3s) |
| One goroutine per stream | Ingestor | OS process limit + IPC overhead |
| Buffer Hub is sole data source | All consumers | Decouples ingest topology from output |
| `internal/store/` is the only DB-importing package | Module boundary | Pluggable storage, testable services |
| Modules talk via interfaces | `coordinator/deps.go` | No sibling-module direct imports |
| All state changes emit events | Coordinator + Manager + Transcoder + Publisher | Hooks must see consistent timeline |
| Pipeline never tears down on crash | Transcoder retry-forever loop | Streams self-heal — no manual ops needed |
| Stream status reflects all degradation sources | `streamDegradation` reconciliation | UI green badge must mean "actually working" |

---

## See also

- [USER_GUIDE.md](./USER_GUIDE.md) — operator workflows
- [CONFIG.md](./CONFIG.md) — every config field
- [APP_FLOW.md](./APP_FLOW.md) — request lifecycles + event sequences
- [FEATURES_CHECKLIST.md](./FEATURES_CHECKLIST.md) — what's implemented
