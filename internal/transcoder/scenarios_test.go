package transcoder

import (
	"strings"
	"testing"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/stretchr/testify/require"
)

// End-to-end scenarios pairing every supported transcode mode (video copy /
// re-encode × audio copy / re-encode) with the FFmpeg argument vector that
// must result. Each scenario asserts the exact `-flag value` adjacency that
// FFmpeg requires, not just argument presence — order-insensitive Contains
// would let bugs like "wrong value paired with the right flag" slip through.

// argAfter returns the value following the first occurrence of flag, or ""
// if flag is missing or has no value. Use this to assert "-c:v h264_nvenc"
// rather than just "h264_nvenc appears somewhere".
func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// allArgsAfter returns every value that appears after `flag` (FFmpeg may
// repeat -map, -c:v copy + -c:v libx264 in ladder builds, etc.).
func allArgsAfter(args []string, flag string) []string {
	out := []string{}
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// ── Mode 1: video.copy=true + audio.copy=true (both passthrough) ────────────
// In production the coordinator gates this and never spawns FFmpeg, but the
// transcoder must still produce a valid, FFmpeg-runnable arg vector — used by
// tooling that probes the command without invoking it.
func TestScenario_BothCopy_EmitsCopyForBothStreams(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{Copy: true},
		Audio: domain.AudioTranscodeConfig{Copy: true},
	}
	args, err := buildFFmpegArgs([]Profile{{Bitrate: "2500k"}}, tc)
	require.NoError(t, err)

	cv := allArgsAfter(args, "-c:v")
	require.Equal(t, []string{"copy"}, cv, "video must be copy passthrough")
	ca := allArgsAfter(args, "-c:a")
	require.Equal(t, []string{"copy"}, ca, "audio must be copy passthrough")

	// No encoder-only args should leak in for copy mode.
	require.Equal(t, "", argAfter(args, "-vf"), "no scale filter for copy")
	require.Equal(t, "", argAfter(args, "-preset"), "no preset for copy")
	require.Equal(t, "", argAfter(args, "-b:v"), "no video bitrate for copy")
	require.Equal(t, "", argAfter(args, "-b:a"), "no audio bitrate for copy")
}

// ── Mode 2: video.copy=true + audio re-encode ───────────────────────────────

func TestScenario_VideoCopy_AudioReencodeAAC(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{Copy: true},
		Audio: domain.AudioTranscodeConfig{
			Codec: domain.AudioCodecAAC, Bitrate: 192,
			SampleRate: 48000, Channels: 2,
		},
	}
	args, err := buildFFmpegArgs([]Profile{{Bitrate: "ignored"}}, tc)
	require.NoError(t, err)

	require.Equal(t, "copy", argAfter(args, "-c:v"))
	require.Equal(t, "aac", argAfter(args, "-c:a"))
	require.Equal(t, "192k", argAfter(args, "-b:a"))
	require.Equal(t, "48000", argAfter(args, "-ar"))
	require.Equal(t, "2", argAfter(args, "-ac"))
}

func TestScenario_VideoCopy_AudioReencodeOpus(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{Copy: true},
		Audio: domain.AudioTranscodeConfig{Codec: domain.AudioCodecOpus, Bitrate: 96},
	}
	args, err := buildFFmpegArgs([]Profile{{Bitrate: "ignored"}}, tc)
	require.NoError(t, err)
	require.Equal(t, "copy", argAfter(args, "-c:v"))
	require.Equal(t, "libopus", argAfter(args, "-c:a"))
	require.Equal(t, "96k", argAfter(args, "-b:a"))
}

func TestScenario_VideoCopy_AudioReencodeMP3(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{Copy: true},
		Audio: domain.AudioTranscodeConfig{Codec: domain.AudioCodecMP3, Bitrate: 128},
	}
	args, err := buildFFmpegArgs([]Profile{{Bitrate: "ignored"}}, tc)
	require.NoError(t, err)
	require.Equal(t, "libmp3lame", argAfter(args, "-c:a"))
	require.Equal(t, "128k", argAfter(args, "-b:a"))
}

func TestScenario_VideoCopy_AudioReencodeAC3(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{Copy: true},
		Audio: domain.AudioTranscodeConfig{Codec: domain.AudioCodecAC3, Bitrate: 384},
	}
	args, err := buildFFmpegArgs([]Profile{{Bitrate: "ignored"}}, tc)
	require.NoError(t, err)
	require.Equal(t, "ac3", argAfter(args, "-c:a"))
	require.Equal(t, "384k", argAfter(args, "-b:a"))
}

func TestScenario_VideoCopy_AudioReencodeDefaults(t *testing.T) {
	t.Parallel()
	// No codec, no bitrate set → defaults to aac@128k.
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{Copy: true},
		Audio: domain.AudioTranscodeConfig{}, // copy=false, codec=""
	}
	args, err := buildFFmpegArgs([]Profile{{Bitrate: "ignored"}}, tc)
	require.NoError(t, err)
	require.Equal(t, "copy", argAfter(args, "-c:v"))
	require.Equal(t, "aac", argAfter(args, "-c:a"))
	require.Equal(t, "128k", argAfter(args, "-b:a"))
}

// ── Mode 3: video re-encode + audio.copy=true ───────────────────────────────

// Regression for Bug #2: empty codec + HW=none must route to libx264 (not
// remain literally "" which would yield "ffmpeg ... -c:v -preset ..." — broken).
func TestScenario_VideoReencodeNoHW_AudioCopy_EmptyCodecRoutesLibx264(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNone},
	}
	p := []Profile{{Width: 1280, Height: 720, Bitrate: "2500k"}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)

	require.Equal(t, "libx264", argAfter(args, "-c:v"),
		"empty codec + hw=none must route to libx264")
	require.Equal(t, "copy", argAfter(args, "-c:a"))
	require.Equal(t, "2500k", argAfter(args, "-b:v"))
	// preset omitted → must NOT appear at all (libx264 picks its own default).
	require.NotContains(t, args, "-preset",
		"omitted preset must not be defaulted on by buildFFmpegArgs")
	vf := argAfter(args, "-vf")
	require.Contains(t, vf, "scale=1280:720:")
}

// Regression for Bug #2 (the original incident): empty codec + HW=nvenc must
// route to h264_nvenc. Previously coordinator hardcoded "libx264" before this
// reached buildFFmpegArgs; that hardcode is now removed and this is the unit
// boundary that proves HW routing works.
func TestScenario_VideoReencodeNVENC_AudioCopy_EmptyCodecRoutesH264NVENC(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	p := []Profile{{Width: 1920, Height: 1080, Bitrate: "4500k"}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)

	require.Equal(t, "h264_nvenc", argAfter(args, "-c:v"),
		"empty codec + hw=nvenc must route to h264_nvenc — Bug #2 regression")
	require.Equal(t, "copy", argAfter(args, "-c:a"))
	require.Equal(t, "4500k", argAfter(args, "-b:v"))
	require.NotContains(t, args, "libx264",
		"libx264 must NOT appear anywhere when hw=nvenc")
}

// HEVC variant of the same regression.
func TestScenario_VideoReencodeNVENC_HEVC_RoutesHEVCNVENC(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	p := []Profile{{
		Width: 1920, Height: 1080, Bitrate: "3500k",
		Codec: "h265", Preset: "p5",
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.Equal(t, "hevc_nvenc", argAfter(args, "-c:v"))
	require.Equal(t, "p5", argAfter(args, "-preset"))
	require.NotContains(t, args, "libx265")
}

// Explicit codec must not be rewritten by HW routing.
func TestScenario_VideoReencodeNoHW_AudioCopy_ExplicitH264Preserved(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNone},
	}
	p := []Profile{{
		Width: 1280, Height: 720, Bitrate: "2500k",
		Codec: "h264", Preset: "veryfast",
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.Equal(t, "libx264", argAfter(args, "-c:v"))
	require.Equal(t, "veryfast", argAfter(args, "-preset"))
}

// codec="h264_nvenc" passthrough — even without hw=nvenc, an explicitly named
// nvenc encoder is preserved verbatim (operator override).
func TestScenario_ExplicitNVENCEncoder_PreservedRegardlessOfHW(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNone}, // mismatch
	}
	p := []Profile{{
		Width: 1280, Height: 720, Bitrate: "2500k",
		Codec: "h264_nvenc", Preset: "p4",
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.Equal(t, "h264_nvenc", argAfter(args, "-c:v"),
		"explicit nvenc encoder must pass through even when global HW=none")
	require.Equal(t, "p4", argAfter(args, "-preset"))
}

// ── Mode 4: video re-encode + audio re-encode (full transcode) ──────────────

func TestScenario_FullReencodeNVENC(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{},
		Audio: domain.AudioTranscodeConfig{
			Codec: domain.AudioCodecAAC, Bitrate: 128,
			SampleRate: 44100, Channels: 2,
		},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC, GOP: 60},
	}
	p := []Profile{{
		Width: 1920, Height: 1080, Bitrate: "5000k",
		MaxBitrate: 7500, Framerate: 30,
		Preset: "p5", CodecProfile: "high", CodecLevel: "4.1",
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)

	require.Equal(t, "h264_nvenc", argAfter(args, "-c:v"))
	require.Equal(t, "p5", argAfter(args, "-preset"))
	require.Equal(t, "high", argAfter(args, "-profile:v"))
	require.Equal(t, "4.1", argAfter(args, "-level"))
	require.Equal(t, "5000k", argAfter(args, "-b:v"))
	require.Equal(t, "7500k", argAfter(args, "-maxrate"))
	require.Equal(t, "15000k", argAfter(args, "-bufsize"),
		"bufsize must be 2× maxrate")
	require.Equal(t, "60", argAfter(args, "-g"))
	require.Equal(t, "30", argAfter(args, "-keyint_min"),
		"keyint_min must be GOP/2")
	require.Equal(t, "30.000", argAfter(args, "-r"))

	require.Equal(t, "aac", argAfter(args, "-c:a"))
	require.Equal(t, "128k", argAfter(args, "-b:a"))
	require.Equal(t, "44100", argAfter(args, "-ar"))
	require.Equal(t, "2", argAfter(args, "-ac"))
}

func TestScenario_FullReencodeNoHW_DefaultsBothSides(t *testing.T) {
	t.Parallel()
	// Empty codec on both sides + HW=none → libx264 + aac@128k defaults.
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNone},
	}
	p := []Profile{{Width: 1280, Height: 720, Bitrate: "2500k"}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.Equal(t, "libx264", argAfter(args, "-c:v"))
	require.Equal(t, "2500k", argAfter(args, "-b:v"))
	require.Equal(t, "aac", argAfter(args, "-c:a"))
	require.Equal(t, "128k", argAfter(args, "-b:a"))
}

// ── Edge cases ──────────────────────────────────────────────────────────────

// codec="copy" at profile level when video.copy=false is meaningless. The
// coordinator strips it to "" before calling here; this test pins the
// transcoder behavior if a caller bypasses that normalization.
func TestScenario_ProfileCodecCopyWithVideoCopyFalse(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{}, // copy=false
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNone},
	}
	// "copy" is not a real encoder name — normalizeVideoEncoder hits the
	// fallthrough and returns "libx264" rather than emitting "copy" alongside
	// preset/bitrate args (which FFmpeg would reject).
	p := []Profile{{Width: 1280, Height: 720, Bitrate: "2500k", Codec: "copy"}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	cv := argAfter(args, "-c:v")
	require.NotEqual(t, "copy", cv,
		"video.copy=false must never produce -c:v copy via profile codec=copy")
	require.Equal(t, "libx264", cv)
}

// Multi-profile ABR: buildFFmpegArgs uses only profiles[0] (one FFmpeg per
// rendition). This pins that contract — adding ladder support later must
// either change this test or keep this single-output behavior intact.
func TestScenario_MultipleProfiles_OnlyFirstUsed(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	p := []Profile{
		{Width: 1920, Height: 1080, Bitrate: "4500k", Codec: "h264", Preset: "p5"},
		{Width: 1280, Height: 720, Bitrate: "2500k", Codec: "h264", Preset: "p4"},
		{Width: 854, Height: 480, Bitrate: "1200k", Codec: "h264", Preset: "p3"},
	}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)

	require.Equal(t, "4500k", argAfter(args, "-b:v"),
		"only profiles[0].Bitrate should appear")
	require.Equal(t, "p5", argAfter(args, "-preset"))
	vf := argAfter(args, "-vf")
	require.Contains(t, vf, "scale=1920:1080:")

	joined := strings.Join(args, " ")
	require.NotContains(t, joined, "2500k", "profiles[1] bitrate must not appear")
	require.NotContains(t, joined, "1200k", "profiles[2] bitrate must not appear")
	require.NotContains(t, joined, "scale=1280:", "profiles[1] scale must not appear")
	require.NotContains(t, joined, "scale=854:", "profiles[2] scale must not appear")
}

// KeyframeInterval × Framerate → GOP (no Global.GOP).
func TestScenario_KeyframeIntervalDerivesGOP(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	p := []Profile{{
		Width: 1280, Height: 720, Bitrate: "2500k",
		Codec: "h264", Preset: "p5",
		KeyframeInterval: 2, Framerate: 30,
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.Equal(t, "60", argAfter(args, "-g"), "2s × 30fps = 60-frame GOP")
	require.Equal(t, "30", argAfter(args, "-keyint_min"))
}

// Profile framerate omitted, Global.FPS used as -r fallback.
func TestScenario_GlobalFPSFallback(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNone, FPS: 25},
	}
	p := []Profile{{Width: 1280, Height: 720, Bitrate: "2500k"}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.Equal(t, "25", argAfter(args, "-r"),
		"profile framerate empty → Global.FPS as integer -r")
}

// Audio normalize loudnorm filter pairs with re-encode mode.
func TestScenario_FullReencode_AudioNormalize(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{},
		Audio: domain.AudioTranscodeConfig{
			Codec: domain.AudioCodecAAC, Bitrate: 128, Normalize: true,
		},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNone},
	}
	p := []Profile{{Width: 1280, Height: 720, Bitrate: "2500k", Codec: "h264", Preset: "fast"}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	af := argAfter(args, "-af")
	require.Contains(t, af, "loudnorm=I=-23:LRA=7:TP=-2",
		"normalize must inject loudnorm filter")
}

// Scale filter omitted when both width and height are 0 (encoder picks input
// resolution). -vf must NOT appear.
func TestScenario_NoScaleWhenDimensionsZero(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	p := []Profile{{Bitrate: "3000k", Codec: "h264", Preset: "p4"}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.NotContains(t, args, "-vf",
		"no -vf when both width and height are 0")
	require.Equal(t, "h264_nvenc", argAfter(args, "-c:v"))
}

// MaxBitrate=0 → no -maxrate / -bufsize at all.
func TestScenario_NoMaxrateWhenUnset(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNone},
	}
	p := []Profile{{Width: 1280, Height: 720, Bitrate: "2500k", Codec: "h264", Preset: "fast"}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.NotContains(t, args, "-maxrate")
	require.NotContains(t, args, "-bufsize")
}

// Pipe contract: stdin TS → stdout TS. Required for the worker pool to
// connect FFmpeg into the buffer hub.
func TestScenario_PipeIOContract(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{Copy: true},
		Audio: domain.AudioTranscodeConfig{Copy: true},
	}
	args, err := buildFFmpegArgs([]Profile{{Bitrate: "2500k"}}, tc)
	require.NoError(t, err)
	require.Equal(t, "pipe:0", argAfter(args, "-i"))
	require.Equal(t, "mpegts", argAfter(args, "-f"),
		"first -f is the input demuxer (mpegts)")
	// Last positional arg must be pipe:1.
	require.Equal(t, "pipe:1", args[len(args)-1])
	// Output muxer must also be mpegts.
	fs := allArgsAfter(args, "-f")
	require.Contains(t, fs, "mpegts")
	require.Equal(t, "mpegts", fs[len(fs)-1], "output muxer must be mpegts")
}

// ExtraArgs appear before the final "-f mpegts pipe:1".
func TestScenario_ExtraArgsAppendedBeforeOutput(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:     domain.VideoTranscodeConfig{Copy: true},
		Audio:     domain.AudioTranscodeConfig{Copy: true},
		ExtraArgs: []string{"-muxdelay", "0", "-mpegts_flags", "+resend_headers"},
	}
	args, err := buildFFmpegArgs([]Profile{{Bitrate: "2500k"}}, tc)
	require.NoError(t, err)
	require.Equal(t, "0", argAfter(args, "-muxdelay"))
	require.Equal(t, "+resend_headers", argAfter(args, "-mpegts_flags"))
	require.Equal(t, "pipe:1", args[len(args)-1])
}

// Map flags tolerate missing streams (?) — protects audio-only / video-only
// inputs from failing buildFFmpegArgs.
func TestScenario_MapFlagsAllowMissingStreams(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{Copy: true},
		Audio: domain.AudioTranscodeConfig{Copy: true},
	}
	args, err := buildFFmpegArgs([]Profile{{Bitrate: "2500k"}}, tc)
	require.NoError(t, err)
	maps := allArgsAfter(args, "-map")
	require.Contains(t, maps, "0:v:0?")
	require.Contains(t, maps, "0:a:0?")
}
