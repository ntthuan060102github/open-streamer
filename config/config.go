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
	InputPacketTimeoutSec int `mapstructure:"input_packet_timeout_sec" json:"input_packet_timeout_sec" yaml:"input_packet_timeout_sec"`
}

// ServerConfig holds HTTP/gRPC server settings.
type ServerConfig struct {
	HTTPAddr string     `mapstructure:"http_addr" json:"http_addr" yaml:"http_addr"`
	CORS     CORSConfig `mapstructure:"cors" json:"cors" yaml:"cors"`
}

// CORSConfig controls Cross-Origin Resource Sharing for the HTTP API and
// static media routes mounted on the same listener.
type CORSConfig struct {
	// Enabled turns CORS middleware on for the HTTP listener.
	Enabled bool `mapstructure:"enabled" json:"enabled" yaml:"enabled"`
	// AllowedOrigins lists values for Access-Control-Allow-Origin. Use "*"
	// for any origin (cannot be used together with AllowCredentials).
	AllowedOrigins []string `mapstructure:"allowed_origins" json:"allowed_origins" yaml:"allowed_origins"`
	// AllowedMethods lists Access-Control-Allow-Methods; empty uses a REST default set.
	AllowedMethods []string `mapstructure:"allowed_methods" json:"allowed_methods,omitempty" yaml:"allowed_methods,omitempty"`
	// AllowedHeaders lists Access-Control-Allow-Headers; empty uses a common API default set.
	AllowedHeaders []string `mapstructure:"allowed_headers" json:"allowed_headers,omitempty" yaml:"allowed_headers,omitempty"`
	// ExposedHeaders lists Access-Control-Expose-Headers.
	ExposedHeaders []string `mapstructure:"exposed_headers" json:"exposed_headers,omitempty" yaml:"exposed_headers,omitempty"`
	// AllowCredentials sets Access-Control-Allow-Credentials. Must be false
	// when AllowedOrigins contains "*".
	AllowCredentials bool `mapstructure:"allow_credentials" json:"allow_credentials" yaml:"allow_credentials"`
	// MaxAge is the preflight cache duration in seconds (Access-Control-Max-Age).
	MaxAge int `mapstructure:"max_age" json:"max_age" yaml:"max_age"`
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
//
// Network listeners (RTMP, SRT, RTSP) are configured in the top-level
// ListenersConfig and shared with the publisher, so the same port serves both
// push (ingest) and pull (play) traffic.
type IngestorConfig struct {
	// HLSMaxSegmentBuffer caps the number of pre-fetched HLS segments held in memory.
	// This is a server-wide memory guard, not a per-stream policy.
	HLSMaxSegmentBuffer int `mapstructure:"hls_max_segment_buffer" json:"hls_max_segment_buffer" yaml:"hls_max_segment_buffer"` // default 8
}

// BufferConfig controls the in-memory ring buffer.
type BufferConfig struct {
	// Capacity is the number of MPEG-TS packets per stream buffer.
	Capacity int `mapstructure:"capacity" json:"capacity" yaml:"capacity"`
}

// TranscoderConfig controls FFmpeg worker pool behaviour.
type TranscoderConfig struct {
	// MaxWorkers caps the number of concurrent FFmpeg processes.
	MaxWorkers int    `mapstructure:"max_workers" json:"max_workers" yaml:"max_workers"`
	FFmpegPath string `mapstructure:"ffmpeg_path" json:"ffmpeg_path" yaml:"ffmpeg_path"`
}

// PublisherConfig controls filesystem-based output delivery (HLS, DASH).
// Live network listeners (RTSP, RTMP, SRT) are configured in ListenersConfig
// because the same port serves both ingest and playback.
type PublisherConfig struct {
	HLS  PublisherHLSConfig  `mapstructure:"hls" json:"hls" yaml:"hls"`
	DASH PublisherDASHConfig `mapstructure:"dash" json:"dash" yaml:"dash"`
}

// PublisherHLSConfig is filesystem + live packaging for Apple HLS (m3u8 + segments).
type PublisherHLSConfig struct {
	Dir string `mapstructure:"dir" json:"dir" yaml:"dir"`
	// LiveEphemeral enables bounded retention (sliding manifest, delete old segments).
	LiveEphemeral bool `mapstructure:"live_ephemeral" json:"live_ephemeral" yaml:"live_ephemeral"`
	// LiveSegmentSec is segment duration in seconds.
	LiveSegmentSec int `mapstructure:"live_segment_sec" json:"live_segment_sec" yaml:"live_segment_sec"`
	// LiveWindow is the sliding window size (segments) in the playlist.
	LiveWindow int `mapstructure:"live_window" json:"live_window" yaml:"live_window"`
	// LiveHistory is extra segments kept on disk after they leave the manifest.
	LiveHistory int `mapstructure:"live_history" json:"live_history" yaml:"live_history"`
}

// PublisherDASHConfig is filesystem + live packaging for MPEG-DASH (dynamic MPD + ISO BMFF init/media .m4s).
// Dir must be set and must not match PublisherHLSConfig.Dir (separate subscribers and segment files).
type PublisherDASHConfig struct {
	Dir string `mapstructure:"dir" json:"dir" yaml:"dir"`
	// Live* mirror HLS packaging semantics for the DASH muxer.
	LiveEphemeral  bool `mapstructure:"live_ephemeral" json:"live_ephemeral" yaml:"live_ephemeral"`
	LiveSegmentSec int  `mapstructure:"live_segment_sec" json:"live_segment_sec" yaml:"live_segment_sec"`
	LiveWindow     int  `mapstructure:"live_window" json:"live_window" yaml:"live_window"`
	LiveHistory    int  `mapstructure:"live_history" json:"live_history" yaml:"live_history"`
}

// ListenersConfig groups all live network listeners.
//
// Each protocol uses ONE port that serves both directions of traffic:
//   - RTMP: encoders push, players pull, on the same TCP port (default 1935).
//   - RTSP: clients pull live streams (default 554).
//   - SRT:  encoders push or clients pull on the same UDP port (default 9999),
//     dispatched by the SRT streamid mode flag.
//
// Setting Enabled=false (or leaving the section out entirely) disables that
// protocol's listener for both ingest and playback.
type ListenersConfig struct {
	RTMP RTMPListenerConfig `mapstructure:"rtmp" json:"rtmp" yaml:"rtmp"`
	RTSP RTSPListenerConfig `mapstructure:"rtsp" json:"rtsp" yaml:"rtsp"`
	SRT  SRTListenerConfig  `mapstructure:"srt"  json:"srt"  yaml:"srt"`
}

// RTMPListenerConfig is the shared RTMP listener used by both ingest and play.
// Encoders publish to rtmp://host:port/<key>; players pull from
// rtmp://host:port/<app>/<key>.
type RTMPListenerConfig struct {
	Enabled    bool   `mapstructure:"enabled" json:"enabled" yaml:"enabled"`
	ListenHost string `mapstructure:"listen_host" json:"listen_host" yaml:"listen_host"`
	Port       int    `mapstructure:"port" json:"port" yaml:"port"` // default 1935
}

// RTSPListenerConfig is the RTSP listener (publisher-side; ingest is pull-only).
// Clients use rtsp://host:port/live/<stream_code>.
type RTSPListenerConfig struct {
	Enabled    bool   `mapstructure:"enabled" json:"enabled" yaml:"enabled"`
	ListenHost string `mapstructure:"listen_host" json:"listen_host" yaml:"listen_host"`
	Port       int    `mapstructure:"port" json:"port" yaml:"port"` // default 554
	// Transport is "tcp" (default) or "udp" for the RTSP muxer.
	Transport string `mapstructure:"transport" json:"transport" yaml:"transport"`
}

// SRTListenerConfig is the shared SRT listener.
// Players set streamid=live/<stream_code>; publish ingest is dispatched by the
// streamid mode flag (mode=publish vs mode=request).
type SRTListenerConfig struct {
	Enabled    bool   `mapstructure:"enabled" json:"enabled" yaml:"enabled"`
	ListenHost string `mapstructure:"listen_host" json:"listen_host" yaml:"listen_host"`
	Port       int    `mapstructure:"port" json:"port" yaml:"port"` // default 9999
	// LatencyMS is the SRT latency in milliseconds applied to the listener.
	LatencyMS int `mapstructure:"latency_ms" json:"latency_ms" yaml:"latency_ms"`
}

// HooksConfig controls the hook dispatcher worker pool.
// Per-hook settings (max retries, timeout, secret, event filter) are
// configured on each Hook via the API.
type HooksConfig struct {
	// WorkerCount is the number of concurrent hook delivery goroutines.
	WorkerCount int `mapstructure:"worker_count" json:"worker_count" yaml:"worker_count"`

	// KafkaBrokers is the list of Kafka broker addresses used by all Kafka-type hooks.
	// Example: ["localhost:9092", "broker2:9092"].
	// Empty = Kafka hooks are not available.
	KafkaBrokers []string `mapstructure:"kafka_brokers" json:"kafka_brokers,omitempty" yaml:"kafka_brokers,omitempty"`
}

// LogConfig controls structured logging output.
type LogConfig struct {
	Level  string `mapstructure:"level" json:"level" yaml:"level"`    // debug | info | warn | error
	Format string `mapstructure:"format" json:"format" yaml:"format"` // text | json
}

// LoadStorage reads only the StorageConfig from environment variables and an optional
// config file. All other config sections are managed by GlobalConfig in the store.
func LoadStorage() (StorageConfig, error) {
	v := viper.New()

	v.SetEnvPrefix("OPEN_STREAMER")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	v.SetDefault("storage.driver", "json")
	v.SetDefault("storage.json_dir", "./test_data")
	v.SetDefault("storage.yaml_dir", "./test_data")

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
