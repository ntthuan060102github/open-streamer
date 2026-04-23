package publisher

import (
	"sort"
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// PushStatus is the lifecycle state of one outbound push destination.
type PushStatus string

// Status values reported via RuntimeStatus.
const (
	// PushStatusStarting — packager goroutine has spawned, no handshake attempt yet.
	PushStatusStarting PushStatus = "starting"
	// PushStatusActive — last connect succeeded; session is currently sending media.
	PushStatusActive PushStatus = "active"
	// PushStatusReconnecting — last session ended (server close / write error / handshake fail);
	// waiting RetryTimeoutSec before next attempt.
	PushStatusReconnecting PushStatus = "reconnecting"
	// PushStatusFailed — exhausted dest.Limit attempts; goroutine has given up.
	PushStatusFailed PushStatus = "failed"
)

const maxPushErrorHistory = 5

// pushState tracks live state for one (stream, push URL) pair. Mutated by
// serveRTMPPush at session boundaries; read by RuntimeStatus snapshots.
type pushState struct {
	mu          sync.Mutex
	status      PushStatus
	attempt     int                 // current retry counter; reset to 0 on discontinuity
	connectedAt time.Time           // last time we entered Active; zero when never connected
	errors      []domain.ErrorEntry // newest at index 0; capped at maxPushErrorHistory
}

// PushSnapshot is the JSON-safe snapshot of one push destination's runtime state.
type PushSnapshot struct {
	URL         string              `json:"url"`
	Status      PushStatus          `json:"status"`
	Attempt     int                 `json:"attempt"`
	ConnectedAt *time.Time          `json:"connected_at,omitempty"`
	Errors      []domain.ErrorEntry `json:"errors,omitempty"`
}

// RuntimeStatus is the JSON-safe snapshot of all push destinations for one stream.
type RuntimeStatus struct {
	Pushes []PushSnapshot `json:"pushes"`
}

// recordPushErrorEntry prepends an entry, capped at maxPushErrorHistory.
// Caller must hold the parent pushState.mu.
func recordPushErrorEntry(st *pushState, msg string, at time.Time) {
	e := domain.ErrorEntry{Message: msg, At: at}
	if len(st.errors) >= maxPushErrorHistory {
		copy(st.errors[1:], st.errors[:maxPushErrorHistory-1])
		st.errors[0] = e
		return
	}
	st.errors = append([]domain.ErrorEntry{e}, st.errors...)
}

// getOrCreatePushState returns the per-(stream,url) state, creating a fresh
// "starting" entry on first call. Lazy creation lets serveRTMPPush call any
// of the setters without an explicit init step.
func (s *Service) getOrCreatePushState(streamID domain.StreamCode, url string) *pushState {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	m, ok := s.pushStates[streamID]
	if !ok {
		m = make(map[string]*pushState)
		s.pushStates[streamID] = m
	}
	st, ok := m[url]
	if !ok {
		st = &pushState{status: PushStatusStarting}
		m[url] = st
	}
	return st
}

// setPushStatus updates the status with the following invariants:
//
//   - Entering Active stamps connectedAt and clears the error history.
//     Past failures are resolved (server came back, network healed, …) so
//     leaving them on the UI would misleadingly suggest the destination is
//     still in trouble.
//   - Leaving Active clears connectedAt. Otherwise the API would keep
//     returning a connected_at value while reconnecting/failed and the UI
//     would compute a bogus uptime against a session that ended seconds ago.
//
// This differs from input/transcoder error history (which persists for the
// lifetime of the registration) — for pushes the user wants a clean slate
// per healthy session.
func (s *Service) setPushStatus(streamID domain.StreamCode, url string, status PushStatus) {
	st := s.getOrCreatePushState(streamID, url)
	st.mu.Lock()
	st.status = status
	if status == PushStatusActive {
		st.connectedAt = time.Now()
		st.errors = nil
	} else {
		// Reset session-anchor so the API/UI can't compute uptime against
		// a stale Active timestamp from a previous session.
		st.connectedAt = time.Time{}
	}
	st.mu.Unlock()
}

// setPushAttempt records the current retry counter. serveRTMPPush calls this
// at the top of every loop iteration so the API reflects the in-flight attempt.
func (s *Service) setPushAttempt(streamID domain.StreamCode, url string, n int) {
	st := s.getOrCreatePushState(streamID, url)
	st.mu.Lock()
	st.attempt = n
	st.mu.Unlock()
}

// recordPushError appends a session failure to the rolling history.
// Frontend reads errors[0] for the most recent failure cause.
func (s *Service) recordPushError(streamID domain.StreamCode, url, msg string) {
	st := s.getOrCreatePushState(streamID, url)
	st.mu.Lock()
	defer st.mu.Unlock()
	recordPushErrorEntry(st, msg, time.Now())
}

// removePushState drops the per-(stream,url) entry; called via defer in
// serveRTMPPush so a fully-stopped destination disappears from the API.
func (s *Service) removePushState(streamID domain.StreamCode, url string) {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	if m, ok := s.pushStates[streamID]; ok {
		delete(m, url)
		if len(m) == 0 {
			delete(s.pushStates, streamID)
		}
	}
}

// RuntimeStatus returns a snapshot of all push destinations for the given
// stream, sorted by URL for stable output. Returns ok=false when no push is
// registered for the stream (no destinations configured, or all torn down).
func (s *Service) RuntimeStatus(streamID domain.StreamCode) (RuntimeStatus, bool) {
	s.pushMu.Lock()
	m, ok := s.pushStates[streamID]
	if !ok {
		s.pushMu.Unlock()
		return RuntimeStatus{}, false
	}
	snaps := make([]PushSnapshot, 0, len(m))
	for url, st := range m {
		st.mu.Lock()
		snap := PushSnapshot{
			URL:     url,
			Status:  st.status,
			Attempt: st.attempt,
		}
		if !st.connectedAt.IsZero() {
			t := st.connectedAt
			snap.ConnectedAt = &t
		}
		if len(st.errors) > 0 {
			// Defensive copy — caller must not see future mutations.
			snap.Errors = make([]domain.ErrorEntry, len(st.errors))
			copy(snap.Errors, st.errors)
		}
		st.mu.Unlock()
		snaps = append(snaps, snap)
	}
	s.pushMu.Unlock()
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].URL < snaps[j].URL })
	return RuntimeStatus{Pushes: snaps}, true
}
