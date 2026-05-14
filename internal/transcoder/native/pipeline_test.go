package native

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NewPipeline rejects an empty config. We don't want a partial init
// to ship a pipeline that crashes later — fail loud at construction so
// the caller knows immediately.
func TestNewPipeline_RejectsMissingStreamID(t *testing.T) {
	t.Parallel()
	_, err := NewPipeline(Config{
		Input:    bytes.NewReader(nil),
		HW:       domain.HWAccelNone,
		Profiles: []ProfileOutput{{Writer: io.Discard}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "StreamID")
}

func TestNewPipeline_RejectsMissingInput(t *testing.T) {
	t.Parallel()
	_, err := NewPipeline(Config{
		StreamID: "live",
		HW:       domain.HWAccelNone,
		Profiles: []ProfileOutput{{Writer: io.Discard}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Input")
}

func TestNewPipeline_RejectsEmptyProfiles(t *testing.T) {
	t.Parallel()
	_, err := NewPipeline(Config{
		StreamID: "live",
		Input:    bytes.NewReader(nil),
		HW:       domain.HWAccelNone,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "profile")
}

// Empty input is a real failure path: the mpegts demuxer can't find
// any streams in zero bytes so FindStreamInfo returns EOF. The
// pipeline must surface this as a wrapped init-stage error so the
// caller knows where in the construction chain it died rather than
// getting a bare "End of file" from libav.
func TestPipeline_RunSurfacesEmptyInputError(t *testing.T) {
	t.Parallel()
	_, err := NewPipeline(Config{
		StreamID: "live",
		Input:    bytes.NewReader([]byte{}),
		HW:       domain.HWAccelNone,
		Profiles: []ProfileOutput{{
			Profile: domain.VideoProfile{Width: 1280, Height: 720, Bitrate: 1500},
			Writer:  io.Discard,
		}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "init input",
		"empty input must surface as wrapped init-input error so caller traces the failure stage")
}

// Close is idempotent — multiple invocations stay safe so the
// constructor's failure path and the deferred caller can both call it.
func TestPipeline_CloseIdempotent(t *testing.T) {
	t.Parallel()
	p := &Pipeline{}
	assert.NoError(t, p.Close())
	assert.NoError(t, p.Close())
}

// Run on a closed pipeline returns a clear error rather than crashing
// or silently no-op'ing. Single-shot lifecycle: build → run once →
// close → discard.
func TestPipeline_RunAfterCloseRejects(t *testing.T) {
	t.Parallel()
	p := &Pipeline{}
	require.NoError(t, p.Close())

	err := p.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "closed")
}
