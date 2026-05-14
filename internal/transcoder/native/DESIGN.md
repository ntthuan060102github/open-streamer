# Native transcoder backend — design doc

Replaces the FFmpeg CLI subprocess backend with an in-process pipeline
built on [asticode/go-astiav][astiav] (libavcodec / libavformat /
libavfilter Go bindings). Motivation, library choice rationale, and
the architectural diff that justifies this work live in the parent
investigation thread; this doc focuses on *how* to implement.

[astiav]: https://github.com/asticode/go-astiav

---

## 1. Pipeline architecture

One `Pipeline` instance per Open-Streamer logical stream. Mirrors the
existing `multi` mode shape: one input → one shared decode → N profile
filter+encode+mux chains.

```
io.Reader (cfg.Input)
    │
    ▼
[ AVIOContext ] (custom IO, pull bytes from Go reader)
    │
    ▼
[ InputFormatContext ] AVFormatContext — mpegts demux
    │
    │  AVPacket (encoded H.264 / AAC)
    ▼
[ Video decoder ] CodecContext — h264_cuvid / libx264 dec
    │     + shared HardwareDeviceContext (CUDA)
    │
    │  AVFrame (CUDA hw frame)
    ▼
  fan-out to every ProfileChain  ◄──┐
    │                                │
    ▼                                │
[ FilterGraph ] yadif_cuda → scale_cuda=W:H
    │
    │  AVFrame (CUDA, profile-sized)
    ▼
[ Video encoder ] CodecContext — h264_nvenc
    │
    │  AVPacket (encoded H.264)
    ▼
[ OutputFormatContext ] mpegts mux
    │
    ▼
io.Writer (cfg.Profiles[i].Writer)
```

Audio rides a parallel decode → resample → encode → same mux per
profile (resampling skipped when input matches output sample rate /
channel layout).

---

## 2. Stage-by-stage astiav calls

### 2.1 `initHardware`

**Input**: `cfg.HW domain.HWAccel`.
**Output**: `p.hwDevice *astiav.HardwareDeviceContext` (nil when HW=none).
**Failure mode**: HW backend missing → return wrapped error so caller
sees which backend rejected init. Open-Streamer's existing
`transcoder.Probe` already validates the FFmpeg build at boot, so
runtime failures here should be the rare hardware-yanked case.

```go
hwType := map[domain.HWAccel]astiav.HardwareDeviceType{
    domain.HWAccelNVENC:        astiav.HardwareDeviceTypeCUDA,
    domain.HWAccelQSV:          astiav.HardwareDeviceTypeQSV,
    domain.HWAccelVAAPI:        astiav.HardwareDeviceTypeVAAPI,
    domain.HWAccelVideoToolbox: astiav.HardwareDeviceTypeVideoToolbox,
}[p.cfg.HW]

if hwType == 0 { // HardwareDeviceTypeNone
    return nil // CPU-only pipeline
}

ctx, err := astiav.CreateHardwareDeviceContext(hwType, "", nil, 0)
if err != nil { return fmt.Errorf("create hw device %s: %w", hwType, err) }
p.hwDevice = ctx
```

The empty `device` string lets libav pick the default device (matches
the FFmpeg CLI `-hwaccel cuda` behaviour). For multi-GPU setups we add
a `cfg.HardwareDeviceID` knob later — out of scope for v1.

### 2.2 `initInput`

The non-trivial part: bridging a Go `io.Reader` into libavformat's
expected callback-based IO. astiav exposes `AllocIOContext` taking Go
closures (per `examples/custom_io_demuxing/main.go`).

```go
// Allocate the demuxer container first.
p.inputFormatCtx = astiav.AllocFormatContext()
if p.inputFormatCtx == nil { return errors.New("alloc input format ctx") }

// Custom IO wrapping cfg.Input. 4 KiB buffer matches astiav's example
// and FFmpeg's default for live streams (smaller = more syscalls,
// larger = wasted memory across 21 pipelines).
//
// Seek callback returns -1 (ENOSYS) because cfg.Input is a one-shot
// live stream — non-seekable. Seek isn't called for mpegts demux in
// streaming mode anyway.
ioCtx, err := astiav.AllocIOContext(
    4096, false,
    func(b []byte) (int, error) { return p.cfg.Input.Read(b) },
    nil, // seek: not supported
    nil, // write: input-side, not used
)
if err != nil { return fmt.Errorf("alloc io ctx: %w", err) }
p.inputFormatCtx.SetPb(ioCtx)

// Force mpegts demuxer (Open-Streamer always feeds TS) — skips probe.
if err := p.inputFormatCtx.OpenInput("", astiav.FindInputFormat("mpegts"), nil); err != nil {
    return fmt.Errorf("open input: %w", err)
}
if err := p.inputFormatCtx.FindStreamInfo(nil); err != nil {
    return fmt.Errorf("find stream info: %w", err)
}

// Locate first video stream; ignore secondary tracks.
for _, st := range p.inputFormatCtx.Streams() {
    if st.CodecParameters().MediaType() == astiav.MediaTypeVideo {
        p.decoderStream = st
        break
    }
}
if p.decoderStream == nil { return errors.New("no video stream") }

// Pick decoder. On NVENC hosts we prefer h264_cuvid so decode lives on
// NVDEC; the encoder context shares the same hwDevice so the decoded
// CUDA frame passes to the filter graph without bridging.
decoderName := decoderNameForHW(p.cfg.HW, p.decoderStream.CodecParameters().CodecID())
decoder := astiav.FindDecoderByName(decoderName)
if decoder == nil { return fmt.Errorf("decoder %q not found", decoderName) }

p.decoderCtx = astiav.AllocCodecContext(decoder)
if p.decoderCtx == nil { return errors.New("alloc decoder ctx") }
if err := p.decoderCtx.FromCodecParameters(p.decoderStream.CodecParameters()); err != nil {
    return fmt.Errorf("decoder params: %w", err)
}
if p.hwDevice != nil {
    p.decoderCtx.SetHardwareDeviceContext(p.hwDevice)
}
if err := p.decoderCtx.Open(decoder, nil); err != nil {
    return fmt.Errorf("open decoder: %w", err)
}
```

#### Decoder name selection (`decoderNameForHW`)

| HW | H.264 input | H.265 input |
|---|---|---|
| NVENC (CUDA) | `h264_cuvid` | `hevc_cuvid` |
| QSV | `h264_qsv` | `hevc_qsv` |
| VAAPI | `h264` + hwDevice (no `h264_vaapi` decoder exists; software decode → upload) | `hevc` + hwDevice |
| VideoToolbox | `h264_videotoolbox` (FFmpeg ≥ 4.4) | `hevc_videotoolbox` |
| None | `h264` (libavcodec software) | `hevc` |

VAAPI is special: there's no `h264_vaapi` *decoder* — the standard
software decoder accepts a hw device and uploads the decoded surface
to VAAPI memory. We just attach `p.hwDevice` and let libav handle the
transfer.

### 2.3 `initOutputs`

One `profileChain` per `cfg.Profiles`. Each chain owns a filter graph,
video encoder, audio encoder, and output mux. Per-profile state is
isolated so a filter / encoder error in one rendition doesn't tear
down the others.

```go
for i, prof := range p.cfg.Profiles {
    chain := &profileChain{profile: prof.Profile}

    // ---- Video filter graph ----
    chain.filterGraph = astiav.AllocFilterGraph()
    if chain.filterGraph == nil { return errors.New("alloc filter graph") }

    // buffer src — entry point for decoded frames.
    bufferSrcFilter := astiav.FindFilterByName("buffer")
    chain.bufferSrcCtx, err = chain.filterGraph.NewBuffersrcFilterContext(bufferSrcFilter, "in")
    if err != nil { return fmt.Errorf("filter buffersrc: %w", err) }

    bufferSrcParams := astiav.AllocBuffersrcFilterContextParameters()
    defer bufferSrcParams.Free()
    bufferSrcParams.SetWidth(p.decoderCtx.Width())
    bufferSrcParams.SetHeight(p.decoderCtx.Height())
    bufferSrcParams.SetPixelFormat(p.decoderCtx.PixelFormat())
    bufferSrcParams.SetTimeBase(p.decoderStream.TimeBase())
    bufferSrcParams.SetSampleAspectRatio(p.decoderCtx.SampleAspectRatio())
    if p.hwDevice != nil {
        // Required when filter chain accepts CUDA frames — the buffer
        // src needs the hw frames context so downstream cuda filters
        // (yadif_cuda, scale_cuda) know how to read the surface.
        bufferSrcParams.SetHardwareFramesContext(p.decoderCtx.HardwareFramesContext())
    }
    if err := chain.bufferSrcCtx.SetParameters(bufferSrcParams); err != nil {
        return fmt.Errorf("filter buffersrc params: %w", err)
    }
    if err := chain.bufferSrcCtx.Initialize(nil); err != nil {
        return fmt.Errorf("filter buffersrc init: %w", err)
    }

    // buffer sink — exit point feeding encoder.
    bufferSinkFilter := astiav.FindFilterByName("buffersink")
    chain.bufferSinkCtx, err = chain.filterGraph.NewBuffersinkFilterContext(bufferSinkFilter, "out")
    if err != nil { return fmt.Errorf("filter buffersink: %w", err) }

    // Build the filter graph DSL string. Identical shape to the
    // existing buildVideoFilter output; the only difference is no
    // setsar (we'll use h264_metadata BSF later, see §2.5).
    filterChain := buildFilterChainString(p.cfg.HW, prof.Profile)
    inputs := astiav.AllocFilterInOut(); defer inputs.Free()
    inputs.SetName("out")
    inputs.SetFilterContext(chain.bufferSinkCtx.FilterContext())
    outputs := astiav.AllocFilterInOut(); defer outputs.Free()
    outputs.SetName("in")
    outputs.SetFilterContext(chain.bufferSrcCtx.FilterContext())

    if err := chain.filterGraph.Parse(filterChain, inputs, outputs); err != nil {
        return fmt.Errorf("filter graph parse %q: %w", filterChain, err)
    }
    if err := chain.filterGraph.Configure(); err != nil {
        return fmt.Errorf("filter graph configure: %w", err)
    }

    // ---- Video encoder ----
    encoderName := encoderNameForHW(p.cfg.HW, prof.Profile.Codec)
    encoder := astiav.FindEncoderByName(encoderName)
    if encoder == nil { return fmt.Errorf("encoder %q not found", encoderName) }

    chain.encoderCtx = astiav.AllocCodecContext(encoder)
    chain.encoderCtx.SetWidth(prof.Profile.Width)
    chain.encoderCtx.SetHeight(prof.Profile.Height)
    chain.encoderCtx.SetBitRate(int64(prof.Profile.Bitrate) * 1000)
    chain.encoderCtx.SetFramerate(astiav.NewRational(prof.Profile.Framerate, 1))
    chain.encoderCtx.SetTimeBase(astiav.NewRational(1, prof.Profile.Framerate))
    chain.encoderCtx.SetGopSize(gopFramesForProfile(prof.Profile))

    if p.hwDevice != nil {
        // Encoder must declare which hw frames it consumes. The buffer
        // sink already produces CUDA frames; tell the encoder to expect
        // CUDA and to use the shared hw device.
        chain.encoderCtx.SetPixelFormat(astiav.PixelFormatCuda)
        // hw frames ctx comes from the filter graph's buffersink — it
        // sets up its own hw frames ctx based on scale_cuda output.
        chain.encoderCtx.SetHardwareFramesContext(chain.bufferSinkCtx.HardwareFramesContext())
    } else {
        chain.encoderCtx.SetPixelFormat(astiav.PixelFormatYuv420P)
    }

    // Codec-private options (preset, profile, level, etc.) go in a
    // Dictionary passed to Open(). Mirrors what the FFmpeg CLI args
    // builder emits — same encoder param mapping function reused.
    encoderOpts := astiav.NewDictionary()
    defer encoderOpts.Free()
    for k, v := range encoderPrivateOptions(p.cfg.HW, prof.Profile) {
        _ = encoderOpts.Set(k, v, 0)
    }
    if err := chain.encoderCtx.Open(encoder, encoderOpts); err != nil {
        return fmt.Errorf("open encoder: %w", err)
    }

    // ---- Audio encoder (mirror video pattern, simpler) ----
    // ... AllocCodecContext for "aac", SetSampleRate / ChannelLayout /
    // BitRate from prof.Audio, Open.

    // ---- Output mux ----
    chain.outputFormat, err = astiav.AllocOutputFormatContext(nil, "mpegts", "")
    if err != nil { return fmt.Errorf("alloc output ctx: %w", err) }

    outIO, err := astiav.AllocIOContext(
        4096, true, // writable
        nil, nil,
        func(b []byte) (int, error) { return prof.Writer.Write(b) },
    )
    if err != nil { return fmt.Errorf("alloc output io: %w", err) }
    chain.outputFormat.SetPb(outIO)

    chain.encoderStream = chain.outputFormat.NewStream(nil)
    if err := chain.encoderStream.CodecParameters().FromCodecContext(chain.encoderCtx); err != nil {
        return fmt.Errorf("output stream params: %w", err)
    }
    // ... NewStream for audio similarly.

    if err := chain.outputFormat.WriteHeader(nil); err != nil {
        return fmt.Errorf("write header: %w", err)
    }

    p.outputs = append(p.outputs, chain)
}
```

#### Filter chain string per backend (`buildFilterChainString`)

Same composition logic as the existing
[`transcoder.buildVideoFilter`](../ffmpeg_args.go) — the
implementation literally re-uses that function:

```go
func buildFilterChainString(hw domain.HWAccel, p domain.VideoProfile) string {
    // yadif_cuda,scale_cuda=W:H:force_original_aspect_ratio=decrease:force_divisible_by=2
    // or yadif,scale=... for non-GPU
    return transcoder.BuildVideoFilter( /* port the existing fn to public */ )
}
```

#### Encoder private options (`encoderPrivateOptions`)

Maps to the same args the FFmpeg CLI builder emits:

| Profile field | astiav Dictionary key |
|---|---|
| `Preset` | `preset` (with `normalizePreset` translation) |
| `Profile` | `profile` |
| `Level` | `level` |
| `Framerate` | (set on CodecContext directly, not Dictionary) |
| `Bframes` | `bf` (string) |
| `Refs` | `refs` |
| `MaxBitrate` | `maxrate` + `bufsize` |

Reused: 100 % of the encoder-param mapping logic from `ffmpeg_args.go`
gets ported once into a helper, called from both the CLI backend and
the native backend.

### 2.4 `runLoop` — the hot path

Classic FFmpeg transcoding loop with fan-out:

```go
pkt := astiav.AllocPacket(); defer pkt.Free()
decFrame := astiav.AllocFrame(); defer decFrame.Free()
filterFrame := astiav.AllocFrame(); defer filterFrame.Free()
encPkt := astiav.AllocPacket(); defer encPkt.Free()

for {
    if ctx.Err() != nil { return p.drainAndFlush() }

    if err := p.inputFormatCtx.ReadFrame(pkt); err != nil {
        if errors.Is(err, astiav.ErrEof) { return p.drainAndFlush() }
        return fmt.Errorf("read frame: %w", err)
    }
    // Only video stream of interest for this skeleton; audio handled
    // separately later.
    if pkt.StreamIndex() != p.decoderStream.Index() {
        pkt.Unref()
        continue
    }

    if err := p.decoderCtx.SendPacket(pkt); err != nil {
        pkt.Unref()
        return fmt.Errorf("decoder send: %w", err)
    }
    pkt.Unref()

    for {
        if err := p.decoderCtx.ReceiveFrame(decFrame); err != nil {
            if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
                break
            }
            return fmt.Errorf("decoder receive: %w", err)
        }

        // Fan-out: each profile's filter+encoder chain processes the
        // decoded frame independently. astiav frame refcounts are
        // managed by libav — Send to multiple chains is safe as each
        // SendFrame internally refs the buffers.
        for _, out := range p.outputs {
            if err := p.processFrame(decFrame, filterFrame, encPkt, out); err != nil {
                // Per-profile error: log + mark chain dead, keep others
                // alive. Matches the FFmpeg CLI multi-output behaviour
                // where one bad output didn't kill the input pipeline.
                slog.Warn("native pipeline: profile chain failed",
                    "stream_id", p.cfg.StreamID, "profile", out.profile.Width,
                    "err", err)
                // TODO: mark chain unhealthy so Service can decide
                // restart strategy. v1: just continue.
            }
        }
        decFrame.Unref()
    }
}
```

`processFrame` is straightforward: `bufferSrcCtx.AddFrame` →
`bufferSinkCtx.GetFrame` (loop until EAGAIN/EOF) →
`encoderCtx.SendFrame` → `encoderCtx.ReceivePacket` (loop) → fix up
PTS/DTS rescale → `outputFormat.WriteInterleavedFrame`.

### 2.5 SAR via bitstream filter

The existing CLI backend emits `-bsf:v h264_metadata=sample_aspect_ratio=1/1`
to set SAR without a setsar filter (saves the CUDA pipeline break).
In astiav this is a `astiav.NewBitStreamFilter("h264_metadata")` with
options set via `Dictionary` and ran on the encoded packet stream
before muxing. Add to v1 to keep parity with the BSF SAR fix already
shipped in the CLI backend.

### 2.6 `drainAndFlush`

Send `nil` frame to each filter graph, drain remaining filtered frames,
flush each encoder (SendFrame nil → drain ReceivePacket loop), then
`outputFormat.WriteTrailer()`. Same pattern as the transcoding example.

---

## 3. Resource management

Every astiav alloc has a corresponding `Free()`. Ownership rules:

| Resource | Owned by | Free order |
|---|---|---|
| `Packet` / `Frame` | Pipeline.runLoop (local var) | `defer Free()` |
| `FilterGraph` | `profileChain` | freed before encoder |
| `BuffersrcFilterContext` / `BuffersinkFilterContext` | FilterGraph (autofreed on graph free) | implicit |
| `CodecContext` (encoder + decoder) | Pipeline / profileChain | freed before format ctx |
| `FormatContext` (input/output) | Pipeline / profileChain | freed last |
| `HardwareDeviceContext` | Pipeline (shared) | freed AFTER everything that references it |
| `IOContext` | FormatContext | freed when format ctx freed |
| `Dictionary` | Caller of Open() | freed after Open() returns |

We **don't** use astikit's `Closer` (the example does) because it
collects free functions in declaration order — fine for examples but
fragile for production where the order matters. Explicit
`freeProfileChain` + the `Pipeline.Close` cleanup chain stays grep-able.

---

## 4. Threading model

One goroutine per `Pipeline.Run` call. astiav resources are **not
goroutine-safe** at the libav level — a single resource may not be
touched from two goroutines without external synchronisation. We don't
need it: the run loop is single-threaded, fan-out happens serially
within the goroutine across profiles.

Concurrency between Pipelines: each `*Pipeline` is independent. The
only shared state is the `HardwareDeviceContext` per pipeline — not
shared across pipelines in v1. Cross-pipeline CUDA context pooling is
a v2 optimization.

---

## 5. Threading model (Service integration)

`transcoder.Service.Start` currently spawns goroutines per profile to
manage subprocesses. The native backend simplifies:

```
Service.Start(stream)
  ├── (subprocess backend) goroutine per profile → fork/exec ffmpeg
  └── (native backend)     ONE goroutine → Pipeline.Run
```

Backend selection via a `transcoder.Service` config flag
(`prefer_native_backend bool`). Falls back to subprocess on error.

---

## 6. Error handling philosophy

- **Init errors** (NewPipeline): bubble up immediately; partial init
  is cleaned by the deferred `Close()`.
- **Per-frame errors** in runLoop: classify into transient (`EAGAIN`,
  `EOF` from decoder/encoder when draining) vs fatal (codec context
  invalid, IO write fail). Transient → continue; fatal → return.
- **Per-profile errors** in fan-out: log + mark dead chain, keep
  pipeline alive for other profiles. Matches CLI backend behaviour.
- **HW backend failure** (CUDA OOM, driver crash): treat as fatal,
  return up to Service which decides restart strategy (typically
  back-off and retry — same as subprocess crash recovery).

---

## 7. Open questions / risks

### Q1: hw_frames_context propagation through filter graph

When the decoder produces CUDA frames and the filter graph contains
`scale_cuda`, the filter graph creates a NEW hw_frames_ctx for the
scaled output. The encoder must use **that** ctx, not the decoder's.
astiav exposes `BuffersinkFilterContext.HardwareFramesContext()` which
returns the right one — confirmed via the
`examples/hardware_decoding_filtering/main.go` example.

**Risk**: scale_cuda hw_frames_ctx might fail to init if the encoder
expects a specific software pixel format the scaler doesn't produce.
Mitigation: probe + log at init, fall back to non-hw encoder on
mismatch.

### Q2: IO context blocking semantics

The custom `AllocIOContext` read callback wraps `cfg.Input.Read()`,
which blocks on a Go channel. libav's internal threading expects this
to be tolerable. Need to verify the demuxer doesn't time out or
spin-poll while waiting.

**Mitigation**: prototype with a real buffer-hub subscriber input
during PoC, watch for thread leaks / CPU spikes.

### Q3: Audio handling complexity

This doc focuses on video. Audio transcoding requires a parallel
decoder + filter graph (`aformat`, `aresample`) + encoder. Audio is
much lighter on resources but doubles the code path size. v1 ships
video-only; audio passthrough copy can pipe input audio packets
directly through the mux without re-encoding.

**Decision**: v1 = video-only re-encode + audio passthrough (CodecID
copy). v2 = audio resample + AAC encode if operator config requests
sample rate / channel layout transcoding.

### Q4: Cross-stream NVENC context pooling

Flussonic's 64 % vs Open-Streamer's 100 % gap is mostly explained by
process count + pacing. Pool sharing would be a v2 optimization on top
of the v1 native backend. v1 ships per-pipeline NVENC contexts
(same shape as CLI backend's per-process contexts).

### Q5: Compile-time FFmpeg version dependency

go-astiav links against the host's libav headers via cgo. Production
needs FFmpeg ≥ 6.0 (`AVCodecContext.hw_frames_ctx` interactions, IO
ctx callbacks) which matches the v2.0.0 release notes statement that
all production FFmpeg builds are ≥ 6.x.

**Risk**: cross-compile from macOS to Linux server. Need a Docker
build stage with matching libav headers. Operator workflow uses
`go build` directly today — migration requires updating CI / build
scripts.

### Q6: Watermark integration

Watermarks (drawtext, overlay) are CPU filters. On GPU pipelines they
need `hwdownload` / `hwupload_cuda` wrapping. Same logic as the
current CLI backend — port `applyWatermark` to produce filter strings,
keep the `colorchannelmixer` opacity path.

---

## 8. Migration phases (no commit until each phase is reviewed)

1. **Phase 1 — single profile, no audio, NVENC only, file input**
   PoC verifying the astiav stages work end-to-end on the dev Mac
   (VideoToolbox fallback) and prod-flussonic-09 (NVENC). Output to a
   .ts file. Measure: avgFps, encoder load, latency.

2. **Phase 2 — multi profile, video copy audio, all hw backends**
   Wire fan-out + per-profile encoder. Hook audio packets straight
   into each output mux without re-encode (audio.copy = true).

3. **Phase 3 — Backend interface + Service integration**
   Refactor `transcoder.Service` to take a `Backend` parameter. Two
   implementations: `subprocessBackend` (existing code, refactored),
   `nativeBackend` (this package). Config flag picks.

4. **Phase 4 — Audio re-encode, watermark, BSF SAR**
   Catch up to feature parity with the CLI backend for cases that
   currently require audio re-encode (loudnorm, channel mix) or
   watermark overlay.

5. **Phase 5 — A/B benchmark + cutover**
   Run both backends side-by-side on prod-flussonic-09 (different
   streams). If native backend metrics match Flussonic-tier (~64 %
   encoder load), flip the default for all streams. Keep subprocess
   backend for fallback.

6. **Phase 6 — Cross-stream NVENC context pool (v2)**
   Optimization layer on top of v1. Shared encoder context for streams
   with identical encoder params (resolution + bitrate + preset). Out
   of scope for the initial migration.

---

## 9. Reference

- astiav examples: `~/go/pkg/mod/github.com/asticode/go-astiav@<ver>/examples/`
  - `transcoding/main.go` — full decode → filter → encode → mux loop
  - `hardware_encoding/main.go` — HardwareDeviceContext + hw frames ctx
  - `custom_io_demuxing/main.go` — io.Reader bridge via AVIOContext
  - `hardware_decoding_filtering/main.go` — CUDA frames through filter
- FFmpeg codec docs: https://ffmpeg.org/ffmpeg-codecs.html
- NVENC SDK reference: https://docs.nvidia.com/video-technologies/video-codec-sdk/
- Open-Streamer FFmpeg CLI argument reference: [`FFMPEG.md`](../../../FFMPEG.md)
- Investigation thread (process model, Flussonic comparison): conversation history that produced this branch

---

## 10. Status & honest reassessment (2026-05-14, branch paused)

This section was added when the branch was paused. It documents the
delta between what the design promised and what investigation
later revealed, so future readers can pick this up without
re-walking the same wrong assumptions.

### What is actually built on this branch

- Phase 1 (single-profile PoC) — **done**
- Phase 2 (multi-profile + audio passthrough) — **done**
- Phase 3 (Backend interface) — **skipped on purpose**: the operator
  chose hard cutover, so the FFmpeg CLI subprocess code was deleted
  outright instead of being kept behind a flag.
- Phase 4 partial — watermark + interlace + BSF SAR + audio
  re-encode all **done**. ExtraArgs operator escape hatch dropped.
- Phase 5 (A/B benchmark + cutover) — **never run**. See below.
- Phase 6 (NVENC pool) — **not started**, and the original premise
  is now believed to be wrong; see "Why paused".

Functional surface that compiles + passes unit tests:

| Capability | State |
|------------|-------|
| MPEG-TS demux via custom IO bridge (io.Reader → AVIOContext) | done |
| HW backends: NVENC, QSV, VAAPI, VideoToolbox, software | done |
| One-decode → fan-out to N profile encoder chains | done |
| Per-profile filter graph (yadif → scale → watermark) | done |
| h264_metadata / hevc_metadata BSF for SAR (parity with PR #35) | done |
| Audio passthrough (Copy=true) — Ref/Unref clone per output mux | done |
| Audio re-encode (decode → aresample/aformat/loudnorm → encode → fan-out) | done |
| Crash/restart pacing + per-profile health (carried from CLI runner) | done |

Unit tests live in `*_test.go` next to each module. End-to-end
smoke tests **were not run** — see "Build path blocker" below.

### Why paused

The branch was started on the hypothesis that go-astiav would
reduce GPU encoder usage on prod-flussonic-09 (which sits at
~100 % NVENC engine load while Flussonic on identical hardware
sits at ~64 %). After ~5 days of work the assumption was
re-examined and is now believed to be wrong:

1. **Same codec path underneath.** go-astiav binds directly to
   libavcodec / libavformat / libavfilter — the same libraries the
   FFmpeg CLI binary uses. Subprocess vs in-process changes the
   API entry point, not the silicon work. NVENC engine processes
   the same number of encoded frames either way.

2. **Phase 6 (cross-stream NVENC pool) is technically infeasible
   as designed.** Each `AVCodecContext` owns one NVENC session,
   and a session is stateful (rate control accumulator, DPB,
   GOP position, SPS/PPS). Feeding frames from multiple streams
   into a shared context corrupts every one of those state
   fields. libavcodec does not expose a session-multiplexing
   API — that's an SDK-level capability. A "pool" of pre-warmed
   sessions only reduces start latency, not steady-state load.

3. **The 100 % vs 64 % gap with Flussonic is more likely
   explained by NVENC parameter tuning** (preset, refs,
   `rc-lookahead`, `async_depth`) than by process model.
   Flussonic ships its own `coder-streamer` binary that calls
   the NVIDIA Video Codec SDK directly, bypassing libavcodec —
   that's a fundamentally different path, not a "better way to
   call libavcodec".

What this branch DOES deliver, even though the original framing
was wrong:

- Lower CPU + RAM (no 21-42 subprocess overhead, no pipe IPC
  serialisation between Go and FFmpeg)
- Lower latency between stages (no pipe wakeup delay)
- Code-level control over the pipeline that the subprocess model
  could never give (custom drain ordering, shared decoder state,
  per-frame instrumentation hooks). Foundation that COULD be
  useful if a future direct-NVENC-SDK integration is attempted.

These are real wins but were not what the migration was sold on.

### Build-path blocker (why no smoke test ran)

`scripts/build-dev.sh` sets `CGO_ENABLED=0`. That worked when the
project shelled out to `ffmpeg`; it does not work for go-astiav
because every astiav identifier is C-typed via cgo. Cross-compile
from macOS → linux/amd64 with cgo requires a cross-toolchain plus
matching libavcodec / libavformat / libavfilter / libavutil dev
headers. None of these are in-tree.

Three deploy paths that would unblock smoke testing on
prod-flussonic-09:

1. **Build on the target box.** rsync source → `apt install
   libav*-dev` → `CGO_ENABLED=1 go build`. Simplest, fragile if
   the server's libav major version drifts from astiav v0.40.0's
   expected FFmpeg ≥ 7.x.
2. **Docker build container.** Pin libav version in a Dockerfile
   that matches prod, build inside, extract artefact, scp.
   Reproducible. Untested on this branch.
3. **GitHub Actions Linux runner.** Modify `.github/workflows/
   release.yml` to install libav-dev + build with cgo. Heaviest
   plumbing change.

Whichever path is chosen, **runtime on the server also needs
libavcodec.so / libavformat.so installed**. They already are on
prod-flussonic-09 because the CLI subprocess backend used the
`ffmpeg` binary that links against them — but the major version
must match astiav's expectation. Run `ffmpeg -version | head -1`
to confirm before building.

### Recommended next steps when resuming (or not)

If the goal is still GPU reduction:

1. **First, try NVENC parameter tuning on the existing FFmpeg
   CLI backend** (`-rc-lookahead 0`, `-refs 1`, `-async_depth 4`,
   `-preset p1`). Measure with `nvidia-smi dmon -s u -d 5`. If
   this closes the gap, all of this branch is irrelevant to the
   production goal.
2. **Only if (1) fails to help**, evaluate writing a thin cgo
   wrapper around the NVIDIA Video Codec SDK directly. That's
   1–2 months of work. This branch's `Pipeline` could host the
   integration since it already owns the per-stream goroutine
   and frame plumbing.
3. **Phase 6 as written should NOT be implemented** without
   first proving that libavcodec session-sharing is actually
   possible at the SDK level. Current evidence says it isn't.

If the goal shifts to "ship the native pipeline for non-GPU
reasons" (CPU/RAM reduction, code control, foundation for future
work), the branch is close but **not yet smoke-tested**. Resolve
the build path first, run a single-stream smoke on dev hardware,
then a 1–2-stream smoke on prod-flussonic-09 alongside the CLI
backend before switching all traffic.

### Known gaps (unrelated to the GPU framing question)

- HW frames_ctx propagation through `scale_cuda` is untested
  when scaler creates a new frames pool. Encoder currently
  inherits the decoder's `hw_frames_ctx` as fallback — may
  produce dimension mismatches at runtime. (Q1 in this design
  doc.)
- `TranscoderMode` (multi vs per_profile) is now meaningless at
  runtime — every native pipeline shares one decode and fans
  out internally. The enum is still in domain/coordinator/handler
  for backwards compatibility but does nothing.
- `ExtraArgs` operator escape hatch dropped silently.
