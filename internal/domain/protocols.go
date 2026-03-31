package domain

// OutputProtocols defines which delivery protocols are opened for a stream.
// Each enabled protocol starts a corresponding listener or packager.
// Protocol-level settings (ports, segment duration, CDN URL, etc.)
// are configured globally in the server config.
type OutputProtocols struct {
	// HLS enables Apple HTTP Live Streaming (m3u8 + segments over HTTP).
	// Compatible with browsers, iOS, Android, Smart TVs.
	HLS bool `json:"hls"`

	// DASH enables MPEG-DASH packaging over HTTP.
	// Required for Widevine/PlayReady DRM.
	DASH bool `json:"dash"`

	// RTSP opens an RTSP listener for pull clients (VLC, broadcast tools).
	RTSP bool `json:"rtsp"`

	// RTMP opens an RTMP publish endpoint for legacy players/CDNs.
	RTMP bool `json:"rtmp"`

	// SRT opens an SRT listener port for contribution-quality pull.
	SRT bool `json:"srt"`

	// RTS enables WebRTC-based real-time streaming (WHEP endpoint).
	// Sub-second glass-to-glass latency.
	RTS bool `json:"rts"`
}
