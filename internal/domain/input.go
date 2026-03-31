package domain

// Input is a single ingest source for a stream.
// Multiple inputs can be configured; the Stream Manager selects the active one
// based on Priority and runtime health (Alive flag).
//
// The only required field is URL.  The ingestor derives the ingest protocol
// and connection mode (pull vs push-listen) automatically from the URL scheme
// and host — no additional protocol configuration is needed.
//
// Supported URL formats:
//
//	Pull (server connects to remote source):
//	  rtmp://server.com/live/stream_key       RTMP pull from remote
//	  rtsp://camera.local:554/stream          RTSP pull (IP camera)
//	  http://cdn.example.com/live.ts          HTTP MPEG-TS stream
//	  https://cdn.example.com/playlist.m3u8   HLS pull (grafov m3u8 parser)
//	  udp://239.1.1.1:5000                    UDP multicast MPEG-TS
//	  srt://relay.example.com:9999            SRT pull (caller mode)
//	  file:///recordings/source.ts            local file (loops if ?loop=true)
//
//	Push (external encoder connects to our server):
//	  rtmp://0.0.0.0:1935/live/stream_key     RTMP push — our RTMP server listens
//	  srt://0.0.0.0:9999?streamid=stream_key  SRT push  — our SRT server listens
//
// Push mode is detected automatically when the URL host is a wildcard address
// (0.0.0.0, ::, empty) and the scheme is rtmp or srt.
type Input struct {
	// URL is the source endpoint. See the package doc for supported formats.
	URL string `json:"url"`

	// Priority determines failover order. Lower value = higher priority.
	// The Stream Manager always prefers the lowest-priority alive input.
	Priority int `json:"priority"`

	// Headers are arbitrary HTTP headers sent with every request for HTTP/HLS inputs.
	// Common uses:
	//   "Authorization": "Bearer <token>"
	//   "Authorization": "Basic <base64(user:pass)>"
	//   "X-Custom-Token": "secret"
	Headers map[string]string `json:"headers,omitempty"`

	// Params are extra URL query parameters merged into the source URL before
	// connecting. Used for protocols that carry credentials or options in the
	// query string (SRT ?passphrase=, S3 ?access_key= / ?secret_key=, etc.).
	//   "passphrase": "my-srt-passphrase"
	//   "access_key": "AKID..."   (S3)
	//   "secret_key": "wJal..."   (S3)
	Params map[string]string `json:"params,omitempty"`

	// Net controls reconnect and timeout behaviour.
	Net InputNetConfig `json:"net,omitempty"`

	// Alive is a runtime-only field updated by the Stream Manager health checker.
	// Not persisted to storage.
	Alive bool `json:"-"`
}

// InputNetConfig controls reconnect and timeout behaviour for an input.
type InputNetConfig struct {
	// ConnectTimeoutSec caps each HTTP round-trip (headers + full body) for pull
	// readers that use net/http (e.g. HLS playlist and segment GETs). Zero uses
	// the reader's default (30s for HLS).
	ConnectTimeoutSec int `json:"connect_timeout_sec,omitempty"`

	// ReadTimeoutSec is the max silence duration before declaring the input dead.
	ReadTimeoutSec int `json:"read_timeout_sec,omitempty"`

	// Reconnect enables automatic reconnection when the input drops.
	Reconnect bool `json:"reconnect,omitempty"`

	// ReconnectDelaySec is the initial delay before the first reconnect attempt.
	ReconnectDelaySec int `json:"reconnect_delay_sec,omitempty"`

	// ReconnectMaxDelaySec caps the exponential backoff delay.
	ReconnectMaxDelaySec int `json:"reconnect_max_delay_sec,omitempty"`

	// MaxReconnects is the total number of reconnect attempts (0 = unlimited).
	MaxReconnects int `json:"max_reconnects,omitempty"`
}
