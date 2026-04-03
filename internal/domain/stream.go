package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// MaxStreamCodeLen is the maximum length of a user-defined stream code.
const MaxStreamCodeLen = 128

var streamCodePattern = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// StreamCode is the user-assigned primary key for a stream.
// Allowed characters: a-z, A-Z, 0-9, underscore.
type StreamCode string

// ValidateStreamCode reports whether s is a non-empty valid stream code.
func ValidateStreamCode(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("stream code is required")
	}
	if len(s) > MaxStreamCodeLen {
		return fmt.Errorf("stream code exceeds max length %d", MaxStreamCodeLen)
	}
	if !streamCodePattern.MatchString(s) {
		return errors.New("stream code must contain only a-z, A-Z, 0-9, and _")
	}
	return nil
}

// StreamStatus represents the lifecycle state of a stream.
type StreamStatus string

// StreamStatus values are used by the stream manager and API.
const (
	StatusIdle     StreamStatus = "idle"
	StatusActive   StreamStatus = "active"
	StatusDegraded StreamStatus = "degraded"
	StatusStopped  StreamStatus = "stopped"
)

// Stream is the central domain entity.
// It describes everything needed to ingest, process, and deliver a live stream.
type Stream struct {
	// Code is the unique key chosen by the user ([a-zA-Z0-9_]).
	Code StreamCode `json:"code"`

	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`

	// StreamKey is used to authenticate RTMP/SRT push ingest.
	StreamKey string `json:"stream_key"`

	// Status is the runtime lifecycle state (not persisted between restarts).
	Status StreamStatus `json:"status"`

	// Disabled when true excludes the stream from server bootstrap and rejects pipeline Start.
	Disabled bool `json:"disabled"`

	// Inputs are the available ingest sources ordered by Priority.
	// The Stream Manager monitors health and switches between them on failure.
	Inputs []Input `json:"inputs"`

	// Transcoder controls encoding/decoding settings.
	// nil means no transcoding for this stream.
	Transcoder *TranscoderConfig `json:"transcoder,omitempty"`

	// Protocols defines which delivery protocols are opened for this stream.
	// The server opens a listener/packager for each enabled protocol.
	// Protocol-level config (ports, segment duration, CDN URL) lives in server config.
	Protocols OutputProtocols `json:"protocols"`

	// Push is the list of external destinations the server actively pushes to.
	// Each entry defines one push target (YouTube, Facebook, Twitch, CDN relay, etc.).
	Push []PushDestination `json:"push"`

	// DVR overrides the global DVR settings for this specific stream.
	// If nil, the global config is used (when DVR is enabled globally).
	DVR *StreamDVRConfig `json:"dvr,omitempty"`

	// Watermark is an optional text or image overlay applied before encoding.
	Watermark *WatermarkConfig `json:"watermark,omitempty"`

	// Thumbnail controls periodic screenshot generation for preview images.
	Thumbnail *ThumbnailConfig `json:"thumbnail,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ValidateInputPriorities enforces that input priorities are contiguous and sorted.
// For N inputs, expected priorities are exactly 0..N-1 in ascending order.
func (s *Stream) ValidateInputPriorities() error {
	if s == nil {
		return nil
	}
	for i, in := range s.Inputs {
		if in.Priority != i {
			return fmt.Errorf("input priority must be %d at index %d", i, i)
		}
	}
	return nil
}

// ValidateUniqueInputs enforces that inputs in one stream are not duplicated.
// Two inputs are considered duplicates if their URL (trimmed) is identical.
func (s *Stream) ValidateUniqueInputs() error {
	if s == nil {
		return nil
	}
	seen := make(map[string]int, len(s.Inputs))
	for i, in := range s.Inputs {
		key := strings.TrimSpace(in.URL)
		if prev, ok := seen[key]; ok {
			return fmt.Errorf("duplicate input URL at indexes %d and %d", prev, i)
		}
		seen[key] = i
	}
	return nil
}
