package transcoder

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// buildFFmpegArgs builds FFmpeg CLI arguments: read MPEG-TS from stdin, write MPEG-TS to stdout.
// When multiple profiles are provided, only the first is used for the single stdout ladder slot.
func buildFFmpegArgs(profiles []Profile, tc *domain.TranscoderConfig) ([]string, error) {
	if tc == nil {
		tc = &domain.TranscoderConfig{}
	}
	if len(profiles) == 0 {
		return nil, fmt.Errorf("transcoder: no video profiles")
	}
	p := profiles[0]

	// Resolve the video encoder up-front: hwaccel input flags only make sense
	// when the encoder is itself a HW encoder of the same family. Mismatched
	// configs (HW=nvenc but explicit codec=libx264) keep the CPU pipeline —
	// libx264 cannot consume CUDA-format frames, forcing hwaccel would crash.
	videoEnc := ""
	if !tc.Video.Copy {
		videoEnc = normalizeVideoEncoder(p.Codec, tc.Global.HW)
	}

	// Input probe: live pipe TS often needs more than default probesize (5 MiB) so
	// SPS/PPS and pixel format are known before libx264 consumes decoded frames.
	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-fflags", "+genpts+discardcorrupt",
		"-analyzeduration", "15000000",
		"-probesize", "33554432",
	}
	// Full-GPU pipeline: when HW matches the encoder, decode + scale + encode
	// all stay in VRAM. Without these flags FFmpeg decodes on CPU and uploads
	// each frame to GPU for nvenc — wastes PCIe bandwidth and adds latency.
	args = append(args, hwInputArgs(tc.Global.HW, videoEnc)...)
	args = append(args,
		"-f", "mpegts",
		"-i", "pipe:0",
		"-map", "0:v:0?",
		"-map", "0:a:0?",
	)

	if tc.Video.Copy {
		args = append(args, "-c:v", "copy")
	} else {
		vf := buildScaleFilter(p.Width, p.Height, tc.Global.HW, videoEnc)
		if vf != "" {
			args = append(args, "-vf", vf)
		}
		args = append(args, "-c:v", videoEnc)
		if p.Preset != "" {
			args = append(args, "-preset", p.Preset)
		}
		if p.CodecProfile != "" {
			args = append(args, "-profile:v", p.CodecProfile)
		}
		if p.CodecLevel != "" {
			args = append(args, "-level", p.CodecLevel)
		}
		args = append(args, "-b:v", p.Bitrate)
		if p.MaxBitrate > 0 {
			args = append(args, "-maxrate", strconv.Itoa(p.MaxBitrate)+"k")
			args = append(args, "-bufsize", strconv.Itoa(p.MaxBitrate*2)+"k")
		}
		gop := gopFrames(tc, p)
		if gop > 0 {
			km := max(1, gop/2)
			args = append(args, "-g", strconv.Itoa(gop), "-keyint_min", strconv.Itoa(km))
		}
		if p.Framerate > 0 {
			args = append(args, "-r", formatFloat(p.Framerate))
		} else if tc.Global.FPS > 0 {
			args = append(args, "-r", strconv.Itoa(tc.Global.FPS))
		}
	}

	if tc.Audio.Copy {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, audioEncodeArgs(tc)...)
	}

	if len(tc.ExtraArgs) > 0 {
		args = append(args, tc.ExtraArgs...)
	}

	args = append(args, "-f", "mpegts", "pipe:1")
	return args, nil
}

// hwInputArgs returns the `-hwaccel` flags to put before `-i` so the decoder
// produces frames already in GPU memory. Only emitted when the resolved
// encoder is a HW encoder of the same family — otherwise the encoder cannot
// consume hardware frames and FFmpeg crashes.
func hwInputArgs(hw domain.HWAccel, encoder string) []string {
	if encoder == "" {
		// video.copy=true → no decode happens at all, hwaccel is wasted init.
		return nil
	}
	enc := strings.ToLower(encoder)
	switch hw {
	case domain.HWAccelNVENC:
		if strings.Contains(enc, "nvenc") {
			return []string{"-hwaccel", "cuda", "-hwaccel_output_format", "cuda"}
		}
	case domain.HWAccelVAAPI:
		if strings.Contains(enc, "vaapi") {
			return []string{"-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi"}
		}
	case domain.HWAccelQSV:
		if strings.Contains(enc, "qsv") {
			return []string{"-hwaccel", "qsv", "-hwaccel_output_format", "qsv"}
		}
	case domain.HWAccelVideoToolbox:
		if strings.Contains(enc, "videotoolbox") {
			// VT auto-maps to/from CPU when needed; no _output_format required.
			return []string{"-hwaccel", "videotoolbox"}
		}
	case domain.HWAccelNone:
		// CPU pipeline.
	}
	return nil
}

func buildScaleFilter(w, h int, hw domain.HWAccel, encoder string) string {
	if w <= 0 && h <= 0 {
		return ""
	}
	if name, ok := gpuScaleFilterName(hw, encoder); ok {
		return gpuScaleFilter(name, w, h)
	}
	return cpuScaleFilter(w, h)
}

// gpuScaleFilterName picks the in-VRAM scaler that pairs with the active
// hwaccel. Returning (_, false) keeps frames on CPU — required when the
// encoder is software (libx264 etc.) or when the backend lacks a usable GPU
// scaler (VideoToolbox: FFmpeg auto-downloads VT-decoded frames for CPU
// filters, so a software scale chain still works without a manual hwdownload).
func gpuScaleFilterName(hw domain.HWAccel, encoder string) (string, bool) {
	enc := strings.ToLower(encoder)
	switch hw {
	case domain.HWAccelNVENC:
		if strings.Contains(enc, "nvenc") {
			return "scale_cuda", true
		}
	case domain.HWAccelVAAPI:
		if strings.Contains(enc, "vaapi") {
			return "scale_vaapi", true
		}
	case domain.HWAccelQSV:
		if strings.Contains(enc, "qsv") {
			return "scale_qsv", true
		}
	case domain.HWAccelNone, domain.HWAccelVideoToolbox:
		// HWAccelNone: CPU pipeline.
		// HWAccelVideoToolbox: no widely-supported GPU scaler; FFmpeg
		// auto-downloads VT-decoded frames for the CPU `scale` filter.
	}
	return "", false
}

func gpuScaleFilter(name string, w, h int) string {
	// `force_divisible_by=2` is the GPU-friendly equivalent of the CPU `pad`
	// chain — NVENC/VAAPI/QSV all reject odd dimensions.
	if w > 0 && h > 0 {
		return fmt.Sprintf("%s=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2", name, w, h)
	}
	if w > 0 {
		return fmt.Sprintf("%s=%d:-2", name, w)
	}
	return fmt.Sprintf("%s=-2:%d", name, h)
}

func cpuScaleFilter(w, h int) string {
	const chain = "force_original_aspect_ratio=decrease,pad=ceil(iw/2)*2:ceil(ih/2)*2:(ow-iw)/2:(oh-ih)/2"
	if w > 0 && h > 0 {
		return fmt.Sprintf("scale=%d:%d:%s", w, h, chain)
	}
	if w > 0 {
		return fmt.Sprintf("scale=%d:-2:%s", w, chain)
	}
	return fmt.Sprintf("scale=-2:%d:%s", h, chain)
}

func normalizeVideoEncoder(codec string, hw domain.HWAccel) string {
	c := strings.TrimSpace(strings.ToLower(codec))
	switch c {
	case "", "h264", "avc":
		if hw == domain.HWAccelNVENC {
			return "h264_nvenc"
		}
		return "libx264"
	case "h265", "hevc":
		if hw == domain.HWAccelNVENC {
			return "hevc_nvenc"
		}
		return "libx265"
	case "vp9":
		return "libvpx-vp9"
	case "av1":
		return "libsvtav1"
	}
	if strings.Contains(c, "nvenc") || strings.Contains(c, "qsv") || strings.Contains(c, "videotoolbox") {
		return codec
	}
	if strings.Contains(c, "264") || strings.Contains(c, "265") || strings.Contains(c, "hevc") {
		return codec
	}
	return "libx264"
}

func gopFrames(tc *domain.TranscoderConfig, p Profile) int {
	if tc.Global.GOP > 0 {
		return tc.Global.GOP
	}
	if p.KeyframeInterval > 0 {
		fps := p.Framerate
		if fps <= 0 && tc.Global.FPS > 0 {
			fps = float64(tc.Global.FPS)
		}
		if fps <= 0 {
			fps = 25
		}
		return max(1, int(float64(p.KeyframeInterval)*fps+0.5))
	}
	return 0
}

func audioEncodeArgs(tc *domain.TranscoderConfig) []string {
	codec := strings.TrimSpace(strings.ToLower(string(tc.Audio.Codec)))
	if codec == "" || codec == string(domain.AudioCodecCopy) {
		codec = "aac"
	}
	switch codec {
	case "aac":
		codec = "aac"
	case "mp3":
		codec = "libmp3lame"
	case "opus":
		codec = "libopus"
	case "ac3":
		codec = "ac3"
	default:
		codec = "aac"
	}
	br := tc.Audio.Bitrate
	if br <= 0 {
		br = 128
	}
	args := []string{"-c:a", codec, "-b:a", strconv.Itoa(br) + "k"}
	if tc.Audio.SampleRate > 0 {
		args = append(args, "-ar", strconv.Itoa(tc.Audio.SampleRate))
	}
	if tc.Audio.Channels > 0 {
		args = append(args, "-ac", strconv.Itoa(tc.Audio.Channels))
	}
	if tc.Audio.Normalize {
		args = append(args, "-af", "loudnorm=I=-23:LRA=7:TP=-2")
	}
	return args
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', 3, 64)
}
