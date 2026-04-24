// Package protocol provides URL-based protocol detection and media stream utilities.
package protocol

import (
	"net"
	"net/url"
	"strings"
)

// Kind classifies a stream URL into a transport category used internally by
// the ingestor to choose the right reader implementation.
type Kind string

// Kind constants classify ingest URLs; see Detect.
const (
	KindUDP     Kind = "udp"     // raw MPEG-TS over UDP (unicast or multicast)
	KindHLS     Kind = "hls"     // HLS playlist pull over HTTP/HTTPS
	KindFile    Kind = "file"    // local filesystem path
	KindRTMP    Kind = "rtmp"    // RTMP / RTMPS (pull or push-listen)
	KindRTSP    Kind = "rtsp"    // RTSP pull
	KindSRT     Kind = "srt"     // SRT (pull caller or push listener)
	KindPublish Kind = "publish" // accept any push protocol; stream code is the routing key
	KindCopy    Kind = "copy"    // re-stream another in-process stream's published output
	KindMixer   Kind = "mixer"   // combine video from one in-process stream with audio from another
	KindUnknown Kind = "unknown"
)

// Detect returns the protocol Kind for the given URL.
// All classification is done purely from the scheme and URL structure — the
// caller never needs to specify the protocol manually.
//
//	rtmp://...                → KindRTMP
//	rtmps://...               → KindRTMP
//	srt://...                 → KindSRT
//	udp://...                 → KindUDP
//	rtsp:// or rtsps://...    → KindRTSP
//	http(s)://...*.m3u8       → KindHLS
//	file:// or /absolute/path → KindFile
//	publish://                → KindPublish (push-listen, any protocol)
//	copy://<stream_code>      → KindCopy   (re-stream another in-process stream)
//	mixer://<video>,<audio>   → KindMixer  (replace audio of one stream with another)
func Detect(rawURL string) Kind {
	u, err := url.Parse(rawURL)
	if err != nil {
		return KindUnknown
	}

	switch strings.ToLower(u.Scheme) {
	case "rtmp", "rtmps":
		return KindRTMP
	case "srt":
		return KindSRT
	case "udp":
		return KindUDP
	case "rtsp", "rtsps":
		return KindRTSP
	case "file":
		return KindFile
	case "publish":
		return KindPublish
	case "copy":
		return KindCopy
	case "mixer":
		return KindMixer
	case "http", "https":
		if strings.HasSuffix(strings.ToLower(u.Path), ".m3u8") ||
			strings.HasSuffix(strings.ToLower(u.Path), ".m3u") {
			return KindHLS
		}
		return KindUnknown
	case "":
		// Bare path — treat as local file.
		if strings.HasPrefix(rawURL, "/") {
			return KindFile
		}
	}

	return KindUnknown
}

// IsPushListen returns true when the URL signals that the server should
// accept incoming encoder connections (push mode) for a stream.
//
// Two forms are recognised:
//
//  1. publish:// — the preferred, protocol-agnostic form.  Encoders may push
//     via RTMP or SRT; the stream code is the only routing key needed.
//
//  2. Legacy wildcard-host form — rtmp://0.0.0.0:port/... or srt://0.0.0.0:port/...
//     Still recognised for backward compatibility.
//
// Examples:
//
//	publish://          → true  (preferred)
//	rtmp://0.0.0.0:1935 → true  (legacy)
//	srt://0.0.0.0:9999  → true  (legacy)
//	rtmp://server.com   → false (remote pull source)
func IsPushListen(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "publish" {
		return true
	}
	if scheme != "rtmp" && scheme != "rtmps" && scheme != "srt" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return true // bare port like ":1935"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// DNS name → definitely a remote server, not a local bind address.
		return false
	}
	return ip.IsUnspecified() || ip.IsLoopback()
}

// CopyTarget parses a `copy://<upstream_stream_code>` URL and returns the
// upstream code. v1 grammar is strict: scheme must be exactly `copy`, host
// must be a non-empty stream code, no path / query / fragment / userinfo are
// allowed (those positions are reserved for future qualifiers like
// `copy://X/raw` or `copy://X/track_2` and rejecting them now keeps the
// surface clean for that extension).
//
// Returns an error message that names the offending part so the API layer
// can surface it directly to the user without further interpretation.
func CopyTarget(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", &copyURLError{Reason: "malformed url: " + err.Error()}
	}
	if !strings.EqualFold(u.Scheme, "copy") {
		return "", &copyURLError{Reason: "scheme must be 'copy'"}
	}
	if u.User != nil {
		return "", &copyURLError{Reason: "userinfo not allowed (use copy://<code>)"}
	}
	if u.Path != "" && u.Path != "/" {
		return "", &copyURLError{Reason: "path not allowed in v1 (use copy://<code>)"}
	}
	if u.RawQuery != "" {
		return "", &copyURLError{Reason: "query string not allowed in v1"}
	}
	if u.Fragment != "" {
		return "", &copyURLError{Reason: "fragment not allowed"}
	}
	code := u.Host
	if code == "" {
		return "", &copyURLError{Reason: "missing upstream stream code"}
	}
	// url.Parse splits host:port; copy:// targets a stream code, never a port.
	if strings.Contains(code, ":") {
		return "", &copyURLError{Reason: "port not allowed (host is the stream code, not an address)"}
	}
	return code, nil
}

// copyURLError tags errors from CopyTarget so callers can match with errors.As.
type copyURLError struct{ Reason string }

func (e *copyURLError) Error() string { return "copy://: " + e.Reason }

// IsCopyURLError reports whether err originated from CopyTarget.
func IsCopyURLError(err error) bool {
	_, ok := err.(*copyURLError)
	return ok
}

// MixerSpec is the parsed result of a `mixer://` URL.
//
//	Video / Audio          — upstream stream codes (host part, comma-separated)
//	AudioFailureContinue   — when true, the mixer keeps forwarding video-only
//	                         after the audio upstream goes away. When false
//	                         (default) audio failure aborts the whole stream
//	                         (video failure ALWAYS aborts).
type MixerSpec struct {
	Video                string
	Audio                string
	AudioFailureContinue bool
}

// MixerTargets parses a `mixer://<video_code>,<audio_code>[?audio_failure=continue]`
// URL. v1 grammar:
//   - scheme exactly "mixer"
//   - host is `<video>,<audio>` — both codes non-empty, no `:` (no port)
//   - no path / fragment / userinfo
//   - only one query parameter accepted: `audio_failure=continue` (default omitted)
//
// Returns an error message naming the offending part so the API layer can
// surface it directly to the user.
func MixerTargets(rawURL string) (MixerSpec, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return MixerSpec{}, &mixerURLError{Reason: "malformed url: " + err.Error()}
	}
	if !strings.EqualFold(u.Scheme, "mixer") {
		return MixerSpec{}, &mixerURLError{Reason: "scheme must be 'mixer'"}
	}
	if u.User != nil {
		return MixerSpec{}, &mixerURLError{Reason: "userinfo not allowed (use mixer://<video>,<audio>)"}
	}
	if u.Path != "" && u.Path != "/" {
		return MixerSpec{}, &mixerURLError{Reason: "path not allowed in v1 (use mixer://<video>,<audio>)"}
	}
	if u.Fragment != "" {
		return MixerSpec{}, &mixerURLError{Reason: "fragment not allowed"}
	}

	host := u.Host
	if host == "" {
		return MixerSpec{}, &mixerURLError{Reason: "missing upstream codes"}
	}
	parts := strings.Split(host, ",")
	if len(parts) != 2 {
		return MixerSpec{}, &mixerURLError{Reason: "expected exactly two codes separated by comma (mixer://<video>,<audio>)"}
	}
	video, audio := parts[0], parts[1]
	if video == "" {
		return MixerSpec{}, &mixerURLError{Reason: "video upstream code is empty"}
	}
	if audio == "" {
		return MixerSpec{}, &mixerURLError{Reason: "audio upstream code is empty"}
	}
	if strings.Contains(video, ":") || strings.Contains(audio, ":") {
		return MixerSpec{}, &mixerURLError{Reason: "port not allowed in upstream codes"}
	}

	spec := MixerSpec{Video: video, Audio: audio}
	if err := parseMixerQuery(u.RawQuery, &spec); err != nil {
		return MixerSpec{}, err
	}
	return spec, nil
}

// parseMixerQuery validates and applies the mixer:// query string to spec.
// Only `audio_failure=down|continue` is recognised; any other key errors out.
func parseMixerQuery(rawQuery string, spec *MixerSpec) error {
	if rawQuery == "" {
		return nil
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return &mixerURLError{Reason: "malformed query: " + err.Error()}
	}
	for key, vals := range q {
		if key != "audio_failure" {
			return &mixerURLError{Reason: "unknown query parameter: " + key}
		}
		if len(vals) != 1 {
			return &mixerURLError{Reason: "audio_failure must appear at most once"}
		}
		switch vals[0] {
		case "continue":
			spec.AudioFailureContinue = true
		case "down", "":
			// explicit default
		default:
			return &mixerURLError{Reason: "audio_failure must be 'down' or 'continue', got: " + vals[0]}
		}
	}
	return nil
}

// mixerURLError tags errors from MixerTargets so callers can match with errors.As.
type mixerURLError struct{ Reason string }

func (e *mixerURLError) Error() string { return "mixer://: " + e.Reason }

// IsMixerURLError reports whether err originated from MixerTargets.
func IsMixerURLError(err error) bool {
	_, ok := err.(*mixerURLError)
	return ok
}

// IsMPEGTS returns true when data begins with the MPEG-TS sync byte 0x47.
func IsMPEGTS(data []byte) bool {
	return len(data) >= 188 && data[0] == 0x47
}

// SplitTSPackets splits a raw byte slice into 188-byte MPEG-TS packets.
// Incomplete trailing bytes are discarded.
func SplitTSPackets(data []byte) [][]byte {
	var packets [][]byte
	for i := 0; i+188 <= len(data); i += 188 {
		pkt := make([]byte, 188)
		copy(pkt, data[i:i+188])
		packets = append(packets, pkt)
	}
	return packets
}
