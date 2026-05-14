package native

// audio.go — audio decode + filter + encode pipeline.
//
// Two operating modes, picked at init time based on cfg.Audio:
//
//   - Passthrough (cfg.Audio.Copy = true OR no transformations
//     requested): input AVPackets are routed straight from the demuxer
//     to every output mux's audio stream. No decode/encode cost.
//     Implemented by forwardAudioPacket in loop.go.
//
//   - Re-encode (cfg.Audio.Copy = false AND any of codec / bitrate /
//     sample-rate / channels / normalize differs from passthrough):
//     ONE shared decoder → optional filter graph (aresample + aformat
//     + optional loudnorm) → ONE shared encoder → fan-out the encoded
//     packets to every output mux. The fan-out is acceptable because
//     output audio params are uniform per stream (operator picks one
//     audio config per stream, all renditions share it).
//
// Why share decoder+encoder across outputs (vs per-output as the
// video chain does): audio config is per-stream, not per-rendition.
// Encoding once and copying the resulting packet into N muxes costs
// ~1 µs per ref + a stream-index rewrite; encoding N times would
// triple AAC CPU at zero benefit. Mirrors what the FFmpeg CLI backend
// did with its single `-c:a aac` + N `-map 0:a` graph.

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/asticode/go-astiav"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// initAudio decides whether audio needs re-encoding and, when it does,
// builds the decoder → filter graph → encoder chain. No-op when the
// input has no audio, when Copy=true, or when the requested config
// matches the source so closely that passthrough produces the same
// bytes (cheaper).
func (p *Pipeline) initAudio() error {
	if p.audioStream == nil {
		return nil
	}
	if !needsAudioReencode(p.cfg.Audio, p.audioStream) {
		// Passthrough — runLoop's forwardAudioPacket handles every
		// packet without any decode/encode allocation here.
		return nil
	}

	if err := p.openAudioDecoder(); err != nil {
		return err
	}
	if err := p.openAudioEncoder(); err != nil {
		return err
	}
	if err := p.buildAudioFilterGraph(); err != nil {
		return err
	}
	p.audioReencodeActive = true
	return nil
}

// needsAudioReencode returns true when any operator-set field would
// produce a different bitstream than passthrough. Empty / zero values
// mean "inherit from source" and don't force re-encode by themselves.
//
// Why this matters: operators can set `audio.copy = false` to opt into
// re-encode globally, but if everything else is at defaults and the
// source already matches, we'd waste CPU running an identity pipeline.
// However, the explicit operator intent (copy=false) wins — if they
// said don't copy, we re-encode. Bitrate or codec differences are
// always sufficient by themselves.
func needsAudioReencode(a domain.AudioTranscodeConfig, src *astiav.Stream) bool {
	if a.Copy {
		return false
	}
	// Codec change → re-encode (except for AudioCodecCopy which means
	// "passthrough" semantically — same as Copy=true).
	if a.Codec == domain.AudioCodecCopy {
		return false
	}
	// Operator explicitly said copy=false. Re-encode unless every
	// targetable field is empty AND the requested codec matches the
	// source codec (in which case passthrough is equivalent).
	if a.Codec != "" && !audioCodecMatchesSource(a.Codec, src) {
		return true
	}
	if a.Bitrate > 0 {
		return true
	}
	if a.SampleRate > 0 && a.SampleRate != src.CodecParameters().SampleRate() {
		return true
	}
	if a.Channels > 0 {
		// Channel mix forces re-encode — can't be done at packet level.
		return true
	}
	if a.Normalize {
		return true
	}
	// Copy=false + every field empty + codec matches source → effectively
	// the same as Copy=true. Take the passthrough fast path.
	return false
}

// audioCodecMatchesSource reports whether the target audio codec is
// the same family as the source stream's codec. Lets the
// needsAudioReencode predicate skip a no-op transcode when the user
// requested codec=aac and the source is already AAC.
func audioCodecMatchesSource(target domain.AudioCodec, src *astiav.Stream) bool {
	srcID := src.CodecParameters().CodecID()
	switch target {
	case domain.AudioCodecAAC:
		return srcID == astiav.CodecIDAac
	case domain.AudioCodecMP2:
		return srcID == astiav.CodecIDMp2
	case domain.AudioCodecMP3:
		return srcID == astiav.CodecIDMp3
	case domain.AudioCodecAC3:
		return srcID == astiav.CodecIDAc3
	case domain.AudioCodecEAC3:
		return srcID == astiav.CodecIDEac3
	}
	return false
}

// openAudioDecoder allocates + opens the decoder for the input audio
// stream. Mirrors openDecoder (video side) but stays software-only —
// audio decode is a few percent of one CPU at most for AAC, the
// portability win of skipping vendor backends outweighs the saving.
func (p *Pipeline) openAudioDecoder() error {
	codecID := p.audioStream.CodecParameters().CodecID()
	decoder := astiav.FindDecoder(codecID)
	if decoder == nil {
		return fmt.Errorf("audio decoder for codec id %d not found", codecID)
	}
	p.audioDecoderCtx = astiav.AllocCodecContext(decoder)
	if p.audioDecoderCtx == nil {
		return errors.New("alloc audio decoder context")
	}
	if err := p.audioDecoderCtx.FromCodecParameters(p.audioStream.CodecParameters()); err != nil {
		return fmt.Errorf("copy audio decoder params: %w", err)
	}
	if err := p.audioDecoderCtx.Open(decoder, nil); err != nil {
		return fmt.Errorf("open audio decoder: %w", err)
	}
	return nil
}

// openAudioEncoder allocates the shared audio encoder. SampleRate +
// Channels default to the decoder's (operator value 0 means
// "inherit"); SampleFormat picks the first one the encoder advertises
// (FLTP for native AAC, S16 for libmp3lame, …).
func (p *Pipeline) openAudioEncoder() error {
	encName := audioEncoderName(p.cfg.Audio.Codec)
	encoder := astiav.FindEncoderByName(encName)
	if encoder == nil {
		return fmt.Errorf("audio encoder %q not in ffmpeg build", encName)
	}
	p.audioEncoderCtx = astiav.AllocCodecContext(encoder)
	if p.audioEncoderCtx == nil {
		return errors.New("alloc audio encoder context")
	}

	sampleRate := p.cfg.Audio.SampleRate
	if sampleRate <= 0 {
		sampleRate = p.audioDecoderCtx.SampleRate()
	}
	channels := p.cfg.Audio.Channels
	layout := p.audioDecoderCtx.ChannelLayout()
	if channels > 0 {
		layout = defaultChannelLayout(channels)
	}

	// SampleFormat: encoders restrict what they accept; pick the
	// first one the codec advertises so we never fight libavcodec on
	// an unsupported format.
	sampleFmt := firstSupportedSampleFormat(encoder)

	p.audioEncoderCtx.SetSampleRate(sampleRate)
	p.audioEncoderCtx.SetChannelLayout(layout)
	p.audioEncoderCtx.SetSampleFormat(sampleFmt)
	p.audioEncoderCtx.SetTimeBase(astiav.NewRational(1, sampleRate))

	if p.cfg.Audio.Bitrate > 0 {
		p.audioEncoderCtx.SetBitRate(int64(p.cfg.Audio.Bitrate) * 1000)
	}

	if err := p.audioEncoderCtx.Open(encoder, nil); err != nil {
		return fmt.Errorf("open audio encoder %s: %w", encName, err)
	}
	return nil
}

// audioEncoderName resolves the domain audio codec name to the
// libavcodec encoder identifier. Empty defaults to AAC, matching the
// CLI backend's resolution path.
func audioEncoderName(codec domain.AudioCodec) string {
	switch codec {
	case "", domain.AudioCodecAAC:
		return "aac"
	case domain.AudioCodecMP2:
		return "mp2"
	case domain.AudioCodecMP3:
		return "libmp3lame"
	case domain.AudioCodecAC3:
		return "ac3"
	case domain.AudioCodecEAC3:
		return "eac3"
	}
	return string(codec)
}

// firstSupportedSampleFormat returns the first sample format the
// encoder advertises support for. Falls back to FLTP (the AAC native
// format) if the encoder doesn't expose its list — defensive only,
// every codec we care about populates it.
func firstSupportedSampleFormat(enc *astiav.Codec) astiav.SampleFormat {
	if fmts := enc.SupportedSampleFormats(); len(fmts) > 0 {
		return fmts[0]
	}
	return astiav.SampleFormatFltp
}

// defaultChannelLayout picks the standard libav layout for a channel
// count. Mono / stereo / 5.1 cover the vast majority of broadcast
// inputs; anything else falls back to stereo because libav can't
// invent a layout from just a number when ambiguous (e.g. 4 could be
// quad or 3.1).
func defaultChannelLayout(channels int) astiav.ChannelLayout {
	switch channels {
	case 1:
		return astiav.ChannelLayoutMono
	case 2:
		return astiav.ChannelLayoutStereo
	case 6:
		return astiav.ChannelLayout5Point1
	}
	return astiav.ChannelLayoutStereo
}

// buildAudioFilterGraph wires the decode-side abuffer source through
// aresample + aformat (and loudnorm when Normalize=true) into an
// abuffersink that the encoder reads from. The graph absorbs every
// transformation libav needs to bridge decoder output → encoder input
// — sample-rate mismatch, channel-layout change, format conversion.
//
// We always build a graph even when the transforms are identity-only
// because AAC encoders mandate a fixed frame size (1024 samples for
// native AAC) that the source decoder may not produce; an abuffersink
// reframes packets to the encoder's frame_size for free.
func (p *Pipeline) buildAudioFilterGraph() error {
	graph := astiav.AllocFilterGraph()
	if graph == nil {
		return errors.New("alloc audio filter graph")
	}
	p.audioFilterGraph = graph

	srcFilter := astiav.FindFilterByName("abuffer")
	if srcFilter == nil {
		return errors.New("ffmpeg build missing abuffer filter")
	}
	srcCtx, err := graph.NewBuffersrcFilterContext(srcFilter, "ain")
	if err != nil {
		return fmt.Errorf("create audio buffer src: %w", err)
	}
	p.audioBufferSrcCtx = srcCtx

	srcParams := astiav.AllocBuffersrcFilterContextParameters()
	defer srcParams.Free()
	srcParams.SetSampleRate(p.audioDecoderCtx.SampleRate())
	srcParams.SetSampleFormat(p.audioDecoderCtx.SampleFormat())
	srcParams.SetChannelLayout(p.audioDecoderCtx.ChannelLayout())
	srcParams.SetTimeBase(astiav.NewRational(1, p.audioDecoderCtx.SampleRate()))
	if err := p.audioBufferSrcCtx.SetParameters(srcParams); err != nil {
		return fmt.Errorf("set audio buffer src params: %w", err)
	}
	if err := p.audioBufferSrcCtx.Initialize(nil); err != nil {
		return fmt.Errorf("init audio buffer src: %w", err)
	}

	sinkFilter := astiav.FindFilterByName("abuffersink")
	if sinkFilter == nil {
		return errors.New("ffmpeg build missing abuffersink filter")
	}
	sinkCtx, err := graph.NewBuffersinkFilterContext(sinkFilter, "aout")
	if err != nil {
		return fmt.Errorf("create audio buffer sink: %w", err)
	}
	p.audioBufferSinkCtx = sinkCtx

	chainStr := audioFilterChainSpec(p.audioEncoderCtx, p.cfg.Audio.Normalize)

	inputs := astiav.AllocFilterInOut()
	defer inputs.Free()
	inputs.SetName("aout")
	inputs.SetFilterContext(p.audioBufferSinkCtx.FilterContext())

	outputs := astiav.AllocFilterInOut()
	defer outputs.Free()
	outputs.SetName("ain")
	outputs.SetFilterContext(p.audioBufferSrcCtx.FilterContext())

	if err := graph.Parse(chainStr, inputs, outputs); err != nil {
		return fmt.Errorf("parse audio filter chain %q: %w", chainStr, err)
	}
	if err := graph.Configure(); err != nil {
		return fmt.Errorf("configure audio filter graph %q: %w", chainStr, err)
	}
	return nil
}

// audioFilterChainSpec composes the audio filter chain string. Always
// includes aresample (sample-rate conversion / async drift fix) and
// aformat (output sample fmt + channel layout) so the chain's tail
// matches the encoder's input contract. loudnorm is prepended when
// Normalize=true; the EBU R128 default targets (-23 LUFS, -2 dBTP, 7
// LU range) match the CLI backend's hard-coded values.
func audioFilterChainSpec(encCtx *astiav.CodecContext, normalize bool) string {
	parts := []string{
		"aresample=async=1000",
	}
	if normalize {
		parts = append(parts, "loudnorm=I=-23:TP=-2:LRA=7")
	}
	parts = append(parts, fmt.Sprintf(
		"aformat=sample_fmts=%s:sample_rates=%d:channel_layouts=%s",
		encCtx.SampleFormat().Name(),
		encCtx.SampleRate(),
		encCtx.ChannelLayout().String(),
	))
	return strings.Join(parts, ",")
}

// processAudioPacket is the re-encode path's per-packet handler.
// Mirrors video's processPacket: send → loop receive → for each frame,
// feed the filter graph → loop sink → encode → fan-out the resulting
// AVPacket to every output mux.
//
// EAGAIN / EOF at any receive stage is normal flow control — return
// silently so the run loop pulls another input packet. Fatal codec
// errors propagate up.
func (p *Pipeline) processAudioPacket(pkt *astiav.Packet, frame *astiav.Frame) error {
	if err := p.audioDecoderCtx.SendPacket(pkt); err != nil {
		if errors.Is(err, astiav.ErrEagain) {
			return nil
		}
		return fmt.Errorf("audio decoder send: %w", err)
	}
	for {
		err := p.audioDecoderCtx.ReceiveFrame(frame)
		switch {
		case errors.Is(err, astiav.ErrEagain), errors.Is(err, astiav.ErrEof):
			return nil
		case err != nil:
			return fmt.Errorf("audio decoder receive: %w", err)
		}
		if err := p.filterAndEncodeAudioFrame(frame); err != nil {
			frame.Unref()
			return err
		}
		frame.Unref()
	}
}

// filterAndEncodeAudioFrame pushes one decoded frame through the
// filter graph and drains every reframed frame through the encoder.
// Each encoded packet is fanned out to every output mux's audio
// stream.
func (p *Pipeline) filterAndEncodeAudioFrame(in *astiav.Frame) error {
	if err := p.audioBufferSrcCtx.AddFrame(in, astiav.NewBuffersrcFlags()); err != nil {
		return fmt.Errorf("audio filter add: %w", err)
	}

	filtered := astiav.AllocFrame()
	defer filtered.Free()
	encPkt := astiav.AllocPacket()
	defer encPkt.Free()

	for {
		err := p.audioBufferSinkCtx.GetFrame(filtered, astiav.NewBuffersinkFlags())
		switch {
		case errors.Is(err, astiav.ErrEagain), errors.Is(err, astiav.ErrEof):
			return nil
		case err != nil:
			return fmt.Errorf("audio filter get: %w", err)
		}
		if err := p.audioEncoderCtx.SendFrame(filtered); err != nil {
			filtered.Unref()
			return fmt.Errorf("audio encoder send: %w", err)
		}
		filtered.Unref()

		if err := p.drainAudioEncoder(encPkt); err != nil {
			return err
		}
	}
}

// drainAudioEncoder pulls packets from the encoder until EAGAIN/EOF
// and fans each one out to every output mux's audio stream.
func (p *Pipeline) drainAudioEncoder(encPkt *astiav.Packet) error {
	for {
		err := p.audioEncoderCtx.ReceivePacket(encPkt)
		switch {
		case errors.Is(err, astiav.ErrEagain), errors.Is(err, astiav.ErrEof):
			return nil
		case err != nil:
			return fmt.Errorf("audio encoder receive: %w", err)
		}
		if err := p.fanoutAudioPacket(encPkt); err != nil {
			encPkt.Unref()
			return err
		}
		encPkt.Unref()
	}
}

// fanoutAudioPacket clones the encoded audio packet into every output
// mux's audio stream. Same pattern as forwardAudioPacket (the
// passthrough path) — ref source, set stream index, rescale TS, write.
// Per-output failure logs + continues so one broken mux doesn't kill
// the audio for the surviving outputs.
func (p *Pipeline) fanoutAudioPacket(src *astiav.Packet) error {
	for _, out := range p.outputs {
		if out.audioOutStream == nil {
			continue
		}
		copyPkt := astiav.AllocPacket()
		if copyPkt == nil {
			return errors.New("alloc audio fanout packet")
		}
		if err := copyPkt.Ref(src); err != nil {
			copyPkt.Free()
			return fmt.Errorf("ref audio fanout packet: %w", err)
		}
		copyPkt.SetStreamIndex(out.audioOutStream.Index())
		copyPkt.RescaleTs(p.audioEncoderCtx.TimeBase(), out.audioOutStream.TimeBase())
		if err := out.outputFormat.WriteInterleavedFrame(copyPkt); err != nil {
			slog.Warn("native pipeline: audio fanout write failed",
				"stream_id", p.cfg.StreamID, "err", err)
		}
		copyPkt.Free()
	}
	return nil
}

// flushAudio drains the decoder → filter → encoder pipeline at input
// EOF. Mirrors the per-profile video drain in cleanup.go's
// flushProfileChain, scaled down to the shared audio path.
//
// No-op when the audio re-encode chain isn't active (Copy=true or
// passthrough).
func (p *Pipeline) flushAudio() error {
	if !p.audioReencodeActive {
		return nil
	}
	if err := p.flushAudioDecoder(); err != nil {
		return err
	}
	if err := p.flushAudioFilter(); err != nil {
		return err
	}
	return p.flushAudioEncoder()
}

// flushAudioDecoder feeds nil into the audio decoder and pushes every
// drained frame through the filter graph.
func (p *Pipeline) flushAudioDecoder() error {
	if p.audioDecoderCtx == nil {
		return nil
	}
	if err := p.audioDecoderCtx.SendPacket(nil); err != nil {
		return fmt.Errorf("audio decoder flush send: %w", err)
	}
	frame := astiav.AllocFrame()
	defer frame.Free()
	for {
		err := p.audioDecoderCtx.ReceiveFrame(frame)
		switch {
		case errors.Is(err, astiav.ErrEagain), errors.Is(err, astiav.ErrEof):
			return nil
		case err != nil:
			return fmt.Errorf("audio decoder flush receive: %w", err)
		}
		if err := p.filterAndEncodeAudioFrame(frame); err != nil {
			frame.Unref()
			return err
		}
		frame.Unref()
	}
}

// flushAudioFilter pushes EOF (nil frame) into the filter graph and
// drains every still-buffered frame through the encoder.
func (p *Pipeline) flushAudioFilter() error {
	if p.audioBufferSrcCtx == nil {
		return nil
	}
	if err := p.audioBufferSrcCtx.AddFrame(nil, astiav.NewBuffersrcFlags()); err != nil {
		return fmt.Errorf("audio filter flush add: %w", err)
	}
	filtered := astiav.AllocFrame()
	defer filtered.Free()
	encPkt := astiav.AllocPacket()
	defer encPkt.Free()
	for {
		err := p.audioBufferSinkCtx.GetFrame(filtered, astiav.NewBuffersinkFlags())
		switch {
		case errors.Is(err, astiav.ErrEagain), errors.Is(err, astiav.ErrEof):
			return nil
		case err != nil:
			return fmt.Errorf("audio filter flush get: %w", err)
		}
		if err := p.audioEncoderCtx.SendFrame(filtered); err != nil {
			filtered.Unref()
			return fmt.Errorf("audio encoder flush send (filtered): %w", err)
		}
		filtered.Unref()
		if err := p.drainAudioEncoder(encPkt); err != nil {
			return err
		}
	}
}

// flushAudioEncoder signals encoder EOF and fans out the last packets.
func (p *Pipeline) flushAudioEncoder() error {
	if p.audioEncoderCtx == nil {
		return nil
	}
	if err := p.audioEncoderCtx.SendFrame(nil); err != nil {
		return fmt.Errorf("audio encoder flush send: %w", err)
	}
	encPkt := astiav.AllocPacket()
	defer encPkt.Free()
	return p.drainAudioEncoder(encPkt)
}
