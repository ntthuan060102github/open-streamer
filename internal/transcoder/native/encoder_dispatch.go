package native

import (
	"strconv"
	"strings"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// encoderNameForHW returns the FFmpeg encoder name that pairs the
// requested codec with the active hardware backend. Empty `codec`
// defaults to h264 (matches Open-Streamer's existing YAML schema
// where omitting `codec:` means h264).
//
// Mirrors the existing FFmpeg CLI backend's domain.ResolveVideoEncoder
// dispatch so a stream's encoder choice stays consistent regardless of
// which backend runs it. Kept inside the native package rather than
// importing from domain to avoid pulling in CLI-specific dependencies
// here — and to make the dispatch explicit when reading.
func encoderNameForHW(hw domain.HWAccel, codec domain.VideoCodec) string {
	c := strings.ToLower(strings.TrimSpace(string(codec)))
	if c == "" {
		c = "h264"
	}

	switch hw {
	case domain.HWAccelNVENC:
		switch c {
		case "h264", "avc":
			return "h264_nvenc"
		case "h265", "hevc":
			return "hevc_nvenc"
		}
	case domain.HWAccelQSV:
		switch c {
		case "h264", "avc":
			return "h264_qsv"
		case "h265", "hevc":
			return "hevc_qsv"
		}
	case domain.HWAccelVAAPI:
		switch c {
		case "h264", "avc":
			return "h264_vaapi"
		case "h265", "hevc":
			return "hevc_vaapi"
		}
	case domain.HWAccelVideoToolbox:
		switch c {
		case "h264", "avc":
			return "h264_videotoolbox"
		case "h265", "hevc":
			return "hevc_videotoolbox"
		}
	}

	// CPU pipeline / unknown HW: software encoders.
	switch c {
	case "h264", "avc":
		return "libx264"
	case "h265", "hevc":
		return "libx265"
	}
	// Pass through explicit encoder names (e.g. operator typed
	// "h264_amf" directly) so the dispatch table doesn't have to know
	// every AMD/Vulkan variant.
	return c
}

// encoderPrivateOptions builds the codec-private option dictionary
// the encoder consumes during Open(). Mirrors the FFmpeg CLI backend's
// arg shape: `-preset`, `-profile:v`, `-level`, `-bf`, `-refs`, plus
// the live-broadcast tune fragment per encoder family.
//
// Returned as a plain map so the caller copies into an astiav.Dictionary
// — keeps this function pure and testable without astiav linkage.
func encoderPrivateOptions(encoderName string, prof domain.VideoProfile) map[string]string {
	opts := make(map[string]string, 6)

	if preset := strings.TrimSpace(prof.Preset); preset != "" {
		opts["preset"] = preset
	}
	if cp := strings.TrimSpace(prof.Profile); cp != "" {
		opts["profile"] = cp
	}
	if lvl := strings.TrimSpace(prof.Level); lvl != "" {
		opts["level"] = lvl
	}
	if prof.Bframes != nil {
		opts["bf"] = strconv.Itoa(*prof.Bframes)
	}
	if prof.Refs != nil && *prof.Refs > 0 {
		opts["refs"] = strconv.Itoa(*prof.Refs)
	}
	return opts
}
