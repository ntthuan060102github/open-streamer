package native

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/asticode/go-astiav"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// Config bundles everything one Pipeline needs to start: where its
// frames come from, what to encode them as, and where to write each
// encoded ladder rung. Mirrors the inputs the existing FFmpeg CLI
// backend takes via [transcoder.Service.Start] (an MPEG-TS source +
// a slice of rendition targets) so the migration is a swap, not a
// schema rewrite.
type Config struct {
	// StreamID is the logical Open-Streamer stream code that owns this
	// pipeline — included in every log line so operators can trace a
	// pipeline back to its stream without grepping ids by hand.
	StreamID domain.StreamCode

	// Input is the MPEG-TS byte stream that feeds the demuxer. The
	// pipeline reads it serially (no random access) and forwards EOF
	// to the decoder so flush + mux trailer happen cleanly.
	//
	// In production this is wired to a buffer-hub subscriber so a
	// pipeline reads exactly the ingest packets without going through
	// FFmpeg's pipe stdin.
	Input io.Reader

	// HW selects the acceleration backend. Matches the existing
	// domain.HWAccel enum so callers don't need to translate values.
	// HWAccelNone keeps the entire pipeline on CPU (libx264).
	HW domain.HWAccel

	// Interlace selects the deinterlace pre-filter applied before
	// scaling. Empty = no deinterlace, mirroring tc.Video.Interlace.
	Interlace domain.InterlaceMode

	// Watermark, when non-nil + Active, layers a drawtext / overlay
	// filter on every profile's filter graph after the scale step.
	// Already resolved by the coordinator — ImagePath is absolute.
	Watermark *domain.WatermarkConfig

	// Audio configures the audio pipeline. Audio is shared across
	// every output (operator-set config is uniform per stream), so
	// there's one decoder + one optional filter graph + one shared
	// encoder feeding all profile muxes. Copy=true short-circuits to
	// the AVPacket passthrough path used since the v1 PoC.
	Audio domain.AudioTranscodeConfig

	// Profiles describes the output ladder. The pipeline shares one
	// decode across every profile (matching FFmpeg `multi` mode), then
	// branches into N filter + encode + mux chains.
	Profiles []ProfileOutput
}

// ProfileOutput binds one ladder rung's encoding parameters to a
// writer where the muxed MPEG-TS bytes land. In production the
// Writer is a buffer-hub publisher subscriber's input channel — the
// equivalent of the per-output `pipe:3`, `pipe:4`, … FDs the FFmpeg
// CLI backend wires through ExtraFiles.
type ProfileOutput struct {
	Profile domain.VideoProfile
	Audio   domain.AudioTranscodeConfig
	Writer  io.Writer
}

// Pipeline owns the long-lived astiav resources for one stream. The
// lifecycle is strictly serial:
//
//	NewPipeline → Run (blocks until ctx done / input EOF / error) → Close
//
// Concurrent Run calls panic — one pipeline = one stream worker.
type Pipeline struct {
	cfg Config

	mu      sync.Mutex
	running bool
	closed  bool

	// hwDevice is the shared CUDA / VAAPI / QSV / VT context handed to
	// the decoder, every filter graph, and every encoder. Sharing
	// avoids GPU↔CPU bridges between stages and keeps surface
	// allocations on the hardware side.
	hwDevice *astiav.HardwareDeviceContext

	// input stage state. Populated by initInput, freed by Close.
	inputFormatCtx *astiav.FormatContext
	decoderStream  *astiav.Stream
	decoderCtx     *astiav.CodecContext

	// audioStream is the source audio track from the input demuxer
	// (nil when the input has no audio). When cfg.Audio.Copy is true
	// (passthrough), the packet bytes are routed straight from this
	// stream into every output mux. When Copy is false the packets
	// flow through audioDecoderCtx → optional filter graph →
	// audioEncoderCtx → fan-out, and the fields below are populated.
	audioStream *astiav.Stream

	// Audio re-encode state. Allocated by initAudio() only when
	// cfg.Audio.Copy=false and a re-encode is genuinely needed
	// (codec / bitrate / sample-rate / channels / loudnorm differ
	// from passthrough). nil-checked at runLoop dispatch so the
	// passthrough path stays cost-free for streams that don't need
	// transcoding.
	audioDecoderCtx     *astiav.CodecContext
	audioEncoderCtx     *astiav.CodecContext
	audioFilterGraph    *astiav.FilterGraph
	audioBufferSrcCtx   *astiav.BuffersrcFilterContext
	audioBufferSinkCtx  *astiav.BuffersinkFilterContext
	audioReencodeActive bool

	// outputs holds one per-profile encoder + mux + writer chain. The
	// slice is parallel to cfg.Profiles so indices match across logs
	// and metrics.
	outputs []*profileChain

	// maxLadderWidth caches the widest configured profile width so each
	// chain can compute its watermark frameScale without re-walking the
	// ladder. Set in initOutputs.
	maxLadderWidth int
}

// profileChain bundles the per-profile filter + encoder + mux state.
// Each profile has its own filter graph (different resolution) and its
// own encoder context (different bitrate / preset / tune), but they
// share the upstream decoder via reference-counted CUDA frames.
type profileChain struct {
	profile domain.VideoProfile

	filterGraph   *astiav.FilterGraph
	bufferSrcCtx  *astiav.BuffersrcFilterContext
	bufferSinkCtx *astiav.BuffersinkFilterContext

	encoderCtx    *astiav.CodecContext
	outputFormat  *astiav.FormatContext
	encoderStream *astiav.Stream

	// bsfCtx is the h264_metadata / hevc_metadata bitstream filter that
	// stamps Sample Aspect Ratio onto encoded packets, when the profile
	// configures SAR. nil when SAR is empty, the BSF isn't applicable
	// (non-H.264/H.265 codec), or the BSF isn't compiled into the
	// runtime libavcodec build (graceful fallback — output still ships,
	// just without the SAR override).
	bsfCtx *astiav.BitStreamFilterContext

	// audioOutStream is the output mux's audio stream (nil when input
	// has no audio). For v1 we copy the input audio AVPacket bytes
	// through without re-encoding so the output container carries the
	// source AAC / Opus track unchanged.
	audioOutStream *astiav.Stream
}

// NewPipeline validates cfg, allocates the shared hwaccel context, and
// runs each stage's init. Failure here cleanly releases everything
// touched so a partial init doesn't leak.
//
// Caller still has to invoke Close to free resources after a
// successful Run — the deferred-cleanup pattern (NewPipeline + defer
// Close) matches astiav idioms.
func NewPipeline(cfg Config) (*Pipeline, error) {
	if cfg.StreamID == "" {
		return nil, errors.New("native pipeline: StreamID is required")
	}
	if cfg.Input == nil {
		return nil, errors.New("native pipeline: Input reader is required")
	}
	if len(cfg.Profiles) == 0 {
		return nil, errors.New("native pipeline: at least one profile is required")
	}

	p := &Pipeline{cfg: cfg}

	if err := p.initHardware(); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("init hardware: %w", err)
	}
	if err := p.initInput(); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("init input: %w", err)
	}
	if err := p.initAudio(); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("init audio: %w", err)
	}
	if err := p.initOutputs(); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("init outputs: %w", err)
	}
	return p, nil
}

// Run pumps frames from input through every stage until ctx is cancelled,
// the input hits EOF, or an unrecoverable error occurs. Returns nil on
// clean EOF or context cancellation; a non-nil error means the pipeline
// halted mid-flight and the caller should restart it.
//
// Single-shot: a Pipeline runs once. To restart, build a new one.
func (p *Pipeline) Run(ctx context.Context) error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return errors.New("native pipeline: already running")
	}
	if p.closed {
		p.mu.Unlock()
		return errors.New("native pipeline: closed; build a new one")
	}
	p.running = true
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()

	return p.runLoop(ctx)
}

// Close releases every astiav resource the pipeline allocated. Safe to
// call multiple times. Idempotent so the NewPipeline-error path and
// the deferred caller can both invoke it.
func (p *Pipeline) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	for _, o := range p.outputs {
		freeProfileChain(o)
	}
	p.outputs = nil

	if p.decoderCtx != nil {
		p.decoderCtx.Free()
		p.decoderCtx = nil
	}
	if p.audioEncoderCtx != nil {
		p.audioEncoderCtx.Free()
		p.audioEncoderCtx = nil
	}
	if p.audioDecoderCtx != nil {
		p.audioDecoderCtx.Free()
		p.audioDecoderCtx = nil
	}
	if p.audioFilterGraph != nil {
		p.audioFilterGraph.Free()
		p.audioFilterGraph = nil
	}
	p.audioBufferSrcCtx = nil
	p.audioBufferSinkCtx = nil
	if p.inputFormatCtx != nil {
		p.inputFormatCtx.CloseInput()
		p.inputFormatCtx.Free()
		p.inputFormatCtx = nil
	}
	if p.hwDevice != nil {
		p.hwDevice.Free()
		p.hwDevice = nil
	}
	return nil
}

// freeProfileChain releases every astiav resource a profile chain
// allocated, in the correct order so libav doesn't dereference a
// stale pointer mid-teardown:
//
//  1. BSF context — holds par refs copied from the encoder, free first
//  2. Encoder context — releases hw_frames_ctx ref this chain held
//  3. Output format context — closes the mux + AVIOContext
//  4. Filter graph — releases buffer src/sink + scale_cuda alloc
//
// Each field is nil-checked because partial init can call this from
// an error path before all fields are populated.
func freeProfileChain(c *profileChain) {
	if c == nil {
		return
	}
	if c.bsfCtx != nil {
		c.bsfCtx.Free()
		c.bsfCtx = nil
	}
	if c.encoderCtx != nil {
		c.encoderCtx.Free()
		c.encoderCtx = nil
	}
	if c.outputFormat != nil {
		// Free includes the AVIOContext set via SetPb.
		c.outputFormat.Free()
		c.outputFormat = nil
	}
	if c.filterGraph != nil {
		// FilterGraph.Free releases the buffer src/sink filter
		// contexts internally — don't free them separately.
		c.filterGraph.Free()
		c.filterGraph = nil
	}
	c.bufferSrcCtx = nil
	c.bufferSinkCtx = nil
	c.encoderStream = nil
	c.audioOutStream = nil
}
