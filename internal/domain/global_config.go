package domain

import "github.com/ntt0601zcoder/open-streamer/config"

// GlobalConfig holds all runtime configuration that is persisted in the store
// (as opposed to config.StorageConfig which is bootstrap-only from config.yaml/env).
//
// Pointer fields: nil means the section is not configured and the corresponding
// service should not start.  This allows users to enable/disable entire subsystems
// by adding or removing config sections via the API.
type GlobalConfig struct {
	Server     *config.ServerConfig     `json:"server,omitempty"`
	Ingestor   *config.IngestorConfig   `json:"ingestor,omitempty"`
	Buffer     *config.BufferConfig     `json:"buffer,omitempty"`
	Transcoder *config.TranscoderConfig `json:"transcoder,omitempty"`
	Publisher  *config.PublisherConfig  `json:"publisher,omitempty"`
	Manager    *config.ManagerConfig    `json:"manager,omitempty"`
	Hooks      *config.HooksConfig      `json:"hooks,omitempty"`
	Metrics    *config.MetricsConfig    `json:"metrics,omitempty"`
	Log        *config.LogConfig        `json:"log,omitempty"`
}

// DefaultGlobalConfig returns a GlobalConfig populated with the same defaults
// that viper/setDefaults previously provided.  Used to seed the store on first boot.
func DefaultGlobalConfig() *GlobalConfig {
	return &GlobalConfig{
		Server: &config.ServerConfig{
			HTTPAddr: ":8080",
			CORS: config.CORSConfig{
				Enabled:        true,
				AllowedOrigins: []string{"*"},
				MaxAge:         300,
			},
		},
		Ingestor: &config.IngestorConfig{
			RTMPEnabled:         true,
			RTMPAddr:            ":1935",
			SRTEnabled:          true,
			SRTAddr:             ":9999",
			HLSMaxSegmentBuffer: 8,
		},
		Buffer: &config.BufferConfig{
			Capacity: 1000,
		},
		Transcoder: &config.TranscoderConfig{
			MaxWorkers:  4,
			FFmpegPath:  "ffmpeg",
			MaxRestarts: 5,
		},
		Publisher: &config.PublisherConfig{
			HLS: config.PublisherHLSConfig{
				Dir:            "./out/hls",
				LiveEphemeral:  true,
				LiveSegmentSec: 4,
				LiveWindow:     10,
				LiveHistory:    20,
			},
			DASH: config.PublisherDASHConfig{
				Dir:            "./out/dash",
				LiveEphemeral:  true,
				LiveSegmentSec: 4,
				LiveWindow:     10,
				LiveHistory:    20,
			},
			RTSP: config.PublisherRTSPConfig{
				ListenHost: "0.0.0.0",
				PortMin:    18554,
				PortMax:    18653,
				Transport:  "tcp",
			},
			RTMP: config.PublisherRTMPServeConfig{
				ListenHost: "0.0.0.0",
			},
			SRT: config.PublisherSRTListenerConfig{
				ListenHost: "0.0.0.0",
				LatencyMS:  120,
			},
		},
		Manager: &config.ManagerConfig{
			InputPacketTimeoutSec: 30,
		},
		Hooks: &config.HooksConfig{
			WorkerCount: 4,
		},
		Metrics: &config.MetricsConfig{
			Addr: ":9091",
			Path: "/metrics",
		},
		Log: &config.LogConfig{
			Level:  "info",
			Format: "text",
		},
	}
}
