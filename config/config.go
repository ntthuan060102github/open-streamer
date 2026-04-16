// Package config holds the root configuration struct and Viper-based loading.
package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// ManagerConfig controls Stream Manager failover and input health checks.
type ManagerConfig struct {
	// InputPacketTimeoutSec is the maximum gap without a successful read on the
	// active input before it is marked failed. Pull protocols that deliver in
	// bursts (e.g. HLS: one segment per Read) need this at least as large as the
	// typical interval between reads (segment duration + playlist poll), or a
	// healthy primary will be falsely failed over to a lower priority.
	InputPacketTimeoutSec int `mapstructure:"input_packet_timeout_sec"`
}

// ServerConfig holds HTTP/gRPC server settings.
type ServerConfig struct {
	HTTPAddr string     `mapstructure:"http_addr"`
	CORS     CORSConfig `mapstructure:"cors"`
}

// CORSConfig controls Cross-Origin Resource Sharing for the HTTP API and
// static media routes mounted on the same listener.
type CORSConfig struct {
	// Enabled turns CORS middleware on for the HTTP listener.
	Enabled bool `mapstructure:"enabled"`
	// AllowedOrigins lists values for Access-Control-Allow-Origin. Use "*"
	// for any origin (cannot be used together with AllowCredentials).
	AllowedOrigins []string `mapstructure:"allowed_origins"`
	// AllowedMethods lists Access-Control-Allow-Methods; empty uses a REST default set.
	AllowedMethods []string `mapstructure:"allowed_methods"`
	// AllowedHeaders lists Access-Control-Allow-Headers; empty uses a common API default set.
	AllowedHeaders []string `mapstructure:"allowed_headers"`
	// ExposedHeaders lists Access-Control-Expose-Headers.
	ExposedHeaders []string `mapstructure:"exposed_headers"`
	// AllowCredentials sets Access-Control-Allow-Credentials. Must be false
	// when AllowedOrigins contains "*".
	AllowCredentials bool `mapstructure:"allow_credentials"`
	// MaxAge is the preflight cache duration in seconds (Access-Control-Max-Age).
	MaxAge int `mapstructure:"max_age"`
}

// StorageConfig selects the storage backend and its connection details.
type StorageConfig struct {
	// Driver selects the backend: "json" | "yaml"
	Driver string `mapstructure:"driver"`

	// JSON backend
	JSONDir string `mapstructure:"json_dir"`

	// YAML backend
	YAMLDir string `mapstructure:"yaml_dir"`
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
	// MaxRestarts is the maximum number of consecutive FFmpeg crashes allowed per
	// profile before the transcoder gives up and triggers a fatal callback.
	// 0 = unlimited retries (not recommended for production).
	MaxRestarts int `mapstructure:"max_restarts"`
}

// PublisherConfig controls output delivery; settings are grouped by protocol.
type PublisherConfig struct {
	HLS  PublisherHLSConfig         `mapstructure:"hls"`
	DASH PublisherDASHConfig        `mapstructure:"dash"`
	RTSP PublisherRTSPConfig        `mapstructure:"rtsp"`
	RTMP PublisherRTMPServeConfig   `mapstructure:"rtmp"`
	SRT  PublisherSRTListenerConfig `mapstructure:"srt"`
}

// PublisherHLSConfig is filesystem + live packaging for Apple HLS (m3u8 + segments).
type PublisherHLSConfig struct {
	Dir     string `mapstructure:"dir"`
	BaseURL string `mapstructure:"base_url"`
	// LiveEphemeral enables bounded retention (sliding manifest, delete old segments).
	LiveEphemeral bool `mapstructure:"live_ephemeral"`
	// LiveSegmentSec is segment duration in seconds.
	LiveSegmentSec int `mapstructure:"live_segment_sec"`
	// LiveWindow is the sliding window size (segments) in the playlist.
	LiveWindow int `mapstructure:"live_window"`
	// LiveHistory is extra segments kept on disk after they leave the manifest.
	LiveHistory int `mapstructure:"live_history"`
}

// PublisherDASHConfig is filesystem + live packaging for MPEG-DASH (dynamic MPD + ISO BMFF init/media .m4s).
// Dir must be set and must not match PublisherHLSConfig.Dir (separate subscribers and segment files).
type PublisherDASHConfig struct {
	Dir string `mapstructure:"dir"`
	// Live* mirror HLS packaging semantics for the DASH muxer.
	LiveEphemeral  bool `mapstructure:"live_ephemeral"`
	LiveSegmentSec int  `mapstructure:"live_segment_sec"`
	LiveWindow     int  `mapstructure:"live_window"`
	LiveHistory    int  `mapstructure:"live_history"`
}

// PublisherRTSPConfig is native RTSP listen mode (protocols.rtsp).
// All streams share PortMin; clients use rtsp://host:PortMin/live/<stream_code>.
type PublisherRTSPConfig struct {
	// ListenHost is the bind address (empty = 0.0.0.0).
	ListenHost string `mapstructure:"listen_host"`
	// PortMin is the single RTSP listen port (PortMax is unused for publisher RTSP).
	PortMin int `mapstructure:"port_min"`
	PortMax int `mapstructure:"port_max"`
	// Transport is "tcp" (default) or "udp" for the RTSP muxer.
	Transport string `mapstructure:"transport"`
}

// PublisherRTMPServeConfig is native RTMP listen output for viewers (not ingestor push).
// Clients use rtmp://host:Port/live/<stream_code>.
// Keep Port distinct from ingestor.rtmp_addr (default :1935).
type PublisherRTMPServeConfig struct {
	ListenHost string `mapstructure:"listen_host"`
	// Port is the single RTMP listen port. 0 = disabled.
	Port int `mapstructure:"port"`
}

// PublisherSRTListenerConfig is native SRT listener (protocols.srt).
// Clients set streamid=live/<stream_code> (or a bare valid stream code).
type PublisherSRTListenerConfig struct {
	ListenHost string `mapstructure:"listen_host"`
	// Port is the single SRT listen port. 0 = disabled.
	Port int `mapstructure:"port"`
	// LatencyMS is the SRT latency in milliseconds for listener URLs.
	LatencyMS int `mapstructure:"latency_ms"`
}

// HooksConfig controls the hook dispatcher worker pool.
// Per-hook settings (max retries, timeout, secret, event filter) are
// configured on each Hook via the API.
type HooksConfig struct {
	// WorkerCount is the number of concurrent hook delivery goroutines.
	WorkerCount int `mapstructure:"worker_count"`

	// KafkaBrokers is the list of Kafka broker addresses used by all Kafka-type hooks.
	// Example: ["localhost:9092", "broker2:9092"].
	// Empty = Kafka hooks are not available.
	KafkaBrokers []string `mapstructure:"kafka_brokers"`
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

// LoadStorage reads only the StorageConfig from environment variables and an optional
// config file. All other config sections are managed by GlobalConfig in the store.
func LoadStorage() (StorageConfig, error) {
	v := viper.New()

	v.SetEnvPrefix("OPEN_STREAMER")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	v.SetDefault("storage.driver", "yaml")
	v.SetDefault("storage.json_dir", "./data")
	v.SetDefault("storage.yaml_dir", "./data")

	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/open-streamer")

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return StorageConfig{}, fmt.Errorf("config: read: %w", err)
		}
	}

	var wrapper struct {
		Storage StorageConfig `mapstructure:"storage"`
	}
	if err := v.Unmarshal(&wrapper); err != nil {
		return StorageConfig{}, fmt.Errorf("config: unmarshal: %w", err)
	}

	return wrapper.Storage, nil
}
