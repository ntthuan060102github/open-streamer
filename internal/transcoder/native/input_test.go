package native

import (
	"testing"

	"github.com/asticode/go-astiav"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/stretchr/testify/assert"
)

// decoderNameForHW pins the HW→decoder dispatch table. New backends or
// codec IDs that need vendor decoders are caught here when the test
// matrix is updated alongside the helper.
func TestDecoderNameForHW(t *testing.T) {
	t.Parallel()
	cases := []struct {
		hw     domain.HWAccel
		codec  astiav.CodecID
		expect string
	}{
		// NVENC: NVDEC vendor decoders.
		{domain.HWAccelNVENC, astiav.CodecIDH264, "h264_cuvid"},
		{domain.HWAccelNVENC, astiav.CodecIDHevc, "hevc_cuvid"},
		// QSV: Intel Quick Sync decoders.
		{domain.HWAccelQSV, astiav.CodecIDH264, "h264_qsv"},
		{domain.HWAccelQSV, astiav.CodecIDHevc, "hevc_qsv"},
		// VideoToolbox: Apple framework decoders.
		{domain.HWAccelVideoToolbox, astiav.CodecIDH264, "h264_videotoolbox"},
		{domain.HWAccelVideoToolbox, astiav.CodecIDHevc, "hevc_videotoolbox"},
		// VAAPI: no vendor-named decoder — libavcodec default handles
		// surface upload when hwDevice is attached.
		{domain.HWAccelVAAPI, astiav.CodecIDH264, ""},
		{domain.HWAccelVAAPI, astiav.CodecIDHevc, ""},
		// HWAccelNone: software path.
		{domain.HWAccelNone, astiav.CodecIDH264, ""},
		// Unknown codec id on any backend → fall through to default.
		{domain.HWAccelNVENC, astiav.CodecIDAv1, ""},
		{domain.HWAccelNVENC, astiav.CodecIDVp9, ""},
	}
	for _, tc := range cases {
		t.Run(string(tc.hw)+"/"+tc.codec.String(), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.expect, decoderNameForHW(tc.hw, tc.codec))
		})
	}
}
