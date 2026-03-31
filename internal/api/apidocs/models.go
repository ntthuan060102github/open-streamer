// Package apidocs holds OpenAPI response/request shapes referenced from swag comments.
package apidocs

import "github.com/open-streamer/open-streamer/internal/domain"

// ErrorBody matches handler writeError JSON shape.
type ErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// StreamData wraps a single stream.
type StreamData struct {
	Data *domain.Stream `json:"data"`
}

// StreamPutRequest is request body for PUT /streams/{code}.
// code is taken from path param; created_at/updated_at are server-managed.
type StreamPutRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`

	StreamKey string `json:"stream_key"`

	Inputs []domain.Input `json:"inputs"`

	Transcoder *domain.TranscoderConfig `json:"transcoder,omitempty"`
	Protocols  domain.OutputProtocols   `json:"protocols"`
	Push       []domain.PushDestination `json:"push"`
	DVR        *domain.StreamDVRConfig  `json:"dvr,omitempty"`
	Watermark  *domain.WatermarkConfig  `json:"watermark,omitempty"`
	Thumbnail  *domain.ThumbnailConfig  `json:"thumbnail,omitempty"`
}

// StreamList wraps listed streams.
type StreamList struct {
	Data  []*domain.Stream `json:"data"`
	Total int              `json:"total"`
}

// StreamStatusData combines persisted stream, pipeline flag, and manager runtime snapshot.
type StreamStatusData struct {
	Data map[string]any `json:"data"`
}

// StreamActionData is returned by POST .../start and .../stop.
type StreamActionData struct {
	Data StreamActionInner `json:"data"`
}

// StreamActionInner holds status string for start/stop responses.
type StreamActionInner struct {
	Status string `json:"status"`
}

// InputData wraps inputs array.
type InputData struct {
	Data []domain.Input `json:"data"`
}

// RecordingData wraps a recording.
type RecordingData struct {
	Data *domain.Recording `json:"data"`
}

// RecordingList wraps recordings for a stream.
type RecordingList struct {
	Data  []*domain.Recording `json:"data"`
	Total int                 `json:"total"`
}

// HookData wraps a hook.
type HookData struct {
	Data *domain.Hook `json:"data"`
}

// HookList wraps hooks.
type HookList struct {
	Data  []*domain.Hook `json:"data"`
	Total int            `json:"total"`
}

// HookTestInner is the payload for hook test success.
type HookTestInner struct {
	Status string `json:"status"`
	At     string `json:"at"`
}

// HookTestData is returned by POST .../test.
type HookTestData struct {
	Data HookTestInner `json:"data"`
}
