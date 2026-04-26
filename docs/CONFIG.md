# Open Streamer — Configuration Reference

Every config field, what it does, what happens when you leave it unset.
Companion to [USER_GUIDE.md](./USER_GUIDE.md) (workflows) and
[FEATURES_CHECKLIST.md](./FEATURES_CHECKLIST.md) (capabilities).

> **Two layers of config**
> - **`config.yaml` + env (StorageConfig only)** — bootstrap-only: where
>   to find the persistent store. Read once at startup.
> - **GlobalConfig + Stream config (the rest)** — persisted in the store,
>   editable at runtime via REST API. Hot-applied without restart.
>
> Defaults are the SINGLE SOURCE OF TRUTH at
> [internal/domain/defaults.go](../internal/domain/defaults.go) and
> reachable live via `GET /api/v1/config/defaults`.

---

## 1. Storage (bootstrap-only)

Loaded by `viper` in this order: defaults → `config.yaml` → env (`OPEN_STREAMER_*`, dots → underscores).

```yaml
# config.yaml
storage:
  driver:   json          # json | yaml | sql | mongo
  json_dir: ./test_data   # for driver=json
  yaml_dir: ./test_data   # for driver=yaml
```

Equivalent env override:

```bash
export OPEN_STREAMER_STORAGE_DRIVER=yaml
export OPEN_STREAMER_STORAGE_YAML_DIR=/var/lib/open-streamer
```

| Driver | Backend | Notes |
|---|---|---|
| `json` (default) | Flat JSON files under `json_dir/` | Single-node; fastest setup |
| `yaml` | Single `open_streamer.yaml` | Human-editable; good for ops-as-code |
| `sql` | Postgres / MySQL via JSONB | HA; auto-migrate on startup |
| `mongo` | MongoDB BSON+JSON | Index init on startup |

SQL/Mongo connection details: `OPEN_STREAMER_STORAGE_SQL_DSN`, `OPEN_STREAMER_STORAGE_MONGO_URI` etc. — see `cmd/server/main.go` for the full list.

---

## 2. GlobalConfig

Edit at runtime via `POST /api/v1/config { ... }` (partial merge — only present fields update). Or wholesale via `PUT /api/v1/config/yaml`.

A section being **`null` / absent** means the corresponding subsystem is **disabled**. Setting a section enables it — the runtime manager hot-starts services to match.

```yaml
global_config:
  server: ...        # HTTP API listener
  buffer: ...        # Per-stream ring buffer capacity
  manager: ...       # Failover settings
  publisher: ...     # HLS / DASH directories + segment params
  listeners: ...     # Network listeners (RTMP / RTSP / SRT)
  ingestor: ...      # Server-wide ingest settings
  transcoder: ...    # FFmpeg path + multi-output toggle
  hooks: ...         # Worker pool + Kafka brokers
  log: ...           # Level + format
```

### 2.1 server

```yaml
server:
  http_addr: ":8080"        # Required. Binds REST API + static delivery + metrics.
  cors:
    enabled:           true
    allowed_origins:   ["https://console.example.com"]
    allowed_methods:   ["GET", "POST", "PUT", "DELETE"]
    allowed_headers:   ["Content-Type", "Authorization"]
    allow_credentials: true       # MUST be false when allowed_origins=["*"]
    max_age:           600        # Preflight cache, seconds
```

`server.http_addr` is **required**. Unset → server cannot start.

### 2.2 buffer

```yaml
buffer:
  capacity: 1024    # MPEG-TS packets per subscriber. Default: 1024 (~1MB / 1.5s of 1080p).
```

Higher = more headroom for HLS pull bursts, more RAM per subscriber. Lower = more drops under load.

### 2.3 manager

```yaml
manager:
  input_packet_timeout_sec: 30   # Default. Active input declared dead after this gap.
```

Tune higher (60+) for HLS pull where one Read = one full segment (segment duration × 2 minimum). Tune lower (5-10) for low-latency RTMP / SRT.

### 2.4 publisher

```yaml
publisher:
  hls:
    dir:                /var/hls    # REQUIRED. Empty disables HLS for all streams.
    live_segment_sec:   2           # Default. Segment target duration.
    live_window:        12          # Default. Segments in sliding playlist.
    live_history:       0           # Default. Extra segments kept on disk after sliding out.
    live_ephemeral:     true        # If true: bounded retention; segments deleted as they leave window.
  dash:
    dir:                /var/dash   # REQUIRED if dash enabled. MUST differ from hls.dir.
    live_segment_sec:   2
    live_window:        12
    live_history:       0
    live_ephemeral:     true
```

**`dir` is required** when the corresponding format is enabled — empty path disables that format silently and logs an error per stream.

### 2.5 listeners

Each protocol shares ONE port serving both ingest and playback:

```yaml
listeners:
  rtmp:
    enabled:     true
    listen_host: "0.0.0.0"   # Default. All interfaces.
    port:        1935        # Default.
  rtsp:
    enabled:     true
    listen_host: "0.0.0.0"
    port:        554         # Default.
    transport:   "tcp"       # Default. tcp | udp.
  srt:
    enabled:     true
    listen_host: "0.0.0.0"
    port:        9999        # Default.
    latency_ms:  120         # Default. Haivision low-latency reference.
```

`listen_host` empty → bind all interfaces. Use specific IP for multi-NIC isolation.

### 2.6 ingestor

```yaml
ingestor:
  hls_max_segment_buffer: 8     # Default. Pre-fetched HLS segments held per stream.
```

Server-wide. Per-input HLS connect / segment timeouts come from `inputs[].net.connect_timeout_sec` (defaults 15s playlist / 60s segment).

### 2.7 transcoder

```yaml
transcoder:
  ffmpeg_path:  ""           # "" = lookup "ffmpeg" via $PATH. Or absolute path.
  multi_output: false        # See below.
```

`multi_output=true` flips the encoder topology:
- **false (default)**: 1 FFmpeg process per profile (legacy). 2 profiles → 2 processes. One profile crash → just that rung restarts.
- **true**: 1 FFmpeg process per stream, emitting N rendition pipes. Saves ~50% NVDEC + ~40% RAM per ABR stream. Trade-off: 1 input glitch brings down all profiles together (~2-3s).

Toggling this hot-restarts every running stream's transcoder (~2-3s downtime per stream).

### 2.8 hooks

```yaml
hooks:
  worker_count:  4                          # Concurrent delivery goroutines.
  kafka_brokers: ["broker1:9092", "broker2:9092"]
```

`worker_count` validation: must be > 0 if hooks section is set. `kafka_brokers` empty → Kafka hooks unavailable (HTTP hooks still work).

Per-hook `max_retries` (default 3) and `timeout_sec` (default 10) live on each Hook record, not GlobalConfig.

### 2.9 log

```yaml
log:
  level:  "info"     # debug | info | warn | error
  format: "text"     # text | json
```

Hot-applied — change without restart.

---

## 3. Stream config

Stored per-stream. Edit via `POST /api/v1/streams/{code}` (partial merge).

```yaml
code:        news                # Required. [a-zA-Z0-9_], max 128 chars.
name:        "News Channel"
description: ""
tags:        ["news", "production"]
stream_key:  ""                  # Auth token for RTMP/SRT push. "" = no auth.
disabled:    false               # true = exclude from boot + reject Start.

inputs:                          # Ordered by priority (must be 0..N-1 contiguous).
  - url:      "rtmp://primary/live"
    priority: 0
    headers:  { "Authorization": "Bearer X" }       # HTTP/HLS only.
    params:   { "passphrase": "..." }               # SRT/S3.
    net:
      connect_timeout_sec: 15    # HLS playlist GET timeout. 0 = use defaults.
      reconnect:           true  # Pull readers auto-reconnect on drop.
      insecure_tls:        false # HTTPS — allow self-signed (use with care).
  - url:      "rtmp://backup/live"
    priority: 1

transcoder:                      # null = no transcoding (passthrough).
  global:
    hw:        "nvenc"           # none | nvenc | vaapi | qsv | videotoolbox
    fps:       0                 # 0 = match source. Override applies to all profiles.
    gop:       0                 # 0 = derive from KeyframeInterval × fps. Else explicit GOP frames.
    deviceid:  0                 # CUDA device index when HW=nvenc on multi-GPU host.
  decoder:
    name:      ""                # FFmpeg decoder name. "" = let FFmpeg auto-pick.
  audio:
    copy:        false           # true = passthrough audio (no re-encode).
    codec:       "aac"           # aac | mp3 | opus | ac3 | copy
    bitrate:     128             # kbps
    sample_rate: 0               # Hz. 0 = match source.
    channels:    0               # 1=mono, 2=stereo, 6=5.1. 0 = match source.
    language:    ""              # ISO 639-1 (en, vi, ...).
    normalize:   false           # EBU R128 -23 LUFS.
  video:
    copy:      false             # true = passthrough video.
    interlace: ""                # "" | auto | tff | bff | progressive
    profiles:                    # Ordered ladder. track_1, track_2, …
      - width:             1920
        height:            1080
        bitrate:           5000  # kbps. 0 = encoder default (~2500).
        max_bitrate:       7500  # kbps peak. 0 = no -maxrate emitted (CBR-ish).
        framerate:         30    # 0 = match source.
        keyframe_interval: 2     # GOP target in seconds.
        codec:             ""    # "" / h264 / h265 / vp9 / av1 — auto-routed by hw.
                                 # OR explicit: h264_nvenc / h264_qsv / h264_vaapi / etc.
        preset:            ""    # libx264: ultrafast..placebo. NVENC: p1..p7 + legacy aliases.
                                 # Auto-translated between families; "veryfast" works on NVENC too.
        profile:           ""    # baseline | main | high (H.264). main | main10 (H.265).
        level:             ""    # "3.1", "4.0", "4.1", ...
        bframes:           null  # null = encoder default. 0 = disable. 2-3 = typical VOD.
        refs:              null  # null = encoder default. Higher = better compression, more CPU.
        sar:               ""    # "" = inherit. "N:M" = explicit Sample Aspect Ratio.
        resize_mode:       "pad" # pad | crop | stretch | fit. Default: pad.
  extra_args: []                 # Raw FFmpeg args appended; use with care.

protocols:                       # Each true → publisher starts that output.
  hls:  true
  dash: false
  rtsp: false
  rtmp: false
  srt:  false

push:                            # Outbound destinations.
  - url:               "rtmp://a.rtmp.youtube.com/live2/{key}"
    enabled:           true
    timeout_sec:       0         # 0 = default (10).
    retry_timeout_sec: 0         # 0 = default (5).
    limit:             0         # 0 = unlimited retries.
    comment:           "YouTube"

dvr:                             # null = no DVR.
  enabled:          true
  segment_duration: 0            # 0 = default (4 sec).
  retention_sec:    0            # 0 = forever.
  storage_path:     ""           # "" = default ./out/dvr/{streamCode}.
  max_size_gb:      0            # 0 = no limit.

watermark: null                  # Schema only — not applied yet.
thumbnail: null                  # Schema only — not applied yet.
```

### 3.1 Input URL formats

```
# Pull (server connects out)
rtmp://server.com/live/key
rtsp://camera.local:554/stream
http://cdn.com/live.ts
https://cdn.com/playlist.m3u8
udp://239.1.1.1:5000
srt://relay.com:9999
file:///recordings/loop.ts                # ?loop=true
s3://bucket/path/file.ts                  # ?endpoint=https://minio.local

# Push (encoder connects in)
rtmp://0.0.0.0:1935/live/key              # detected by 0.0.0.0 host
srt://0.0.0.0:9999?streamid=key

# In-process re-stream
copy://upstream_code                       # subscribe to another stream
mixer://video_code?audio=audio_code        # mix video + audio from 2 streams
```

### 3.2 Codec / encoder routing

The user-facing `codec` field is encoder-agnostic — server routes to the right FFmpeg encoder based on `global.hw`:

| codec input | hw=none | hw=nvenc | hw=vaapi | hw=qsv | hw=videotoolbox |
|---|---|---|---|---|---|
| `""` / `h264` / `avc` | libx264 | h264_nvenc | libx264 | libx264 | libx264 |
| `h265` / `hevc` | libx265 | hevc_nvenc | libx265 | libx265 | libx265 |
| `vp9` | libvpx-vp9 | libvpx-vp9 | libvpx-vp9 | libvpx-vp9 | libvpx-vp9 |
| `av1` | libsvtav1 | libsvtav1 | libsvtav1 | libsvtav1 | libsvtav1 |

For VAAPI/QSV/VT operators must spell the full encoder name explicitly (`h264_vaapi`, `h264_qsv`, `h264_videotoolbox`) — empty codec defaults to libx264 on those backends.

The full routing table is in `GET /api/v1/config/defaults` for frontend lookup.

### 3.3 Preset translation

Cross-family preset names are auto-translated so the value works on whichever encoder the codec/HW selection resolves to:

| User input | NVENC encoder | libx264/libx265/QSV |
|---|---|---|
| `ultrafast` / `superfast` | `p1` | passthrough |
| `veryfast` | `p2` | passthrough |
| `faster` / `fast` | `p3` / passthrough (legacy alias) | passthrough |
| `medium` | passthrough (legacy alias) | passthrough |
| `slow` | passthrough (legacy alias) | passthrough |
| `slower` | `p6` | passthrough |
| `veryslow` / `placebo` | `p7` | passthrough |
| `p1` — `p7` | passthrough | translated to nearest libx264 name |

VAAPI / VideoToolbox don't have `-preset` — value dropped silently (encoder uses its own defaults).

---

## 4. Hook config

```yaml
id:           "ops-pager"           # Required. Unique hook ID.
name:         "Pager for ops"
type:         "http"                # http | kafka
target:       "https://ops/events"  # HTTP URL OR Kafka topic
secret:       "shared-secret"       # HMAC-SHA256 signing for HTTP. Empty = no signing.
event_types:  ["stream.stopped", "transcoder.error", "input.failover"]
                                    # [] = all events
stream_codes:                       # null = all streams
  only:   ["news", "sports"]
  except: ["test_*"]                # only / except mutually exclusive — only wins
metadata:                           # merged into every payload as `metadata.*`
  environment: "prod"
  region:      "ap-southeast-1"
enabled:      true
max_retries:  3                     # Per-attempt retry budget. 0 = use default (3).
timeout_sec:  10                    # Per-attempt deadline. 0 = use default (10).
```

For event payload schemas see [APP_FLOW.md § Events reference](./APP_FLOW.md#events-reference).

---

## 5. Defaults reference

Single source of truth: [internal/domain/defaults.go](../internal/domain/defaults.go).

| Field | Default | Sentinel meaning of 0 / "" |
|---|---|---|
| `buffer.capacity` | 1024 | — |
| `manager.input_packet_timeout_sec` | 30 | — |
| `publisher.hls.live_segment_sec` | 2 | — |
| `publisher.hls.live_window` | 12 | — |
| `publisher.hls.live_history` | 0 | drop after sliding out |
| `publisher.hls.live_ephemeral` | false | — |
| (DASH same defaults as HLS) |  | |
| `listeners.rtmp.port` | 1935 | — |
| `listeners.rtmp.listen_host` | "0.0.0.0" | all interfaces |
| `listeners.rtsp.port` | 554 | — |
| `listeners.rtsp.transport` | "tcp" | — |
| `listeners.srt.port` | 9999 | — |
| `listeners.srt.latency_ms` | 120 | — |
| `ingestor.hls_max_segment_buffer` | 8 | — |
| `ingestor.rtmp_connect_timeout_sec` | 10 | — |
| `ingestor.rtsp_connect_timeout_sec` | 10 | — |
| `ingestor.hls_playlist_timeout_sec` | 15 | — |
| `ingestor.hls_segment_timeout_sec` | 60 | — |
| `transcoder.ffmpeg_path` | "ffmpeg" | $PATH lookup |
| `transcoder.multi_output` | false | — |
| `transcoder.video.bitrate_k` | 2500 | — |
| `transcoder.video.resize_mode` | "pad" | — |
| `transcoder.audio.codec` | "aac" | — |
| `transcoder.audio.bitrate_k` | 128 | — |
| `transcoder.global.hw` | "none" | — |
| `transcoder.global.deviceid` | 0 | CUDA device 0 |
| `dvr.segment_duration` | 4 | — |
| `dvr.storage_path_template` | `./out/dvr/{streamCode}` | — |
| `dvr.retention_sec` | — | **0 = forever** |
| `dvr.max_size_gb` | — | **0 = unlimited** |
| `push.timeout_sec` | 10 | — |
| `push.retry_timeout_sec` | 5 | — |
| `push.limit` | — | **0 = unlimited retries** |
| `hook.max_retries` | 3 | 0 = use default |
| `hook.timeout_sec` | 10 | 0 = use default |

---

## 6. Worked examples

### Pull HLS, transcode 3 rungs NVENC, publish HLS + DASH + RTSP, push to YouTube + Twitch, record DVR

```yaml
code: tv1
inputs:
  - url: "https://upstream.example.com/tv1.m3u8"
    priority: 0
transcoder:
  global: { hw: "nvenc" }
  audio:  { codec: "aac", bitrate: 128 }
  video:
    profiles:
      - { width: 1920, height: 1080, bitrate: 5000, framerate: 30, keyframe_interval: 2, preset: "p4", profile: "high", level: "4.1" }
      - { width: 1280, height:  720, bitrate: 2500, framerate: 30, keyframe_interval: 2, preset: "p4", profile: "main" }
      - { width:  854, height:  480, bitrate: 1200, framerate: 30, keyframe_interval: 2, preset: "p4", profile: "main" }
protocols:
  hls:  true
  dash: true
  rtsp: true
push:
  - { url: "rtmp://a.rtmp.youtube.com/live2/YT_KEY",     enabled: true, comment: "YouTube" }
  - { url: "rtmp://live.twitch.tv/app/TWITCH_KEY",       enabled: true, comment: "Twitch"  }
dvr:
  enabled:          true
  segment_duration: 4
  retention_sec:    604800     # 7 days
  max_size_gb:      200
```

### RTMP push ingest from OBS, copy passthrough to HLS

```yaml
code: obs_feed
inputs:
  - url: "rtmp://0.0.0.0:1935/live/secret_key"   # 0.0.0.0 host = push listen
    priority: 0
protocols:
  hls: true
# no transcoder — copy passthrough
```

OBS configuration: server `rtmp://your-server:1935/live`, key `secret_key`.

### Multi-input failover with manual override

```yaml
code: redundant_feed
inputs:
  - { url: "rtmp://primary/live",   priority: 0 }
  - { url: "rtmp://backup/live",    priority: 1 }
  - { url: "https://cdn/live.m3u8", priority: 2 }
manager:                  # Per-stream manager override is NOT supported — uses global config.
                          # See global_config.manager.input_packet_timeout_sec instead.
protocols: { hls: true }
```

Force priority 1 manually:
```bash
curl -XPOST /api/v1/streams/redundant_feed/inputs/switch -d '{"priority": 1}'
```

The manager records this as `runtime.switches[0]={ reason: "manual", to: 1 }`.

### copy:// re-stream

```yaml
# Stream A is the source (encoded once)
code: news_master
inputs:
  - { url: "rtmp://upstream/news", priority: 0 }
transcoder:
  global: { hw: "nvenc" }
  video:  { profiles: [ {width:1280,height:720,bitrate:2500}, {width:854,height:480,bitrate:1200} ] }
protocols: { hls: true }

# Stream B re-streams A as DASH (no re-encode, subscribes to A's published rendition buffers)
code: news_dash
inputs:
  - { url: "copy://news_master", priority: 0 }
protocols: { dash: true }
```

`news_dash` shares `news_master`'s ABR ladder via in-process buffer
subscription — zero extra encode cost.

### Hot-add a profile without disrupting other rungs

```bash
# Original config: 1080p + 720p
# Operator wants to add 480p without restarting 1080p / 720p FFmpeg.

curl -XPOST /api/v1/streams/news -d '{
  "transcoder": {
    "video": {
      "profiles": [
        { "width": 1920, "height": 1080, "bitrate": 5000 },
        { "width": 1280, "height": 720,  "bitrate": 2500 },
        { "width": 854,  "height": 480,  "bitrate": 1200 }
      ]
    }
  }
}'
```

The diff engine identifies "added profile" → spawns one new FFmpeg
worker for 480p → leaves 1080p + 720p untouched. HLS master playlist
updates with the new variant within ~50ms. DASH ABR MPD includes the
new track on next segment flush.

---

## See also

- [USER_GUIDE.md](./USER_GUIDE.md) — install, common workflows
- [ARCHITECTURE.md](./ARCHITECTURE.md) — what each subsystem does
  internally
- [APP_FLOW.md](./APP_FLOW.md) — request lifecycles + event sequences
- [FEATURES_CHECKLIST.md](./FEATURES_CHECKLIST.md) — completion status
