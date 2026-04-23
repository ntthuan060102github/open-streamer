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
	p := []Profile{{Width: 1280, Height: 720, Bitrate: "2500k", ResizeMode: string(domain.ResizeModeFit)}}
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
	p := []Profile{{Width: 1280, Height: 720, Bitrate: "2500k", ResizeMode: string(domain.ResizeModeFit)}}
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
		// fit mode keeps the chain fully in VRAM; pad mode would round-trip
		// through CPU pad, breaking the scale_cuda assertion below.
		{Width: 1920, Height: 1080, Bitrate: "4500k", Codec: "h264", Preset: "p5", ResizeMode: string(domain.ResizeModeFit)},
		{Width: 1280, Height: 720, Bitrate: "2500k", Codec: "h264", Preset: "p4", ResizeMode: string(domain.ResizeModeFit)},
		{Width: 854, Height: 480, Bitrate: "1200k", Codec: "h264", Preset: "p3", ResizeMode: string(domain.ResizeModeFit)},
	}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)

	require.Equal(t, "4500k", argAfter(args, "-b:v"),
		"only profiles[0].Bitrate should appear")
	require.Equal(t, "p5", argAfter(args, "-preset"))
	vf := argAfter(args, "-vf")
	// HW=nvenc → GPU scale filter, not CPU `scale=`.
	require.Contains(t, vf, "scale_cuda=1920:1080:")

	joined := strings.Join(args, " ")
	require.NotContains(t, joined, "2500k", "profiles[1] bitrate must not appear")
	require.NotContains(t, joined, "1200k", "profiles[2] bitrate must not appear")
	require.NotContains(t, joined, "scale_cuda=1280:", "profiles[1] scale must not appear")
	require.NotContains(t, joined, "scale_cuda=854:", "profiles[2] scale must not appear")
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
	p := []Profile{{Width: 1280, Height: 720, Bitrate: "2500k", ResizeMode: string(domain.ResizeModeFit)}}
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

// ── Full-GPU pipeline contract ──────────────────────────────────────────────
//
// When user picks a HW backend AND the resolved encoder belongs to that
// backend, decode + scale + encode must all stay in VRAM. Mixing CPU decode
// with GPU encode burns PCIe bandwidth on every frame and adds latency.
//
// hwInputArgs flags must appear BEFORE -i, scale must use the GPU filter, and
// the encoder must match the backend.

// inputIdx returns the position of `-i` so we can assert that hwaccel flags
// appear before it (input options vs output options ordering matters).
func inputIdx(t *testing.T, args []string) int {
	t.Helper()
	i := indexOf(args, "-i")
	require.Greater(t, i, 0, "missing -i")
	return i
}

func TestScenario_NVENC_FullGPUPipeline(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	p := []Profile{{Width: 1280, Height: 720, Bitrate: "2500k", ResizeMode: string(domain.ResizeModeFit)}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)

	// 1. hwaccel flags must appear before -i (input option, not output option).
	hwIdx := indexOf(args, "-hwaccel")
	require.Greater(t, hwIdx, -1, "must emit -hwaccel for NVENC")
	require.Equal(t, "cuda", args[hwIdx+1])
	require.Less(t, hwIdx, inputIdx(t, args), "-hwaccel must precede -i")

	// 2. Output format must be cuda so decoder produces VRAM frames.
	hwOutIdx := indexOf(args, "-hwaccel_output_format")
	require.Greater(t, hwOutIdx, -1, "must emit -hwaccel_output_format")
	require.Equal(t, "cuda", args[hwOutIdx+1])
	require.Less(t, hwOutIdx, inputIdx(t, args), "-hwaccel_output_format must precede -i")

	// 3. Scale stays on GPU.
	vf := argAfter(args, "-vf")
	require.Contains(t, vf, "scale_cuda=1280:720:",
		"scale must use scale_cuda to keep frames in VRAM")
	require.Contains(t, vf, "force_divisible_by=2",
		"GPU scaler needs even dims for NVENC")

	// 4. CPU scale chain must NOT appear anywhere.
	require.NotContains(t, strings.Join(args, " "), "pad=ceil(iw/2)",
		"no CPU pad chain when on GPU pipeline")

	// 5. Encoder is GPU.
	require.Equal(t, "h264_nvenc", argAfter(args, "-c:v"))
}

// HEVC variant — must produce hevc_nvenc + scale_cuda + cuda hwaccel.
func TestScenario_NVENC_HEVC_FullGPUPipeline(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	p := []Profile{{Width: 1920, Height: 1080, Bitrate: "3500k", Codec: "h265", ResizeMode: string(domain.ResizeModeFit)}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.Equal(t, "cuda", argAfter(args, "-hwaccel"))
	require.Equal(t, "cuda", argAfter(args, "-hwaccel_output_format"))
	require.Contains(t, argAfter(args, "-vf"), "scale_cuda=1920:1080:")
	require.Equal(t, "hevc_nvenc", argAfter(args, "-c:v"))
}

// Bug #2 partner: explicit codec=libx264 with HW=nvenc must NOT enable
// hwaccel, because libx264 cannot read CUDA frames — FFmpeg would crash.
// This is the safety valve when an operator wants CPU encode despite a GPU
// being present (e.g. quality testing).
func TestScenario_NVENC_ExplicitLibx264_KeepsCPUPipeline(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	// "h264_videotoolbox" or other codecs that include "264" pass through
	// normalizeVideoEncoder verbatim. Use libx264 explicitly.
	p := []Profile{{
		Width: 1280, Height: 720, Bitrate: "2500k",
		Codec: "libx264", Preset: "veryfast",
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)

	require.Equal(t, "libx264", argAfter(args, "-c:v"))
	require.NotContains(t, args, "-hwaccel",
		"explicit CPU encoder + HW=nvenc must NOT emit hwaccel — libx264 cannot read CUDA frames")
	require.NotContains(t, args, "-hwaccel_output_format")
	vf := argAfter(args, "-vf")
	require.Contains(t, vf, "scale=1280:720:",
		"CPU encoder requires CPU scale (scale_cuda would output GPU frames libx264 cannot consume)")
	require.NotContains(t, vf, "scale_cuda")
}

// video.copy=true → no decode → hwaccel is wasted init, must NOT be emitted.
func TestScenario_NVENC_VideoCopy_NoHWAccel(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{Copy: true},
		Audio:  domain.AudioTranscodeConfig{Codec: domain.AudioCodecAAC, Bitrate: 128},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	args, err := buildFFmpegArgs([]Profile{{Bitrate: "ignored"}}, tc)
	require.NoError(t, err)
	require.NotContains(t, args, "-hwaccel",
		"video.copy=true skips decode — hwaccel context init is wasted")
	require.Equal(t, "copy", argAfter(args, "-c:v"))
	require.Equal(t, "aac", argAfter(args, "-c:a"))
}

// HW=none → CPU pipeline, no hwaccel flags ever.
func TestScenario_NoHW_NoHWAccelFlags(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNone},
	}
	p := []Profile{{Width: 1280, Height: 720, Bitrate: "2500k", ResizeMode: string(domain.ResizeModeFit)}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.NotContains(t, args, "-hwaccel")
	require.NotContains(t, args, "-hwaccel_output_format")
	require.Equal(t, "libx264", argAfter(args, "-c:v"))
}

// VAAPI parallel: same full-GPU contract.
func TestScenario_VAAPI_FullGPUPipeline(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelVAAPI},
	}
	p := []Profile{{Width: 1280, Height: 720, Bitrate: "2500k", Codec: "h264_vaapi"}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.Equal(t, "vaapi", argAfter(args, "-hwaccel"))
	require.Equal(t, "vaapi", argAfter(args, "-hwaccel_output_format"))
	require.Contains(t, argAfter(args, "-vf"), "scale_vaapi=1280:720:")
	require.Equal(t, "h264_vaapi", argAfter(args, "-c:v"))
}

// QSV parallel: same full-GPU contract.
func TestScenario_QSV_FullGPUPipeline(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelQSV},
	}
	p := []Profile{{Width: 1280, Height: 720, Bitrate: "2500k", Codec: "h264_qsv"}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.Equal(t, "qsv", argAfter(args, "-hwaccel"))
	require.Equal(t, "qsv", argAfter(args, "-hwaccel_output_format"))
	require.Contains(t, argAfter(args, "-vf"), "scale_qsv=1280:720:")
	require.Equal(t, "h264_qsv", argAfter(args, "-c:v"))
}

// VideoToolbox: hwaccel emitted (no _output_format), but scale stays CPU
// because there's no widely-supported scale_videotoolbox filter — VT decoder
// auto-downloads frames for CPU filters.
func TestScenario_VideoToolbox_HWAccelButCPUScale(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelVideoToolbox},
	}
	p := []Profile{{Width: 1280, Height: 720, Bitrate: "2500k", Codec: "h264_videotoolbox"}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	require.Equal(t, "videotoolbox", argAfter(args, "-hwaccel"))
	require.NotContains(t, args, "-hwaccel_output_format",
		"VT auto-maps frames; output_format would force a specific surface type")
	require.Contains(t, argAfter(args, "-vf"), "scale=1280:720:",
		"no scale_videotoolbox filter; CPU scale + auto-download is the working chain")
}

// Order contract: hwaccel block must precede -f mpegts -i pipe:0. FFmpeg
// treats flags before -i as input options, after as output options.
func TestScenario_NVENC_HWAccelOrderingBeforeInput(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	p := []Profile{{Width: 1920, Height: 1080, Bitrate: "4500k"}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)

	iIdx := inputIdx(t, args)
	for _, flag := range []string{"-hwaccel", "-hwaccel_output_format"} {
		idx := indexOf(args, flag)
		require.Greater(t, idx, -1, "missing %s", flag)
		require.Less(t, idx, iIdx, "%s must come before -i", flag)
	}
	// And -c:v must come after -i (output option).
	cvIdx := indexOf(args, "-c:v")
	require.Greater(t, cvIdx, iIdx, "-c:v must come after -i")
}

// ── Mode 5: Combined v0.0.6 features (Bframes / Refs / SAR / Interlace / Resize) ──
//
// These scenarios pair the new fields with the existing GPU/CPU pipelines to
// verify they compose without breaking earlier contracts (full-GPU pipeline,
// flag/value adjacency, hwaccel-before-input ordering).

func TestScenario_NVENC_FullProfile_AllNewFields(t *testing.T) {
	t.Parallel()
	bf := 0
	refs := 3
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{
			Interlace: domain.InterlaceTopField,
		},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC, GOP: 60},
	}
	p := []Profile{{
		Width: 1920, Height: 1080, Bitrate: "5000k", MaxBitrate: 6000,
		Codec: "h264", Preset: "p4",
		Bframes:    &bf,
		Refs:       &refs,
		SAR:        "1:1",
		ResizeMode: string(domain.ResizeModeFit),
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)

	require.Equal(t, "h264_nvenc", argAfter(args, "-c:v"))
	require.Equal(t, "0", argAfter(args, "-bf"), "explicit bframes=0 must propagate (low-latency)")
	require.Equal(t, "3", argAfter(args, "-refs"))

	vf := argAfter(args, "-vf")
	require.Contains(t, vf, "yadif_cuda=mode=0:parity=0:deint=0", "GPU pipeline must use yadif_cuda")
	require.Contains(t, vf, "scale_cuda=1920:1080:force_original_aspect_ratio=decrease:force_divisible_by=2",
		"fit mode → scale_cuda with force_divisible_by, no pad_cuda")
	require.NotContains(t, vf, "pad_cuda", "fit must skip pad")
	require.Contains(t, vf, "setsar=1:1")

	// bframes=0 → no b_ref_mode.
	require.Equal(t, "", argAfter(args, "-b_ref_mode"))

	// Hwaccel still emitted (full-GPU pipeline contract).
	require.Equal(t, "cuda", argAfter(args, "-hwaccel"))
	require.Equal(t, "cuda", argAfter(args, "-hwaccel_output_format"))
}

func TestScenario_NVENC_BframesPositive_EnablesBRefMode(t *testing.T) {
	t.Parallel()
	bf := 3
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	p := []Profile{{
		Width: 1280, Height: 720, Bitrate: "3000k",
		Codec: "h264", Preset: "p5",
		Bframes: &bf,
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)

	require.Equal(t, "3", argAfter(args, "-bf"))
	require.Equal(t, "middle", argAfter(args, "-b_ref_mode"),
		"NVENC + bframes>0 must enable HW b-ref pyramid (free quality on Turing+)")
}

func TestScenario_CPU_PadModeProducesPadFilter(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNone},
	}
	p := []Profile{{
		Width: 1280, Height: 720, Bitrate: "2000k",
		Codec: "h264", Preset: "fast",
		ResizeMode: string(domain.ResizeModePad),
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	vf := argAfter(args, "-vf")
	require.Contains(t, vf, "scale=1280:720:")
	require.Contains(t, vf, "pad=ceil(iw/2)*2:ceil(ih/2)*2")
}

func TestScenario_CPU_CropMode_HasNoPad(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{},
		Audio: domain.AudioTranscodeConfig{Copy: true},
	}
	p := []Profile{{
		Width: 1280, Height: 720, Bitrate: "2000k",
		Codec: "h264", Preset: "fast",
		ResizeMode: string(domain.ResizeModeCrop),
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	vf := argAfter(args, "-vf")
	require.Contains(t, vf, "scale=1280:720:force_original_aspect_ratio=increase")
	require.Contains(t, vf, "crop=1280:720")
	require.NotContains(t, vf, "pad=", "crop mode must not emit pad")
}

func TestScenario_NVENC_CropMode_RoundTripsThroughCPU(t *testing.T) {
	t.Parallel()
	// CUDA filter graph has no crop primitive — chain must round-trip via CPU
	// (hwdownload → CPU crop → hwupload_cuda) so the encoder still gets CUDA
	// frames. Encoder stays on GPU; only the crop step pays PCIe cost.
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	p := []Profile{{
		Width: 1280, Height: 720, Bitrate: "3000k",
		Codec: "h264", Preset: "p4",
		ResizeMode: string(domain.ResizeModeCrop),
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)

	require.Equal(t, "h264_nvenc", argAfter(args, "-c:v"))
	vf := argAfter(args, "-vf")
	require.Contains(t, vf, "hwdownload")
	require.Contains(t, vf, "format=nv12")
	require.Contains(t, vf, "crop=1280:720")
	require.Contains(t, vf, "hwupload_cuda")
	// Encoder pipeline still gets cuda frames overall.
	require.Equal(t, "cuda", argAfter(args, "-hwaccel_output_format"))
}

func TestScenario_NVENC_StretchMode_NoPadOrAspect(t *testing.T) {
	t.Parallel()
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Copy: true},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	p := []Profile{{
		Width: 1280, Height: 720, Bitrate: "3000k",
		Codec: "h264", Preset: "p4",
		ResizeMode: string(domain.ResizeModeStretch),
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	vf := argAfter(args, "-vf")
	require.Equal(t, "scale_cuda=1280:720", vf,
		"stretch on GPU must be a bare scale_cuda — no aspect, no pad")
}

func TestScenario_Interlace_AppliesToEveryProfileIndependently(t *testing.T) {
	t.Parallel()
	// Interlace lives on VideoTranscodeConfig (source-level) — every per-profile
	// FFmpeg subprocess emits the same yadif filter against its own decoded copy.
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{Interlace: domain.InterlaceAuto},
		Audio: domain.AudioTranscodeConfig{Copy: true},
	}
	for _, p := range []Profile{
		{Width: 1920, Height: 1080, Bitrate: "5000k", Codec: "h264", Preset: "fast"},
		{Width: 1280, Height: 720, Bitrate: "2500k", Codec: "h264", Preset: "fast"},
		{Width: 854, Height: 480, Bitrate: "1200k", Codec: "h264", Preset: "fast"},
	} {
		args, err := buildFFmpegArgs([]Profile{p}, tc)
		require.NoError(t, err)
		vf := argAfter(args, "-vf")
		require.Contains(t, vf, "yadif=mode=0:parity=-1:deint=0")
	}
}

func TestScenario_InterlaceProgressive_SkipsFilter(t *testing.T) {
	t.Parallel()
	// "progressive" is an assertion that the source has no interlace artifacts;
	// the deinterlacer should be skipped so we don't waste GPU/CPU per frame.
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{Interlace: domain.InterlaceProgressive},
		Audio: domain.AudioTranscodeConfig{Copy: true},
	}
	p := []Profile{{
		Width: 1280, Height: 720, Bitrate: "2000k", Codec: "h264", Preset: "fast",
	}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)
	vf := argAfter(args, "-vf")
	require.NotContains(t, vf, "yadif")
}

func TestScenario_NewFields_NoneSet_NoExtraArgs(t *testing.T) {
	t.Parallel()
	// A config with only the v0.0.5 fields must produce a minimal FFmpeg
	// command — no per-profile encoder tweaks beyond the implicit
	// `-bf 0` default we now apply for live-stream / RTMP-push smoothness.
	tc := &domain.TranscoderConfig{
		Video: domain.VideoTranscodeConfig{},
		Audio: domain.AudioTranscodeConfig{Copy: true},
	}
	p := []Profile{{Width: 1280, Height: 720, Bitrate: "2000k", Codec: "h264", Preset: "fast"}}
	args, err := buildFFmpegArgs(p, tc)
	require.NoError(t, err)

	// `-bf 0` is the implicit default — see bframesArgs doc comment for rationale.
	require.Equal(t, "0", argAfter(args, "-bf"))
	require.Equal(t, "", argAfter(args, "-refs"))
	require.Equal(t, "", argAfter(args, "-b_ref_mode"), "no b_ref_mode when bf=0")
	vf := argAfter(args, "-vf")
	require.NotContains(t, vf, "yadif")
	require.NotContains(t, vf, "setsar")
}
