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
		vf := buildVideoFilter(p, tc, videoEnc)
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
		args = append(args, bframesArgs(p.Bframes, videoEnc)...)
		if p.Refs != nil && *p.Refs > 0 {
			args = append(args, "-refs", strconv.Itoa(*p.Refs))
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

// buildVideoFilter composes the full -vf chain in pipeline order:
// deinterlace → resize/pad/crop → setsar. Skipped if all parts are no-ops.
// Each filter is GPU- or CPU-flavored to match the active hwaccel; pad and
// crop on GPU pipelines round-trip through CPU (no cuda primitives are used).
func buildVideoFilter(p Profile, tc *domain.TranscoderConfig, encoder string) string {
	hw := tc.Global.HW
	_, onGPU := gpuScaleFilterName(hw, encoder)

	chain := make([]string, 0, 3)
	if df := deinterlaceFilter(tc.Video.Interlace, onGPU); df != "" {
		chain = append(chain, df)
	}
	if rs := resizeFilter(p.Width, p.Height, p.ResizeMode, hw, encoder); rs != "" {
		chain = append(chain, rs)
	}
	if p.SAR != "" {
		chain = append(chain, "setsar="+p.SAR)
	}
	return strings.Join(chain, ",")
}

// buildScaleFilter remains the simple resize entry point. Defaults to ResizeModePad.
func buildScaleFilter(w, h int, hw domain.HWAccel, encoder string) string {
	return resizeFilter(w, h, "", hw, encoder)
}

// resizeFilter dispatches to GPU/CPU scaler chains based on the active hwaccel.
// mode "" defaults to ResizeModePad. pad and crop on GPU round-trip through
// CPU (hwdownload→cpu filter→hwupload) — the cuda filter graph has no crop
// primitive, and pad_cuda was deliberately dropped for stability across
// ffmpeg builds (Ubuntu apt skips --enable-cuda-nvcc). See gpuResizeFilter.
func resizeFilter(w, h int, mode string, hw domain.HWAccel, encoder string) string {
	if w <= 0 && h <= 0 {
		return ""
	}
	m := normalizeResizeMode(mode)
	if name, onGPU := gpuScaleFilterName(hw, encoder); onGPU {
		return gpuResizeFilter(name, w, h, m)
	}
	return cpuResizeFilter(w, h, m)
}

func normalizeResizeMode(m string) domain.ResizeMode {
	switch domain.ResizeMode(strings.ToLower(strings.TrimSpace(m))) {
	case domain.ResizeModeCrop:
		return domain.ResizeModeCrop
	case domain.ResizeModeStretch:
		return domain.ResizeModeStretch
	case domain.ResizeModeFit:
		return domain.ResizeModeFit
	case domain.ResizeModePad:
		return domain.ResizeModePad
	default:
		return domain.ResizeModePad
	}
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

// gpuResizeFilter builds an in-VRAM scaler chain for the active hwaccel.
// stretch and fit stay fully in VRAM. pad and crop both round-trip through
// CPU (hwdownload→cpu filter→hwupload_cuda) — pad_cuda was deliberately
// dropped: the round-trip overhead is only paid when source/target aspects
// differ (no-op for typical 16:9 ABR ladders), and the CPU pad filter is
// rock-solid across every ffmpeg build, while pad_cuda needs a CUDA-enabled
// build (Ubuntu apt skips it) and has had option-syntax bugs in the wild.
func gpuResizeFilter(name string, w, h int, mode domain.ResizeMode) string {
	// Single-axis specifications collapse to a fit/stretch behavior — there's
	// no aspect to enforce when one dimension is auto.
	if w <= 0 || h <= 0 {
		if w > 0 {
			return fmt.Sprintf("%s=%d:-2", name, w)
		}
		return fmt.Sprintf("%s=-2:%d", name, h)
	}
	switch mode {
	case domain.ResizeModeStretch:
		return fmt.Sprintf("%s=%d:%d", name, w, h)
	case domain.ResizeModeFit:
		return fmt.Sprintf("%s=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2", name, w, h)
	case domain.ResizeModeCrop:
		// CUDA filter graph has no crop primitive; round-trip via CPU.
		// hwupload_cuda promotes the cropped frame back to VRAM for the encoder.
		return fmt.Sprintf("hwdownload,format=nv12,%s,hwupload_cuda", cpuResizeFilter(w, h, domain.ResizeModeCrop))
	case domain.ResizeModePad:
		fallthrough
	default:
		if name == "scale_cuda" {
			// CPU pad round-trip — see function doc for rationale.
			return fmt.Sprintf("hwdownload,format=nv12,%s,hwupload_cuda", cpuResizeFilter(w, h, domain.ResizeModePad))
		}
		// Non-CUDA GPU backends (VAAPI/QSV): no universal pad_<backend> filter
		// and the hwupload variant is backend-specific. Degrade pad → fit
		// (aspect-preserving scale, no letterbox bars).
		return fmt.Sprintf("%s=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2", name, w, h)
	}
}

func cpuResizeFilter(w, h int, mode domain.ResizeMode) string {
	if w <= 0 || h <= 0 {
		if w > 0 {
			return fmt.Sprintf("scale=%d:-2", w)
		}
		return fmt.Sprintf("scale=-2:%d", h)
	}
	switch mode {
	case domain.ResizeModeStretch:
		return fmt.Sprintf("scale=%d:%d", w, h)
	case domain.ResizeModeFit:
		// Round to even — H.264/HEVC require even dimensions.
		return fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2", w, h)
	case domain.ResizeModeCrop:
		return fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d", w, h, w, h)
	case domain.ResizeModePad:
		fallthrough
	default:
		const padChain = "force_original_aspect_ratio=decrease,pad=ceil(iw/2)*2:ceil(ih/2)*2:(ow-iw)/2:(oh-ih)/2"
		return fmt.Sprintf("scale=%d:%d:%s", w, h, padChain)
	}
}

// deinterlaceFilter returns a yadif/yadif_cuda fragment, or "" when disabled
// or when the source is asserted progressive. mode=0 keeps source FPS;
// parity is auto unless caller specifies tff/bff.
func deinterlaceFilter(im domain.InterlaceMode, onGPU bool) string {
	parity := -1
	switch im {
	case "":
		return ""
	case domain.InterlaceProgressive:
		// Source asserted progressive; FFmpeg's deinterlacer would still run
		// per-frame detection, costing GPU/CPU for no reason. Skip entirely.
		return ""
	case domain.InterlaceTopField:
		parity = 0
	case domain.InterlaceBottomField:
		parity = 1
	case domain.InterlaceAuto:
		// auto-detect parity per frame.
	default:
		return ""
	}
	name := "yadif"
	if onGPU {
		name = "yadif_cuda"
	}
	return fmt.Sprintf("%s=mode=0:parity=%d:deint=0", name, parity)
}

// bframesArgs emits -bf and (for NVENC) -b_ref_mode.
//
// Default = 0 B-frames (when bf is nil). Rationale:
//
//   - Live-streaming convention favours 0 B-frames for low end-to-end latency
//     (each B-frame adds ~1 frame of decoder buffering, ~33 ms at 30fps).
//   - The lal-based RTMP push out path drops PTS / composition_time when
//     muxing FLV tags (lal's AvPacket2RtmpRemuxer doesn't propagate
//     pkt.Pts), so any B-frames would be displayed at their DTS instead of
//     PTS at the receiver — visible as fine-grained motion jitter ("even
//     but not smooth") for downstream RTMP players (YouTube, Flussonic, …).
//
// Power users can opt back into B-frames by setting bframes explicitly per
// profile; HLS/DASH playback handles them correctly via TS PES PTS, only
// RTMP push is sensitive.
func bframesArgs(bf *int, encoder string) []string {
	n := 0
	if bf != nil {
		n = *bf
		if n < 0 {
			n = 0
		}
	}
	out := []string{"-bf", strconv.Itoa(n)}
	if n > 0 && strings.Contains(strings.ToLower(encoder), "nvenc") {
		// b_ref_mode=middle enables HW B-frame as reference (Turing+);
		// improves quality at no extra GPU cost when B-frames are on.
		out = append(out, "-b_ref_mode", "middle")
	}
	return out
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

// formatFFmpegCmd renders the binary + args as a single shell-pasteable string.
// Args containing whitespace or shell metacharacters (parentheses, $, etc. —
// common in FFmpeg pad expressions like `(ow-iw)/2`) are wrapped in single
// quotes; embedded single quotes are escaped using the standard `'\”` form.
func formatFFmpegCmd(bin string, args []string) string {
	var sb strings.Builder
	sb.WriteString(shellQuote(bin))
	for _, a := range args {
		sb.WriteByte(' ')
		sb.WriteString(shellQuote(a))
	}
	return sb.String()
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'`$\\&|;<>()*?#~![]{}") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
