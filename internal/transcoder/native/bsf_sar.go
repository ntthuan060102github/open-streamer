package native

// bsf_sar.go — Sample Aspect Ratio stamping via the h264_metadata /
// hevc_metadata bitstream filter.
//
// Why a BSF and not `setsar` in the filter graph: setsar is CPU-only —
// no setsar_cuda exists. On a CUDA pipeline, having setsar in the chain
// forces libavfilter to auto-insert hwdownload + hwupload around it,
// which means every frame round-trips GPU → CPU → GPU just to stamp two
// integers into the AVFrame.sample_aspect_ratio field. With 21 streams
// × 2 profiles × 25 fps the bridge stays in constant use.
//
// The bitstream filter runs AFTER the encoder on the encoded packets
// (touching only the SPS VUI block, once per GOP) so the CUDA frame
// pipeline stays end-to-end on the GPU.
//
// Same code path as the deleted FFmpeg-CLI backend's
// `transcoder.sarBSFArg`, ported to libavfilter's BSF API via astiav.

import (
	"fmt"
	"strings"

	"github.com/asticode/go-astiav"
)

// bsfNameForEncoder returns the metadata BSF name matched to the
// encoder family. Returns "" when this encoder has no metadata BSF
// (e.g. mpegts copy, av1) — caller falls back to no SAR stamping.
//
// Mirrors transcoder.sarBSFArg's encoder-name lookup so a stream's
// SAR config produces identical bitstreams regardless of backend.
func bsfNameForEncoder(encoderName string) string {
	enc := strings.ToLower(encoderName)
	switch {
	case strings.Contains(enc, "hevc"), strings.Contains(enc, "265"):
		return "hevc_metadata"
	case strings.Contains(enc, "264"):
		return "h264_metadata"
	}
	return ""
}

// allocSARBitstreamFilter builds a configured & initialised BSF that
// stamps the given SAR onto every encoded packet. Returns (nil, nil)
// when the BSF isn't applicable: empty SAR string, no metadata BSF for
// the encoder, or the BSF isn't compiled into the runtime libavcodec
// (graceful fallback — older builds without `--enable-bsf=h264_metadata`
// keep working without SAR stamping, matching the CLI backend's
// `sarBSFArg` fallback shape).
//
// On success the caller owns the returned context and is responsible
// for Free()ing it during chain teardown.
//
// SAR syntax: domain.VideoProfile uses "W:H" (e.g. "1:1", "8:9"); the
// BSF option expects "W/N" rational notation — translate here.
func allocSARBitstreamFilter(
	sar string,
	encoderCtx *astiav.CodecContext,
	encoderName string,
) (*astiav.BitStreamFilterContext, error) {
	if strings.TrimSpace(sar) == "" {
		return nil, nil
	}
	bsfName := bsfNameForEncoder(encoderName)
	if bsfName == "" {
		return nil, nil
	}
	bsf := astiav.FindBitStreamFilterByName(bsfName)
	if bsf == nil {
		// BSF absent in this libavcodec build — silently fall back. The
		// stream still encodes; the output just won't carry the operator-
		// requested SAR override. Matches the CLI backend's
		// "graceful degradation" behaviour.
		return nil, nil
	}

	bsfCtx, err := astiav.AllocBitStreamFilterContext(bsf)
	if err != nil {
		return nil, fmt.Errorf("alloc bsf %s: %w", bsfName, err)
	}

	// par_in must reflect what we feed into the BSF. The encoder is the
	// upstream producer; copying its params keeps codec / profile /
	// extradata aligned so the BSF can parse SPS NALUs out of the box.
	if err := bsfCtx.InputCodecParameters().FromCodecContext(encoderCtx); err != nil {
		bsfCtx.Free()
		return nil, fmt.Errorf("bsf %s: copy in params: %w", bsfName, err)
	}
	bsfCtx.SetInputTimeBase(encoderCtx.TimeBase())

	// h264_metadata / hevc_metadata take "sample_aspect_ratio" as a
	// rational ("W/N"). The schema's "W:H" form maps with a simple
	// substitution.
	sarValue := strings.ReplaceAll(strings.TrimSpace(sar), ":", "/")
	if err := bsfCtx.PrivateData().Options().Set(
		"sample_aspect_ratio",
		sarValue,
		astiav.OptionSearchFlags(0),
	); err != nil {
		bsfCtx.Free()
		return nil, fmt.Errorf("bsf %s: set sample_aspect_ratio=%s: %w", bsfName, sarValue, err)
	}

	if err := bsfCtx.Initialize(); err != nil {
		bsfCtx.Free()
		return nil, fmt.Errorf("bsf %s: init: %w", bsfName, err)
	}
	return bsfCtx, nil
}
