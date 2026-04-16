// Package apidocs holds OpenAPI response/request shapes referenced from swag comments.
package apidocs

import "github.com/ntt0601zcoder/open-streamer/internal/domain"

// ErrorBody matches handler writeError JSON shape.
type ErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// StreamRuntime holds live pipeline state overlaid on the persisted stream config.
// Null when the pipeline is not running.
type StreamRuntime struct {
	ActiveInputPriority int `json:"active_input_priority"`
}

// StreamResponse is the API representation of a stream returned by GET /streams and GET /streams/{code}.
// It adds runtime-computed fields on top of the persisted configuration.
type StreamResponse struct {
	domain.Stream
	Status  domain.StreamStatus `json:"status"`
	Runtime *StreamRuntime      `json:"runtime"`
}

// StreamData wraps a single stream response.
type StreamData struct {
	Data StreamResponse `json:"data"`
}

// StreamPutRequest is request body for PUT /streams/{code}.
// code is taken from path param; created_at/updated_at are server-managed.
type StreamPutRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`

	StreamKey string `json:"stream_key"`

	Disabled bool `json:"disabled"`

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
	Data  []StreamResponse `json:"data"`
	Total int              `json:"total"`
}

// InputSwitchRequest is the request body for POST /streams/{code}/inputs/switch.
type InputSwitchRequest struct {
	Priority int `json:"priority"`
}

// ConfigData wraps the GET /config response.
type ConfigData struct {
	HWAccels           []domain.HWAccel           `json:"hwAccels"`
	VideoCodecs        []domain.VideoCodec        `json:"videoCodecs"`
	AudioCodecs        []domain.AudioCodec        `json:"audioCodecs"`
	OutputProtocols    []string                   `json:"outputProtocols"`
	StreamStatuses     []domain.StreamStatus      `json:"streamStatuses"`
	WatermarkTypes     []domain.WatermarkType     `json:"watermarkTypes"`
	WatermarkPositions []domain.WatermarkPosition `json:"watermarkPositions"`
}

// ConfigUpdateResponse wraps the POST /config response.
type ConfigUpdateResponse struct {
	GlobalConfig *domain.GlobalConfig `json:"globalConfig"`
	Ports        ConfigPorts          `json:"ports"`
}

// ConfigPorts exposes listener port configuration so the UI can build output URLs.
type ConfigPorts struct {
	HTTPAddr string `json:"http_addr"`
	RTSPPort int    `json:"rtsp_port"`
	RTMPPort int    `json:"rtmp_port"`
	SRTPort  int    `json:"srt_port"`
}

// ErrorResponse wraps an error body for swagger documentation.
type ErrorResponse = ErrorBody

// StreamActionData is returned by POST .../restart.
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
