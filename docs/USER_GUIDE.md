# Open Streamer — User Guide

How to install, configure, and operate Open Streamer end-to-end.

> Companion docs: [CONFIG.md](./CONFIG.md) (every config field + examples) ·
> [ARCHITECTURE.md](./ARCHITECTURE.md) (how it works) ·
> [APP_FLOW.md](./APP_FLOW.md) (pipeline + events) ·
> [FEATURES_CHECKLIST.md](./FEATURES_CHECKLIST.md) (what's implemented).

---

## 1. Install

### Binary release (recommended)

Latest release ships pre-built archives + Linux installer:

```bash
# Linux/systemd installer — downloads + verifies + installs as a service.
# Idempotent: re-running with a new tag uninstalls cleanly first.
sudo bash <(curl -sL https://raw.githubusercontent.com/ntt0601zcoder/open-streamer/main/build/reinstall.sh) v0.0.31
```

This installs the binary to `/usr/local/bin/open-streamer`, a systemd unit
to `/etc/systemd/system/open-streamer.service`, and creates the data dir
at `/var/lib/open-streamer` (preserved across version upgrades).

### From source

```bash
git clone https://github.com/ntt0601zcoder/open-streamer.git
cd open-streamer
make build          # → bin/open-streamer
make run            # run without persisting binary
```

Requires Go 1.25.9+. FFmpeg is required for transcoding (see § 2).

### Docker

```bash
make docker-build   # → open-streamer:local
make compose-up     # docker compose up
```

---

## 2. FFmpeg

Open Streamer spawns FFmpeg only for transcoding (ingest is pure Go). At
boot the server probes `transcoder.ffmpeg_path` (or `ffmpeg` from
`$PATH`) and fails fast on missing required encoders:

- **Required**: `libx264`, `aac`, muxer `mpegts`
- **Optional warnings**: `h264_nvenc`, `hevc_nvenc` (NVENC), `libx265`,
  `libvpx-vp9`, `libsvtav1`, `libopus`, `libmp3lame`, `ac3`, muxers
  `hls` / `dash`

Probe an arbitrary binary live via `POST /api/v1/config/transcoder/probe`
or the UI's "Test FFmpeg" button before saving the path.

Recommended: ffmpeg ≥ 5.1 with `--enable-libx264 --enable-libx265
--enable-nvenc` (or your HW backend).

---

## 3. First boot

Default storage is JSON flat-file under `./test_data/`. To override
location or backend, set env vars BEFORE first boot:

```bash
export OPEN_STREAMER_STORAGE_DRIVER=yaml
export OPEN_STREAMER_STORAGE_YAML_DIR=/var/lib/open-streamer
./bin/open-streamer
```

The server starts unconfigured (no listeners, no streams) on first boot.
Configure via the REST API or UI. To enable HTTP API, POST a server
config:

```bash
curl -XPOST http://localhost:8080/api/v1/config -d '{
  "server":    { "http_addr": ":8080" },
  "buffer":    {},
  "manager":   {},
  "publisher": { "hls":  { "dir": "/var/hls"  },
                 "dash": { "dir": "/var/dash" } },
  "listeners": { "rtmp": { "enabled": true, "port": 1935 },
                 "rtsp": { "enabled": true, "port": 554  },
                 "srt":  { "enabled": true, "port": 9999 } }
}'
```

If the server isn't running on `:8080` yet, initialize via the YAML
endpoint or pre-seed `open_streamer.yaml` in your data dir.

---

## 4. Create a stream

Streams are the central entity — each binds N inputs (with priority
failover), an optional transcoder ladder, output protocols, push
destinations, and DVR settings. Stream code (`[a-zA-Z0-9_]`) is your
primary key.

### Minimum viable: pull HLS, publish HLS

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news -d '{
  "name": "News Channel",
  "inputs": [
    { "url": "https://upstream.example.com/news/playlist.m3u8", "priority": 0 }
  ],
  "protocols": { "hls": true }
}'
```

Stream is up at `http://localhost:8080/news/index.m3u8`. No transcoding
— packets pass through untouched.

### Multi-input failover

Add backup sources sorted by priority (lower = preferred):

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news -d '{
  "inputs": [
    { "url": "rtmp://primary.example.com/live/news",   "priority": 0 },
    { "url": "rtmp://backup.example.com/live/news",    "priority": 1 },
    { "url": "https://cdn.example.com/news/index.m3u8","priority": 2 }
  ],
  "protocols": { "hls": true }
}'
```

Stream Manager monitors the active input. On failure (configurable via
`manager.input_packet_timeout_sec`, default 30s) it switches to the
next-priority input within ~150ms — **without restarting FFmpeg**. The
switch is recorded in `runtime.switches[]`.

When the higher-priority input recovers (background probe succeeds), the
manager fails back automatically. Override manually:

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news/inputs/switch \
     -d '{ "priority": 0 }'
```

### Add ABR transcoding

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news -d '{
  "transcoder": {
    "global": { "hw": "nvenc" },
    "audio":  { "codec": "aac", "bitrate": 128 },
    "video": {
      "profiles": [
        { "width": 1920, "height": 1080, "bitrate": 5000, "codec": "h264" },
        { "width": 1280, "height": 720,  "bitrate": 2500, "codec": "h264" },
        { "width": 854,  "height": 480,  "bitrate": 1200, "codec": "h264" }
      ]
    }
  },
  "protocols": { "hls": true, "dash": true }
}'
```

Each profile becomes one FFmpeg process emitting an MPEG-TS stream into
its own buffer. HLS master playlist auto-aggregates them at
`/news/index.m3u8`. DASH MPD at `/news/index.mpd`.

**Multi-output mode** (single FFmpeg, N output pipes) cuts NVDEC + RAM
~50% per stream — toggle server-wide:

```bash
curl -XPOST http://localhost:8080/api/v1/config -d '{
  "transcoder": { "multi_output": true }
}'
```

Restart of running streams is automatic. Trade-off: 1 input glitch
brings down all profiles together (~2-3s) instead of just one
rendition.

### Push to platforms

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news -d '{
  "push": [
    {
      "url": "rtmp://a.rtmp.youtube.com/live2/STREAM_KEY",
      "enabled": true
    },
    {
      "url": "rtmps://live-api-s.facebook.com:443/rtmp/STREAM_KEY",
      "enabled": true,
      "limit": 10,
      "retry_timeout_sec": 30
    }
  ]
}'
```

Per-destination state visible at `runtime.publisher.pushes[]` —
status (`starting` / `active` / `reconnecting` / `failed`), attempt
counter, last 5 errors, connected_at timestamp.

### DVR

Per-stream opt-in:

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news -d '{
  "dvr": {
    "enabled":           true,
    "segment_duration":  4,
    "retention_sec":     604800,
    "max_size_gb":       100
  }
}'
```

Storage path defaults to `./out/dvr/{streamCode}`. Recording resumes
across restarts using the on-disk `playlist.m3u8` + `index.json`.
`#EXT-X-DISCONTINUITY` markers on every gap (signal loss + server
downtime).

VOD playback — full recording:
```
GET /recordings/news/playlist.m3u8
```

Timeshift — last 30 minutes:
```
GET /recordings/news/timeshift.m3u8?offset_sec=-1800&duration=1800
```

Timeshift — absolute window:
```
GET /recordings/news/timeshift.m3u8?from=2026-04-26T10:00:00Z&duration=3600
```

---

## 5. Hot-reload

`PUT /streams/{code}` (or repeat `POST`) merges only the changed fields:

```bash
# Add 360p rung — only ONE new FFmpeg process spawns; existing rungs untouched.
curl -XPOST http://localhost:8080/api/v1/streams/news -d '{
  "transcoder": {
    "video": {
      "profiles": [
        { "width": 1920, "height": 1080, "bitrate": 5000 },
        { "width": 1280, "height": 720,  "bitrate": 2500 },
        { "width": 854,  "height": 480,  "bitrate": 1200 },
        { "width": 640,  "height": 360,  "bitrate": 800  }
      ]
    }
  }
}'
```

The diff engine handles 5 categories independently — see
[ARCHITECTURE.md § Coordinator](./ARCHITECTURE.md#coordinator).

To force a full pipeline restart (e.g. switch stream key):

```bash
curl -XPOST http://localhost:8080/api/v1/streams/news/restart
```

To remove a stream entirely:

```bash
curl -XDELETE http://localhost:8080/api/v1/streams/news
```

---

## 6. Hooks (webhooks + Kafka)

Subscribe to lifecycle events:

```bash
# HTTP webhook with HMAC signing.
curl -XPOST http://localhost:8080/api/v1/hooks -d '{
  "id":        "log-everything",
  "type":      "http",
  "target":    "https://ops.example.com/streams/events",
  "secret":    "shared-secret-for-hmac",
  "enabled":   true,
  "max_retries": 5,
  "timeout_sec": 10
}'

# Kafka
curl -XPOST http://localhost:8080/api/v1/hooks -d '{
  "id":      "kafka-events",
  "type":    "kafka",
  "target":  "stream-events-topic",
  "enabled": true,
  "event_types": ["stream.started", "stream.stopped", "input.failover"]
}'
```

Filter by event type or stream code:

```json
{
  "event_types": ["input.failover", "transcoder.error"],
  "stream_codes": { "only": ["news", "sports"] }
}
```

Test delivery:

```bash
curl -XPOST http://localhost:8080/api/v1/hooks/log-everything/test
```

Hook server-wide settings (Kafka brokers, worker pool size) are at
`global_config.hooks` — see [CONFIG.md](./CONFIG.md). Per-hook
defaults: 3 retries, 10s timeout (overridden via Hook fields above).

For the full event catalogue see [APP_FLOW.md § Events
reference](./APP_FLOW.md#events-reference).

---

## 7. copy:// and mixer:// — in-process re-stream

Re-stream another in-process stream as input. No network round-trip; the
downstream stream subscribes directly to the upstream's published
buffer.

### copy:// — straight relay

```json
{
  "code": "news_backup",
  "inputs": [
    { "url": "copy://news", "priority": 0 }
  ],
  "protocols": { "hls": true }
}
```

If `news` has an ABR ladder, `news_backup` mirrors every rendition into
a parallel ABR ladder (no transcoding, just buffer subscriptions). This
is **ABR-copy** mode — the coordinator detects it and bypasses ingest +
transcoder entirely.

If `news` has no ladder, `copy://news` subscribes to its main playback
buffer — single-rendition copy.

Cycle detection prevents `copy://A` ↔ `copy://B` infinite loops via the
copy-graph validator at save time.

### mixer:// — combine video + audio from two streams

```json
{
  "code": "tv_with_radio",
  "inputs": [
    { "url": "mixer://tv_silent?audio=radio_fm", "priority": 0 }
  ],
  "protocols": { "hls": true }
}
```

Video tracks come from `tv_silent`, audio tracks from `radio_fm`.
Useful for muting a TV broadcast and overlaying a radio commentary.

Both streams must already exist; mixer:// validates at save time
(`audio=` param required, no self-mix, no nested mixer chains).

---

## 8. Operations

### Health checks

- `GET /healthz` — liveness (always 200 if process is up)
- `GET /readyz` — readiness (200 once services initialised)

### Metrics

Prometheus scrape endpoint at `GET /metrics`:

- `manager_failovers_total{stream_code}` — rate of input switches
- `transcoder_restarts_total{stream_code}` — FFmpeg crash count
- `transcoder_workers_active{stream_code}` — running profile count
- `manager_input_health{stream_code, input_priority}` — 1 healthy, 0 degraded
- Buffer depth, bytes/packets per stream

### Per-stream runtime status

```bash
curl http://localhost:8080/api/v1/streams/news | jq .data.runtime
```

Returns:
- `status` — `active` / `degraded` / `stopped` / `idle`
- `pipeline_active` — bool
- `active_input_priority` — current source
- `exhausted` — true when all inputs degraded
- `inputs[]` — per-input health + last 5 errors
- `switches[]` — last 20 active-input switches with reason
- `transcoder.profiles[]` — per-rung restart count + errors
- `publisher.pushes[]` — per-destination state

A stream is `degraded` when EITHER:
- All inputs exhausted (manager: no failover candidate), OR
- Transcoder is in a crash loop (3 consecutive FFmpeg crashes < 30s
  apart). Recovers automatically when a process runs > 30s sustained.

### YAML editor (entire system state)

```bash
curl http://localhost:8080/api/v1/config/yaml > backup.yaml
# edit backup.yaml...
curl -XPUT http://localhost:8080/api/v1/config/yaml \
     -H "Content-Type: application/yaml" \
     --data-binary @backup.yaml
```

Round-trips GlobalConfig + all streams + all hooks. Useful for
ops-as-code workflows.

### Logs

`slog` structured output to stderr. Configurable via `log.level` (debug
/ info / warn / error) and `log.format` (text / json). Common ops grep:

```bash
journalctl -u open-streamer | grep -E '(failover|crashed|degraded)'
```

---

## 9. Troubleshooting

| Symptom | Likely cause | Where to look |
|---|---|---|
| Status `degraded` while inputs healthy | Transcoder is in a crash loop on at least one rung | `runtime.transcoder.profiles[].errors[]` (UI or `/streams/{code}`) |
| FFmpeg rejects an encoder option | Codec/preset typo, or option unsupported by the resolved encoder build | Stream config + Settings → Probe FFmpeg checklist |
| Stream up but HLS 404 | `publisher.hls.dir` is empty, or transcoder not producing output | Server logs (`publisher: HLS disabled — …` etc.) |
| Push stuck in `reconnecting` | Destination reject, auth fail, or network loss | `runtime.publisher.pushes[].errors[]` |
| GPU encoder near 100% saturation | NVENC chip overloaded — reduce profile count / framerate, set `bframes=0`, or enable `transcoder.multi_output` | `nvidia-smi` or Grafana GPU dashboard |
| `copy://X` save rejected | `X` doesn't exist, OR cycle detected, OR shape constraint violated | API error message details which check failed |
| Boot fails with "ffmpeg incompatible" | A required encoder/muxer is missing from the FFmpeg build | Probe response under `errors[]`; rebuild ffmpeg with `--enable-libx264` etc. |

For deeper troubleshooting see [APP_FLOW.md](./APP_FLOW.md) — covers
exact event sequences, status reconciliation logic, and what each error
in the runtime snapshot means.

---

## 10. Production checklist

- [ ] FFmpeg installed with required encoders (boot probe will catch this)
- [ ] HLS + DASH dirs are different (when both enabled)
- [ ] HTTP server bind address chosen — reverse proxy in front for TLS
- [ ] Storage backend chosen (`json` for single-node, `sql` / `mongo` for HA)
- [ ] Hooks configured for at least `stream.stopped` + `transcoder.error` so ops gets paged on crashes
- [ ] DVR retention sized against disk capacity
- [ ] `manager.input_packet_timeout_sec` tuned per-protocol (HLS pull bursts may need ≥ segment duration × 2)
- [ ] Prometheus scrape configured against `/metrics`
- [ ] Pre-commit hook installed for contributors (`make hooks-install`)

---

## 11. Updating

```bash
# Use the provided installer for atomic upgrades on systemd hosts:
sudo bash build/reinstall.sh v0.0.32
```

Stops the service, swaps the binary + systemd unit, restarts. Data
directory (streams, recordings, hooks) is preserved across upgrades.

For source builds: `git pull && make build && systemctl restart
open-streamer`.

---

## See also

- [CONFIG.md](./CONFIG.md) — every config field explained, with examples
- [ARCHITECTURE.md](./ARCHITECTURE.md) — design rationale, data flow,
  invariants
- [APP_FLOW.md](./APP_FLOW.md) — pipeline lifecycle, event sequences,
  status reconciliation
- [FEATURES_CHECKLIST.md](./FEATURES_CHECKLIST.md) — what's
  implemented vs planned
