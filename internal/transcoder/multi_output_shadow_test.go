package transcoder

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// Multi-output mode runs ONE FFmpeg process for all profiles, but
// runStreamEncoder records every crash against ALL ladder indices via
// recordProfileError(streamID, i, …). This test verifies that the shadow
// profile entries created by spawnMultiOutput accept those calls — without
// shadows the calls would silently no-op for indices 1..N-1 and the UI
// would only show track_1 having errors.
func TestRecordProfileError_FanOutsToShadowEntries(t *testing.T) {
	t.Parallel()
	sw := &streamWorker{
		profiles: map[int]*profileWorker{
			0: {},                       // real entry
			1: newShadowProfileWorker(), // shadow
			2: newShadowProfileWorker(), // shadow
		},
	}
	s := &Service{
		workers: map[domain.StreamCode]*streamWorker{"live": sw},
	}

	// Simulate runStreamEncoder's fan-out loop after one crash.
	for i := 0; i < 3; i++ {
		s.recordProfileError("live", i, "ffmpeg exit: status 234")
	}

	// All three rungs must show the same crash + bumped restart counter.
	for idx := 0; idx < 3; idx++ {
		pw := sw.profiles[idx]
		require.Lenf(t, pw.errors, 1, "profile %d must have 1 error", idx)
		assert.Equal(t, "ffmpeg exit: status 234", pw.errors[0].Message)
		assert.Equal(t, 1, pw.restartCount, "profile %d restart_count", idx)
	}
}

// RuntimeStatus must return one snapshot per ladder rung in multi-output
// mode (matching per-profile mode's shape) so the UI doesn't need to
// branch on transcoder.multi_output to render the profile grid.
func TestRuntimeStatus_MultiOutputReturnsAllShadowProfiles(t *testing.T) {
	t.Parallel()
	sw := &streamWorker{
		profiles: map[int]*profileWorker{
			0: {restartCount: 2},                                        // real entry
			1: {cancel: func() {}, done: shadowDoneCh, restartCount: 2}, // shadow
			2: {cancel: func() {}, done: shadowDoneCh, restartCount: 2}, // shadow
		},
	}
	s := &Service{
		workers: map[domain.StreamCode]*streamWorker{"live": sw},
	}

	rt, ok := s.RuntimeStatus("live")
	require.True(t, ok)
	require.Len(t, rt.Profiles, 3, "multi-output ladder must expose all rungs")

	indexes := make([]int, len(rt.Profiles))
	for i, p := range rt.Profiles {
		indexes[i] = p.Index
		assert.Equal(t, 2, p.RestartCount,
			"all rungs share the same restart count under multi-output")
	}
	sort.Ints(indexes)
	assert.Equal(t, []int{0, 1, 2}, indexes)
}

// Shadow's pre-closed done channel must not block any reader. Stop's wait
// loop relies on this — without it, Stop would hang forever because no
// goroutine ever closes shadow.done.
func TestShadowProfileWorker_DoneIsPreClosed(t *testing.T) {
	t.Parallel()
	pw := newShadowProfileWorker()
	select {
	case <-pw.done:
		// expected: read from a closed channel returns immediately
	default:
		t.Fatal("shadow done channel must be pre-closed so Stop's wait does not block")
	}
}

// Shadow cancel must be a no-op — StopProfile on a shadow rung must NOT
// propagate cancellation to the real multi-output FFmpeg process. Calling
// it twice is also safe (idempotent).
func TestShadowProfileWorker_CancelIsNoop(t *testing.T) {
	t.Parallel()
	pw := newShadowProfileWorker()
	// If cancel weren't a no-op these would panic / affect external state.
	pw.cancel()
	pw.cancel()
}
