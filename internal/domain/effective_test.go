package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/config"
)

// ResolveVideoEncoder must mirror transcoder.normalizeVideoEncoder exactly
// — any divergence produces an "effective" view that lies about the real
// FFmpeg encoder. The cases below pin every routing branch.
func TestResolveVideoEncoder_HW_Routing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		codec VideoCodec
		hw    HWAccel
		want  string
	}{
		{"empty codec + nvenc → h264_nvenc", "", HWAccelNVENC, "h264_nvenc"},
		{"empty codec + cpu → libx264", "", HWAccelNone, "libx264"},
		{"empty codec + vaapi → libx264 (no implicit routing)", "", HWAccelVAAPI, "libx264"},
		{"h264 alias + nvenc → h264_nvenc", "h264", HWAccelNVENC, "h264_nvenc"},
		{"avc alias + cpu → libx264", "avc", HWAccelNone, "libx264"},
		{"h265 + nvenc → hevc_nvenc", "h265", HWAccelNVENC, "hevc_nvenc"},
		{"hevc + cpu → libx265", "hevc", HWAccelNone, "libx265"},
		{"vp9 → libvpx-vp9", "vp9", HWAccelNone, "libvpx-vp9"},
		{"av1 → libsvtav1", "av1", HWAccelNone, "libsvtav1"},
		{"explicit h264_nvenc preserved", "h264_nvenc", HWAccelNone, "h264_nvenc"},
		{"explicit h264_qsv preserved", "h264_qsv", HWAccelNone, "h264_qsv"},
		{"unknown garbage → libx264 fallback", "garbage", HWAccelNone, "libx264"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ResolveVideoEncoder(tt.codec, tt.hw)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveAudioEncoder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		codec AudioCodec
		want  string
	}{
		{"", "aac"},
		{AudioCodecCopy, "aac"},
		{AudioCodecAAC, "aac"},
		{AudioCodecMP3, "libmp3lame"},
		{AudioCodecOpus, "libopus"},
		{AudioCodecAC3, "ac3"},
		{"unknown", "aac"},
	}
	for _, tt := range cases {
		t.Run(string(tt.codec), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ResolveAudioEncoder(tt.codec))
		})
	}
}

func TestResolveResizeMode(t *testing.T) {
	t.Parallel()
	assert.Equal(t, ResizeModePad, ResolveResizeMode(""))
	assert.Equal(t, ResizeModePad, ResolveResizeMode("garbage"))
	assert.Equal(t, ResizeModeCrop, ResolveResizeMode("crop"))
	assert.Equal(t, ResizeModeCrop, ResolveResizeMode("CROP"))
	assert.Equal(t, ResizeModeStretch, ResolveResizeMode("stretch"))
	assert.Equal(t, ResizeModeFit, ResolveResizeMode("fit"))
	assert.Equal(t, ResizeModePad, ResolveResizeMode("pad"))
}

// EffectiveVideoProfile is the headline UX win: every "default" placeholder
// the UI used to render now becomes a concrete value. Lock the resolution
// for the most common rung shape.
func TestEffectiveVideoProfile_FillsCodecBitrateAndResize(t *testing.T) {
	t.Parallel()
	got := EffectiveVideoProfile(VideoProfile{
		Width:  1280,
		Height: 720,
	}, HWAccelNVENC)
	assert.Equal(t, VideoCodec("h264_nvenc"), got.Codec)
	assert.Equal(t, DefaultVideoBitrateK, got.Bitrate)
	assert.Equal(t, ResizeModePad, got.ResizeMode)
}

func TestEffectiveVideoProfile_PreservesExplicitValues(t *testing.T) {
	t.Parallel()
	bf := 2
	in := VideoProfile{
		Width:      1920,
		Height:     1080,
		Bitrate:    5000,
		Codec:      "h264_qsv",
		Preset:     "p5",
		Profile:    "high",
		Level:      "4.1",
		Bframes:    &bf,
		SAR:        "1:1",
		ResizeMode: ResizeModeCrop,
	}
	got := EffectiveVideoProfile(in, HWAccelNVENC)
	assert.Equal(t, VideoCodec("h264_qsv"), got.Codec, "explicit encoder name must not be rerouted")
	assert.Equal(t, 5000, got.Bitrate)
	assert.Equal(t, "p5", got.Preset)
	assert.Equal(t, "high", got.Profile)
	assert.Equal(t, "4.1", got.Level)
	assert.Equal(t, "1:1", got.SAR)
	assert.Equal(t, ResizeModeCrop, got.ResizeMode)
	require.NotNil(t, got.Bframes)
	assert.Equal(t, 2, *got.Bframes)
}

// Profile-level codec="copy" is meaningless when Video.Copy is false — the
// coordinator strips it to "" before reaching the transcoder so HW routing
// works. EffectiveVideoProfile must mirror that semantics so the UI shows
// the encoder that will actually run, not the dead "copy" alias.
func TestEffectiveVideoProfile_CodecCopyTreatedAsEmpty(t *testing.T) {
	t.Parallel()
	got := EffectiveVideoProfile(VideoProfile{
		Width: 1280, Height: 720, Codec: VideoCodecCopy,
	}, HWAccelNVENC)
	assert.Equal(t, VideoCodec("h264_nvenc"), got.Codec,
		"profile-level copy should resolve to the HW-routed encoder, not stay 'copy'")
}

func TestEffectiveAudioTranscodeConfig_FillsCodecAndBitrate(t *testing.T) {
	t.Parallel()
	got := EffectiveAudioTranscodeConfig(AudioTranscodeConfig{})
	assert.Equal(t, DefaultAudioCodec, got.Codec)
	assert.Equal(t, DefaultAudioBitrateK, got.Bitrate)
}

func TestEffectiveAudioTranscodeConfig_CopyPreserved(t *testing.T) {
	t.Parallel()
	in := AudioTranscodeConfig{Copy: true}
	got := EffectiveAudioTranscodeConfig(in)
	assert.Equal(t, AudioTranscodeConfig{Copy: true}, got,
		"audio.copy=true must NOT be overlaid with encode defaults — none of those flags would actually be emitted")
}

func TestEffectiveTranscoderConfig_NilReturnsNil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, EffectiveTranscoderConfig(nil))
}

// The transcoder's HW field decides per-profile encoder routing; verify the
// effective view applies the resolved Global.HW (not the user's empty
// value) when computing per-profile defaults. Otherwise an unset HW would
// still route to libx264 in the effective output even when the user set
// Global.HW=nvenc explicitly.
func TestEffectiveTranscoderConfig_PropagatesGlobalHWToProfiles(t *testing.T) {
	t.Parallel()
	tc := &TranscoderConfig{
		Global: TranscoderGlobalConfig{HW: HWAccelNVENC},
		Video: VideoTranscodeConfig{
			Profiles: []VideoProfile{{Width: 1280, Height: 720}},
		},
	}
	got := EffectiveTranscoderConfig(tc)
	require.Len(t, got.Video.Profiles, 1)
	assert.Equal(t, VideoCodec("h264_nvenc"), got.Video.Profiles[0].Codec)
}

// Mutation safety: callers must be free to inspect the resolved view
// without affecting the persisted Stream config.
func TestEffectiveTranscoderConfig_DoesNotMutateInput(t *testing.T) {
	t.Parallel()
	tc := &TranscoderConfig{
		Global: TranscoderGlobalConfig{HW: HWAccelNVENC},
		Video: VideoTranscodeConfig{
			Profiles: []VideoProfile{{Width: 1280, Height: 720}},
		},
	}
	_ = EffectiveTranscoderConfig(tc)
	// Original profile must keep its empty codec (no mutation).
	assert.Equal(t, VideoCodec(""), tc.Video.Profiles[0].Codec)
	assert.Equal(t, 0, tc.Video.Profiles[0].Bitrate)
}

func TestEffectiveStreamDVRConfig_FillsSegmentAndStoragePath(t *testing.T) {
	t.Parallel()
	got := EffectiveStreamDVRConfig(&StreamDVRConfig{Enabled: true}, "abc")
	require.NotNil(t, got)
	assert.Equal(t, DefaultDVRSegmentDuration, got.SegmentDuration)
	assert.Equal(t, DefaultDVRRoot+"/abc", got.StoragePath)
}

func TestEffectiveStreamDVRConfig_NilStaysNil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, EffectiveStreamDVRConfig(nil, "abc"))
}

func TestEffectivePushDestination_FillsTimeouts(t *testing.T) {
	t.Parallel()
	got := EffectivePushDestination(PushDestination{URL: "rtmp://a/b", Enabled: true})
	assert.Equal(t, DefaultPushTimeoutSec, got.TimeoutSec)
	assert.Equal(t, DefaultPushRetryTimeoutSec, got.RetryTimeoutSec)
}

func TestEffectiveHook_FillsRetriesAndTimeout(t *testing.T) {
	t.Parallel()
	got := EffectiveHook(&Hook{ID: "h"})
	require.NotNil(t, got)
	assert.Equal(t, DefaultHookMaxRetries, got.MaxRetries)
	assert.Equal(t, DefaultHookTimeoutSec, got.TimeoutSec)
}

func TestEffectiveBufferConfig_NilFillsDefault(t *testing.T) {
	t.Parallel()
	got := EffectiveBufferConfig(nil)
	require.NotNil(t, got)
	assert.Equal(t, DefaultBufferCapacity, got.Capacity)
}

func TestEffectiveManagerConfig_NilFillsDefault(t *testing.T) {
	t.Parallel()
	got := EffectiveManagerConfig(nil)
	require.NotNil(t, got)
	assert.Equal(t, DefaultInputPacketTimeoutSec, got.InputPacketTimeoutSec)
}

func TestEffectivePublisherConfig_FillsHLSAndDASH(t *testing.T) {
	t.Parallel()
	got := EffectivePublisherConfig(&config.PublisherConfig{})
	assert.Equal(t, DefaultLiveSegmentSec, got.HLS.LiveSegmentSec)
	assert.Equal(t, DefaultLiveWindow, got.HLS.LiveWindow)
	assert.Equal(t, DefaultLiveSegmentSec, got.DASH.LiveSegmentSec)
	assert.Equal(t, DefaultLiveWindow, got.DASH.LiveWindow)
}

// Disabled listeners must keep Port=0 — the UI uses Enabled, not the
// presence of a port, to know whether to render the section.
func TestEffectiveListenersConfig_DisabledKeepsPortZero(t *testing.T) {
	t.Parallel()
	got := EffectiveListenersConfig(&config.ListenersConfig{
		RTMP: config.RTMPListenerConfig{Enabled: false},
		RTSP: config.RTSPListenerConfig{Enabled: false},
		SRT:  config.SRTListenerConfig{Enabled: false},
	})
	assert.Equal(t, 0, got.RTMP.Port)
	assert.Equal(t, 0, got.RTSP.Port)
	assert.Equal(t, 0, got.SRT.Port)
}

func TestEffectiveListenersConfig_EnabledFillsStandardPorts(t *testing.T) {
	t.Parallel()
	got := EffectiveListenersConfig(&config.ListenersConfig{
		RTMP: config.RTMPListenerConfig{Enabled: true},
		RTSP: config.RTSPListenerConfig{Enabled: true},
		SRT:  config.SRTListenerConfig{Enabled: true},
	})
	assert.Equal(t, DefaultRTMPPort, got.RTMP.Port)
	assert.Equal(t, DefaultRTSPPort, got.RTSP.Port)
	assert.Equal(t, DefaultRTSPTransport, got.RTSP.Transport)
	assert.Equal(t, DefaultSRTPort, got.SRT.Port)
}

func TestEffectiveGlobalConfig_NilSectionsStayNil(t *testing.T) {
	t.Parallel()
	// Buffer / Manager / Publisher / Listeners are all nil → service is
	// intentionally disabled. Effective view must NOT silently materialize
	// them (would imply the service is on).
	got := EffectiveGlobalConfig(&GlobalConfig{})
	require.NotNil(t, got)
	assert.Nil(t, got.Buffer)
	assert.Nil(t, got.Manager)
	assert.Nil(t, got.Publisher)
	assert.Nil(t, got.Listeners)
}

func TestEffectiveStream_TopLevelDoesNotMutateOriginal(t *testing.T) {
	t.Parallel()
	s := &Stream{
		Code: "abc",
		Transcoder: &TranscoderConfig{
			Global: TranscoderGlobalConfig{HW: HWAccelNVENC},
			Video: VideoTranscodeConfig{
				Profiles: []VideoProfile{{Width: 1280, Height: 720}},
			},
		},
		DVR: &StreamDVRConfig{Enabled: true},
	}
	got := EffectiveStream(s)
	require.NotNil(t, got)
	require.NotNil(t, got.Transcoder)
	require.Len(t, got.Transcoder.Video.Profiles, 1)
	assert.Equal(t, VideoCodec("h264_nvenc"), got.Transcoder.Video.Profiles[0].Codec)
	assert.Equal(t, DefaultVideoBitrateK, got.Transcoder.Video.Profiles[0].Bitrate)
	assert.Equal(t, DefaultDVRRoot+"/abc", got.DVR.StoragePath)

	// Original is untouched.
	assert.Equal(t, VideoCodec(""), s.Transcoder.Video.Profiles[0].Codec)
	assert.Equal(t, 0, s.Transcoder.Video.Profiles[0].Bitrate)
	assert.Empty(t, s.DVR.StoragePath)
}
