package native

// watermark.go — text & image overlay filter graph generation for the
// native (in-process libavfilter) backend.
//
// The generated string is fed straight into astiav.FilterGraph.Parse,
// so the syntax mirrors what the FFmpeg CLI backend used to emit. Two
// flavours:
//
//   - CPU path: drawtext / overlay run on libavfilter's CPU frames.
//     Single linear chain.
//   - GPU/NVENC path: drawtext is CPU-only and overlay_cuda needs
//     `--enable-cuda-nvcc` (Ubuntu apt builds skip it). We round-trip
//     via CPU memory — hwdownload → drawtext/overlay → hwupload_cuda.
//     Costs ~1 frame copy each way; portability win > 5% CPU cost.

import (
	"fmt"
	"strings"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// applyWatermark wraps the base filter chain (scale / setsar fragments)
// with the watermark filter graph. Returns base unchanged when wm is
// nil, disabled, or otherwise inactive.
//
// onGPU=true triggers the hwdownload→filter→hwupload_cuda round-trip
// — only true for NVENC. Other HW backends don't currently support a
// portable hwoverlay either, so they take the same CPU path as the
// software pipeline.
//
// frameScale is the per-profile sizing factor (1.0 = native, <1.0 =
// shrink) — used only when Watermark.Resize is true. Computed by the
// caller as `profile_width / max_ladder_width`.
func applyWatermark(base string, wm *domain.WatermarkConfig, onGPU bool, frameScale float64) string {
	if wm == nil || !wm.IsActive() {
		return base
	}
	r := scaledForRender(wm.Resolved(), frameScale)
	switch r.Type {
	case domain.WatermarkTypeText:
		return appendTextWatermark(base, r, onGPU)
	case domain.WatermarkTypeImage:
		return appendImageWatermark(base, r, onGPU, frameScale)
	}
	return base
}

// scaledForRender returns a copy of r whose pixel-scale fields
// (FontSize, OffsetX, OffsetY) are multiplied by frameScale. Used by
// the reference-frame sizing model: largest profile renders at native
// size (frameScale=1.0); smaller profiles shrink proportionally so the
// on-screen ratio is consistent across the ladder.
//
// Fast-paths the no-op cases (Resize=false or frameScale>=1.0).
func scaledForRender(r *domain.WatermarkConfig, frameScale float64) *domain.WatermarkConfig {
	if r == nil || !r.Resize || frameScale >= 1.0 {
		return r
	}
	out := *r
	if out.FontSize > 0 {
		out.FontSize = int(float64(out.FontSize)*frameScale + 0.5)
	}
	if out.OffsetX > 0 {
		out.OffsetX = int(float64(out.OffsetX)*frameScale + 0.5)
	}
	if out.OffsetY > 0 {
		out.OffsetY = int(float64(out.OffsetY)*frameScale + 0.5)
	}
	return &out
}

// appendTextWatermark composes a single-chain drawtext filter. No
// labels needed because drawtext is 1-in/1-out. GPU pipelines wrap
// with hwdownload / hwupload_cuda.
func appendTextWatermark(base string, r *domain.WatermarkConfig, onGPU bool) string {
	x, y := resolveTextCoords(r)
	dt := strings.Join([]string{
		"drawtext=text=" + ffmpegQuote(r.Text),
		"fontsize=" + itoa(r.FontSize),
		"fontcolor=" + ffmpegQuote(fmt.Sprintf("%s@%.2f", r.FontColor, r.Opacity)),
		"x=" + ffmpegQuote(x),
		"y=" + ffmpegQuote(y),
	}, ":")
	if r.FontFile != "" {
		dt += ":fontfile=" + ffmpegQuote(r.FontFile)
	}
	if !onGPU {
		return joinChain(base, dt)
	}
	return joinChain(base, "hwdownload", "format=nv12", dt, "hwupload_cuda")
}

// appendImageWatermark composes the filter graph that loads the
// watermark asset via `movie=` and overlays it on the main stream.
//
// Asset sizing: the asset renders at native pixel size on the largest
// profile; smaller profiles scale the asset down by frameScale. Same
// model as the deleted CLI backend's applyWatermark — proportional
// shrink baked into the movie source chain, not via scale2ref (no
// second input needed since iw/ih multiplied by a known constant).
//
// Output shape:
//
//	<base>[,hwdownload,format=nv12][mid];
//	movie=...,format=rgba[,colorchannelmixer=...][,scale=iw*f:ih*f][wm];
//	[mid][wm]overlay=x=…:y=…[,hwupload_cuda]
func appendImageWatermark(base string, r *domain.WatermarkConfig, onGPU bool, frameScale float64) string {
	leading := base
	if onGPU {
		leading = joinChain(leading, "hwdownload", "format=nv12")
	}
	leading += "[mid]"

	srcParts := []string{
		"movie=" + ffmpegQuote(r.ImagePath),
		"format=rgba",
	}
	if r.Opacity > 0 && r.Opacity < 1 {
		srcParts = append(srcParts, fmt.Sprintf("colorchannelmixer=aa=%.2f", r.Opacity))
	}
	if r.Resize && frameScale > 0 && frameScale < 1.0 {
		srcParts = append(srcParts, fmt.Sprintf("scale=iw*%.4f:ih*%.4f", frameScale, frameScale))
	}
	srcChain := strings.Join(srcParts, ",") + "[wm]"

	x, y := resolveImageCoords(r)
	overlay := fmt.Sprintf("[mid][wm]overlay=x=%s:y=%s", ffmpegQuote(x), ffmpegQuote(y))
	if onGPU {
		overlay += ",hwupload_cuda"
	}
	return leading + ";" + srcChain + ";" + overlay
}

// resolveTextCoords picks the drawtext coordinate expressions for the
// given watermark config. position=custom forwards user-supplied X/Y
// unchanged (defaulting to "0" when blank); presets dispatch to the
// canned drawTextCoords formulas.
func resolveTextCoords(r *domain.WatermarkConfig) (x, y string) {
	if r.Position == domain.WatermarkCustom {
		return defaultExpr(r.X), defaultExpr(r.Y)
	}
	return drawTextCoords(r.Position, r.OffsetX, r.OffsetY)
}

// resolveImageCoords is the overlay-flavoured equivalent of
// resolveTextCoords (capital W/H = main frame, lowercase w/h = overlay).
func resolveImageCoords(r *domain.WatermarkConfig) (x, y string) {
	if r.Position == domain.WatermarkCustom {
		return defaultExpr(r.X), defaultExpr(r.Y)
	}
	return overlayCoords(r.Position, r.OffsetX, r.OffsetY)
}

// defaultExpr substitutes "0" for an empty / whitespace-only expression
// so the resulting filter fragment is always syntactically valid.
func defaultExpr(s string) string {
	if t := strings.TrimSpace(s); t != "" {
		return t
	}
	return "0"
}

// drawTextCoords returns the (x, y) expression for a drawtext origin
// given a position preset. tw/th = drawtext-evaluated text size;
// w/h = input frame size.
func drawTextCoords(pos domain.WatermarkPosition, ox, oy int) (x, y string) {
	switch pos {
	case domain.WatermarkTopLeft:
		return itoa(ox), itoa(oy)
	case domain.WatermarkTopRight:
		return fmt.Sprintf("w-tw-%d", ox), itoa(oy)
	case domain.WatermarkBottomLeft:
		return itoa(ox), fmt.Sprintf("h-th-%d", oy)
	case domain.WatermarkCenter:
		return "(w-tw)/2", "(h-th)/2"
	case domain.WatermarkCustom:
		fallthrough
	case domain.WatermarkBottomRight:
		fallthrough
	default:
		return fmt.Sprintf("w-tw-%d", ox), fmt.Sprintf("h-th-%d", oy)
	}
}

// overlayCoords returns the (x, y) expression for an overlay origin.
// W/H = main video, w/h = overlay (asset) size.
func overlayCoords(pos domain.WatermarkPosition, ox, oy int) (x, y string) {
	switch pos {
	case domain.WatermarkTopLeft:
		return itoa(ox), itoa(oy)
	case domain.WatermarkTopRight:
		return fmt.Sprintf("W-w-%d", ox), itoa(oy)
	case domain.WatermarkBottomLeft:
		return itoa(ox), fmt.Sprintf("H-h-%d", oy)
	case domain.WatermarkCenter:
		return "(W-w)/2", "(H-h)/2"
	case domain.WatermarkCustom:
		fallthrough
	case domain.WatermarkBottomRight:
		fallthrough
	default:
		return fmt.Sprintf("W-w-%d", ox), fmt.Sprintf("H-h-%d", oy)
	}
}

// joinChain joins filter nodes with a comma, skipping empties so the
// caller can feed in optional fragments without sentinel checks.
func joinChain(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, ",")
}

// ffmpegQuote wraps a value in single quotes, escaping any embedded
// single-quote with the FFmpeg-mandated `'\''` sequence. Required when
// a filter argument contains commas, semicolons, colons, brackets, or
// whitespace.
func ffmpegQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
