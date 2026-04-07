// Package config holds the root configuration struct and Viper-based loading.
package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// defaultPublisherListenHost is the default bind address for RTSP/RTMP/SRT publisher listeners.
const defaultPublisherListenHost = "0.0.0.0"

// Config is the root configuration for the entire application.
// Each module receives only its own sub-config.
type Config struct {
	Server     ServerConfig
	Storage    StorageConfig
	Ingestor   IngestorConfig
	Buffer     BufferConfig
	Transcoder TranscoderConfig
	Publisher  PublisherConfig
	Manager    ManagerConfig
	Hooks      HooksConfig
	Metrics    MetricsConfig
	Log        LogConfig
}

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
	v.SetDefault("transcoder.max_restarts", 5)

	v.SetDefault("manager.input_packet_timeout_sec", 30)

	v.SetDefault("publisher.hls.dir", "./out/hls")
	v.SetDefault("publisher.hls.base_url", "http://localhost:8080/hls")
	v.SetDefault("publisher.hls.live_ephemeral", true)
	v.SetDefault("publisher.hls.live_segment_sec", 4)
	v.SetDefault("publisher.hls.live_window", 10)
	v.SetDefault("publisher.hls.live_history", 20)

	v.SetDefault("publisher.dash.dir", "./out/dash")
	v.SetDefault("publisher.dash.live_ephemeral", true)
	v.SetDefault("publisher.dash.live_segment_sec", 4)
	v.SetDefault("publisher.dash.live_window", 10)
	v.SetDefault("publisher.dash.live_history", 20)

	v.SetDefault("publisher.rtsp.listen_host", defaultPublisherListenHost)
	v.SetDefault("publisher.rtsp.port_min", 18554)
	v.SetDefault("publisher.rtsp.port_max", 18653)
	v.SetDefault("publisher.rtsp.transport", "tcp")

	v.SetDefault("publisher.rtmp.listen_host", defaultPublisherListenHost)
	v.SetDefault("publisher.rtmp.port", 0) // 0 = disabled; set to e.g. 1936 to enable

	v.SetDefault("publisher.srt.listen_host", defaultPublisherListenHost)
	v.SetDefault("publisher.srt.port", 0) // 0 = disabled; set to e.g. 10000 to enable
	v.SetDefault("publisher.srt.latency_ms", 120)

	v.SetDefault("hooks.worker_count", 4)

	v.SetDefault("metrics.addr", ":9091")
	v.SetDefault("metrics.path", "/metrics")

	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "text")
}
