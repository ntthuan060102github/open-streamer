package transcoder

import (
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
		{"CPU both dims", 1280, 720, domain.HWAccelNone, "libx264", "scale=1280:720:", false},
		{"CPU width only", 1280, 0, domain.HWAccelNone, "libx264", "scale=1280:-2:", false},
		{"CPU height only", 0, 720, domain.HWAccelNone, "libx264", "scale=-2:720:", false},
		{"NVENC both dims → scale_cuda", 1280, 720, domain.HWAccelNVENC, "h264_nvenc", "scale_cuda=1280:720:", false},
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
