package domain

import "time"

// EventType identifies the kind of domain event that occurred.
type EventType string

const (
	EventStreamCreated     EventType = "stream.created"
	EventStreamStarted     EventType = "stream.started"
	EventStreamStopped     EventType = "stream.stopped"
	EventStreamDeleted     EventType = "stream.deleted"
	EventInputDegraded     EventType = "input.degraded"
	EventInputFailed       EventType = "input.failed"
	EventInputFailover     EventType = "input.failover"
	EventRecordingStarted  EventType = "recording.started"
	EventRecordingStopped  EventType = "recording.stopped"
	EventRecordingFailed   EventType = "recording.failed"
	EventSegmentWritten    EventType = "segment.written"
)

// Event is an immutable fact describing a domain state change.
type Event struct {
	ID         string         `json:"id"` // UUID for idempotent delivery
	Type       EventType      `json:"type"`
	StreamCode StreamCode     `json:"stream_code"`
	OccurredAt time.Time      `json:"occurred_at"`
	Payload    map[string]any `json:"payload,omitempty"` // event-specific fields
}
