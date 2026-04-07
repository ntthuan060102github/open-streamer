// Package storetest provides shared test fixtures for all store implementations.
package storetest

import (
	"time"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

// NewFullStream returns a Stream with every field populated.
func NewFullStream(code domain.StreamCode) *domain.Stream {
	return &domain.Stream{
		Code:        code,
		Name:        "Test Stream",
		Description: "A test stream with all fields set",
		Tags:        []string{"live", "test", "hd"},
		StreamKey:   "secret-key-123",
		Status:      domain.StatusActive,
		Disabled:    false,
		Inputs: []domain.Input{
			{
				URL:      "rtmp://source.example.com/live/key1",
				Priority: 0,
				Headers:  map[string]string{"Authorization": "Bearer tok1"},
				Params:   map[string]string{"passphrase": "abc"},
				Net: domain.InputNetConfig{
					ConnectTimeoutSec:    10,
					ReadTimeoutSec:       30,
					Reconnect:            true,
					ReconnectDelaySec:    2,
					ReconnectMaxDelaySec: 60,
					MaxReconnects:        5,
				},
			},
			{
				URL:      "rtmp://backup.example.com/live/key2",
				Priority: 1,
				Net: domain.InputNetConfig{
					Reconnect:            true,
					ReconnectDelaySec:    5,
					ReconnectMaxDelaySec: 120,
				},
			},
		},
		Transcoder: &domain.TranscoderConfig{
			Video: domain.VideoTranscodeConfig{
				Copy: false,
				Profiles: []domain.VideoProfile{
					{
						Width:            1920,
						Height:           1080,
						Bitrate:          4000,
						MaxBitrate:       5000,
						Framerate:        30,
						KeyframeInterval: 2,
						Codec:            domain.VideoCodecH264,
						Preset:           "veryfast",
						Profile:          "high",
						Level:            "4.1",
					},
					{
						Width:            1280,
						Height:           720,
						Bitrate:          2000,
						MaxBitrate:       2500,
						Framerate:        30,
						KeyframeInterval: 2,
						Codec:            domain.VideoCodecH264,
						Preset:           "veryfast",
						Profile:          "main",
						Level:            "4.0",
					},
				},
			},
			Audio: domain.AudioTranscodeConfig{
				Copy:       false,
				Codec:      domain.AudioCodecAAC,
				Bitrate:    128,
				SampleRate: 48000,
				Channels:   2,
				Language:   "en",
				Normalize:  true,
			},
			Decoder: domain.DecoderConfig{Name: "h264_cuvid"},
			Global: domain.TranscoderGlobalConfig{
	
				HW:       domain.HWAccelNVENC,
				FPS:      30,
				GOP:      60,
				DeviceID: 0,
			},
			ExtraArgs: []string{"-threads", "4"},
		},
		Protocols: domain.OutputProtocols{
			HLS:  true,
			DASH: true,
			RTSP: false,
			RTMP: true,
			SRT:  false,
			RTS:  false,
		},
		Push: []domain.PushDestination{
			{
				URL:             "rtmp://a.rtmp.youtube.com/live2/yt-key",
				Enabled:         true,
				TimeoutSec:      10,
				RetryTimeoutSec: 5,
				Limit:           3,
				Comment:         "YouTube Live",
			},
		},
		DVR: &domain.StreamDVRConfig{
			Enabled:         true,
			RetentionSec:    86400,
			SegmentDuration: 4,
			StoragePath:     "/data/dvr/test",
			MaxSizeGB:       10,
		},
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}
}

// NewFullRecording returns a Recording with every field populated, including StoppedAt.
func NewFullRecording(id domain.RecordingID, code domain.StreamCode) *domain.Recording {
	stoppedAt := time.Date(2026, 1, 2, 6, 0, 0, 0, time.UTC)
	return &domain.Recording{
		ID:         id,
		StreamCode: code,
		StartedAt:  time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		StoppedAt:  &stoppedAt,
		Status:     domain.RecordingStatusRecording,
		SegmentDir: "/data/dvr/test/segments",
	}
}

// NewFullHook returns a Hook with every field populated.
func NewFullHook(id domain.HookID) *domain.Hook {
	return &domain.Hook{
		ID:         id,
		Name:       "My Webhook",
		Type:       domain.HookTypeHTTP,
		Target:     "https://hooks.example.com/events",
		Secret:     "hmac-secret-xyz",
		EventTypes: []domain.EventType{"stream.started", "stream.stopped", "recording.started"},
		Enabled:    true,
		MaxRetries: 5,
		TimeoutSec: 15,
	}
}
