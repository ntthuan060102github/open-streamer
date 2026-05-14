package native

import (
	"fmt"
	"strings"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// filterChainSpec composes the filter-graph string a profile will run
// through (deinterlace → resize → watermark). Same conceptual shape
// as the deleted FFmpeg CLI backend's buildVideoFilter; the strings
// are pasted into astiav.FilterGraph.Parse instead of `-vf` on a CLI
// subprocess.
//
// SAR is deliberately NOT emitted in the filter graph. The FFmpeg CLI
// backend handled SAR via `-bsf:v h264_metadata` (bitstream filter,
// post-encoder) which the native backend will mirror in a later phase.
//
// frameScale is the per-profile watermark sizing factor (1.0 = native
// size, <1.0 = proportional shrink). Computed by the caller as
// `profile_width / max_ladder_width`. Only consulted when wm has
// Resize=true; ignored otherwise.
func filterChainSpec(
	hw domain.HWAccel,
	p domain.VideoProfile,
	interlace domain.InterlaceMode,
	wm *domain.WatermarkConfig,
	frameScale float64,
) string {
	deinterlace := deinterlaceFilterSpec(hw, interlace)
	resize := resizeFilterSpec(hw, p)

	parts := make([]string, 0, 2)
	if deinterlace != "" {
		parts = append(parts, deinterlace)
	}
	if resize != "" {
		parts = append(parts, resize)
	}
	base := strings.Join(parts, ",")
	if base == "" {
		// libavfilter rejects an empty filter graph. A null filter is
		// the no-op pass-through: useful when the source already
		// matches the requested width/height and isn't interlaced.
		base = "null"
	}

	// Watermark layers AFTER scale so the overlay coordinates evaluate
	// against the final output frame size, matching what operators see
	// on the player. onGPU is NVENC-only — other HW backends don't
	// have a portable hwoverlay so they take the CPU path inside
	// applyWatermark just like the software pipeline.
	onGPU := hw == domain.HWAccelNVENC
	return applyWatermark(base, wm, onGPU, frameScale)
}

// deinterlaceFilterSpec returns the yadif fragment matched to the
// active backend and operator-requested mode. Returns "" when no
// deinterlace is requested (mode "" or "progressive").
//
// The yadif modes are mapped from domain.InterlaceMode:
//   - "" / progressive: skip the filter entirely
//   - auto / tff / bff: emit a yadif with the matching parity
func deinterlaceFilterSpec(hw domain.HWAccel, mode domain.InterlaceMode) string {
	if mode == "" || mode == domain.InterlaceProgressive {
		return ""
	}
	parity := yadifParity(mode)
	switch hw {
	case domain.HWAccelNVENC:
		return fmt.Sprintf("yadif_cuda=mode=0:parity=%s:deint=0", parity)
	case domain.HWAccelQSV:
		// QSV has no widely-shipped yadif equivalent; rely on the
		// VPP scaler's implicit deinterlace when supported. Empty
		// here means the chain starts at scale_qsv directly.
		return ""
	case domain.HWAccelVAAPI:
		// deinterlace_vaapi is the standard VAAPI deinterlacer.
		return "deinterlace_vaapi"
	case domain.HWAccelVideoToolbox:
		// VT has no dedicated hw yadif; libav would fall back to CPU
		// yadif which forces hwdownload. Source on Mac dev is usually
		// progressive — skip.
		return ""
	}
	return fmt.Sprintf("yadif=mode=0:parity=%s:deint=0", parity)
}

// yadifParity translates the domain enum to libav's parity argument.
// auto → -1 (libav detects per frame); tff → 0; bff → 1.
func yadifParity(mode domain.InterlaceMode) string {
	switch mode {
	case domain.InterlaceTopField:
		return "0"
	case domain.InterlaceBottomField:
		return "1"
	}
	return "-1"
}

// resizeFilterSpec returns the scaler fragment for the active backend.
// Mirrors the deleted CLI backend's resizeFilter: aspect-preserving
// scale with dimensions rounded to multiples of 2 (H.264/HEVC require
// even W/H).
//
// CPU pad mode (the CLI backend's default "letterbox to exact size")
// is deliberately NOT replicated here. GPU pipelines lose too much
// CUDA-resident throughput on hwdownload + pad_cuda variants; the
// native backend follows the same "fit, let player letterbox" trade-
// off the CLI backend's GPU path settled on.
func resizeFilterSpec(hw domain.HWAccel, p domain.VideoProfile) string {
	if p.Width <= 0 && p.Height <= 0 {
		return "" // source dimensions, no resize.
	}
	w, h := p.Width, p.Height
	switch hw {
	case domain.HWAccelNVENC:
		return fmt.Sprintf("scale_cuda=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2", w, h)
	case domain.HWAccelQSV:
		return fmt.Sprintf("scale_qsv=%d:%d", w, h)
	case domain.HWAccelVAAPI:
		return fmt.Sprintf("scale_vaapi=%d:%d", w, h)
	}
	// CPU / VideoToolbox: software libswscale.
	return fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2", w, h)
}
