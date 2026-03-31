package domain

import "time"

// RecordingID is the unique identifier for a DVR recording.
type RecordingID string

// RecordingStatus represents the lifecycle state of a recording.
type RecordingStatus string

const (
	RecordingStatusRecording RecordingStatus = "recording"
	RecordingStatusStopped   RecordingStatus = "stopped"
	RecordingStatusFailed    RecordingStatus = "failed"
)

// Segment is a single MPEG-TS chunk produced by the DVR.
type Segment struct {
	Index     int           `json:"index"`
	Path      string        `json:"path"` // relative path under DVR root dir
	Duration  time.Duration `json:"duration" swaggertype:"string" example:"6s"`
	Size      int64         `json:"size"` // bytes
	CreatedAt time.Time     `json:"created_at"`
}

// Recording represents a DVR recording session for a stream.
type Recording struct {
	ID         RecordingID `json:"id"`
	StreamCode StreamCode  `json:"stream_code"`
	StartedAt time.Time       `json:"started_at"`
	StoppedAt *time.Time      `json:"stopped_at,omitempty"`
	Status    RecordingStatus `json:"status"`
	Segments  []Segment       `json:"segments"`

	// TotalSizeBytes is the sum of all segment sizes. Updated as segments are flushed.
	TotalSizeBytes int64 `json:"total_size_bytes"`
}

// StreamDVRConfig overrides the global DVR settings for a specific stream.
// All zero values mean "use the global default".
type StreamDVRConfig struct {
	Enabled bool `json:"enabled"`

	// RetentionHours overrides the global retention window.
	// 0 = keep forever (or use global setting).
	RetentionHours int `json:"retention_hours"`

	// SegmentDuration overrides the global segment length in seconds.
	// Must align with the transcoder KeyframeInterval.
	// 0 = use global default (typically 6s).
	SegmentDuration int `json:"segment_duration"`

	// StoragePath overrides the global DVR root directory for this stream.
	// "" = use global DVR root dir.
	StoragePath string `json:"storage_path"`

	// MaxSizeGB caps the total disk usage for this stream's recordings.
	// When exceeded, the oldest segments are pruned. 0 = no limit.
	MaxSizeGB float64 `json:"max_size_gb"`
}
