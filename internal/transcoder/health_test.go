package transcoder

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// markProfileUnhealthy must return true exactly once per stream — the
// edge from "no failing profile" to "at least one failing profile". A
// second profile failing on the same stream returns false so the
// coordinator only sees the transition (no duplicate StatusDegraded
// fire on every per-profile crash).
func TestMarkProfileUnhealthy_OnlyFiresOnEdge(t *testing.T) {
	t.Parallel()
	s := newHealthService()

	assert.True(t, s.markProfileUnhealthy("s1", 0),
		"first failing profile must report transition")
	assert.False(t, s.markProfileUnhealthy("s1", 0),
		"same profile failing again must not re-fire")
	assert.False(t, s.markProfileUnhealthy("s1", 1),
		"second profile on already-unhealthy stream must not re-fire")

	// Different stream → its own edge.
	assert.True(t, s.markProfileUnhealthy("s2", 0),
		"different stream must fire its own first transition")
}

// markProfileHealthy must return true exactly once per stream — the
// edge from "at least one failing profile" to "all healthy". Caller
// fires onHealthy only on this edge.
func TestMarkProfileHealthy_OnlyFiresOnAllClear(t *testing.T) {
	t.Parallel()
	s := newHealthService()

	// Two profiles failing.
	s.markProfileUnhealthy("s1", 0)
	s.markProfileUnhealthy("s1", 1)

	// First recovery — stream still has profile 1 failing → no edge.
	assert.False(t, s.markProfileHealthy("s1", 0),
		"recovery while another profile still failing must not fire")
	// Second recovery — set is now empty → fire the all-clear edge.
	assert.True(t, s.markProfileHealthy("s1", 1),
		"recovery of last failing profile must fire all-clear edge")

	// After full recovery, marking healthy on a profile that wasn't
	// failing must be a no-op.
	assert.False(t, s.markProfileHealthy("s1", 0),
		"healthy mark on already-healthy profile must not fire")
}

// fire helpers must invoke the registered callback EXACTLY on the
// transition edge — not on every crash. End-to-end check that
// SetUnhealthyCallback / SetHealthyCallback see a single call per
// degrade / recover cycle even when many crashes happen in between.
func TestFireUnhealthy_CoalesceMultipleCrashes(t *testing.T) {
	t.Parallel()
	s := newHealthService()

	var (
		mu             sync.Mutex
		unhealthyCalls int
		healthyCalls   int
		lastReason     string
	)
	s.SetUnhealthyCallback(func(_ domain.StreamCode, reason string) {
		mu.Lock()
		unhealthyCalls++
		lastReason = reason
		mu.Unlock()
	})
	s.SetHealthyCallback(func(_ domain.StreamCode) {
		mu.Lock()
		healthyCalls++
		mu.Unlock()
	})

	// 5 consecutive crashes on profile 0 → callback fires ONCE.
	for i := 0; i < 5; i++ {
		s.fireUnhealthyIfTransitioned("s1", 0, "crash msg")
	}
	mu.Lock()
	assert.Equal(t, 1, unhealthyCalls)
	assert.Equal(t, "crash msg", lastReason)
	mu.Unlock()

	// Recovery fires healthy ONCE.
	s.fireHealthyIfTransitioned("s1", 0)
	s.fireHealthyIfTransitioned("s1", 0)
	mu.Lock()
	assert.Equal(t, 1, healthyCalls)
	mu.Unlock()
}

// dropHealthState must fire onHealthy when the stream had unhealthy
// entries at drop time. Hot restart (Update → Stop → Start to swap
// transcoder config) relies on this — without the callback the
// coordinator's mirrored transcoderUnhealthy flag stays true forever
// because the new transcoder process starts clean and has nothing to
// "recover" from.
//
// Regression: changing bframes from 10000 (crashes) → 0 (valid) used
// to leave status stuck at Degraded even though the new FFmpeg ran
// healthy.
func TestDropHealthState_FiresHealthyWhenEntriesExist(t *testing.T) {
	t.Parallel()
	s := newHealthService()
	healthyCalls := 0
	s.SetHealthyCallback(func(_ domain.StreamCode) { healthyCalls++ })

	s.markProfileUnhealthy("s1", 0)
	s.dropHealthState("s1")

	assert.Equal(t, 1, healthyCalls,
		"dropHealthState must fire onHealthy so coordinator clears its mirrored flag")
	// Subsequent unhealthy mark must fire as a fresh edge (state was
	// fully wiped, not just transitioned).
	assert.True(t, s.markProfileUnhealthy("s1", 0),
		"after dropHealthState the stream is back to baseline; new failure is a fresh edge")
}

// dropHealthState on a healthy stream (no entries) must NOT fire
// onHealthy — would be a synthetic recovery event with no semantic
// meaning. Stop on healthy streams is common (graceful shutdown) so
// suppressing the callback keeps the coordinator's event log clean.
func TestDropHealthState_NoFireOnHealthyStream(t *testing.T) {
	t.Parallel()
	s := newHealthService()
	healthyCalls := 0
	s.SetHealthyCallback(func(_ domain.StreamCode) { healthyCalls++ })

	// No prior markProfileUnhealthy → no entries.
	s.dropHealthState("s1")

	assert.Equal(t, 0, healthyCalls,
		"dropHealthState on a stream that was never unhealthy must not fire")
}

// nil callbacks must be safe — Service operates without coordinator
// wiring (e.g. unit tests, embedded uses).
func TestFireWithoutCallbacks_NoPanic(t *testing.T) {
	t.Parallel()
	s := newHealthService()
	// No SetUnhealthyCallback / SetHealthyCallback called.
	require.NotPanics(t, func() {
		s.fireUnhealthyIfTransitioned("s1", 0, "x")
		s.fireHealthyIfTransitioned("s1", 0)
	})
}

// newHealthService builds a Service with only the fields the health
// helpers touch — avoids spinning up buffer/bus/metrics for state
// tests.
func newHealthService() *Service {
	return &Service{
		unhealthyProfiles: make(map[domain.StreamCode]map[int]struct{}),
	}
}
