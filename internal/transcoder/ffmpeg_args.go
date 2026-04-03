package transcoder

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
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

	// Input probe: live pipe TS often needs more than default probesize (5 MiB) so
	// SPS/PPS and pixel format are known before libx264 consumes decoded frames.
	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-fflags", "+genpts+discardcorrupt",
		"-analyzeduration", "15000000",
		"-probesize", "33554432",
		"-f", "mpegts",
		"-i", "pipe:0",
		"-map", "0:v:0?",
		"-map", "0:a:0?",
	}

	if tc.Video.Copy {
		args = append(args, "-c:v", "copy")
	} else {
		vf := buildScaleFilter(p.Width, p.Height)
		if vf != "" {
			args = append(args, "-vf", vf)
		}
		enc := normalizeVideoEncoder(p.Codec, tc.Global.HW)
		args = append(args, "-c:v", enc)
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

func buildScaleFilter(w, h int) string {
	if w <= 0 && h <= 0 {
		return ""
	}
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
