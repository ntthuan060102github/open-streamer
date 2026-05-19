package domain

// PushStatus is the runtime state of a push destination.
type PushStatus string

// PushStatus values describe outbound publisher connectivity.
const (
	PushStatusIdle       PushStatus = "idle"
	PushStatusConnecting PushStatus = "connecting"
	PushStatusActive     PushStatus = "active"
	PushStatusRetrying   PushStatus = "retrying"
	PushStatusFailed     PushStatus = "failed"
	PushStatusDisabled   PushStatus = "disabled"
)

// PushDestination is an external endpoint the server actively pushes the stream to.
type PushDestination struct {
	// URL is the destination ingest endpoint.
	// Supported schemes:
	//   rtmp://  — plain TCP, default port 1935 (e.g. rtmp://rtmp.example.com/live2/{key})
	//   rtmps:// — TLS-wrapped RTMP, default port 443 (e.g. rtmps://rtmps.example.com:443/rtmp/{key})
	URL string `json:"url" yaml:"url"`

	// Enabled controls whether this destination is active.
	Enabled bool `json:"enabled" yaml:"enabled"`

	// TimeoutSec is the connection/write timeout in seconds.
	TimeoutSec int `json:"timeout_sec" yaml:"timeout_sec"`

	// RetryTimeoutSec is the delay between retry attempts in seconds.
	RetryTimeoutSec int `json:"retry_timeout_sec" yaml:"retry_timeout_sec"`

	// Limit is the maximum number of retry attempts. 0 = unlimited.
	Limit int `json:"limit" yaml:"limit"`

	// Comment is a human-readable note for this destination.
	Comment string `json:"comment" yaml:"comment"`

	// Status is a runtime-only field updated by the publisher.
	// Not persisted to storage.
	Status PushStatus `json:"status,omitempty" yaml:"-"`
}
