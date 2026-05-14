package native

import (
	"errors"
	"fmt"
	"io"

	"github.com/asticode/go-astiav"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// inputReadBufferSize is the AVIOContext read-buffer the demuxer hands
// to libavformat. 4 KiB matches astiav's custom_io example and FFmpeg's
// default for live streams — small enough that the demuxer doesn't sit
// on a fat block of unprocessed bytes while waiting for header probe,
// big enough that callback overhead stays negligible (~1024 syscalls
// at 25 fps × 200 KB/s ÷ 4 KB).
const inputReadBufferSize = 4096

// initInput wires the configured io.Reader into a libavformat demuxer
// (MPEG-TS only — Open-Streamer's wire format), locates the first
// video stream, and opens the matching decoder. The decoder runs on
// the shared hardware device context when one was created so the
// decoded frames can flow into the filter graph without a GPU↔CPU
// round-trip.
func (p *Pipeline) initInput() error {
	if err := p.openInputContext(); err != nil {
		return err
	}
	if err := p.locateVideoStream(); err != nil {
		return err
	}
	return p.openDecoder()
}

// openInputContext allocates the format context, bridges cfg.Input
// through a custom IOContext, and opens the demuxer.
//
// The MPEG-TS demuxer is named explicitly (`FindInputFormat("mpegts")`)
// to skip libavformat's probe step — Open-Streamer always feeds TS,
// and probing on a pipe that doesn't support seek can stall the open
// path for several seconds while libav reads megabytes from the
// reader looking for a recognizable header.
func (p *Pipeline) openInputContext() error {
	p.inputFormatCtx = astiav.AllocFormatContext()
	if p.inputFormatCtx == nil {
		return errors.New("native pipeline: alloc input format context")
	}

	ioCtx, err := astiav.AllocIOContext(
		inputReadBufferSize,
		false, // writable: input
		p.readFromInput,
		nil, // seek: live stream is not seekable
		nil, // write: input-side
	)
	if err != nil {
		return fmt.Errorf("native pipeline: alloc input io context: %w", err)
	}
	p.inputFormatCtx.SetPb(ioCtx)

	mpegts := astiav.FindInputFormat("mpegts")
	if mpegts == nil {
		return errors.New("native pipeline: ffmpeg build missing mpegts demuxer")
	}
	if err := p.inputFormatCtx.OpenInput("", mpegts, nil); err != nil {
		return fmt.Errorf("native pipeline: open input: %w", err)
	}
	if err := p.inputFormatCtx.FindStreamInfo(nil); err != nil {
		return fmt.Errorf("native pipeline: find stream info: %w", err)
	}
	return nil
}

// readFromInput is the IOContextReadFunc wired into the AVIOContext.
// libavformat calls it whenever the demuxer needs more bytes; we just
// pull from cfg.Input. EOF + io.ErrUnexpectedEOF both translate to
// astiav's "end of input" signal — astiav recognises io.EOF directly
// and translates to AVERROR_EOF for the underlying C code.
func (p *Pipeline) readFromInput(b []byte) (int, error) {
	n, err := p.cfg.Input.Read(b)
	if err != nil && errors.Is(err, io.ErrUnexpectedEOF) {
		// Treat a short tail-read as a clean EOF so the demuxer flushes
		// remaining packets instead of erroring.
		return n, io.EOF
	}
	return n, err
}

// locateVideoStream picks the first video stream from the demuxed
// container plus (optionally) the first audio stream. Video is
// required; audio is passthrough so we only stash the stream for
// later mux-side replication. Secondary tracks (teletext, alternate
// language audio) are ignored — v1 ships single-track per type to
// match the existing CLI backend's `-map 0:v:0? -map 0:a:0?` shape.
func (p *Pipeline) locateVideoStream() error {
	for _, st := range p.inputFormatCtx.Streams() {
		switch st.CodecParameters().MediaType() {
		case astiav.MediaTypeVideo:
			if p.decoderStream == nil {
				p.decoderStream = st
			}
		case astiav.MediaTypeAudio:
			if p.audioStream == nil {
				p.audioStream = st
			}
		}
	}
	if p.decoderStream == nil {
		return errors.New("native pipeline: no video stream in input")
	}
	return nil
}

// openDecoder picks the right decoder for the input codec + hwaccel
// pair, attaches the shared hardware device context when present, and
// opens the decoder context. The decoder name dispatch is centralised
// in decoderNameForHW so the encoder side can mirror the mapping when
// it has its own lookup.
func (p *Pipeline) openDecoder() error {
	codecID := p.decoderStream.CodecParameters().CodecID()
	name := decoderNameForHW(p.cfg.HW, codecID)

	var decoder *astiav.Codec
	if name != "" {
		decoder = astiav.FindDecoderByName(name)
	}
	if decoder == nil {
		// Fall back to libav's default decoder lookup (software path
		// or whichever decoder is registered for this codec id). This
		// covers VAAPI where there isn't a vendor-specific `h264_vaapi`
		// decoder — the standard libavcodec decoder accepts the hw
		// device context for surface upload.
		decoder = astiav.FindDecoder(codecID)
	}
	if decoder == nil {
		return fmt.Errorf("native pipeline: no decoder for codec id %d", codecID)
	}

	p.decoderCtx = astiav.AllocCodecContext(decoder)
	if p.decoderCtx == nil {
		return errors.New("native pipeline: alloc decoder context")
	}
	if err := p.decoderCtx.FromCodecParameters(p.decoderStream.CodecParameters()); err != nil {
		return fmt.Errorf("native pipeline: copy decoder params: %w", err)
	}
	if p.hwDevice != nil {
		// Attaching the shared device makes the decoder produce frames
		// on the hardware (CUDA / VAAPI / QSV / VT surface), letting
		// the filter graph and encoder consume them without an
		// explicit hwdownload bridge.
		p.decoderCtx.SetHardwareDeviceContext(p.hwDevice)
	}
	if err := p.decoderCtx.Open(decoder, nil); err != nil {
		return fmt.Errorf("native pipeline: open decoder: %w", err)
	}
	return nil
}

// decoderNameForHW returns the FFmpeg decoder name that pairs the given
// HW backend with the input codec. Returns "" when there's no
// vendor-specific decoder for the combination — caller falls back to
// FindDecoder(codecID) which yields the standard libavcodec decoder.
//
// Pin reference table:
//
//	NVENC (CUDA): h264_cuvid, hevc_cuvid           — decode on NVDEC
//	QSV:          h264_qsv,   hevc_qsv             — Intel Quick Sync decode
//	VAAPI:        (no vendor decoder; use software) — encoder still gets surface
//	VT:           h264_videotoolbox, hevc_videotoolbox (FFmpeg ≥ 4.4)
//	None:         (libavcodec default — software libx264/libx265 path)
func decoderNameForHW(hw domain.HWAccel, codecID astiav.CodecID) string {
	switch hw {
	case domain.HWAccelNVENC:
		switch codecID {
		case astiav.CodecIDH264:
			return "h264_cuvid"
		case astiav.CodecIDHevc:
			return "hevc_cuvid"
		}
	case domain.HWAccelQSV:
		switch codecID {
		case astiav.CodecIDH264:
			return "h264_qsv"
		case astiav.CodecIDHevc:
			return "hevc_qsv"
		}
	case domain.HWAccelVideoToolbox:
		switch codecID {
		case astiav.CodecIDH264:
			return "h264_videotoolbox"
		case astiav.CodecIDHevc:
			return "hevc_videotoolbox"
		}
	}
	// HWAccelVAAPI: no vendor decoder, software decode + hw upload.
	// HWAccelNone / unknown: software decode default.
	return ""
}
