package domain

import "strings"

// Resolve* helpers map user-facing config aliases to the values FFmpeg
// (or downstream services) will actually use. They live here so the
// transcoder package can call them without importing FFmpeg-specific
// internals — the same lookup tables stay testable in pure Go.

// ResolveVideoEncoder maps a user-facing codec string + global HW backend
// to the FFmpeg encoder name that buildFFmpegArgs will emit.
//
// Routing highlights:
//   - "" / "h264" / "avc" + HW=nvenc → "h264_nvenc"; else "libx264"
//   - "h265" / "hevc"     + HW=nvenc → "hevc_nvenc"; else "libx265"
//   - VAAPI / QSV / VideoToolbox: no implicit routing — caller must spell
//     the full encoder name (e.g. "h264_vaapi"); empty stays "libx264".
func ResolveVideoEncoder(codec VideoCodec, hw HWAccel) string {
	c := strings.TrimSpace(strings.ToLower(string(codec)))
	switch c {
	case "", "h264", "avc":
		if hw == HWAccelNVENC {
			return "h264_nvenc"
		}
		return "libx264"
	case "h265", "hevc":
		if hw == HWAccelNVENC {
			return "hevc_nvenc"
		}
		return "libx265"
	case "av1":
		return "libsvtav1"
	case "mp2v", "mpeg2video":
		// FFmpeg's built-in MPEG-2 Part 2 encoder — always available, no
		// external library. Used when feeding a DVB transmitter chain or
		// legacy headend system that consumes MPEG-2 input.
		return "mpeg2video"
	}
	if strings.Contains(c, "nvenc") || strings.Contains(c, "qsv") || strings.Contains(c, "videotoolbox") {
		return string(codec)
	}
	if strings.Contains(c, "264") || strings.Contains(c, "265") || strings.Contains(c, "hevc") {
		return string(codec)
	}
	return "libx264"
}

// ResolveAudioEncoder maps a user-facing codec to FFmpeg's encoder name.
// Empty / "copy" → AAC default (since copy is decided separately via
// AudioTranscodeConfig.Copy).
//
// MP2 (mp2a) maps to FFmpeg's built-in `mp2` encoder — MPEG-1 Layer II,
// always available in standard FFmpeg builds (no external library needed,
// unlike libtwolame). Valid bitrates 32-384 kbps; FFmpeg silently rounds
// to the nearest valid Layer II rate. Used for DVB broadcast contribution
// feeds and legacy IPTV headends that consume Layer II input.
func ResolveAudioEncoder(codec AudioCodec) string {
	c := strings.TrimSpace(strings.ToLower(string(codec)))
	switch c {
	case "", string(AudioCodecCopy), string(AudioCodecAAC):
		return "aac"
	case string(AudioCodecMP2):
		return "mp2"
	case string(AudioCodecMP3):
		return "libmp3lame"
	case string(AudioCodecAC3):
		return "ac3"
	case string(AudioCodecEAC3):
		return "eac3"
	}
	return "aac"
}

// ResolveResizeMode normalizes a free-form resize mode to the canonical
// constant. Empty / unknown → ResizeModePad.
func ResolveResizeMode(m ResizeMode) ResizeMode {
	switch ResizeMode(strings.ToLower(strings.TrimSpace(string(m)))) {
	case ResizeModeCrop:
		return ResizeModeCrop
	case ResizeModeStretch:
		return ResizeModeStretch
	case ResizeModeFit:
		return ResizeModeFit
	case ResizeModePad:
		return ResizeModePad
	}
	return ResizeModePad
}
