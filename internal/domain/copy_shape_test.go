package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func mkABRStream(code string, profiles int) *Stream {
	pp := make([]VideoProfile, profiles)
	for i := range pp {
		pp[i] = VideoProfile{Width: 1920 - i*640, Height: 1080 - i*360, Bitrate: 4500 - i*1500}
	}
	return &Stream{
		Code: StreamCode(code),
		Transcoder: &TranscoderConfig{
			Video: VideoTranscodeConfig{Profiles: pp},
		},
	}
}

func mkSingleStream(code string) *Stream {
	return &Stream{Code: StreamCode(code)}
}

func mkLookup(streams ...*Stream) StreamLookup {
	m := make(map[StreamCode]*Stream, len(streams))
	for _, s := range streams {
		m[s.Code] = s
	}
	return func(c StreamCode) (*Stream, bool) {
		s, ok := m[c]
		return s, ok
	}
}

// Self-copy is rejected up front — even the cycle detector would catch it,
// but ValidateCopyShape gives a clearer error message at API write time.
func TestValidateCopyShape_RejectsSelfCopy(t *testing.T) {
	t.Parallel()
	s := &Stream{Code: "a", Inputs: []Input{{URL: "copy://a"}}}
	err := ValidateCopyShape(s, mkLookup(s))
	require.Error(t, err)
	require.True(t, IsCopyShapeError(err))
	require.Contains(t, err.Error(), "self-copy")
}

// ABR-copy must be the only input — no fallback supported in v1.
func TestValidateCopyShape_RejectsABRCopyWithFallback(t *testing.T) {
	t.Parallel()
	upstream := mkABRStream("up", 3)
	downstream := &Stream{
		Code: "dn",
		Inputs: []Input{
			{Priority: 0, URL: "copy://up"},
			{Priority: 1, URL: "rtmp://backup/stream"},
		},
	}
	err := ValidateCopyShape(downstream, mkLookup(upstream, downstream))
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be the only input")
	require.Contains(t, err.Error(), "3 rungs")
}

// ABR-copy + downstream transcoder is ambiguous (which ladder wins?).
// Forbid early so users don't get surprising re-encode behaviour.
func TestValidateCopyShape_RejectsABRCopyWithLocalTranscoder(t *testing.T) {
	t.Parallel()
	upstream := mkABRStream("up", 2)
	downstream := &Stream{
		Code:       "dn",
		Inputs:     []Input{{URL: "copy://up"}},
		Transcoder: &TranscoderConfig{Video: VideoTranscodeConfig{Profiles: []VideoProfile{{Width: 640, Height: 360}}}},
	}
	err := ValidateCopyShape(downstream, mkLookup(upstream, downstream))
	require.Error(t, err)
	require.Contains(t, err.Error(), "must not configure its own transcoder")
}

// ABR-copy as sole input with no local transcoder — the supported v1 case.
func TestValidateCopyShape_AllowsABRCopyAsSoleInput(t *testing.T) {
	t.Parallel()
	upstream := mkABRStream("up", 3)
	downstream := &Stream{Code: "dn", Inputs: []Input{{URL: "copy://up"}}}
	require.NoError(t, ValidateCopyShape(downstream, mkLookup(upstream, downstream)))
}

// Single-stream copy + network fallback — the publishers see one TS source
// regardless of which input is active, no shape mismatch.
func TestValidateCopyShape_AllowsSingleCopyWithFallback(t *testing.T) {
	t.Parallel()
	upstream := mkSingleStream("up")
	downstream := &Stream{
		Code: "dn",
		Inputs: []Input{
			{Priority: 0, URL: "copy://up"},
			{Priority: 1, URL: "rtmp://backup/stream"},
		},
	}
	require.NoError(t, ValidateCopyShape(downstream, mkLookup(upstream, downstream)))
}

// Single-stream copy + own transcoder — re-encoding the copied raw is
// fine, that's exactly what a downstream "remix" stream looks like.
func TestValidateCopyShape_AllowsSingleCopyWithLocalTranscoder(t *testing.T) {
	t.Parallel()
	upstream := mkSingleStream("up")
	downstream := &Stream{
		Code:       "dn",
		Inputs:     []Input{{URL: "copy://up"}},
		Transcoder: &TranscoderConfig{Video: VideoTranscodeConfig{Profiles: []VideoProfile{{Width: 640, Height: 360}}}},
	}
	require.NoError(t, ValidateCopyShape(downstream, mkLookup(upstream, downstream)))
}

// Missing upstream is treated as "shape unknown" — not an error here.
// The coordinator validates upstream presence at start time as a hard error.
func TestValidateCopyShape_MissingUpstreamSkipsRule(t *testing.T) {
	t.Parallel()
	// `copy://ghost` cannot be resolved → ABR rules never fire.
	downstream := &Stream{
		Code:       "dn",
		Inputs:     []Input{{URL: "copy://ghost"}},
		Transcoder: &TranscoderConfig{Video: VideoTranscodeConfig{Profiles: []VideoProfile{{Width: 640, Height: 360}}}},
	}
	require.NoError(t, ValidateCopyShape(downstream, mkLookup(downstream)))
}

// `video.copy=true` upstream is single-stream regardless of profiles count.
func TestValidateCopyShape_VideoCopyUpstreamIsSingleStream(t *testing.T) {
	t.Parallel()
	upstream := &Stream{
		Code: "up",
		Transcoder: &TranscoderConfig{
			Video: VideoTranscodeConfig{Copy: true, Profiles: []VideoProfile{{Width: 1920, Height: 1080}}},
		},
	}
	downstream := &Stream{
		Code: "dn",
		Inputs: []Input{
			{Priority: 0, URL: "copy://up"},
			{Priority: 1, URL: "rtmp://backup/stream"},
		},
	}
	require.NoError(t, ValidateCopyShape(downstream, mkLookup(upstream, downstream)),
		"copy=true upstream is single-stream, fallback is allowed")
}

// Pure non-copy input list — validator is a no-op.
func TestValidateCopyShape_NoCopyInputsIsNoOp(t *testing.T) {
	t.Parallel()
	s := &Stream{Code: "a", Inputs: []Input{{URL: "rtmp://origin/a"}}}
	require.NoError(t, ValidateCopyShape(s, mkLookup(s)))
}
