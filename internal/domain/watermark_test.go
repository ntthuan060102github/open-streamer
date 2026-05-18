package domain

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWatermarkIsActive(t *testing.T) {
	cases := map[string]struct {
		w    *WatermarkConfig
		want bool
	}{
		"nil":            {nil, false},
		"disabled":       {&WatermarkConfig{Enabled: false, Type: WatermarkTypeText, Text: "x"}, false},
		"empty text":     {&WatermarkConfig{Enabled: true, Type: WatermarkTypeText, Text: "  "}, false},
		"empty image":    {&WatermarkConfig{Enabled: true, Type: WatermarkTypeImage, ImagePath: ""}, false},
		"unknown type":   {&WatermarkConfig{Enabled: true, Type: "blink", Text: "x"}, false},
		"opaque text":    {&WatermarkConfig{Enabled: true, Type: WatermarkTypeText, Text: "x", Opacity: 1.0}, true},
		"image set":      {&WatermarkConfig{Enabled: true, Type: WatermarkTypeImage, ImagePath: "/x.png"}, true},
		"barely visible": {&WatermarkConfig{Enabled: true, Type: WatermarkTypeText, Text: "x", Opacity: 0.001}, false},
	}
	for name, c := range cases {
		if got := c.w.IsActive(); got != c.want {
			t.Errorf("%s: IsActive=%v, want %v", name, got, c.want)
		}
	}
}

func TestWatermarkValidate(t *testing.T) {
	dir := t.TempDir()
	font := filepath.Join(dir, "f.ttf")
	img := filepath.Join(dir, "logo.png")
	if err := os.WriteFile(font, []byte("FONT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(img, []byte("PNG"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		w       *WatermarkConfig
		wantErr bool
	}{
		"nil":      {nil, false},
		"disabled": {&WatermarkConfig{Enabled: false, Type: WatermarkTypeImage}, false},
		"text ok": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeText, Text: "LIVE", Opacity: 0.8,
			Position: WatermarkBottomRight,
		}, false},
		"text missing": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeText,
		}, true},
		"image ok": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeImage, Filename: "logo.png", Opacity: 1,
		}, false},
		"image missing filename": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeImage,
		}, true},
		"font ok": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeText, Text: "x", FontFile: font,
		}, false},
		"font not found": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeText, Text: "x", FontFile: "/nope/font.ttf",
		}, true},
		"opacity high": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeText, Text: "x", Opacity: 1.5,
		}, true},
		"opacity negative": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeText, Text: "x", Opacity: -0.1,
		}, true},
		"unknown position": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeText, Text: "x", Position: "side_eye",
		}, true},
		"unknown type": {&WatermarkConfig{
			Enabled: true, Type: "fancy", Text: "x",
		}, true},
		"custom no coords": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeText, Text: "x", Position: WatermarkCustom,
		}, true},
		"custom x only": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeText, Text: "x", Position: WatermarkCustom, X: "100",
		}, false},
		"filename ok": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeImage, Filename: "logo.png",
		}, false},
		"filename missing": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeImage,
		}, true},
		"filename invalid (path traversal)": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeImage, Filename: "../../etc/passwd",
		}, true},
		"filename missing extension": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeImage, Filename: "logo",
		}, true},
		"filename multiple dots": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeImage, Filename: "logo.tar.gz",
		}, true},
		"resize toggle on": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeText, Text: "x", Resize: true,
		}, false},
	}
	for name, c := range cases {
		err := c.w.Validate()
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", name, err, c.wantErr)
		}
	}
}

func TestWatermarkResolved(t *testing.T) {
	w := &WatermarkConfig{Enabled: true, Type: WatermarkTypeText, Text: "x"}
	r := w.Resolved()
	if r.FontSize != defaultWatermarkFontSize {
		t.Errorf("FontSize=%d", r.FontSize)
	}
	if r.FontColor != defaultWatermarkFontColor {
		t.Errorf("FontColor=%s", r.FontColor)
	}
	if r.Opacity != defaultWatermarkOpacity {
		t.Errorf("Opacity=%f", r.Opacity)
	}
	if r.Position != defaultWatermarkPosition {
		t.Errorf("Position=%s", r.Position)
	}
	if r.OffsetX != defaultWatermarkOffset || r.OffsetY != defaultWatermarkOffset {
		t.Errorf("Offsets=%d,%d", r.OffsetX, r.OffsetY)
	}

	// Center position should NOT inject the corner padding default.
	w2 := &WatermarkConfig{Enabled: true, Type: WatermarkTypeText, Text: "x", Position: WatermarkCenter}
	if r := w2.Resolved(); r.OffsetX != 0 || r.OffsetY != 0 {
		t.Errorf("center offsets should default 0, got %d,%d", r.OffsetX, r.OffsetY)
	}

	// Nil round-trip is safe.
	if (*WatermarkConfig)(nil).Resolved() != nil {
		t.Error("nil.Resolved() should be nil")
	}

	// Resize=true is preserved verbatim — sizing is computed downstream
	// (transcoder builds the per-profile frameScale from the ladder),
	// not via a default-fill on Resolved.
	w3 := &WatermarkConfig{
		Enabled: true, Type: WatermarkTypeText, Text: "x", Resize: true,
	}
	if r := w3.Resolved(); !r.Resize {
		t.Errorf("Resize toggle dropped by Resolved()")
	}
}
