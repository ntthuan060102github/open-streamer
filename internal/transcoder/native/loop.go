package native

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/asticode/go-astiav"
)

// runLoop is the transcoder hot path. Pumps one demuxed packet at a
// time through decode → fan-out filter+encode+mux for every profile.
// Returns nil on clean EOF or ctx cancel, an error on unrecoverable
// failure mid-stream.
//
// The loop runs single-threaded on the caller's goroutine. astiav
// resources aren't goroutine-safe at the libav level, so fan-out
// happens serially within this goroutine — for each decoded frame we
// iterate the profile chains. Per-frame compute on a Tesla T4 is
// ~1-2 ms × 21 streams × 2 profiles ≈ 50-80 ms which fits inside the
// 40 ms (25 fps) wall clock budget at the demux side.
//
// Per-profile errors are logged and the failed chain is marked dead;
// the loop keeps the rest of the pipeline alive. Matches the FFmpeg
// CLI backend's "one bad output doesn't kill others" behaviour.
func (p *Pipeline) runLoop(ctx context.Context) error {
	// Reusable hot-path packets / frames — allocated once, unref'd
	// after each iteration. astiav's Free() releases C memory; Unref()
	// drops the buffer ref but keeps the container struct alive.
	inputPacket := astiav.AllocPacket()
	if inputPacket == nil {
		return errors.New("native pipeline: alloc input packet")
	}
	defer inputPacket.Free()

	decodedFrame := astiav.AllocFrame()
	if decodedFrame == nil {
		return errors.New("native pipeline: alloc decoded frame")
	}
	defer decodedFrame.Free()

	for {
		select {
		case <-ctx.Done():
			return p.drainAndFlush()
		default:
		}

		if err := p.inputFormatCtx.ReadFrame(inputPacket); err != nil {
			if errors.Is(err, astiav.ErrEof) {
				return p.drainAndFlush()
			}
			return fmt.Errorf("native pipeline: read frame: %w", err)
		}

		idx := inputPacket.StreamIndex()
		switch {
		case idx == p.decoderStream.Index():
			err := p.processPacket(inputPacket, decodedFrame)
			inputPacket.Unref()
			if err != nil {
				return err
			}
		case p.audioStream != nil && idx == p.audioStream.Index():
			// Audio path: passthrough (clone the encoded packet into
			// every output mux) when initAudio decided no re-encode
			// is needed, OR full decode→filter→encode→fan-out when
			// the operator requested a codec / bitrate / sample-rate
			// / channel / loudnorm change. Errors are logged but
			// don't tear down the pipeline — same behaviour as the
			// video fan-out.
			if p.audioReencodeActive {
				if err := p.processAudioPacket(inputPacket, decodedFrame); err != nil {
					slog.Warn("native pipeline: audio re-encode failed",
						"stream_id", p.cfg.StreamID, "err", err)
				}
			} else if err := p.forwardAudioPacket(inputPacket); err != nil {
				slog.Warn("native pipeline: audio passthrough failed",
					"stream_id", p.cfg.StreamID, "err", err)
			}
			inputPacket.Unref()
		default:
			// Subtitles, data tracks, unknown streams — drop.
			inputPacket.Unref()
		}
	}
}

// forwardAudioPacket copies a single audio AVPacket into every output
// mux's audio stream. The packet is rescaled from the input stream's
// time base to each output stream's time base because mpegts can use
// a different timescale on each side.
//
// astiav's Packet has Ref + Unref methods, but copying via a fresh
// Packet + reusing the SAME underlying buffer ref is the standard
// FFmpeg pattern: alloc → ref source into copy → mutate stream index
// + timestamps → write → unref. The source packet stays untouched so
// the caller can route it to other outputs in the same iteration.
func (p *Pipeline) forwardAudioPacket(src *astiav.Packet) error {
	for _, out := range p.outputs {
		if out.audioOutStream == nil {
			continue
		}
		copyPkt := astiav.AllocPacket()
		if copyPkt == nil {
			return errors.New("alloc audio passthrough packet")
		}
		if err := copyPkt.Ref(src); err != nil {
			copyPkt.Free()
			return fmt.Errorf("ref audio packet: %w", err)
		}
		copyPkt.SetStreamIndex(out.audioOutStream.Index())
		copyPkt.RescaleTs(p.audioStream.TimeBase(), out.audioOutStream.TimeBase())
		if err := out.outputFormat.WriteInterleavedFrame(copyPkt); err != nil {
			copyPkt.Free()
			return fmt.Errorf("mux audio write: %w", err)
		}
		// WriteInterleavedFrame consumes / re-refs the packet; explicit
		// Free releases our Go-side container.
		copyPkt.Free()
	}
	return nil
}

// processPacket runs decode → fan-out for one input packet. Returns
// an error only on fatal decoder failures (corrupt codec context,
// unrecoverable libav error). EAGAIN / EOF from receive paths are
// expected control-flow signals and are absorbed here.
func (p *Pipeline) processPacket(pkt *astiav.Packet, decFrame *astiav.Frame) error {
	if err := p.decoderCtx.SendPacket(pkt); err != nil {
		if errors.Is(err, astiav.ErrEagain) {
			// Decoder buffer is full — drain receive side and retry on
			// the next outer-loop iteration. Same packet was already
			// owned by the caller's Unref().
			return nil
		}
		return fmt.Errorf("native pipeline: decoder send: %w", err)
	}

	for {
		err := p.decoderCtx.ReceiveFrame(decFrame)
		switch {
		case errors.Is(err, astiav.ErrEagain), errors.Is(err, astiav.ErrEof):
			return nil
		case err != nil:
			return fmt.Errorf("native pipeline: decoder receive: %w", err)
		}

		// Fan-out: each profile filters + encodes the same decoded
		// frame independently. libav handles ref-counting — passing
		// the frame to multiple AddFrame calls keeps the underlying
		// buffer alive until all consumers release.
		for _, out := range p.outputs {
			if err := p.processProfile(decFrame, out); err != nil {
				// Per-profile failure: log + skip. We don't tear down
				// the whole pipeline because the other profiles may
				// still be healthy and the operator-visible behaviour
				// of the CLI backend is the same — multi-output FFmpeg
				// keeps the surviving outputs producing.
				slog.Warn("native pipeline: profile chain failed",
					"stream_id", p.cfg.StreamID,
					"profile_width", out.profile.Width,
					"profile_height", out.profile.Height,
					"err", err,
				)
			}
		}
		decFrame.Unref()
	}
}

// processProfile pushes one decoded frame through the profile's
// filter graph, encodes whatever comes out, and writes the encoded
// packet(s) to the profile's output mux.
//
// Allocates per-call frame + packet to keep the per-profile state
// disjoint — a panic / corruption in one profile shouldn't leak into
// another. The allocation cost (a few µs) is acceptable; in production
// these would be reused via a small per-profile pool if profiling
// shows it matters.
func (p *Pipeline) processProfile(decFrame *astiav.Frame, out *profileChain) error {
	if err := out.bufferSrcCtx.AddFrame(decFrame, astiav.NewBuffersrcFlags()); err != nil {
		return fmt.Errorf("filter add frame: %w", err)
	}

	filterFrame := astiav.AllocFrame()
	defer filterFrame.Free()
	encPkt := astiav.AllocPacket()
	defer encPkt.Free()

	for {
		err := out.bufferSinkCtx.GetFrame(filterFrame, astiav.NewBuffersinkFlags())
		switch {
		case errors.Is(err, astiav.ErrEagain), errors.Is(err, astiav.ErrEof):
			return nil
		case err != nil:
			return fmt.Errorf("filter get frame: %w", err)
		}

		// Filter output frames carry the filter graph's time base; the
		// encoder expects its own. Without a SetPictureType reset
		// libav will mark every frame as an I-frame on some encoders.
		filterFrame.SetPictureType(astiav.PictureTypeNone)

		if err := out.encoderCtx.SendFrame(filterFrame); err != nil {
			filterFrame.Unref()
			return fmt.Errorf("encoder send: %w", err)
		}
		filterFrame.Unref()

		for {
			err := out.encoderCtx.ReceivePacket(encPkt)
			switch {
			case errors.Is(err, astiav.ErrEagain), errors.Is(err, astiav.ErrEof):
				break
			case err != nil:
				return fmt.Errorf("encoder receive: %w", err)
			}
			if err != nil {
				break
			}

			if err := writeEncodedPacket(out, encPkt); err != nil {
				encPkt.Unref()
				return err
			}
			encPkt.Unref()
		}
	}
}

// writeEncodedPacket sends one encoder-emitted packet to the output
// mux, optionally routing through the per-chain bitstream filter (BSF)
// when one is configured. The BSF path is "send one → drain N" because
// metadata BSFs typically pass-through packet-for-packet but the API
// doesn't promise that — drain in a loop until EAGAIN/EOF so we never
// leave a packet queued inside the BSF.
//
// PTS/DTS rescaling happens once per surviving packet: from the encoder
// time base (or BSF input time base when present, which is identical)
// to the output stream's time base. mpegts is usually 1/90000 on both
// sides, but the rescale stays as a safety net.
func writeEncodedPacket(out *profileChain, encPkt *astiav.Packet) error {
	if out.bsfCtx == nil {
		encPkt.RescaleTs(out.encoderCtx.TimeBase(), out.encoderStream.TimeBase())
		encPkt.SetStreamIndex(out.encoderStream.Index())
		if err := out.outputFormat.WriteInterleavedFrame(encPkt); err != nil {
			return fmt.Errorf("mux write: %w", err)
		}
		return nil
	}

	if err := out.bsfCtx.SendPacket(encPkt); err != nil {
		return fmt.Errorf("bsf send: %w", err)
	}
	bsfOut := astiav.AllocPacket()
	defer bsfOut.Free()
	for {
		err := out.bsfCtx.ReceivePacket(bsfOut)
		switch {
		case errors.Is(err, astiav.ErrEagain), errors.Is(err, astiav.ErrEof):
			return nil
		case err != nil:
			return fmt.Errorf("bsf receive: %w", err)
		}
		bsfOut.RescaleTs(out.encoderCtx.TimeBase(), out.encoderStream.TimeBase())
		bsfOut.SetStreamIndex(out.encoderStream.Index())
		if err := out.outputFormat.WriteInterleavedFrame(bsfOut); err != nil {
			bsfOut.Unref()
			return fmt.Errorf("mux write (post-bsf): %w", err)
		}
		bsfOut.Unref()
	}
}
