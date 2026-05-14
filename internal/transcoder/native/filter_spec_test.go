package native

import (
	"strings"
	"testing"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// TestFilterChainSpec_Watermark exercises the deinterlace / scale /
// watermark interleave for the native filter graph builder.
func TestFilterChainSpec_Watermark(t *testing.T) {
	prof := domain.VideoProfile{Width: 1280, Height: 720}

	tests := []struct {
		name         string
		hw           domain.HWAccel
		interlace    domain.InterlaceMode
		wm           *domain.WatermarkConfig
		frameScale   float64
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "no watermark + no interlace falls through to scale",
			hw:           domain.HWAccelNone,
			interlace:    "",
			wm:           nil,
			frameScale:   1.0,
			wantContains: []string{"scale=1280:720"},
			wantAbsent:   []string{"yadif", "drawtext", "overlay", "movie="},
		},
		{
			name:       "yadif emitted when interlace=auto",
			hw:         domain.HWAccelNone,
			interlace:  domain.InterlaceAuto,
			wm:         nil,
			frameScale: 1.0,
			wantContains: []string{
				"yadif=mode=0:parity=-1:deint=0",
				"scale=1280:720",
			},
		},
		{
			name:      "yadif suppressed for explicit progressive",
			hw:        domain.HWAccelNone,
			interlace: domain.InterlaceProgressive,
			wm:        nil,
			wantAbsent: []string{
				"yadif",
			},
		},
		{
			name:      "yadif_cuda for NVENC + auto",
			hw:        domain.HWAccelNVENC,
			interlace: domain.InterlaceAuto,
			wm:        nil,
			wantContains: []string{
				"yadif_cuda=mode=0:parity=-1:deint=0",
				"scale_cuda=1280:720",
			},
		},
		{
			name: "text watermark appends drawtext on CPU",
			hw:   domain.HWAccelNone,
			wm: &domain.WatermarkConfig{
				Enabled:  true,
				Type:     domain.WatermarkTypeText,
				Text:     "LIVE",
				Position: domain.WatermarkTopLeft,
				OffsetX:  10,
				OffsetY:  10,
			},
			frameScale: 1.0,
			wantContains: []string{
				"scale=1280:720",
				"drawtext=text='LIVE'",
			},
			wantAbsent: []string{
				"hwdownload",
				"hwupload_cuda",
			},
		},
		{
			name: "text watermark wraps in hwdownload/hwupload_cuda on NVENC",
			hw:   domain.HWAccelNVENC,
			wm: &domain.WatermarkConfig{
				Enabled:  true,
				Type:     domain.WatermarkTypeText,
				Text:     "X",
				Position: domain.WatermarkTopLeft,
			},
			frameScale: 1.0,
			wantContains: []string{
				"scale_cuda=1280:720",
				"hwdownload",
				"format=nv12",
				"drawtext=text='X'",
				"hwupload_cuda",
			},
		},
		{
			name: "image watermark emits movie+overlay chain",
			hw:   domain.HWAccelNone,
			wm: &domain.WatermarkConfig{
				Enabled:   true,
				Type:      domain.WatermarkTypeImage,
				ImagePath: "/srv/wm/logo.png",
				Position:  domain.WatermarkTopRight,
				OffsetX:   20,
				OffsetY:   20,
			},
			frameScale: 1.0,
			wantContains: []string{
				"[mid]",
				"movie='/srv/wm/logo.png'",
				"format=rgba",
				"[wm]",
				"overlay=x=",
			},
		},
		{
			name: "image watermark with Resize shrinks the asset on smaller profile",
			hw:   domain.HWAccelNone,
			wm: &domain.WatermarkConfig{
				Enabled:   true,
				Type:      domain.WatermarkTypeImage,
				ImagePath: "/srv/wm/logo.png",
				Resize:    true,
				Position:  domain.WatermarkTopLeft,
			},
			frameScale: 0.5,
			wantContains: []string{
				"scale=iw*0.5000:ih*0.5000",
			},
		},
		{
			name: "disabled watermark falls through unchanged",
			hw:   domain.HWAccelNone,
			wm: &domain.WatermarkConfig{
				Enabled: false, Type: domain.WatermarkTypeText, Text: "x",
			},
			frameScale:   1.0,
			wantContains: []string{"scale=1280:720"},
			wantAbsent:   []string{"drawtext", "overlay", "movie="},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := filterChainSpec(tc.hw, prof, tc.interlace, tc.wm, tc.frameScale)
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("expected %q in filter chain, got: %s", want, got)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("did NOT expect %q in filter chain, got: %s", absent, got)
				}
			}
		})
	}
}

// TestFilterChainSpec_NullFallback verifies an empty filter set
// collapses to "null" (libavfilter rejects an empty graph string).
func TestFilterChainSpec_NullFallback(t *testing.T) {
	// Source-size profile (Width=0, Height=0) + progressive + no
	// watermark = the filter chain has nothing to emit. Expect "null".
	got := filterChainSpec(
		domain.HWAccelNone,
		domain.VideoProfile{},
		domain.InterlaceProgressive,
		nil,
		1.0,
	)
	if got != "null" {
		t.Errorf("expected pass-through null, got %q", got)
	}
}
