package transcoder

import (
	"testing"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
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
