package native

import (
	"errors"
	"fmt"

	"github.com/asticode/go-astiav"
)

// drainAndFlush walks every stage in reverse order pushing the
// libavcodec "drain" signal (nil frame for encoder, nil packet for
// decoder, nil flag on filter add) so any frame still inside a buffer
// makes it through to the mux. Each output's trailer is written so the
// MPEG-TS files / pipes terminate cleanly.
//
// Returns the first error encountered but keeps walking to give every
// downstream resource a chance to finalise — partial flush is better
// than failing on the first hiccup and leaving disk state torn.
func (p *Pipeline) drainAndFlush() error {
	var firstErr error
	captureErr := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// 1. Flush the video decoder: SendPacket(nil) → drain ReceiveFrame.
	if err := p.flushDecoder(); err != nil {
		captureErr(err)
	}

	// 2. Flush the audio re-encode chain (no-op when passthrough).
	// Audio must drain BEFORE profile-chain flush so the last AAC
	// packets land before the mux trailer.
	if err := p.flushAudio(); err != nil {
		captureErr(err)
	}

	// 3. For each profile chain, flush its filter graph + encoder +
	// write the mux trailer. Errors here are logged via captureErr but
	// don't short-circuit — the other profiles still need their
	// trailer to land or the resulting TS will be unparseable.
	for _, out := range p.outputs {
		if err := p.flushProfileChain(out); err != nil {
			captureErr(err)
		}
	}

	return firstErr
}

// flushDecoder feeds a nil packet to the decoder (signalling EOF) and
// drains every remaining frame through the same fan-out path the run
// loop uses. Errors here propagate up because a failed flush usually
// signals a fundamentally broken pipeline.
func (p *Pipeline) flushDecoder() error {
	if p.decoderCtx == nil {
		return nil
	}
	if err := p.decoderCtx.SendPacket(nil); err != nil {
		return fmt.Errorf("native pipeline: decoder flush send: %w", err)
	}

	drainFrame := astiav.AllocFrame()
	defer drainFrame.Free()

	for {
		err := p.decoderCtx.ReceiveFrame(drainFrame)
		switch {
		case errors.Is(err, astiav.ErrEof):
			return nil
		case errors.Is(err, astiav.ErrEagain):
			// EAGAIN during drain means "no more frames" effectively;
			// libav uses EAGAIN before EOF for some codecs.
			return nil
		case err != nil:
			return fmt.Errorf("native pipeline: decoder flush receive: %w", err)
		}

		for _, out := range p.outputs {
			// Use the same per-profile path the run loop uses so the
			// drained frame goes through filter + encode + mux.
			if err := p.processProfile(drainFrame, out); err != nil {
				return fmt.Errorf("native pipeline: drain profile: %w", err)
			}
		}
		drainFrame.Unref()
	}
}

// flushProfileChain drains the filter graph (nil-frame AddFrame) and
// encoder (nil-frame SendFrame), then writes the mux trailer. Each
// phase is its own helper so a partial init (filter built but encoder
// nil, encoder built but mux nil) flows through naturally and so the
// linear control flow doesn't need labeled jumps.
func (p *Pipeline) flushProfileChain(out *profileChain) error {
	if out == nil {
		return nil
	}
	if err := flushFilterGraph(out); err != nil {
		return err
	}
	if err := flushEncoder(out); err != nil {
		return err
	}
	return flushMuxTrailer(out)
}

// flushFilterGraph pushes EOF (nil frame) into the filter graph and
// drains every still-buffered filtered frame straight into the encoder.
// No-op when the graph wasn't built (early init failure path).
func flushFilterGraph(out *profileChain) error {
	if out.bufferSrcCtx == nil {
		return nil
	}
	if err := out.bufferSrcCtx.AddFrame(nil, astiav.NewBuffersrcFlags()); err != nil {
		return fmt.Errorf("filter drain add: %w", err)
	}

	drainFiltered := astiav.AllocFrame()
	defer drainFiltered.Free()
	for {
		err := out.bufferSinkCtx.GetFrame(drainFiltered, astiav.NewBuffersinkFlags())
		switch {
		case errors.Is(err, astiav.ErrEof), errors.Is(err, astiav.ErrEagain):
			return nil
		case err != nil:
			return fmt.Errorf("filter drain get: %w", err)
		}
		drainFiltered.SetPictureType(astiav.PictureTypeNone)
		if err := out.encoderCtx.SendFrame(drainFiltered); err != nil {
			drainFiltered.Unref()
			return fmt.Errorf("encoder drain send (filtered): %w", err)
		}
		drainFiltered.Unref()
	}
}

// flushEncoder signals encoder EOF and drains every remaining packet
// into the output mux (via the BSF when one is attached). No-op when
// the encoder wasn't built.
//
// Each drained encoder packet runs through writeEncodedPacket so the
// BSF path stays identical to the hot loop. After the encoder reports
// EOF we send a final nil packet through the BSF so it flushes any
// internally queued packets (h264_metadata is single-input-single-
// output in practice, but the API contract is "send nil to drain").
func flushEncoder(out *profileChain) error {
	if out.encoderCtx == nil {
		return nil
	}
	if err := out.encoderCtx.SendFrame(nil); err != nil {
		return fmt.Errorf("encoder flush send: %w", err)
	}
	drainPkt := astiav.AllocPacket()
	defer drainPkt.Free()
	for {
		err := out.encoderCtx.ReceivePacket(drainPkt)
		switch {
		case errors.Is(err, astiav.ErrEof), errors.Is(err, astiav.ErrEagain):
			return flushBSF(out)
		case err != nil:
			return fmt.Errorf("encoder drain receive: %w", err)
		}
		if werr := writeEncodedPacket(out, drainPkt); werr != nil {
			drainPkt.Unref()
			return werr
		}
		drainPkt.Unref()
	}
}

// flushBSF signals the BSF input EOF (nil packet) and drains every
// remaining post-BSF packet into the mux. No-op when no BSF is wired.
func flushBSF(out *profileChain) error {
	if out.bsfCtx == nil {
		return nil
	}
	if err := out.bsfCtx.SendPacket(nil); err != nil {
		return fmt.Errorf("bsf flush send: %w", err)
	}
	tail := astiav.AllocPacket()
	defer tail.Free()
	for {
		err := out.bsfCtx.ReceivePacket(tail)
		switch {
		case errors.Is(err, astiav.ErrEof), errors.Is(err, astiav.ErrEagain):
			return nil
		case err != nil:
			return fmt.Errorf("bsf drain receive: %w", err)
		}
		tail.RescaleTs(out.encoderCtx.TimeBase(), out.encoderStream.TimeBase())
		tail.SetStreamIndex(out.encoderStream.Index())
		if err := out.outputFormat.WriteInterleavedFrame(tail); err != nil {
			tail.Unref()
			return fmt.Errorf("mux drain write (post-bsf): %w", err)
		}
		tail.Unref()
	}
}

// flushMuxTrailer writes the MPEG-TS container trailer. No-op when the
// mux wasn't built.
func flushMuxTrailer(out *profileChain) error {
	if out.outputFormat == nil {
		return nil
	}
	if err := out.outputFormat.WriteTrailer(); err != nil {
		return fmt.Errorf("mux trailer: %w", err)
	}
	return nil
}
