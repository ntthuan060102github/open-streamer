package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// mkMixerStream returns a stream coded "mix1" whose only input is
// `mixer://video,audio`. All mixer_shape tests use the same downstream code.
func mkMixerStream(video, audio string) *Stream {
	return &Stream{
		Code:   "mix1",
		Inputs: []Input{{URL: "mixer://" + video + "," + audio}},
	}
}

// Self-mix is rejected — a stream can't reference itself as either source.
func TestValidateMixerShape_RejectsSelfMixVideo(t *testing.T) {
	t.Parallel()
	s := mkMixerStream("mix1", "audioStream")
	err := ValidateMixerShape(s, mkLookup(s, mkSingleStream("audioStream")))
	require.Error(t, err)
	require.True(t, IsMixerShapeError(err))
	require.Contains(t, err.Error(), "self-mix")
}

func TestValidateMixerShape_RejectsSelfMixAudio(t *testing.T) {
	t.Parallel()
	s := mkMixerStream("videoStream", "mix1")
	err := ValidateMixerShape(s, mkLookup(s, mkSingleStream("videoStream")))
	require.Error(t, err)
	require.True(t, IsMixerShapeError(err))
}

// mixer:// must be the sole input — fallback inputs not supported in v1.
func TestValidateMixerShape_RejectsFallbackInput(t *testing.T) {
	t.Parallel()
	s := &Stream{
		Code: "mix1",
		Inputs: []Input{
			{URL: "mixer://cam,radio"},
			{URL: "rtmp://backup/mix"},
		},
	}
	err := ValidateMixerShape(s, mkLookup(mkSingleStream("cam"), mkSingleStream("radio")))
	require.Error(t, err)
	require.True(t, IsMixerShapeError(err))
	require.Contains(t, err.Error(), "must be the only input")
}

// ABR upstream as video source is rejected.
func TestValidateMixerShape_RejectsABRVideoUpstream(t *testing.T) {
	t.Parallel()
	s := mkMixerStream("camABR", "radio")
	err := ValidateMixerShape(s, mkLookup(mkABRStream("camABR", 2), mkSingleStream("radio")))
	require.Error(t, err)
	require.True(t, IsMixerShapeError(err))
	require.Contains(t, err.Error(), "video upstream")
	require.Contains(t, err.Error(), "ABR ladder")
}

// ABR upstream as audio source is rejected.
func TestValidateMixerShape_RejectsABRAudioUpstream(t *testing.T) {
	t.Parallel()
	s := mkMixerStream("cam", "radioABR")
	err := ValidateMixerShape(s, mkLookup(mkSingleStream("cam"), mkABRStream("radioABR", 3)))
	require.Error(t, err)
	require.True(t, IsMixerShapeError(err))
	require.Contains(t, err.Error(), "audio upstream")
}

// Downstream having its own transcoder is rejected.
func TestValidateMixerShape_RejectsDownstreamTranscoder(t *testing.T) {
	t.Parallel()
	s := mkMixerStream("cam", "radio")
	s.Transcoder = &TranscoderConfig{
		Video: VideoTranscodeConfig{Profiles: []VideoProfile{{Width: 1280, Height: 720}}},
	}
	err := ValidateMixerShape(s, mkLookup(mkSingleStream("cam"), mkSingleStream("radio")))
	require.Error(t, err)
	require.True(t, IsMixerShapeError(err))
	require.Contains(t, err.Error(), "must not configure its own transcoder")
}

// Single + single upstream as sole input + no transcoder = the supported v1 case.
func TestValidateMixerShape_AllowsSingleSinglePair(t *testing.T) {
	t.Parallel()
	s := mkMixerStream("cam", "radio")
	require.Nil(t, ValidateMixerShape(s, mkLookup(mkSingleStream("cam"), mkSingleStream("radio"))))
}

// Missing upstream is NOT a write-time error — upstream may be created
// later. MixerReader catches the missing-upstream case at runtime.
func TestValidateMixerShape_AllowsMissingUpstreams(t *testing.T) {
	t.Parallel()
	s := mkMixerStream("ghost_video", "ghost_audio")
	require.Nil(t, ValidateMixerShape(s, mkLookup()))
}

// No mixer input → validator is a no-op even when other rules would have fired.
func TestValidateMixerShape_NoMixerInputIsNoOp(t *testing.T) {
	t.Parallel()
	s := &Stream{
		Code:   "stream",
		Inputs: []Input{{URL: "rtmp://origin/stream"}},
		Transcoder: &TranscoderConfig{
			Video: VideoTranscodeConfig{Profiles: []VideoProfile{{Width: 1280}}},
		},
	}
	require.Nil(t, ValidateMixerShape(s, mkLookup()))
}

// Malformed mixer URL is reported by URL grammar validation, not shape —
// shape validator skips silently to avoid double-error.
func TestValidateMixerShape_MalformedURLIsSkipped(t *testing.T) {
	t.Parallel()
	s := &Stream{
		Code:   "mix1",
		Inputs: []Input{{URL: "mixer://"}}, // missing both codes
	}
	require.Nil(t, ValidateMixerShape(s, mkLookup()))
}
