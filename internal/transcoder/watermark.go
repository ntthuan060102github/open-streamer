package transcoder

// watermark.go — text & image overlay filter graph generation.
//
// Two output flavours, depending on the active hardware pipeline:
//
//   - CPU path (libx264 / VAAPI / VideoToolbox / NVENC-with-no-cuda-overlay):
//     drawtext / overlay run directly on CPU frames. Filter chain stays
//     a single -vf chain.
//
//   - GPU/NVENC path:
//     drawtext is CPU-only and overlay_cuda requires --enable-cuda-nvcc
//     (Ubuntu apt builds skip it). To stay portable across distros we
//     round-trip through CPU memory: scale_cuda → hwdownload → drawtext/
//     overlay → hwupload_cuda. The cost is ~1 frame copy in each direction,
//     measured around 4-6% of one CPU core per FFmpeg process at 1080p25 —
//     acceptable trade-off vs requiring a custom FFmpeg build on every
//     deployment.
//
// Image overlays use the `movie=` source filter instead of an extra `-i`
// input so the args structure (single -vf) stays uniform with text watermarks
// and the multi-output mode doesn't need filter_complex restructuring.

import (
	"fmt"
	"strings"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// applyWatermark wraps the existing scale/setsar chain (`base`) with a
// watermark filter graph. Returns the original chain unchanged when the
// watermark config is missing / disabled / inactive.
//
// onGPU=true triggers a hwdownload→filter→hwupload_cuda round-trip because
// drawtext is CPU-only and overlay_cuda isn't part of every distro's
// FFmpeg build (Ubuntu apt). The portability win outweighs the ~5% CPU
// cost of the round-trip.
func applyWatermark(base string, wm *domain.WatermarkConfig, onGPU bool) string {
	if !wm.IsActive() {
		return base
	}
	r := wm.Resolved()
	switch r.Type {
	case domain.WatermarkTypeText:
		return appendTextWatermark(base, r, onGPU)
	case domain.WatermarkTypeImage:
		return appendImageWatermark(base, r, onGPU)
	}
	return base
}

// appendTextWatermark composes a single-chain filter (no labels needed)
// because drawtext takes one input and produces one output. On GPU
// pipelines we wrap with hwdownload / hwupload_cuda.
//
// Resize=true replaces the static FontSize pixel value with `h*ratio`
// where h is the main frame height — drawtext exposes h as a built-in
// variable, so the substitution makes glyphs scale proportionally with
// each ABR rendition's resolution.
func appendTextWatermark(base string, r *domain.WatermarkConfig, onGPU bool) string {
	x, y := resolveTextCoords(r)
	fontsize := itoa(r.FontSize)
	if r.Resize {
		// r.ResizeRatio is filled by WatermarkConfig.Resolved() — defaults
		// to defaultWatermarkResizeRatio when the operator left it 0.
		fontsize = ffmpegQuote(fmt.Sprintf("h*%.4f", r.ResizeRatio))
	}
	dt := strings.Join([]string{
		"drawtext=text=" + ffmpegQuote(r.Text),
		"fontsize=" + fontsize,
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

// appendImageWatermark composes a 3-chain filter graph that loads the image
// via `movie=` (avoids an extra `-i` input which would complicate the
// multi-output args builder), and overlays it on the main stream.
//
// Layout (Resize=false, native pixel size):
//
//	<base>[,hwdownload,format=nv12][mid];
//	movie=...,format=rgba,colorchannelmixer=aa=...[wm];
//	[mid][wm]overlay=x=…:y=…[,hwupload_cuda]
//
// Layout (Resize=true) — adds a scale2ref pass so the overlay scales
// relative to the main frame's width. scale2ref takes two inputs (the
// reference + the source) and emits two outputs (reference unchanged +
// scaled source); we feed [mid][wm] in and recapture as [mid][wm]:
//
//	<base>...[mid];
//	movie=...,format=rgba...[wm];
//	[wm][mid]scale2ref=oh*mdar:W*ratio[wm][mid];
//	[mid][wm]overlay=x=…:y=…[,hwupload_cuda]
//
// `oh*mdar` keeps the watermark's aspect ratio (output_height *
// main_display_aspect_ratio computes the matching width). We pass the
// reference second in the scale2ref input order because the filter wants
// "scale this [first] using ref [second]" — we want to scale the image
// (wm) using main video (mid) as ref.
//
// Each `;`-separated segment is its own chain; explicit labels wire them
// together. The implicit `-vf` input feeds the first chain (no `[in]`
// needed), and the final chain implicitly produces the `-vf` output.
func appendImageWatermark(base string, r *domain.WatermarkConfig, onGPU bool) string {
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
	srcChain := strings.Join(srcParts, ",") + "[wm]"

	x, y := resolveImageCoords(r)
	overlay := fmt.Sprintf("[mid][wm]overlay=x=%s:y=%s", ffmpegQuote(x), ffmpegQuote(y))
	if onGPU {
		overlay += ",hwupload_cuda"
	}

	// Two-chain default; insert a scale2ref chain between source and overlay
	// when proportional resize is on.
	//
	// scale2ref takes [scale_target][reference] and emits [scaled][reference].
	// We pass [wm][mid] so the watermark gets scaled while [mid] passes
	// through unchanged, then feed both into the overlay below.
	//
	// Width expression `main_w*ratio` reads main video width from the
	// reference (FFmpeg exposes main_w / main_h inside scale2ref expressions).
	// Height `ow/iw*ih` keeps the watermark's original aspect ratio: target
	// height = (target_width / source_width) * source_height — using `ow`/`iw`
	// avoids a divide-by-zero edge that `-1` triggers on some libavfilter
	// builds when the source is non-square-pixel.
	if r.Resize {
		scaleChain := fmt.Sprintf(
			"[wm][mid]scale2ref=%s:%s[wm][mid]",
			ffmpegQuote(fmt.Sprintf("main_w*%.4f", r.ResizeRatio)),
			ffmpegQuote("ow/iw*ih"),
		)
		return leading + ";" + srcChain + ";" + scaleChain + ";" + overlay
	}

	return leading + ";" + srcChain + ";" + overlay
}

// resolveTextCoords picks the drawtext coordinate expressions for the given
// watermark config. position=custom forwards user-supplied X/Y unchanged
// (defaulting to "0" when blank), giving operators full FFmpeg expression
// power; presets dispatch to the canned drawTextCoords() formulas.
func resolveTextCoords(r *domain.WatermarkConfig) (x, y string) {
	if r.Position == domain.WatermarkCustom {
		return defaultExpr(r.X), defaultExpr(r.Y)
	}
	return drawTextCoords(r.Position, r.OffsetX, r.OffsetY)
}

// resolveImageCoords is the overlay-flavoured equivalent of resolveTextCoords.
// Same X/Y forwarding; preset presets dispatch to overlayCoords (capital W/H,
// lowercase w/h).
func resolveImageCoords(r *domain.WatermarkConfig) (x, y string) {
	if r.Position == domain.WatermarkCustom {
		return defaultExpr(r.X), defaultExpr(r.Y)
	}
	return overlayCoords(r.Position, r.OffsetX, r.OffsetY)
}

// defaultExpr substitutes "0" for an empty / whitespace-only expression so
// the resulting FFmpeg fragment is always syntactically valid.
func defaultExpr(s string) string {
	if t := strings.TrimSpace(s); t != "" {
		return t
	}
	return "0"
}

// drawTextCoords returns the FFmpeg expression for the (x, y) drawtext
// origin given a position preset. `tw`/`th` are drawtext-evaluated text
// width/height; `w`/`h` are the input frame size.
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
		// Caller (resolveTextCoords) intercepts WatermarkCustom before
		// this fn is reached, but cover it for exhaustiveness — fall
		// through to the BottomRight default rather than emitting a
		// useless empty expression.
		fallthrough
	case domain.WatermarkBottomRight:
		fallthrough
	default:
		return fmt.Sprintf("w-tw-%d", ox), fmt.Sprintf("h-th-%d", oy)
	}
}

// overlayCoords returns the FFmpeg expression for the (x, y) overlay origin.
// `W`/`H` are the main video size; `w`/`h` are the overlay (image) size.
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
		// resolveImageCoords intercepts custom before us; fall through
		// to BottomRight default for exhaustiveness.
		fallthrough
	case domain.WatermarkBottomRight:
		fallthrough
	default:
		return fmt.Sprintf("W-w-%d", ox), fmt.Sprintf("H-h-%d", oy)
	}
}

// joinChain joins filter nodes with a comma, skipping empty entries so
// callers can feed in optional fragments without sentinel checks.
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
// single-quote with the FFmpeg-mandated `\'` outside-the-quotes sequence.
// Safe for arbitrary user-supplied strings (paths, drawtext bodies, color
// expressions). FFmpeg requires this within filtergraph descriptors when
// a value contains the graph delimiters comma, semicolon, colon, square
// brackets, or whitespace.
func ffmpegQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
