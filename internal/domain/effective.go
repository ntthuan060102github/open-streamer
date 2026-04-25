package domain

import (
	"fmt"
	"strings"

	"github.com/ntt0601zcoder/open-streamer/config"
)

// Effective* resolvers return a deep-cloned config value with every
// implicitly-defaulted field replaced by its actual runtime value (see
// defaults.go for the constants). Used by the API layer to populate the
// `effective` envelope in stream / config responses so the UI can show
// what the system will actually do, not just what the user typed.
//
// All resolvers are safe with nil inputs — they synthesize an empty
// struct, apply defaults, and return a non-nil pointer. Callers can rely
// on the result being fully populated.
//
// The originals passed in are never mutated.

// ResolveVideoEncoder maps the user-facing codec string + global HW backend
// to the actual FFmpeg encoder name that buildFFmpegArgs will use. Mirrors
// transcoder.normalizeVideoEncoder so the API can preview the encoder
// without importing transcoder (and creating an import cycle through the
// handler package).
//
// Behaviour highlights:
//   - "" / "h264" / "avc" + HW=nvenc → "h264_nvenc"; else "libx264"
//   - "h265" / "hevc"     + HW=nvenc → "hevc_nvenc"; else "libx265"
//   - VAAPI / QSV / VideoToolbox: no implicit routing — user must spell
//     the full encoder name (e.g. "h264_vaapi"); empty stays libx264.
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
	case "vp9":
		return "libvpx-vp9"
	case "av1":
		return "libsvtav1"
	}
	if strings.Contains(c, "nvenc") || strings.Contains(c, "qsv") || strings.Contains(c, "videotoolbox") {
		return string(codec)
	}
	if strings.Contains(c, "264") || strings.Contains(c, "265") || strings.Contains(c, "hevc") {
		return string(codec)
	}
	return "libx264"
}

// ResolveAudioEncoder maps the user-facing codec to FFmpeg's encoder name.
// Mirrors transcoder.audioEncodeArgs codec selection. Empty / "copy" =>
// AAC default (since copy is decided separately via Audio.Copy).
func ResolveAudioEncoder(codec AudioCodec) string {
	c := strings.TrimSpace(strings.ToLower(string(codec)))
	switch c {
	case "", string(AudioCodecCopy), string(AudioCodecAAC):
		return "aac"
	case string(AudioCodecMP3):
		return "libmp3lame"
	case string(AudioCodecOpus):
		return "libopus"
	case string(AudioCodecAC3):
		return "ac3"
	}
	return "aac"
}

// ResolveResizeMode normalizes a free-form resize mode string to the
// canonical ResizeMode constant. Empty / unknown → ResizeModePad.
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
	return DefaultVideoResizeMode
}

// EffectiveVideoProfile returns a copy of p with every implicit default
// surfaced. `hw` is the parent TranscoderGlobalConfig.HW so codec/encoder
// routing matches what FFmpeg will actually receive.
//
// Does NOT change Width/Height (0 means "keep source"), MaxBitrate (0 =
// "no -maxrate"), Framerate (0 = "match source"), KeyframeInterval (0 =
// "encoder default GOP"), Bframes (nil = encoder default), Refs (nil =
// encoder default), or SAR (empty = inherit). Those zero/nil/empty values
// are themselves the runtime behaviour and have no scalar equivalent.
func EffectiveVideoProfile(p VideoProfile, hw HWAccel) VideoProfile {
	out := p
	if out.Bitrate <= 0 {
		out.Bitrate = DefaultVideoBitrateK
	}
	out.ResizeMode = ResolveResizeMode(out.ResizeMode)
	// Profile-level codec="copy" is meaningless when Video.Copy is false
	// — coordinator strips it to "" before reaching the transcoder so HW
	// routing kicks in. Mirror that here so the effective view shows the
	// real encoder (libx264 / h264_nvenc / …) instead of the dead value.
	codec := out.Codec
	if codec == VideoCodecCopy {
		codec = ""
	}
	// Codec: replace the user-facing alias ("", "h264", "avc"…) with the
	// fully-resolved FFmpeg encoder name. The UI renders this verbatim.
	out.Codec = VideoCodec(ResolveVideoEncoder(codec, hw))
	return out
}

// EffectiveAudioTranscodeConfig fills in audio defaults. Copy=true is
// preserved as-is — none of the encode fields apply when the audio is
// passed through.
func EffectiveAudioTranscodeConfig(a AudioTranscodeConfig) AudioTranscodeConfig {
	if a.Copy {
		return a
	}
	out := a
	if out.Codec == "" || out.Codec == AudioCodecCopy {
		out.Codec = DefaultAudioCodec
	}
	if out.Bitrate <= 0 {
		out.Bitrate = DefaultAudioBitrateK
	}
	return out
}

// EffectiveTranscoderGlobalConfig fills in HW defaults. FPS/GOP/DeviceID
// stay at their declared zero values because zero is itself a valid
// runtime semantic ("source FPS", "encoder default GOP", "device 0").
func EffectiveTranscoderGlobalConfig(g TranscoderGlobalConfig) TranscoderGlobalConfig {
	out := g
	if out.HW == "" {
		out.HW = DefaultHWAccel
	}
	return out
}

// EffectiveTranscoderConfig returns a fully-resolved transcoder config.
// Profile defaults are applied per-rung using the (resolved) Global.HW.
func EffectiveTranscoderConfig(tc *TranscoderConfig) *TranscoderConfig {
	if tc == nil {
		return nil
	}
	out := *tc
	out.Global = EffectiveTranscoderGlobalConfig(tc.Global)
	out.Audio = EffectiveAudioTranscodeConfig(tc.Audio)

	if !tc.Video.Copy && len(tc.Video.Profiles) > 0 {
		profiles := make([]VideoProfile, len(tc.Video.Profiles))
		for i, p := range tc.Video.Profiles {
			profiles[i] = EffectiveVideoProfile(p, out.Global.HW)
		}
		out.Video = VideoTranscodeConfig{
			Copy:      tc.Video.Copy,
			Interlace: tc.Video.Interlace,
			Profiles:  profiles,
		}
	}
	return &out
}

// EffectiveStreamDVRConfig fills in DVR defaults. Returns nil when the
// stream has no DVR configured (caller should not show a DVR section in
// the UI).
func EffectiveStreamDVRConfig(d *StreamDVRConfig, code StreamCode) *StreamDVRConfig {
	if d == nil {
		return nil
	}
	out := *d
	if out.SegmentDuration <= 0 {
		out.SegmentDuration = DefaultDVRSegmentDuration
	}
	if strings.TrimSpace(out.StoragePath) == "" {
		out.StoragePath = fmt.Sprintf("%s/%s", DefaultDVRRoot, code)
	}
	return &out
}

// EffectivePushDestination fills in push destination defaults.
func EffectivePushDestination(p PushDestination) PushDestination {
	out := p
	if out.TimeoutSec <= 0 {
		out.TimeoutSec = DefaultPushTimeoutSec
	}
	if out.RetryTimeoutSec <= 0 {
		out.RetryTimeoutSec = DefaultPushRetryTimeoutSec
	}
	return out
}

// EffectiveStream returns a stream with every implicit default surfaced
// across its nested config sections. The original is not mutated.
//
// Runtime-only fields (Status, Inputs[].Alive, Push[].Status) are copied
// as-is; they are not "config" and have no defaults.
func EffectiveStream(s *Stream) *Stream {
	if s == nil {
		return nil
	}
	out := *s
	out.Transcoder = EffectiveTranscoderConfig(s.Transcoder)
	out.DVR = EffectiveStreamDVRConfig(s.DVR, s.Code)

	if len(s.Push) > 0 {
		push := make([]PushDestination, len(s.Push))
		for i, p := range s.Push {
			push[i] = EffectivePushDestination(p)
		}
		out.Push = push
	}
	return &out
}

// EffectiveHook fills in per-hook delivery defaults.
func EffectiveHook(h *Hook) *Hook {
	if h == nil {
		return nil
	}
	out := *h
	if out.MaxRetries <= 0 {
		out.MaxRetries = DefaultHookMaxRetries
	}
	if out.TimeoutSec <= 0 {
		out.TimeoutSec = DefaultHookTimeoutSec
	}
	return &out
}

// EffectiveBufferConfig returns a non-nil BufferConfig with default
// capacity applied when the input is nil or Capacity ≤ 0.
func EffectiveBufferConfig(b *config.BufferConfig) *config.BufferConfig {
	out := config.BufferConfig{}
	if b != nil {
		out = *b
	}
	if out.Capacity <= 0 {
		out.Capacity = DefaultBufferCapacity
	}
	return &out
}

// EffectiveManagerConfig applies failover defaults.
func EffectiveManagerConfig(m *config.ManagerConfig) *config.ManagerConfig {
	out := config.ManagerConfig{}
	if m != nil {
		out = *m
	}
	if out.InputPacketTimeoutSec <= 0 {
		out.InputPacketTimeoutSec = DefaultInputPacketTimeoutSec
	}
	return &out
}

// EffectivePublisherConfig fills in HLS / DASH live packaging defaults.
func EffectivePublisherConfig(p *config.PublisherConfig) *config.PublisherConfig {
	out := config.PublisherConfig{}
	if p != nil {
		out = *p
	}
	out.HLS = effectivePublisherHLS(out.HLS)
	out.DASH = effectivePublisherDASH(out.DASH)
	return &out
}

func effectivePublisherHLS(h config.PublisherHLSConfig) config.PublisherHLSConfig {
	if h.LiveSegmentSec <= 0 {
		h.LiveSegmentSec = DefaultLiveSegmentSec
	}
	if h.LiveWindow <= 0 {
		h.LiveWindow = DefaultLiveWindow
	}
	if h.LiveHistory < 0 {
		h.LiveHistory = DefaultLiveHistory
	}
	return h
}

func effectivePublisherDASH(d config.PublisherDASHConfig) config.PublisherDASHConfig {
	if d.LiveSegmentSec <= 0 {
		d.LiveSegmentSec = DefaultLiveSegmentSec
	}
	if d.LiveWindow <= 0 {
		d.LiveWindow = DefaultLiveWindow
	}
	if d.LiveHistory < 0 {
		d.LiveHistory = DefaultLiveHistory
	}
	return d
}

// EffectiveListenersConfig fills in standard ports and transports for any
// enabled listener. Disabled listeners keep their zero values — the UI
// uses the Enabled flag, not Port=0, to know whether to hide the section.
func EffectiveListenersConfig(l *config.ListenersConfig) *config.ListenersConfig {
	out := config.ListenersConfig{}
	if l != nil {
		out = *l
	}
	if out.RTMP.Enabled && out.RTMP.Port <= 0 {
		out.RTMP.Port = DefaultRTMPPort
	}
	if out.RTSP.Enabled {
		if out.RTSP.Port <= 0 {
			out.RTSP.Port = DefaultRTSPPort
		}
		if strings.TrimSpace(out.RTSP.Transport) == "" {
			out.RTSP.Transport = DefaultRTSPTransport
		}
	}
	if out.SRT.Enabled && out.SRT.Port <= 0 {
		out.SRT.Port = DefaultSRTPort
	}
	return &out
}

// EffectiveGlobalConfig returns a copy of the global config with every
// nested section's defaults applied. Sections that are nil (i.e. the
// service is intentionally disabled) stay nil.
func EffectiveGlobalConfig(g *GlobalConfig) *GlobalConfig {
	if g == nil {
		return nil
	}
	out := *g
	if g.Buffer != nil {
		out.Buffer = EffectiveBufferConfig(g.Buffer)
	}
	if g.Manager != nil {
		out.Manager = EffectiveManagerConfig(g.Manager)
	}
	if g.Publisher != nil {
		out.Publisher = EffectivePublisherConfig(g.Publisher)
	}
	if g.Listeners != nil {
		out.Listeners = EffectiveListenersConfig(g.Listeners)
	}
	return &out
}
