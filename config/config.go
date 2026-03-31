package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// Config is the root configuration for the entire application.
// Each module receives only its own sub-config.
type Config struct {
	Server     ServerConfig
	Storage    StorageConfig
	Ingestor   IngestorConfig
	Buffer     BufferConfig
	Transcoder TranscoderConfig
	Publisher  PublisherConfig
	DVR        DVRConfig
	Hooks      HooksConfig
	Metrics    MetricsConfig
	Log        LogConfig
}

// ServerConfig holds HTTP/gRPC server settings.
type ServerConfig struct {
	HTTPAddr string `mapstructure:"http_addr"`
	GRPCAddr string `mapstructure:"grpc_addr"`
}

// StorageConfig selects the storage backend and its connection details.
type StorageConfig struct {
	// Driver selects the backend: "json" | "sql" | "mongo"
	Driver string `mapstructure:"driver"`

	// JSON backend
	JSONDir string `mapstructure:"json_dir"`

	// SQL backend (Postgres / MySQL)
	SQLDSN string `mapstructure:"sql_dsn"`

	// MongoDB backend
	MongoURI      string `mapstructure:"mongo_uri"`
	MongoDatabase string `mapstructure:"mongo_database"`
}

// IngestorConfig controls server-level ingestion infrastructure.
// Per-input settings (timeouts, S3 region, SRT latency, etc.) are configured
// on each Input via the API and stored in the data storage.
type IngestorConfig struct {
	// RTMP push server — external encoders connect to us on this address.
	RTMPEnabled bool   `mapstructure:"rtmp_enabled"`
	RTMPAddr    string `mapstructure:"rtmp_addr"` // e.g. ":1935"

	// SRT push server — external encoders connect to our SRT listener.
	SRTEnabled bool   `mapstructure:"srt_enabled"`
	SRTAddr    string `mapstructure:"srt_addr"` // e.g. ":9999"

	// HLSMaxSegmentBuffer caps the number of pre-fetched HLS segments held in memory.
	// This is a server-wide memory guard, not a per-stream policy.
	HLSMaxSegmentBuffer int `mapstructure:"hls_max_segment_buffer"` // default 8
}

// BufferConfig controls the in-memory ring buffer.
type BufferConfig struct {
	// Capacity is the number of MPEG-TS packets per stream buffer.
	Capacity int `mapstructure:"capacity"`
}

// TranscoderConfig controls FFmpeg worker pool behaviour.
type TranscoderConfig struct {
	// MaxWorkers caps the number of concurrent FFmpeg processes.
	MaxWorkers int    `mapstructure:"max_workers"`
	FFmpegPath string `mapstructure:"ffmpeg_path"`
}

// PublisherConfig controls output delivery.
type PublisherConfig struct {
	HLSDir     string `mapstructure:"hls_dir"`
	DASHDir    string `mapstructure:"dash_dir"`
	HLSBaseURL string `mapstructure:"hls_base_url"`
	// LiveEphemeral enables low-retention live output for HLS/DASH.
	// It keeps only a tiny rolling window and aggressively deletes old segments.
	LiveEphemeral bool `mapstructure:"live_ephemeral"`
	// LiveSegmentSec is segment duration in seconds for live outputs.
	LiveSegmentSec int `mapstructure:"live_segment_sec"`
	// LiveWindow is the number of segments retained in live playlists/manifests.
	LiveWindow int `mapstructure:"live_window"`
	// LiveHistory is extra historical segments kept on disk/RAM for safety.
	// This helps slow clients recover when they briefly lag behind live edge.
	LiveHistory int `mapstructure:"live_history"`
}

// DVRConfig controls server-level DVR infrastructure.
// Per-stream DVR settings (segment duration, retention, storage path) are
// configured on each Stream via the API.
type DVRConfig struct {
	// Enabled is the global on/off switch for DVR. When false, no stream
	// can record regardless of its own DVR config.
	Enabled bool `mapstructure:"enabled"`

	// RootDir is the base directory where all stream recordings are stored.
	RootDir string `mapstructure:"root_dir"`
}

// HooksConfig controls the hook dispatcher worker pool.
// Per-hook settings (max retries, timeout, secret, event filter) are
// configured on each Hook via the API.
type HooksConfig struct {
	// WorkerCount is the number of concurrent hook delivery goroutines.
	WorkerCount int `mapstructure:"worker_count"`
}

// MetricsConfig controls Prometheus exposition.
type MetricsConfig struct {
	Addr string `mapstructure:"addr"`
	Path string `mapstructure:"path"`
}

// LogConfig controls structured logging output.
type LogConfig struct {
	Level  string `mapstructure:"level"`  // debug | info | warn | error
	Format string `mapstructure:"format"` // text | json
}

// Load reads configuration from environment variables and an optional config file.
// Environment variables take precedence; keys use OPEN_STREAMER_ prefix.
func Load() (*Config, error) {
	v := viper.New()

	v.SetEnvPrefix("OPEN_STREAMER")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	setDefaults(v)

	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/open-streamer")

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("config: read: %w", err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}

	return &cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("server.http_addr", ":8080")
	v.SetDefault("server.grpc_addr", ":9090")

	v.SetDefault("storage.driver", "json")
	v.SetDefault("storage.json_dir", "./data")
	v.SetDefault("storage.mongo_database", "open_streamer")

	v.SetDefault("buffer.capacity", 1000)

	v.SetDefault("ingestor.rtmp_enabled", true)
	v.SetDefault("ingestor.rtmp_addr", ":1935")
	v.SetDefault("ingestor.srt_enabled", true)
	v.SetDefault("ingestor.srt_addr", ":9999")
	v.SetDefault("ingestor.hls_max_segment_buffer", 8)

	v.SetDefault("transcoder.max_workers", 4)
	v.SetDefault("transcoder.ffmpeg_path", "ffmpeg")

	v.SetDefault("publisher.hls_dir", "./hls")
	v.SetDefault("publisher.dash_dir", "./dash")
	v.SetDefault("publisher.hls_base_url", "http://localhost:8080/hls")
	v.SetDefault("publisher.live_ephemeral", true)
	v.SetDefault("publisher.live_segment_sec", 2)
	v.SetDefault("publisher.live_window", 6)
	v.SetDefault("publisher.live_history", 3)

	v.SetDefault("dvr.enabled", false)
	v.SetDefault("dvr.root_dir", "./dvr")

	v.SetDefault("hooks.worker_count", 4)

	v.SetDefault("metrics.addr", ":9091")
	v.SetDefault("metrics.path", "/metrics")

	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "text")
}
