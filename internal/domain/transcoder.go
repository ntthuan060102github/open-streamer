package domain

// TranscodeMode controls how much processing is applied to the stream.
type TranscodeMode string

const (
	// TranscodeModePassthrough copies both video and audio without any re-encoding.
	// Lowest CPU usage; output format must match source.
	TranscodeModePassthrough TranscodeMode = "passthrough"

	// TranscodeModeRemux rewraps the stream into a new container without re-encoding.
	// E.g. RTMP → MPEG-TS. Very low CPU usage.
	TranscodeModeRemux TranscodeMode = "remux"

	// TranscodeModeFull performs full video and audio re-encoding.
	// Required for ABR ladder, codec conversion, or applying filters.
	TranscodeModeFull TranscodeMode = "transcode"
)

// HWAccel selects the hardware acceleration backend for encoding/decoding.
type HWAccel string

const (
	HWAccelNone         HWAccel = "none"         // CPU only (libx264, libx265)
	HWAccelNVENC        HWAccel = "nvenc"        // NVIDIA GPU (h264_nvenc, hevc_nvenc)
	HWAccelVAAPI        HWAccel = "vaapi"        // Intel/AMD GPU via VA-API (Linux)
	HWAccelVideoToolbox HWAccel = "videotoolbox" // Apple GPU (macOS)
	HWAccelQSV          HWAccel = "qsv"          // Intel Quick Sync Video
)

// VideoCodec identifies the video compression format.
type VideoCodec string

const (
	VideoCodecH264 VideoCodec = "h264" // AVC — widest device support
	VideoCodecH265 VideoCodec = "h265" // HEVC — ~50% smaller than H.264
	VideoCodecAV1  VideoCodec = "av1"  // royalty-free, best compression (high CPU)
	VideoCodecVP9  VideoCodec = "vp9"  // Google codec, WebRTC-friendly
	VideoCodecCopy VideoCodec = "copy" // passthrough — no re-encode
)

// AudioCodec identifies the audio compression format.
type AudioCodec string

const (
	AudioCodecAAC  AudioCodec = "aac"  // default for HLS/DASH
	AudioCodecMP3  AudioCodec = "mp3"  // legacy compatibility
	AudioCodecOpus AudioCodec = "opus" // best for WebRTC / low-latency
	AudioCodecAC3  AudioCodec = "ac3"  // Dolby Digital — broadcast use
	AudioCodecCopy AudioCodec = "copy" // passthrough — no re-encode
)

// VideoProfile is a single rendition in the ABR (Adaptive Bitrate) ladder.
// The Transcoder produces one FFmpeg output per profile.
type VideoProfile struct {
	// Name is the human-readable label, e.g. "1080p", "720p", "480p".
	Name string `json:"name"`

	// Width and Height define the output resolution.
	// Set to 0 to keep the source dimensions (Width=0 & Height=0 = no scaling).
	Width  int `json:"width"`
	Height int `json:"height"`

	// Bitrate is the target video bitrate in kbps. 0 = encoder auto.
	Bitrate int `json:"bitrate"`

	// MaxBitrate caps the peak bitrate in kbps (CBR/VBR ceiling). 0 = no cap.
	MaxBitrate int `json:"max_bitrate"`

	// Framerate is the output frame rate (fps). 0 = match source.
	Framerate float64 `json:"framerate"`

	// KeyframeInterval is the GOP size in seconds.
	// Must match or be a multiple of the HLS/DASH segment duration.
	KeyframeInterval int `json:"keyframe_interval"`

	Codec VideoCodec `json:"codec"`

	// Preset controls the encoder speed/quality tradeoff.
	// libx264: "ultrafast" | "superfast" | "veryfast" | "faster" | "fast" | "medium" | "slow" | "veryslow"
	// NVENC:   "p1" (fastest) .. "p7" (highest quality)
	Preset string `json:"preset"`

	// Profile controls the H.264/H.265 encoding profile.
	// "baseline" | "main" | "high" (H.264); "main" | "main10" (H.265)
	Profile string `json:"profile"`

	// Level controls the H.264/H.265 encoding level.
	// Common: "3.1", "4.0", "4.1", "4.2", "5.0", "5.1"
	Level string `json:"level"`
}

// AudioConfig defines the audio encoding settings applied to all output profiles.
type AudioConfig struct {
	Codec AudioCodec `json:"codec"`

	// Bitrate is the audio bitrate in kbps. Typical: 128 (stereo), 192 (high quality).
	Bitrate int `json:"bitrate"`

	// SampleRate is the output sample rate in Hz. Typical: 44100, 48000.
	SampleRate int `json:"sample_rate"`

	// Channels: 1 = mono, 2 = stereo, 6 = 5.1 surround.
	Channels int `json:"channels"`

	// Language is the ISO 639-1 code embedded in HLS/DASH metadata, e.g. "en", "vi".
	Language string `json:"language"`

	// Normalize applies EBU R128 loudness normalization (-23 LUFS).
	// Useful for broadcast compliance.
	Normalize bool `json:"normalize"`
}

// DecoderConfig defines decoder behavior.
type DecoderConfig struct {
	// Name is the FFmpeg decoder name.
	// "" = let FFmpeg choose automatically.
	// Examples: "h264_cuvid", "h264_qsv".
	Name string `json:"name,omitempty"`
}

// TranscoderGlobalConfig holds global transcoder parameters.
type TranscoderGlobalConfig struct {
	// External indicates transcoding is handled by an external pipeline.
	External bool `json:"external"`

	// HW selects the acceleration backend.
	HW HWAccel `json:"hw"`

	// FPS sets output framerate. 0 = source/default.
	FPS int `json:"fps"`

	// GOP sets keyframe interval in frames. 0 = encoder default.
	GOP int `json:"gop"`

	// DeviceID selects hardware device index.
	DeviceID int `json:"deviceid"`
}

// VideoTranscodeConfig defines video transcoding behavior.
type VideoTranscodeConfig struct {
	// Copy copies origin video without re-encoding.
	Copy bool `json:"copy"`

	// Profiles defines ABR renditions when re-encoding.
	Profiles []VideoProfile `json:"profiles,omitempty"`
}

// AudioTranscodeConfig defines audio transcoding behavior.
type AudioTranscodeConfig struct {
	// Copy copies origin audio without re-encoding.
	Copy bool `json:"copy"`

	Codec AudioCodec `json:"codec"`

	// Bitrate is the audio bitrate in kbps.
	Bitrate int `json:"bitrate"`

	// SampleRate is output sample rate in Hz.
	SampleRate int `json:"sample_rate"`

	// Channels: 1 = mono, 2 = stereo, 6 = 5.1.
	Channels int `json:"channels"`

	// Language is ISO 639-1 code, e.g. "en", "vi".
	Language string `json:"language"`

	// Normalize applies EBU R128 loudness normalization.
	Normalize bool `json:"normalize"`
}

// TranscoderConfig is the complete transcoding configuration for a stream.
type TranscoderConfig struct {
	Video   VideoTranscodeConfig   `json:"video"`
	Audio   AudioTranscodeConfig   `json:"audio"`
	Decoder DecoderConfig          `json:"decoder"`
	Global  TranscoderGlobalConfig `json:"global"`

	// ExtraArgs are raw FFmpeg arguments appended after the generated command.
	// Use with caution — may conflict with generated arguments.
	ExtraArgs []string `json:"extra_args,omitempty"`
}
