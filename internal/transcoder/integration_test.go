//go:build integration

// Integration tests: spawn the real `ffmpeg` binary against the args our
// builders produce. Catches the class of bug where Go-level unit tests pass
// but ffmpeg rejects the chain at runtime — e.g. the v0.0.6 pad_cuda
// "No option name near '...'" failure that only surfaced in production logs.
//
// Run: make test-integration   (or: go test -tags integration ./internal/transcoder/...)
//
// Skips gracefully when:
//   - ffmpeg is not on PATH
//   - the build lacks a filter the case needs (e.g. pad_cuda on a non-CUDA build)
//   - CUDA device init fails (no NVIDIA GPU / no driver)

package transcoder

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/stretchr/testify/require"
)

const ffmpegProbeTimeout = 15 * time.Second

var (
	ffmpegPathOnce sync.Once
	ffmpegPath     string

	filtersOnce sync.Once
	filterSet   map[string]bool

	cudaOnce      sync.Once
	cudaWorks     bool
	cudaProbeMsg  string
)

func locateFFmpeg(t *testing.T) string {
	t.Helper()
	ffmpegPathOnce.Do(func() {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegPath = p
		}
	})
	if ffmpegPath == "" {
		t.Skip("ffmpeg not on PATH — install ffmpeg to run integration tests")
	}
	return ffmpegPath
}

func haveFilter(t *testing.T, name string) bool {
	t.Helper()
	filtersOnce.Do(func() {
		filterSet = map[string]bool{}
		ctx, cancel := context.WithTimeout(context.Background(), ffmpegProbeTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, locateFFmpeg(t), "-hide_banner", "-filters").Output()
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(out), "\n") {
			// `ffmpeg -filters` rows: " T.. filter_name  I->O  description"
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				filterSet[fields[1]] = true
			}
		}
	})
	return filterSet[name]
}

func cudaAvailable(t *testing.T) (bool, string) {
	t.Helper()
	cudaOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), ffmpegProbeTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, locateFFmpeg(t),
			"-hide_banner", "-loglevel", "error", "-nostdin",
			"-init_hw_device", "cuda=cu",
			"-f", "lavfi", "-i", "color=c=black:s=64x64:r=1:d=0.04",
			"-frames:v", "1", "-f", "null", "-",
		).CombinedOutput()
		if err != nil {
			cudaProbeMsg = strings.TrimSpace(string(out))
			return
		}
		cudaWorks = true
	})
	return cudaWorks, cudaProbeMsg
}

// runFilterChainOnFFmpeg pipes 1 frame of synthetic video through `vf` and
// fails the test if ffmpeg returns non-zero. Auto-skips on missing prereqs.
func runFilterChainOnFFmpeg(t *testing.T, vf string) {
	t.Helper()
	bin := locateFFmpeg(t)

	usesCUDA := false
	for _, f := range []string{"scale_cuda", "pad_cuda", "yadif_cuda", "hwupload_cuda"} {
		if strings.Contains(vf, f) {
			if !haveFilter(t, f) {
				t.Skipf("ffmpeg build lacks %q filter", f)
			}
			usesCUDA = true
		}
	}
	if strings.Contains(vf, "hwdownload") && !haveFilter(t, "hwdownload") {
		t.Skip("ffmpeg build lacks hwdownload filter")
	}

	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if usesCUDA {
		ok, msg := cudaAvailable(t)
		if !ok {
			t.Skipf("CUDA device unavailable: %s", msg)
		}
		args = append(args,
			"-init_hw_device", "cuda=cu",
			"-filter_hw_device", "cu",
			// Lavfi produces a CPU color frame, format=nv12 normalises pixfmt,
			// hwupload promotes it to a CUDA frame so the chain sees the same
			// frame type a CUDA decoder would emit in production.
			"-f", "lavfi",
			"-i", "color=c=black:s=320x240:r=25:d=0.08,format=nv12,hwupload",
		)
	} else {
		args = append(args,
			"-f", "lavfi",
			"-i", "testsrc2=size=320x240:rate=25:duration=0.08",
		)
	}
	args = append(args, "-vf", vf, "-frames:v", "1", "-f", "null", "-")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	require.NoErrorf(t, err,
		"ffmpeg rejected -vf chain\nvf:   %s\ncmd:  %s %s\nout:\n%s",
		vf, bin, strings.Join(args, " "), string(out),
	)
}

// TestRuntime_ResizeFilters_AllModes runs every (resize-mode × hwaccel) combo
// through real ffmpeg. Regression for the v0.0.6 pad_cuda positional-args
// bug — that one passed all unit tests but failed at runtime.
func TestRuntime_ResizeFilters_AllModes(t *testing.T) {
	cases := []struct {
		name string
		w, h int
		mode string
		hw   domain.HWAccel
		enc  string
	}{
		{"cpu_pad", 1280, 720, "pad", domain.HWAccelNone, "libx264"},
		{"cpu_fit", 1280, 720, "fit", domain.HWAccelNone, "libx264"},
		{"cpu_crop", 1280, 720, "crop", domain.HWAccelNone, "libx264"},
		{"cpu_stretch", 1280, 720, "stretch", domain.HWAccelNone, "libx264"},
		{"nvenc_pad_1080p", 1920, 1080, "pad", domain.HWAccelNVENC, "h264_nvenc"},
		{"nvenc_pad_720p", 1280, 720, "pad", domain.HWAccelNVENC, "h264_nvenc"},
		{"nvenc_pad_480p", 854, 480, "pad", domain.HWAccelNVENC, "h264_nvenc"},
		{"nvenc_fit", 1280, 720, "fit", domain.HWAccelNVENC, "h264_nvenc"},
		{"nvenc_stretch", 1280, 720, "stretch", domain.HWAccelNVENC, "h264_nvenc"},
		{"nvenc_crop_roundtrip", 1280, 720, "crop", domain.HWAccelNVENC, "h264_nvenc"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			vf := resizeFilter(tt.w, tt.h, tt.mode, tt.hw, tt.enc)
			require.NotEmptyf(t, vf, "no -vf produced for %+v", tt)
			runFilterChainOnFFmpeg(t, vf)
		})
	}
}

// TestRuntime_BuildVideoFilter_FullChain runs the composed deinterlace +
// resize + setsar chain (the real shape buildFFmpegArgs ships).
func TestRuntime_BuildVideoFilter_FullChain(t *testing.T) {
	cases := []struct {
		name string
		tc   *domain.TranscoderConfig
		p    Profile
		enc  string
	}{
		{
			name: "cpu_yadif_pad_setsar",
			tc: &domain.TranscoderConfig{
				Video: domain.VideoTranscodeConfig{Interlace: domain.InterlaceTopField},
			},
			p:   Profile{Width: 1280, Height: 720, ResizeMode: "pad", SAR: "1:1"},
			enc: "libx264",
		},
		{
			name: "nvenc_yadif_pad_1080p",
			tc: &domain.TranscoderConfig{
				Video:  domain.VideoTranscodeConfig{Interlace: domain.InterlaceAuto},
				Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
			},
			p:   Profile{Width: 1920, Height: 1080, ResizeMode: "pad"},
			enc: "h264_nvenc",
		},
		{
			name: "nvenc_fit_no_pad",
			tc: &domain.TranscoderConfig{
				Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
			},
			p:   Profile{Width: 1280, Height: 720, ResizeMode: "fit"},
			enc: "h264_nvenc",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			vf := buildVideoFilter(tt.p, tt.tc, tt.enc)
			require.NotEmpty(t, vf, "expected a -vf chain")
			runFilterChainOnFFmpeg(t, vf)
		})
	}
}

// TestRuntime_UserConfigScenario reproduces the v0.0.6 production config that
// triggered the pad_cuda crash: NVENC + 3 ABR rungs, all defaulted to pad.
// Generates the same -vf chain for each rung and validates against ffmpeg.
func TestRuntime_UserConfigScenario(t *testing.T) {
	tc := &domain.TranscoderConfig{
		Video:  domain.VideoTranscodeConfig{},
		Audio:  domain.AudioTranscodeConfig{Codec: domain.AudioCodecAAC, Bitrate: 192},
		Global: domain.TranscoderGlobalConfig{HW: domain.HWAccelNVENC},
	}
	rungs := []Profile{
		{Width: 1280, Height: 720, Bitrate: "2500k", Framerate: 30},
		{Width: 854, Height: 480, Bitrate: "1200k", Framerate: 30},
		{Width: 1920, Height: 1080, Bitrate: "4500k", Framerate: 30},
	}
	for _, p := range rungs {
		t.Run(buildScenarioName(p), func(t *testing.T) {
			vf := buildVideoFilter(p, tc, "h264_nvenc")
			require.NotEmpty(t, vf)
			runFilterChainOnFFmpeg(t, vf)
		})
	}
}

func buildScenarioName(p Profile) string {
	switch {
	case p.Width >= 1920:
		return "1080p"
	case p.Width >= 1280:
		return "720p"
	default:
		return "480p"
	}
}
