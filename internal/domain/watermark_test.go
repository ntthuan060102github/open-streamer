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
			Enabled: true, Type: WatermarkTypeImage, ImagePath: img, Opacity: 1,
		}, false},
		"image missing path": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeImage,
		}, true},
		"image relative path": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeImage, ImagePath: "logo.png",
		}, true},
		"image not found": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeImage, ImagePath: "/nope/never.png",
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
		"both image_path and asset_id": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeImage, ImagePath: img, AssetID: "abc-123",
		}, true},
		"asset_id ok": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeImage, AssetID: "abc-123",
		}, false},
		"asset_id invalid chars": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeImage, AssetID: "../../etc/passwd",
		}, true},
		"resize_ratio in range": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeText, Text: "x", Resize: true, ResizeRatio: 0.10,
		}, false},
		"resize_ratio negative": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeText, Text: "x", Resize: true, ResizeRatio: -0.05,
		}, true},
		"resize_ratio too large": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeText, Text: "x", Resize: true, ResizeRatio: 1.5,
		}, true},
		"resize_ratio zero ok": {&WatermarkConfig{
			Enabled: true, Type: WatermarkTypeText, Text: "x", Resize: true, ResizeRatio: 0,
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

	// ResizeRatio defaults only when Resize=true AND ratio is 0 — leaving
	// it untouched on disabled-resize configs avoids surprising operators
	// with phantom values in serialised state.
	w3 := &WatermarkConfig{
		Enabled: true, Type: WatermarkTypeText, Text: "x", Resize: true,
	}
	if r := w3.Resolved(); r.ResizeRatio != defaultWatermarkResizeRatio {
		t.Errorf("Resize=true, ratio=0 should default to %v, got %v", defaultWatermarkResizeRatio, r.ResizeRatio)
	}

	// Operator override survives Resolved().
	w4 := &WatermarkConfig{
		Enabled: true, Type: WatermarkTypeText, Text: "x", Resize: true, ResizeRatio: 0.20,
	}
	if r := w4.Resolved(); r.ResizeRatio != 0.20 {
		t.Errorf("operator-set ResizeRatio=0.20 lost, got %v", r.ResizeRatio)
	}

	// Resize=false should NOT inject the default — keep the field at 0.
	w5 := &WatermarkConfig{
		Enabled: true, Type: WatermarkTypeText, Text: "x", Resize: false,
	}
	if r := w5.Resolved(); r.ResizeRatio != 0 {
		t.Errorf("Resize=false should leave ResizeRatio=0, got %v", r.ResizeRatio)
	}
}
