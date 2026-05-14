package native

import (
	"errors"
	"fmt"
	"io"

	"github.com/asticode/go-astiav"
)

// outputWriteBufferSize matches inputReadBufferSize — same trade-off:
// small enough to keep memory predictable across 21 pipelines, big
// enough that per-write callback overhead stays in the noise.
const outputWriteBufferSize = 4096

// initOutputs builds one profileChain per cfg.Profiles entry. Each
// chain owns:
//
//   - a filter graph (yadif → resize → watermark) sized to that profile
//   - a video encoder configured with the profile's bitrate / GOP /
//     preset / etc.
//   - an output format context (mpegts) routed through the profile's
//     io.Writer
//
// Profiles share the upstream decoder + decoded frames via the run
// loop's fan-out — the chains here are encoder-side only.
//
// Failure at any chain partially populates p.outputs. The caller
// (NewPipeline) defers Close() which iterates and frees every entry,
// so a partial init never leaks resources.
//
// maxLadderWidth caches the widest profile so per-chain frameScale
// (used by the watermark "Resize" mode) doesn't re-walk the ladder for
// every output.
func (p *Pipeline) initOutputs() error {
	if len(p.cfg.Profiles) == 0 {
		return errors.New("native pipeline: no profiles configured")
	}

	p.maxLadderWidth = 0
	for _, po := range p.cfg.Profiles {
		if w := po.Profile.Width; w > p.maxLadderWidth {
			p.maxLadderWidth = w
		}
	}
	if p.maxLadderWidth <= 0 {
		// All-zero ladder (source dimensions everywhere) — fall back to
		// 1 so frameScale stays at exactly 1.0 (no shrink).
		p.maxLadderWidth = 1
	}

	for i, prof := range p.cfg.Profiles {
		chain, err := p.buildProfileChain(prof)
		if err != nil {
			return fmt.Errorf("native pipeline: profile %d: %w", i, err)
		}
		p.outputs = append(p.outputs, chain)
	}
	return nil
}

// buildProfileChain wires one rendition end to end. Order matters:
// filter graph first (it determines the encoder's input format), then
// the encoder (so its codecpar is ready for the mux's stream
// declaration), then the SAR bitstream filter (must read encoder
// params during Init), then the mux (declares its video stream from
// either the BSF's output params or — when BSF is absent — the
// encoder's params).
func (p *Pipeline) buildProfileChain(prof ProfileOutput) (*profileChain, error) {
	chain := &profileChain{profile: prof.Profile}

	if err := p.buildVideoFilter(chain); err != nil {
		freeProfileChain(chain)
		return nil, err
	}
	if err := p.buildVideoEncoder(chain); err != nil {
		freeProfileChain(chain)
		return nil, err
	}
	if err := p.buildSARBitstreamFilter(chain); err != nil {
		freeProfileChain(chain)
		return nil, err
	}
	if err := p.buildOutputMux(chain, prof.Writer); err != nil {
		freeProfileChain(chain)
		return nil, err
	}
	return chain, nil
}

// buildSARBitstreamFilter allocates and initialises the
// h264_metadata / hevc_metadata BSF when the profile configures SAR.
// No-op when SAR is empty or the BSF isn't applicable — leaves
// chain.bsfCtx as nil and the hot path writes encoder packets straight
// to the mux.
func (p *Pipeline) buildSARBitstreamFilter(chain *profileChain) error {
	encoderName := encoderNameForHW(p.cfg.HW, chain.profile.Codec)
	bsfCtx, err := allocSARBitstreamFilter(chain.profile.SAR, chain.encoderCtx, encoderName)
	if err != nil {
		return err
	}
	chain.bsfCtx = bsfCtx
	return nil
}

// buildVideoFilter constructs the filter graph: buffer src → filter
// chain string → buffer sink. The buffer src copies its time base and
// hardware frames context from the decoder so frames flow into the
// graph in the same format the decoder produces them.
func (p *Pipeline) buildVideoFilter(chain *profileChain) error {
	chain.filterGraph = astiav.AllocFilterGraph()
	if chain.filterGraph == nil {
		return errors.New("alloc filter graph")
	}

	// Buffer src — the graph's entry point. Parameters come from the
	// decoder context: dimensions, pixel format, time base, SAR all
	// must match the frames runLoop will push in.
	bufferSrcFilter := astiav.FindFilterByName("buffer")
	if bufferSrcFilter == nil {
		return errors.New("ffmpeg build missing buffer filter")
	}
	srcCtx, err := chain.filterGraph.NewBuffersrcFilterContext(bufferSrcFilter, "in")
	if err != nil {
		return fmt.Errorf("create buffer src: %w", err)
	}
	chain.bufferSrcCtx = srcCtx

	srcParams := astiav.AllocBuffersrcFilterContextParameters()
	defer srcParams.Free()
	srcParams.SetWidth(p.decoderCtx.Width())
	srcParams.SetHeight(p.decoderCtx.Height())
	srcParams.SetPixelFormat(p.decoderCtx.PixelFormat())
	srcParams.SetTimeBase(p.decoderStream.TimeBase())
	srcParams.SetSampleAspectRatio(p.decoderCtx.SampleAspectRatio())
	if hfc := p.decoderCtx.HardwareFramesContext(); hfc != nil {
		// CUDA pipeline: the buffer src must know the hw frames ctx so
		// downstream cuda filters (yadif_cuda, scale_cuda) get valid
		// hardware surfaces to operate on.
		srcParams.SetHardwareFramesContext(hfc)
	}
	if err := chain.bufferSrcCtx.SetParameters(srcParams); err != nil {
		return fmt.Errorf("set buffer src params: %w", err)
	}
	if err := chain.bufferSrcCtx.Initialize(nil); err != nil {
		return fmt.Errorf("init buffer src: %w", err)
	}

	// Buffer sink — the graph's exit point feeding the encoder.
	bufferSinkFilter := astiav.FindFilterByName("buffersink")
	if bufferSinkFilter == nil {
		return errors.New("ffmpeg build missing buffersink filter")
	}
	sinkCtx, err := chain.filterGraph.NewBuffersinkFilterContext(bufferSinkFilter, "out")
	if err != nil {
		return fmt.Errorf("create buffer sink: %w", err)
	}
	chain.bufferSinkCtx = sinkCtx

	// Parse the deinterlace + scale + watermark chain. "null" is
	// libav's pass-through for the no-filter case (already covered by
	// filterChainSpec when both deinterlace and resize are absent).
	//
	// frameScale shrinks the watermark on smaller profiles so the
	// on-screen ratio stays constant across the ladder; defaults to
	// 1.0 (no shrink) when the watermark's Resize=false or this is
	// the widest rendition.
	frameScale := 1.0
	if p.maxLadderWidth > 0 && chain.profile.Width > 0 {
		frameScale = float64(chain.profile.Width) / float64(p.maxLadderWidth)
	}
	chainStr := filterChainSpec(
		p.cfg.HW,
		chain.profile,
		p.cfg.Interlace,
		p.cfg.Watermark,
		frameScale,
	)

	inputs := astiav.AllocFilterInOut()
	defer inputs.Free()
	inputs.SetName("out")
	inputs.SetFilterContext(chain.bufferSinkCtx.FilterContext())

	outputs := astiav.AllocFilterInOut()
	defer outputs.Free()
	outputs.SetName("in")
	outputs.SetFilterContext(chain.bufferSrcCtx.FilterContext())

	if err := chain.filterGraph.Parse(chainStr, inputs, outputs); err != nil {
		return fmt.Errorf("parse filter chain %q: %w", chainStr, err)
	}
	if err := chain.filterGraph.Configure(); err != nil {
		return fmt.Errorf("configure filter graph %q: %w", chainStr, err)
	}
	return nil
}

// buildVideoEncoder allocates the encoder context, copies geometry +
// timing parameters from the filter graph's sink (which reflects what
// the filtered frames actually look like), sets bitrate / GOP / etc.,
// and opens the encoder with the codec-private options dictionary
// (preset, profile, level, bf, refs).
func (p *Pipeline) buildVideoEncoder(chain *profileChain) error {
	encoderName := encoderNameForHW(p.cfg.HW, chain.profile.Codec)
	encoder := astiav.FindEncoderByName(encoderName)
	if encoder == nil {
		return fmt.Errorf("encoder %q not found in ffmpeg build", encoderName)
	}

	chain.encoderCtx = astiav.AllocCodecContext(encoder)
	if chain.encoderCtx == nil {
		return errors.New("alloc encoder context")
	}

	// Geometry / timing — pull from buffer sink so the encoder
	// matches whatever the filter graph produces (handles odd cases
	// where the graph adjusted dims for alignment).
	chain.encoderCtx.SetWidth(chain.bufferSinkCtx.Width())
	chain.encoderCtx.SetHeight(chain.bufferSinkCtx.Height())
	chain.encoderCtx.SetSampleAspectRatio(chain.bufferSinkCtx.SampleAspectRatio())
	chain.encoderCtx.SetTimeBase(chain.bufferSinkCtx.TimeBase())
	// The filter graph's output pix fmt is what the encoder receives.
	// For CUDA pipelines this is PIX_FMT_CUDA; for CPU it's the
	// software pix fmt the scaler produced (typically YUV420P).
	chain.encoderCtx.SetPixelFormat(chain.bufferSinkCtx.PixelFormat())

	// HW frames context propagation: the buffer sink doesn't expose
	// the AVBufferRef directly in astiav; libavfilter does propagate
	// hw_frames_ctx through CUDA filters internally, but the encoder
	// needs an explicit handle. For v1 we fall back to the decoder's
	// hw_frames_ctx — works when scale_cuda preserves the device but
	// can produce dimension mismatches if the scaler creates a new
	// frames pool. Revisit during HW pipeline benchmarking.
	if p.hwDevice != nil {
		if hfc := p.decoderCtx.HardwareFramesContext(); hfc != nil {
			chain.encoderCtx.SetHardwareFramesContext(hfc)
		}
	}

	// Rate control — bitrate is in kbps in YAML, libav wants bps.
	if chain.profile.Bitrate > 0 {
		chain.encoderCtx.SetBitRate(int64(chain.profile.Bitrate) * 1000)
	}
	if chain.profile.MaxBitrate > 0 {
		chain.encoderCtx.SetRateControlMaxRate(int64(chain.profile.MaxBitrate) * 1000)
		chain.encoderCtx.SetRateControlBufferSize(chain.profile.MaxBitrate * 1000 * 2)
	}

	// Framerate + GOP. Framerate defaults to 25 — same as the CLI
	// backend's fallback when no explicit framerate is set on the
	// profile or globally.
	fps := chain.profile.Framerate
	if fps <= 0 {
		fps = 25
	}
	chain.encoderCtx.SetFramerate(astiav.NewRational(int(fps), 1))
	if chain.profile.KeyframeInterval > 0 {
		chain.encoderCtx.SetGopSize(int(float64(chain.profile.KeyframeInterval) * fps))
	}

	// Codec-private options (preset, profile, bf, refs). encoderOpts
	// must outlive Open() so the C side can copy the strings before we
	// free the dictionary.
	optsMap := encoderPrivateOptions(encoderName, chain.profile)
	encoderOpts := astiav.NewDictionary()
	defer encoderOpts.Free()
	for k, v := range optsMap {
		if err := encoderOpts.Set(k, v, 0); err != nil {
			return fmt.Errorf("set encoder option %s=%s: %w", k, v, err)
		}
	}
	if err := chain.encoderCtx.Open(encoder, encoderOpts); err != nil {
		return fmt.Errorf("open encoder %s: %w", encoderName, err)
	}
	return nil
}

// buildOutputMux allocates the output format context (mpegts),
// declares the video stream from the encoder's codec parameters,
// bridges the io.Writer via a custom AVIOContext, and writes the
// container header. Audio stream declaration lives here too once
// audio support lands — for v1 video-only the mux still works.
func (p *Pipeline) buildOutputMux(chain *profileChain, w io.Writer) error {
	if w == nil {
		return errors.New("output writer is nil")
	}

	formatCtx, err := astiav.AllocOutputFormatContext(nil, "mpegts", "")
	if err != nil {
		return fmt.Errorf("alloc output format context: %w", err)
	}
	chain.outputFormat = formatCtx

	// Write callback wraps the Go io.Writer. We don't supply read or
	// seek — mpegts is a one-way streaming format, libavformat won't
	// try to seek on it.
	outIO, err := astiav.AllocIOContext(
		outputWriteBufferSize,
		true, // writable
		nil,  // read
		nil,  // seek
		func(b []byte) (int, error) { return w.Write(b) },
	)
	if err != nil {
		return fmt.Errorf("alloc output io context: %w", err)
	}
	chain.outputFormat.SetPb(outIO)

	chain.encoderStream = chain.outputFormat.NewStream(nil)
	if chain.encoderStream == nil {
		return errors.New("new output stream")
	}
	// Codec parameters on the output stream MUST reflect what the mux
	// actually receives. When a BSF is in front of the mux, its
	// output params (which carry the rewritten SAR in the SPS) take
	// precedence over the raw encoder params.
	if chain.bsfCtx != nil {
		if err := chain.encoderStream.CodecParameters().Copy(chain.bsfCtx.OutputCodecParameters()); err != nil {
			return fmt.Errorf("copy bsf params to stream: %w", err)
		}
	} else if err := chain.encoderStream.CodecParameters().FromCodecContext(chain.encoderCtx); err != nil {
		return fmt.Errorf("copy encoder params to stream: %w", err)
	}
	chain.encoderStream.SetTimeBase(chain.encoderCtx.TimeBase())

	// Audio output stream: when the input carries audio, declare a
	// matching mux stream. Source of codec params depends on whether
	// audio is being re-encoded:
	//
	//   - passthrough (audioReencodeActive=false): copy params from
	//     the input stream so the mux emits the source codec bytes
	//     verbatim.
	//   - re-encode (audioReencodeActive=true): copy params from the
	//     shared audio encoder so the mux's stream declaration matches
	//     what runLoop's fan-out actually writes (AAC / MP3 / …).
	//
	// In both cases the codec tag is cleared so the mpegts mux derives
	// it from codec_id — leaving the source container's tag intact
	// trips "could not find tag for codec X" warnings on some inputs.
	if p.audioStream != nil {
		chain.audioOutStream = chain.outputFormat.NewStream(nil)
		if chain.audioOutStream == nil {
			return errors.New("new audio output stream")
		}
		if p.audioReencodeActive {
			if err := chain.audioOutStream.CodecParameters().FromCodecContext(p.audioEncoderCtx); err != nil {
				return fmt.Errorf("copy audio encoder params to stream: %w", err)
			}
			chain.audioOutStream.SetTimeBase(p.audioEncoderCtx.TimeBase())
		} else {
			if err := chain.audioOutStream.CodecParameters().Copy(p.audioStream.CodecParameters()); err != nil {
				return fmt.Errorf("copy audio params to stream: %w", err)
			}
			chain.audioOutStream.SetTimeBase(p.audioStream.TimeBase())
		}
		chain.audioOutStream.CodecParameters().SetCodecTag(0)
	}

	if err := chain.outputFormat.WriteHeader(nil); err != nil {
		return fmt.Errorf("write output header: %w", err)
	}
	return nil
}
