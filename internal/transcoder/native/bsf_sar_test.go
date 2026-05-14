package native

import "testing"

// TestBSFNameForEncoder verifies the encoder-name → metadata-BSF lookup
// that drives whether a chain wires a h264_metadata / hevc_metadata
// bitstream filter. The matcher is substring-based so vendor variants
// (h264_nvenc, h264_qsv, libx264, …) all route to the same BSF.
func TestBSFNameForEncoder(t *testing.T) {
	tests := []struct {
		name    string
		encoder string
		want    string
	}{
		// H.264 — every flavour resolves to h264_metadata.
		{"libx264", "libx264", "h264_metadata"},
		{"h264_nvenc", "h264_nvenc", "h264_metadata"},
		{"h264_qsv", "h264_qsv", "h264_metadata"},
		{"h264_vaapi", "h264_vaapi", "h264_metadata"},
		{"h264_videotoolbox", "h264_videotoolbox", "h264_metadata"},
		// H.265 — substring "hevc" and "265" both hit hevc_metadata.
		{"libx265", "libx265", "hevc_metadata"},
		{"hevc_nvenc", "hevc_nvenc", "hevc_metadata"},
		{"hevc_qsv", "hevc_qsv", "hevc_metadata"},
		// AV1 / MPEG-2 have no metadata BSF — caller falls back to
		// no SAR stamping, matches CLI backend behaviour.
		{"libsvtav1", "libsvtav1", ""},
		{"mpeg2video", "mpeg2video", ""},
		// Empty / unknown — bail out.
		{"empty", "", ""},
		{"copy", "copy", ""},
		// Mixed-case input — matcher is case-insensitive so operator
		// typos like "H264_NVENC" still resolve correctly.
		{"upper-case h264_nvenc", "H264_NVENC", "h264_metadata"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := bsfNameForEncoder(tc.encoder); got != tc.want {
				t.Errorf("bsfNameForEncoder(%q) = %q, want %q", tc.encoder, got, tc.want)
			}
		})
	}
}

// TestAllocSARBitstreamFilter_SkipPaths verifies that the alloc helper
// gracefully no-ops on inputs that shouldn't produce a BSF — covering
// the fast-path returns before any C allocation happens.
func TestAllocSARBitstreamFilter_SkipPaths(t *testing.T) {
	tests := []struct {
		name        string
		sar         string
		encoderName string
	}{
		{"empty SAR skips", "", "h264_nvenc"},
		{"whitespace-only SAR skips", "   ", "libx264"},
		{"unsupported encoder skips", "1:1", "libsvtav1"},
		{"empty encoder skips", "1:1", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// encoderCtx=nil is fine here — the skip paths return
			// before any field on it is touched.
			bsfCtx, err := allocSARBitstreamFilter(tc.sar, nil, tc.encoderName)
			if err != nil {
				t.Fatalf("expected nil error on skip path, got %v", err)
			}
			if bsfCtx != nil {
				t.Errorf("expected nil bsfCtx on skip path, got %v", bsfCtx)
			}
		})
	}
}
