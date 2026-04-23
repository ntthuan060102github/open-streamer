package transcoder

import (
	"strings"
	"testing"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestBuildFFmpegArgs_CopyVideoAndAudio(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{Copy: true},
		Audio: domain.AudioTranscodeConfig{Copy: true},
	}
	args, err := buildFFmpegArgs([]Profile{{Bitrate: "1k"}}, tc)
	require.NoError(t, err)
	require.Contains(t, args, "-analyzeduration")
	require.Contains(t, args, "-probesize")
	require.Contains(t, args, "-c:v")
	require.Contains(t, args, "copy")
	require.Contains(t, args, "-c:a")
	require.Contains(t, args, "pipe:1")
}

func TestBuildFFmpegArgs_ScaleAndEncode(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{},
		Audio: domain.AudioTranscodeConfig{Codec: domain.AudioCodecAAC, Bitrate: 192},
		Global: domain.TranscoderGlobalConfig{
			HW:  domain.HWAccelNone,
			GOP: 60,
		},
	}
	p := []Profile{{
		Width: 1280, Height: 720, Bitrate: "2000k",
		Codec: "h264", Preset: "veryfast",
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	vf := vfFilterValue(t, args)
	require.Contains(t, vf, "scale=1280:720:")
	require.Contains(t, args, "libx264")
	require.Contains(t, args, "-g")
	require.Contains(t, args, "60")
	require.Contains(t, args, "-b:a")
	require.Contains(t, args, "192k")
}

func TestBuildFFmpegArgs_NVENCFromHW(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	p := []Profile{{Width: 0, Height: 0, Bitrate: "3000k", Codec: "h264", Preset: "p4"}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.Contains(t, args, "h264_nvenc")
}

func TestBuildFFmpegArgs_ExtraArgs(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:     domain.VideoTranscodeConfig{Copy: true},
		Audio:     domain.AudioTranscodeConfig{Copy: true},
		ExtraArgs: []string{"-muxdelay", "0"},
	}
	args, err := buildFFmpegArgs([]Profile{{Bitrate: "1k"}}, tc)
	require.NoError(t, err)
	idx := indexOf(args, "-muxdelay")
	require.Greater(t, idx, 0)
	require.Equal(t, "0", args[idx+1])
}

func TestBuildFFmpegArgs_MaxBitrateAndFramerate(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{},
		Audio: domain.AudioTranscodeConfig{Copy: true},
	}
	p := []Profile{{
		Width: 1280, Height: 720, Bitrate: "3000k",
		Codec: "h264", Preset: "fast",
		MaxBitrate: 4000, Framerate: 30,
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.Contains(t, args, "-maxrate")
	require.Contains(t, args, "4000k")
	require.Contains(t, args, "-bufsize")
	require.Contains(t, args, "8000k")
	require.Contains(t, args, "-r")
	require.Contains(t, args, "30.000")
}

func TestBuildFFmpegArgs_CodecProfileAndLevel(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{},
		Audio: domain.AudioTranscodeConfig{Copy: true},
	}
	p := []Profile{{
		Bitrate: "2000k", Codec: "h264", Preset: "fast",
		CodecProfile: "high", CodecLevel: "4.1",
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.Contains(t, args, "-profile:v")
	require.Contains(t, args, "high")
	require.Contains(t, args, "-level")
	require.Contains(t, args, "4.1")
}

func TestBuildFFmpegArgs_NoProfiles(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{}
	_, err := buildFFmpegArgs(nil, tc)
	require.Error(t, err)
}

func TestBuildFFmpegArgs_NilTranscoderConfig(t *testing.T) {
	t.Parallel()
	args, err := buildFFmpegArgs([]Profile{{Bitrate: "1k", Codec: "h264"}}, nil)
	require.NoError(t, err)
	require.Contains(t, args, "pipe:1")
}

func TestBuildScaleFilter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		w, h    int
		hw      domain.HWAccel
		enc     string
		wantPfx string
		wantNil bool
	}{
		{"zero dims → empty", 0, 0, domain.HWAccelNone, "libx264", "", true},
		{"CPU both dims (default pad)", 1280, 720, domain.HWAccelNone, "libx264", "scale=1280:720:", false},
		{"CPU width only", 1280, 0, domain.HWAccelNone, "libx264", "scale=1280:-2", false},
		{"CPU height only", 0, 720, domain.HWAccelNone, "libx264", "scale=-2:720", false},
		{"NVENC both dims → scale_cuda + pad_cuda", 1280, 720, domain.HWAccelNVENC, "h264_nvenc", "scale_cuda=1280:720:", false},
		{"NVENC + CPU encoder mismatch → CPU scale", 1280, 720, domain.HWAccelNVENC, "libx264", "scale=1280:720:", false},
		{"VAAPI both dims → scale_vaapi", 1920, 1080, domain.HWAccelVAAPI, "h264_vaapi", "scale_vaapi=1920:1080:", false},
		{"QSV both dims → scale_qsv", 1280, 720, domain.HWAccelQSV, "h264_qsv", "scale_qsv=1280:720:", false},
		{"VideoToolbox falls back to CPU scale", 1280, 720, domain.HWAccelVideoToolbox, "h264_videotoolbox", "scale=1280:720:", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildScaleFilter(tt.w, tt.h, tt.hw, tt.enc)
			if tt.wantNil {
				require.Empty(t, got)
			} else {
				require.Contains(t, got, tt.wantPfx)
			}
		})
	}
}

func TestResizeFilter_Modes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		w, h     int
		mode     string
		hw       domain.HWAccel
		enc      string
		wantSubs []string
		wantNot  []string
	}{
		{
			name: "CPU pad (default)",
			w:    1280, h: 720, mode: "", hw: domain.HWAccelNone, enc: "libx264",
			wantSubs: []string{"scale=1280:720:", "force_original_aspect_ratio=decrease", "pad="},
		},
		{
			name: "CPU pad explicit",
			w:    1280, h: 720, mode: "pad", hw: domain.HWAccelNone, enc: "libx264",
			wantSubs: []string{"scale=1280:720:", "pad="},
		},
		{
			name: "CPU stretch",
			w:    1280, h: 720, mode: "stretch", hw: domain.HWAccelNone, enc: "libx264",
			wantSubs: []string{"scale=1280:720"},
			wantNot:  []string{"force_original_aspect_ratio", "pad="},
		},
		{
			name: "CPU fit (no pad)",
			w:    1280, h: 720, mode: "fit", hw: domain.HWAccelNone, enc: "libx264",
			wantSubs: []string{"scale=1280:720:", "force_original_aspect_ratio=decrease", "force_divisible_by=2"},
			wantNot:  []string{"pad="},
		},
		{
			name: "CPU crop",
			w:    1280, h: 720, mode: "crop", hw: domain.HWAccelNone, enc: "libx264",
			wantSubs: []string{"scale=1280:720:force_original_aspect_ratio=increase", "crop=1280:720"},
			wantNot:  []string{"pad="},
		},
		{
			name: "GPU NVENC pad → scale_cuda + pad_cuda (named params required)",
			w:    1920, h: 1080, mode: "pad", hw: domain.HWAccelNVENC, enc: "h264_nvenc",
			// pad_cuda has no shorthand list, so positional `w:h:x:y` parses as
			// one option name and FFmpeg fails with "No option name near ...".
			wantSubs: []string{
				"scale_cuda=1920:1080:",
				"pad_cuda=w=1920:h=1080:x=(1920-iw)/2:y=(1080-ih)/2",
			},
		},
		{
			name: "GPU NVENC stretch",
			w:    1280, h: 720, mode: "stretch", hw: domain.HWAccelNVENC, enc: "h264_nvenc",
			wantSubs: []string{"scale_cuda=1280:720"},
			wantNot:  []string{"force_original_aspect_ratio", "pad_cuda"},
		},
		{
			name: "GPU NVENC fit",
			w:    1280, h: 720, mode: "fit", hw: domain.HWAccelNVENC, enc: "h264_nvenc",
			wantSubs: []string{"scale_cuda=1280:720:force_original_aspect_ratio=decrease:force_divisible_by=2"},
			wantNot:  []string{"pad_cuda"},
		},
		{
			name: "GPU NVENC crop → CPU round-trip",
			w:    1280, h: 720, mode: "crop", hw: domain.HWAccelNVENC, enc: "h264_nvenc",
			wantSubs: []string{"hwdownload", "format=nv12", "crop=1280:720", "hwupload_cuda"},
		},
		{
			name: "Unknown mode falls back to pad",
			w:    1280, h: 720, mode: "weird", hw: domain.HWAccelNone, enc: "libx264",
			wantSubs: []string{"scale=1280:720:", "pad="},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := resizeFilter(tt.w, tt.h, tt.mode, tt.hw, tt.enc)
			for _, s := range tt.wantSubs {
				require.Contains(t, got, s, "filter=%q", got)
			}
			for _, s := range tt.wantNot {
				require.NotContains(t, got, s, "filter=%q", got)
			}
		})
	}
}

func TestDeinterlaceFilter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mode domain.InterlaceMode
		gpu  bool
		want string
	}{
		{"unset → no filter", "", false, ""},
		{"unset on GPU → no filter", "", true, ""},
		{"progressive → skip", domain.InterlaceProgressive, false, ""},
		{"progressive on GPU → skip", domain.InterlaceProgressive, true, ""},
		{"auto CPU", domain.InterlaceAuto, false, "yadif=mode=0:parity=-1:deint=0"},
		{"auto GPU", domain.InterlaceAuto, true, "yadif_cuda=mode=0:parity=-1:deint=0"},
		{"tff CPU", domain.InterlaceTopField, false, "yadif=mode=0:parity=0:deint=0"},
		{"bff CPU", domain.InterlaceBottomField, false, "yadif=mode=0:parity=1:deint=0"},
		{"tff GPU", domain.InterlaceTopField, true, "yadif_cuda=mode=0:parity=0:deint=0"},
		{"bff GPU", domain.InterlaceBottomField, true, "yadif_cuda=mode=0:parity=1:deint=0"},
		{"unknown mode → no filter", "garbage", false, ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, deinterlaceFilter(tt.mode, tt.gpu))
		})
	}
}

func TestBframesArgs(t *testing.T) {
	t.Parallel()
	bf := func(n int) *int { return &n }
	cases := []struct {
		name string
		bf   *int
		enc  string
		want []string
	}{
		{"nil → no flag (encoder default)", nil, "h264_nvenc", nil},
		{"explicit zero on NVENC → -bf 0 only", bf(0), "h264_nvenc", []string{"-bf", "0"}},
		{"3 on NVENC → -bf 3 + b_ref_mode middle", bf(3), "h264_nvenc", []string{"-bf", "3", "-b_ref_mode", "middle"}},
		{"3 on libx264 → -bf only (no b_ref_mode)", bf(3), "libx264", []string{"-bf", "3"}},
		{"2 on hevc_nvenc → b_ref_mode set", bf(2), "hevc_nvenc", []string{"-bf", "2", "-b_ref_mode", "middle"}},
		{"negative clamped to 0", bf(-5), "libx264", []string{"-bf", "0"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, bframesArgs(tt.bf, tt.enc))
		})
	}
}

func TestBuildVideoFilter_ChainOrder(t *testing.T) {
	t.Parallel()
	// Pipeline order matters: deinterlace must run before scale (interlaced
	// frames scaled directly produce comb artifacts), and setsar is metadata
	// that goes last so it stamps the final output frame.
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{
			Interlace: domain.InterlaceAuto,
		},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNone},
	}
	p := Profile{Width: 1280, Height: 720, SAR: "1:1", ResizeMode: "pad"}
	got := buildVideoFilter(p, tc, "libx264")
	yadifAt := strings.Index(got, "yadif")
	scaleAt := strings.Index(got, "scale=")
	sarAt := strings.Index(got, "setsar=1:1")
	require.Greater(t, yadifAt, -1, "vf=%q", got)
	require.Greater(t, scaleAt, -1, "vf=%q", got)
	require.Greater(t, sarAt, -1, "vf=%q", got)
	require.Less(t, yadifAt, scaleAt, "deinterlace must precede scale: vf=%q", got)
	require.Less(t, scaleAt, sarAt, "scale must precede setsar: vf=%q", got)
}

func TestBuildVideoFilter_Empty(t *testing.T) {
	t.Parallel()
	// No interlace, no dims, no SAR → no -vf at all.
	tc := &domain.TranscoderConfig{}
	got := buildVideoFilter(Profile{}, tc, "libx264")
	require.Empty(t, got)
}

func TestBuildVideoFilter_GPUKeepsCudaSuffixes(t *testing.T) {
	t.Parallel()
	// On NVENC pipeline both deinterlace and scale must be _cuda variants;
	// mixing yadif (CPU) with scale_cuda would force hwdownload and lose the
	// VRAM-resident pipeline.
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{Interlace: domain.InterlaceTopField},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	p := Profile{Width: 1920, Height: 1080, ResizeMode: "fit"}
	got := buildVideoFilter(p, tc, "h264_nvenc")
	require.Contains(t, got, "yadif_cuda")
	require.Contains(t, got, "scale_cuda")
	require.NotContains(t, got, "yadif=")
	require.NotContains(t, got, "scale=1920")
}

func TestBuildFFmpegArgs_BframesAndRefs(t *testing.T) {
	t.Parallel()
	bf := 0
	refs := 4
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{},
		Audio: domain.AudioTranscodeConfig{Copy: true},
	}
	p := []Profile{{
		Bitrate: "2000k", Codec: "h264", Preset: "fast",
		Bframes: &bf, Refs: &refs,
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	bfIdx := indexOf(args, "-bf")
	require.Greater(t, bfIdx, -1)
	require.Equal(t, "0", args[bfIdx+1])
	refsIdx := indexOf(args, "-refs")
	require.Greater(t, refsIdx, -1)
	require.Equal(t, "4", args[refsIdx+1])
	// libx264 → no b_ref_mode.
	require.Equal(t, -1, indexOf(args, "-b_ref_mode"))
}

func TestBuildFFmpegArgs_BframesOnNVENCEnablesBRefMode(t *testing.T) {
	t.Parallel()
	bf := 3
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	p := []Profile{{
		Bitrate: "3000k", Codec: "h264", Preset: "p4",
		Bframes: &bf,
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.Contains(t, args, "h264_nvenc")
	bfIdx := indexOf(args, "-bf")
	require.Greater(t, bfIdx, -1)
	require.Equal(t, "3", args[bfIdx+1])
	bRefIdx := indexOf(args, "-b_ref_mode")
	require.Greater(t, bRefIdx, -1)
	require.Equal(t, "middle", args[bRefIdx+1])
}

func TestBuildFFmpegArgs_SARAppendedAfterScale(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{},
		Audio: domain.AudioTranscodeConfig{Copy: true},
	}
	p := []Profile{{
		Width: 1280, Height: 720, Bitrate: "2000k", Codec: "h264", Preset: "fast",
		SAR: "1:1",
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	vf := vfFilterValue(t, args)
	require.Contains(t, vf, "setsar=1:1")
	scaleAt := strings.Index(vf, "scale=")
	sarAt := strings.Index(vf, "setsar=")
	require.Less(t, scaleAt, sarAt, "scale must precede setsar")
}

func TestBuildFFmpegArgs_InterlaceCPUPipeline(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{Interlace: domain.InterlaceTopField},
		Audio: domain.AudioTranscodeConfig{Copy: true},
	}
	p := []Profile{{
		Width: 1280, Height: 720, Bitrate: "2000k", Codec: "h264", Preset: "fast",
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	vf := vfFilterValue(t, args)
	require.Contains(t, vf, "yadif=mode=0:parity=0:deint=0")
	yadifAt := strings.Index(vf, "yadif")
	scaleAt := strings.Index(vf, "scale=")
	require.Less(t, yadifAt, scaleAt, "deinterlace must run before scale")
}

func TestBuildFFmpegArgs_ResizeModeStretch(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{},
		Audio: domain.AudioTranscodeConfig{Copy: true},
	}
	p := []Profile{{
		Width: 1280, Height: 720, Bitrate: "2000k", Codec: "h264", Preset: "fast",
		ResizeMode: string(domain.ResizeModeStretch),
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	vf := vfFilterValue(t, args)
	require.Equal(t, "scale=1280:720", vf)
}

func TestNormalizeVideoEncoder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		codec string
		hw    domain.HWAccel
		want  string
	}{
		{"", domain.HWAccelNone, "libx264"},
		{"h264", domain.HWAccelNone, "libx264"},
		{"avc", domain.HWAccelNone, "libx264"},
		{"h264", domain.HWAccelNVENC, "h264_nvenc"},
		{"h265", domain.HWAccelNone, "libx265"},
		{"hevc", domain.HWAccelNVENC, "hevc_nvenc"},
		{"vp9", domain.HWAccelNone, "libvpx-vp9"},
		{"av1", domain.HWAccelNone, "libsvtav1"},
		{"h264_nvenc", domain.HWAccelNone, "h264_nvenc"}, // passthrough
	}
	for _, tt := range cases {
		got := normalizeVideoEncoder(tt.codec, tt.hw)
		require.Equal(t, tt.want, got, "codec=%q hw=%v", tt.codec, tt.hw)
	}
}

func TestGopFrames(t *testing.T) {
	t.Parallel()
	// Global GOP takes precedence.
	tc := &domain.TranscoderConfig{Global: domain.TranscoderGlobalConfig{GOP: 50}}
	require.Equal(t, 50, gopFrames(tc, Profile{}))

	// KeyframeInterval × fps.
	tc2 := &domain.TranscoderConfig{}
	p := Profile{KeyframeInterval: 2, Framerate: 25}
	require.Equal(t, 50, gopFrames(tc2, p))

	// KeyframeInterval with global fps fallback.
	tc3 := &domain.TranscoderConfig{Global: domain.TranscoderGlobalConfig{FPS: 30}}
	p3 := Profile{KeyframeInterval: 2}
	require.Equal(t, 60, gopFrames(tc3, p3))

	// No info → 0 (encoder default).
	tc4 := &domain.TranscoderConfig{}
	require.Equal(t, 0, gopFrames(tc4, Profile{}))
}

func TestAudioEncodeArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cfg     domain.AudioTranscodeConfig
		wantEnc string
		wantBr  string
	}{
		{domain.AudioTranscodeConfig{Codec: domain.AudioCodecAAC, Bitrate: 192}, "aac", "192k"},
		{domain.AudioTranscodeConfig{Codec: "mp3", Bitrate: 128}, "libmp3lame", "128k"},
		{domain.AudioTranscodeConfig{Codec: "opus", Bitrate: 96}, "libopus", "96k"},
		{domain.AudioTranscodeConfig{Codec: "ac3", Bitrate: 384}, "ac3", "384k"},
		{domain.AudioTranscodeConfig{}, "aac", "128k"}, // defaults
	}
	for _, tt := range cases {
		tc := &domain.TranscoderConfig{Audio: tt.cfg}
		args := audioEncodeArgs(tc)
		require.Contains(t, args, tt.wantEnc)
		require.Contains(t, args, tt.wantBr)
	}
}

func TestAudioEncodeArgs_SampleRateAndChannels(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Audio: domain.AudioTranscodeConfig{
			Codec:      domain.AudioCodecAAC,
			Bitrate:    128,
			SampleRate: 48000,
			Channels:   2,
		},
	}
	args := audioEncodeArgs(tc)
	require.Contains(t, args, "-ar")
	require.Contains(t, args, "48000")
	require.Contains(t, args, "-ac")
	require.Contains(t, args, "2")
}

func TestAudioEncodeArgs_Normalize(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Audio: domain.AudioTranscodeConfig{Codec: domain.AudioCodecAAC, Normalize: true},
	}
	args := audioEncodeArgs(tc)
	require.Contains(t, args, "-af")
	normalize := args[indexOf(args, "-af")+1]
	require.Contains(t, normalize, "loudnorm")
}

func TestFormatFFmpegCmd_QuotesShellMetachars(t *testing.T) {
	t.Parallel()
	got := formatFFmpegCmd("/usr/bin/ffmpeg", []string{
		"-vf", "scale=1280:720,pad=ceil(iw/2)*2:ceil(ih/2)*2:(ow-iw)/2:(oh-ih)/2",
		"-c:v", "libx264",
		"-bf", "0",
		"--empty=", "",
	})
	// Plain tokens stay unquoted; tokens with parens/* must be single-quoted;
	// empty arg becomes ''.
	require.Contains(t, got, "/usr/bin/ffmpeg -vf 'scale=1280:720,pad=ceil(iw/2)*2:ceil(ih/2)*2:(ow-iw)/2:(oh-ih)/2'")
	require.Contains(t, got, "-c:v libx264")
	require.Contains(t, got, "-bf 0")
	require.Contains(t, got, "--empty= ''")
}

func TestFormatFFmpegCmd_EscapesSingleQuote(t *testing.T) {
	t.Parallel()
	got := formatFFmpegCmd("ffmpeg", []string{"-metadata", "title=Bob's Show"})
	// Embedded ' becomes '\'' (close, escaped, reopen) — the canonical
	// POSIX-shell idiom that survives copy-paste.
	require.Contains(t, got, `'title=Bob'\''s Show'`)
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func vfFilterValue(t *testing.T, args []string) string {
	t.Helper()
	i := indexOf(args, "-vf")
	require.Greater(t, i, -1, "missing -vf")
	require.Less(t, i+1, len(args), "missing -vf value")
	return args[i+1]
}
