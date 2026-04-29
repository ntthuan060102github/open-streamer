package transcoder

import (
	"strings"
	"testing"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// testBaseChain is the placeholder scale chain the watermark tests prepend
// to. Kept identical across cases so changes in chain wrapping show up in
// diff rather than via duplicated literals.
const testBaseChain = "scale=1280:720"

// TestApplyImageWatermarkResize verifies the scale2ref chain is injected
// only when Resize=true, and that the width expression references main_w
// (not the watermark's own iw — that bug would produce wrong sizing).
func TestApplyImageWatermarkResize(t *testing.T) {
	base := testBaseChain + ",setsar=1"
	wm := &domain.WatermarkConfig{
		Enabled:   true,
		Type:      domain.WatermarkTypeImage,
		ImagePath: "/tmp/logo.png",
		Position:  domain.WatermarkTopRight,
		Resize:    true,
	}
	got := applyWatermark(base, wm.Resolved(), false)

	for _, frag := range []string{
		"scale2ref=",
		"main_w*",
		"ow/iw*ih",
		"[wm][mid]",
	} {
		if !strings.Contains(got, frag) {
			t.Fatalf("expected %q in resize chain, got: %s", frag, got)
		}
	}

	// Resize=false → scale2ref must NOT appear.
	wm.Resize = false
	got = applyWatermark(base, wm.Resolved(), false)
	if strings.Contains(got, "scale2ref") {
		t.Fatalf("scale2ref should be absent when Resize=false: %s", got)
	}
}

// TestApplyImageWatermarkResizeRatioOverride verifies a per-stream
// ResizeRatio overrides the package default in the scale2ref expression.
func TestApplyImageWatermarkResizeRatioOverride(t *testing.T) {
	base := testBaseChain
	wm := &domain.WatermarkConfig{
		Enabled:     true,
		Type:        domain.WatermarkTypeImage,
		ImagePath:   "/tmp/logo.png",
		Position:    domain.WatermarkTopRight,
		Resize:      true,
		ResizeRatio: 0.20, // 20% banner-sized
	}
	got := applyWatermark(base, wm.Resolved(), false)
	if !strings.Contains(got, "main_w*0.2000") {
		t.Fatalf("expected main_w*0.2000 (20%% override), got: %s", got)
	}
	if strings.Contains(got, "main_w*0.0800") {
		t.Fatalf("default 0.08 should not leak when ResizeRatio is set: %s", got)
	}
}

// TestApplyTextWatermarkResizeRatioOverride mirrors the image override test
// for the drawtext fontsize=h*ratio path.
func TestApplyTextWatermarkResizeRatioOverride(t *testing.T) {
	base := testBaseChain
	wm := &domain.WatermarkConfig{
		Enabled:     true,
		Type:        domain.WatermarkTypeText,
		Text:        "LIVE",
		FontSize:    24,
		Position:    domain.WatermarkTopRight,
		Resize:      true,
		ResizeRatio: 0.12,
	}
	got := applyWatermark(base, wm.Resolved(), false)
	if !strings.Contains(got, "fontsize='h*0.1200'") {
		t.Fatalf("expected fontsize=h*0.1200, got: %s", got)
	}
}

// TestApplyTextWatermarkResize verifies fontsize switches from a fixed
// pixel value to a frame-height fraction expression when Resize=true.
func TestApplyTextWatermarkResize(t *testing.T) {
	base := testBaseChain
	wm := &domain.WatermarkConfig{
		Enabled:  true,
		Type:     domain.WatermarkTypeText,
		Text:     "LIVE",
		FontSize: 24,
		Position: domain.WatermarkTopRight,
		Resize:   true,
	}
	got := applyWatermark(base, wm.Resolved(), false)
	if !strings.Contains(got, "fontsize='h*") {
		t.Fatalf("expected fontsize=h*ratio when Resize=true, got: %s", got)
	}
	if strings.Contains(got, "fontsize=24") {
		t.Fatalf("fixed fontsize=24 should not be present when Resize=true: %s", got)
	}

	// Resize=false → static FontSize.
	wm.Resize = false
	got = applyWatermark(base, wm.Resolved(), false)
	if !strings.Contains(got, "fontsize=24") {
		t.Fatalf("expected fixed fontsize=24 when Resize=false, got: %s", got)
	}
}

func TestApplyWatermarkInactivePassthrough(t *testing.T) {
	base := "scale=1920:1080,setsar=1"
	cases := []*domain.WatermarkConfig{
		nil,
		{Enabled: false, Type: domain.WatermarkTypeText, Text: "x"},
		{Enabled: true, Type: domain.WatermarkTypeText, Text: "  "},
	}
	for i, wm := range cases {
		got := applyWatermark(base, wm, false)
		if got != base {
			t.Errorf("case %d: chain mutated when watermark inactive: %s", i, got)
		}
	}
}

func TestApplyTextWatermarkCPU(t *testing.T) {
	base := "scale=1920:1080"
	wm := &domain.WatermarkConfig{
		Enabled:   true,
		Type:      domain.WatermarkTypeText,
		Text:      "LIVE",
		FontSize:  32,
		FontColor: "yellow",
		Opacity:   0.8,
		Position:  domain.WatermarkTopRight,
		OffsetX:   20, OffsetY: 30,
	}
	got := applyWatermark(base, wm, false)

	for _, frag := range []string{
		"scale=1920:1080,",
		"drawtext=text='LIVE'",
		"fontsize=32",
		"fontcolor='yellow@0.80'",
		"x='w-tw-20'",
		"y='30'",
	} {
		if !strings.Contains(got, frag) {
			t.Errorf("missing fragment %q in %q", frag, got)
		}
	}
	if strings.Contains(got, "hwdownload") || strings.Contains(got, "hwupload_cuda") {
		t.Errorf("CPU pipeline should not insert HW shuffle: %s", got)
	}
}

func TestApplyTextWatermarkGPU(t *testing.T) {
	base := "scale_cuda=1280:720"
	wm := &domain.WatermarkConfig{
		Enabled: true, Type: domain.WatermarkTypeText, Text: "LIVE",
	}
	got := applyWatermark(base, wm, true)

	for _, frag := range []string{
		"scale_cuda=1280:720,",
		"hwdownload",
		"format=nv12",
		"drawtext=",
		"hwupload_cuda",
	} {
		if !strings.Contains(got, frag) {
			t.Errorf("missing fragment %q in %q", frag, got)
		}
	}
	// The hwdownload must precede drawtext and hwupload_cuda must follow it.
	dl := strings.Index(got, "hwdownload")
	dt := strings.Index(got, "drawtext")
	up := strings.Index(got, "hwupload_cuda")
	if dl >= dt || dt >= up {
		t.Errorf("expected hwdownload < drawtext < hwupload_cuda, got %d %d %d", dl, dt, up)
	}
}

func TestApplyImageWatermarkCPU(t *testing.T) {
	base := "scale=1920:1080,setsar=1"
	wm := &domain.WatermarkConfig{
		Enabled:   true,
		Type:      domain.WatermarkTypeImage,
		ImagePath: "/var/lib/open-streamer/wm/logo.png",
		Opacity:   0.5,
		Position:  domain.WatermarkBottomLeft,
		OffsetX:   25, OffsetY: 25,
	}
	got := applyWatermark(base, wm, false)

	// Three-segment graph: <base>[mid]; movie=...,format=rgba,colorchannelmixer[wm]; [mid][wm]overlay=...
	segs := strings.Split(got, ";")
	if len(segs) != 3 {
		t.Fatalf("expected 3 segments, got %d: %s", len(segs), got)
	}
	if !strings.HasSuffix(segs[0], "[mid]") {
		t.Errorf("seg0 missing [mid] label: %q", segs[0])
	}
	for _, frag := range []string{"movie='/var/lib/open-streamer/wm/logo.png'", "format=rgba", "colorchannelmixer=aa=0.50", "[wm]"} {
		if !strings.Contains(segs[1], frag) {
			t.Errorf("seg1 missing %q: %q", frag, segs[1])
		}
	}
	for _, frag := range []string{"[mid][wm]overlay=", "x='25'", "y='H-h-25'"} {
		if !strings.Contains(segs[2], frag) {
			t.Errorf("seg2 missing %q: %q", frag, segs[2])
		}
	}
}

func TestApplyImageWatermarkGPU(t *testing.T) {
	base := "scale_cuda=1920:1080"
	wm := &domain.WatermarkConfig{
		Enabled: true, Type: domain.WatermarkTypeImage,
		ImagePath: "/img/logo.png", Opacity: 1.0, Position: domain.WatermarkCenter,
	}
	got := applyWatermark(base, wm, true)

	// hwdownload joins seg0 (before [mid]), hwupload_cuda joins seg2 (after overlay).
	if !strings.Contains(got, "hwdownload,format=nv12[mid]") {
		t.Errorf("seg0 should end with hwdownload,format=nv12[mid]: %q", got)
	}
	if !strings.HasSuffix(got, "hwupload_cuda") {
		t.Errorf("expected GPU graph to end with hwupload_cuda: %q", got)
	}
	// Opacity=1.0 → no colorchannelmixer needed (movie's alpha used as-is).
	if strings.Contains(got, "colorchannelmixer") {
		t.Errorf("opaque watermark should skip colorchannelmixer: %q", got)
	}
	// Center coords use W/w / H/h (overlay context), not w/h alone.
	if !strings.Contains(got, "x='(W-w)/2'") || !strings.Contains(got, "y='(H-h)/2'") {
		t.Errorf("center coords missing or wrong: %q", got)
	}
}

func TestApplyTextWatermarkCustomPosition(t *testing.T) {
	wm := &domain.WatermarkConfig{
		Enabled: true, Type: domain.WatermarkTypeText, Text: "x",
		Position: domain.WatermarkCustom,
		X:        "main_w-overlay_w-50", Y: "if(gt(t,5),10,-100)",
	}
	got := applyWatermark(testBaseChain, wm, false)
	if !strings.Contains(got, "x='main_w-overlay_w-50'") {
		t.Errorf("custom x not forwarded: %q", got)
	}
	if !strings.Contains(got, "y='if(gt(t,5),10,-100)'") {
		t.Errorf("custom y not forwarded: %q", got)
	}
}

func TestApplyImageWatermarkCustomPosition(t *testing.T) {
	wm := &domain.WatermarkConfig{
		Enabled: true, Type: domain.WatermarkTypeImage, ImagePath: "/img/logo.png",
		Position: domain.WatermarkCustom, X: "100", Y: "200", Opacity: 1,
	}
	got := applyWatermark("scale=1920:1080", wm, false)
	if !strings.Contains(got, "x='100'") || !strings.Contains(got, "y='200'") {
		t.Errorf("custom coords not in overlay: %q", got)
	}
}

func TestDefaultExprFallbackToZero(t *testing.T) {
	wm := &domain.WatermarkConfig{
		Enabled: true, Type: domain.WatermarkTypeText, Text: "x",
		Position: domain.WatermarkCustom,
		X:        "  ", // blank → "0"
	}
	got := applyWatermark("", wm, false)
	if !strings.Contains(got, "x='0'") {
		t.Errorf("blank custom x should default to 0: %q", got)
	}
}

func TestDrawTextCoordsAllPositions(t *testing.T) {
	cases := map[domain.WatermarkPosition]struct {
		x, y string
	}{
		domain.WatermarkTopLeft:     {"10", "10"},
		domain.WatermarkTopRight:    {"w-tw-10", "10"},
		domain.WatermarkBottomLeft:  {"10", "h-th-10"},
		domain.WatermarkBottomRight: {"w-tw-10", "h-th-10"},
		domain.WatermarkCenter:      {"(w-tw)/2", "(h-th)/2"},
	}
	for pos, want := range cases {
		x, y := drawTextCoords(pos, 10, 10)
		if x != want.x || y != want.y {
			t.Errorf("%s: got (%s,%s), want (%s,%s)", pos, x, y, want.x, want.y)
		}
	}
}

func TestFFmpegQuoteEscapesSingleQuote(t *testing.T) {
	if got := ffmpegQuote("Bob's Show"); got != `'Bob'\''s Show'` {
		t.Errorf("got %q", got)
	}
	if got := ffmpegQuote("LIVE %{localtime:%H}"); got != `'LIVE %{localtime:%H}'` {
		t.Errorf("strftime body should pass through unescaped: %q", got)
	}
}

// TestApplyWatermarkEmptyBase covers the corner case where there is no
// scale chain (e.g. profile with width/height=0 and no resize): the output
// should still produce a valid filter graph using just the watermark.
func TestApplyWatermarkEmptyBase(t *testing.T) {
	wm := &domain.WatermarkConfig{
		Enabled: true, Type: domain.WatermarkTypeText, Text: "TAG",
	}
	got := applyWatermark("", wm, false)
	if !strings.HasPrefix(got, "drawtext=") {
		t.Errorf("empty base should start with drawtext, got %q", got)
	}
}

// TestBuildFFmpegArgsAppliesWatermark — end-to-end through buildFFmpegArgs:
// confirms the watermark survives composition with the rest of the args
// (single-output, legacy mode).
func TestBuildFFmpegArgsAppliesWatermark(t *testing.T) {
	tc := &domain.TranscoderConfig{
		Watermark: &domain.WatermarkConfig{
			Enabled: true, Type: domain.WatermarkTypeText, Text: "WM",
		},
	}
	args, err := buildFFmpegArgs([]Profile{{Width: 1280, Height: 720, Bitrate: "1500k"}}, tc)
	if err != nil {
		t.Fatal(err)
	}
	vf, ok := readFlagValue(args, "-vf")
	if !ok {
		t.Fatal("no -vf in args")
	}
	if !strings.Contains(vf, "drawtext=text='WM'") {
		t.Errorf("watermark not in -vf: %s", vf)
	}
}

// TestBuildMultiOutputArgsAppliesWatermarkPerOutput — confirms each output
// in multi-output mode gets the watermark filter on its own -vf:v:0 chain.
func TestBuildMultiOutputArgsAppliesWatermarkPerOutput(t *testing.T) {
	tc := &domain.TranscoderConfig{
		Watermark: &domain.WatermarkConfig{
			Enabled: true, Type: domain.WatermarkTypeText, Text: "WM",
		},
	}
	args, err := buildMultiOutputArgs([]Profile{
		{Width: 1920, Height: 1080, Bitrate: "4500k"},
		{Width: 1280, Height: 720, Bitrate: "2500k"},
	}, tc)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for i, a := range args {
		if a == "-vf:v:0" && i+1 < len(args) && strings.Contains(args[i+1], "drawtext=text='WM'") {
			count++
		}
	}
	if count != 2 {
		t.Errorf("expected watermark on 2 outputs, got %d", count)
	}
}

// readFlagValue scans an FFmpeg argv for `flag <value>` and returns value.
func readFlagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}
