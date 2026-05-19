package domain

// MediaTrackKind identifies whether a track carries video or audio.
type MediaTrackKind string

// MediaTrackKind values.
const (
	MediaTrackVideo MediaTrackKind = "video"
	MediaTrackAudio MediaTrackKind = "audio"
)

// MediaTrackInfo describes one elementary track in a stream's input or output.
//
// The runtime envelope exposes lists of these so the UI can render the
// "Input media info / Output media info" panels (codec, resolution, bitrate)
// without inferring shape from per-protocol config blobs.
//
// Fields are best-effort:
//   - Width/Height are populated only after the first keyframe whose SPS
//     could be parsed; zero means "not yet decoded".
//   - BitrateKbps is an EWMA over the last few seconds of bytes seen for
//     this codec on this input; zero means "no bytes yet" (just-connected
//     stream, or output not running).
type MediaTrackInfo struct {
	Kind        MediaTrackKind `json:"kind"`
	Codec       string         `json:"codec"`            // "h264" | "h265" | "aac" | "mp2a"
	Width       int            `json:"width,omitempty"`  // video only
	Height      int            `json:"height,omitempty"` // video only
	BitrateKbps int            `json:"bitrate_kbps"`
}

// CodecLabel returns the canonical lowercase string used in MediaTrackInfo.Codec
// for an AVCodec — kept here (rather than on the AVCodec type) so the JSON
// contract lives next to the struct that exposes it.
//
// "mp2a" is the FourCC for MPEG-1/2 Audio (Layer I/II/III). Matches the
// operator-tooling UI label convention so operators familiar with established tooling see
// the same string for the same codec on both servers; the underlying format
// covers everything that arrives via TS stream_type 0x03 or 0x04.
//
//nolint:exhaustive // AVCodecUnknown intentionally falls to the empty-string default.
func CodecLabel(c AVCodec) string {
	switch c {
	case AVCodecH264:
		return "h264"
	case AVCodecH265:
		return "h265"
	case AVCodecAAC:
		return "aac"
	case AVCodecMP2:
		return "mp2a"
	case AVCodecMP3:
		return "mp3"
	case AVCodecAC3:
		return "ac3"
	case AVCodecEAC3:
		return "eac3"
	case AVCodecAV1:
		return "av1"
	case AVCodecMPEG2Video:
		return "mp2v"
	default:
		return ""
	}
}

// OutputTracks derives the list of output media tracks from a stream's
// persisted Transcoder config. One MediaTrackInfo per ABR rendition is
// emitted, plus one entry for the audio output. Passthrough renditions
// (Codec=="copy") show codec="copy" with zero resolution/bitrate — the
// UI can render them as "follows input" or fall back to the input track.
//
// When `tc` is nil the stream has no transcoder configured — caller is
// expected to fall back to input tracks (no-transcode mirror).
func OutputTracks(tc *TranscoderConfig) []MediaTrackInfo {
	if tc == nil {
		return nil
	}

	tracks := make([]MediaTrackInfo, 0, len(tc.Video.Profiles)+1)

	// Audio first or last? The reference UI lists audio above renditions
	// when bandwidth varies per profile but audio is shared. Mirror that.
	audio := outputAudioTrack(tc.Audio)
	if audio != nil {
		tracks = append(tracks, *audio)
	}

	for _, p := range tc.Video.Profiles {
		tracks = append(tracks, outputVideoTrack(p, tc.Video.Copy))
	}
	return tracks
}

// outputVideoTrack builds the MediaTrackInfo for one rendition. `videoCopy`
// is the top-level Video.Copy flag — when true, Profiles are typically empty
// but a caller may still ask for a track and we want a sensible label.
func outputVideoTrack(p VideoProfile, videoCopy bool) MediaTrackInfo {
	codec := string(p.Codec)
	if videoCopy || p.Codec == VideoCodecCopy {
		codec = "copy"
	}
	return MediaTrackInfo{
		Kind:        MediaTrackVideo,
		Codec:       codec,
		Width:       p.Width,
		Height:      p.Height,
		BitrateKbps: p.Bitrate,
	}
}

// outputAudioTrack builds the audio track entry. Returns nil when audio is
// disabled (no codec configured AND no copy flag) so the caller knows to
// suppress the row entirely.
func outputAudioTrack(a AudioTranscodeConfig) *MediaTrackInfo {
	if a.Copy {
		return &MediaTrackInfo{
			Kind:        MediaTrackAudio,
			Codec:       "copy",
			BitrateKbps: a.Bitrate,
		}
	}
	if a.Codec == "" {
		return nil
	}
	return &MediaTrackInfo{
		Kind:        MediaTrackAudio,
		Codec:       string(a.Codec),
		BitrateKbps: a.Bitrate,
	}
}

// AggregateBitrateKbps sums every track's BitrateKbps into a single number,
// the value the runtime envelope publishes as "input total" / "output total".
func AggregateBitrateKbps(tracks []MediaTrackInfo) int {
	var total int
	for _, t := range tracks {
		total += t.BitrateKbps
	}
	return total
}
