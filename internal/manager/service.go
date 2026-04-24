// Package manager implements the Stream Manager — the failover engine.
// It monitors input health (bitrate, FPS, packet loss, timeout) and seamlessly
// switches to the best available input when the active one degrades or fails.
// Failover is handled entirely in Go — FFmpeg is never restarted for this purpose.
//
// # Concurrency model
//
// Each stream has its own [streamState] protected by state.mu.
// The global Service.mu is an RWMutex that guards only the streams map and is
// never held while performing I/O or while acquiring state.mu (consistent lock
// ordering: Service.mu → state.mu prevents deadlocks).
//
// RecordPacket (hot path) does a global RLock to locate the state pointer, then
// a per-stream Lock to update LastPacketAt and Status.
// At typical packet rates (< 100/s per stream) the per-packet mutex overhead
// is negligible while being completely race-free.
//
// # Failover state machine
//
//	StatusIdle → StatusActive   (first packet arrives on the active input)
//	StatusActive → StatusDegraded (timeout detected by monitor, or error from ingestor)
//	StatusDegraded → StatusIdle   (background probe succeeds; input is a failback candidate)
//	StatusIdle → StatusActive   (ingestor switches to this input after tryFailover)
package manager

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/events"
	"github.com/ntt0601zcoder/open-streamer/internal/ingestor"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/ntt0601zcoder/open-streamer/internal/publisher"
	"github.com/ntt0601zcoder/open-streamer/internal/transcoder"
	"github.com/samber/do/v2"
)

const (
	monitorInterval        = 2 * time.Second
	failbackProbeCooldown  = 8 * time.Second  // min time after degradation before first probe
	failbackSwitchCooldown = 12 * time.Second // min time between input switches
	probeTimeout           = 3 * time.Second
)

// InputHealth tracks the runtime health of one input source.
// All mutable fields are protected by the parent streamState.mu.
type InputHealth struct {
	Input        domain.Input
	LastPacketAt time.Time
	Bitrate      float64 // kbps (reserved for future bitrate estimator)
	PacketLoss   float64 // percent (reserved)
	Status       domain.StreamStatus
	// Errors is a bounded rolling history (newest at index 0, max maxInputErrorHistory).
	// Persists for the lifetime of the manager registration — cleared only when
	// the stream pipeline is stopped/restarted (Unregister drops the whole state).
	Errors []domain.ErrorEntry
}

const maxInputErrorHistory = 5

// recordInputError prepends an entry, capped at maxInputErrorHistory.
// Caller must hold the parent streamState.mu.
func recordInputError(h *InputHealth, msg string, at time.Time) {
	e := domain.ErrorEntry{Message: msg, At: at}
	if len(h.Errors) >= maxInputErrorHistory {
		copy(h.Errors[1:], h.Errors[:maxInputErrorHistory-1])
		h.Errors[0] = e
		return
	}
	h.Errors = append([]domain.ErrorEntry{e}, h.Errors...)
}

// streamState holds all monitoring data for a single stream.
// Lock order: Service.mu (read or write) → state.mu — never in reverse.
type streamState struct {
	mu sync.Mutex

	inputs        map[int]*InputHealth // keyed by Input.Priority
	active        int                  // active Input.Priority
	bufferWriteID domain.StreamCode
	degradedAt    map[int]time.Time // when each input was last marked degraded
	probing       map[int]bool      // true while a probe goroutine is in-flight for that priority
	lastSwitchAt  time.Time         // time of the most recent active-input switch

	// overridePriority is set by a manual SwitchInput call.
	// selectBest treats this input as highest-priority (always wins if healthy).
	// Cleared automatically when the overridden input degrades permanently.
	overridePriority *int

	// dead is set by Unregister. All goroutines that touch state must bail immediately on sight.
	dead bool
	// exhausted is set when all inputs are degraded and no failover candidate exists.
	// Cleared when a new input becomes active.
	exhausted bool

	// monCtx is cancelled when the stream is unregistered.
	// It is used as the base context for ingestor operations to ensure they
	// are tied to the stream's lifetime and not context.Background.
	monCtx context.Context
	cancel context.CancelFunc
}

// RuntimeStatus is the single "runtime" envelope returned by the API for a
// stream. Sub-systems contribute their own sections — manager owns input
// health; transcoder owns per-profile state — but the API exposes one shape so
// clients have a single root for all live data.
//
// Status and PipelineActive are populated by the API handler from the
// coordinator (not by manager itself). Transcoder and Publisher are also
// handler-populated and named after the subsystem they wrap, so frontend
// reads `runtime.transcoder.profiles[]` and `runtime.publisher.pushes[]` —
// no collision with the persisted `transcoder` / `push` config fields on
// domain.Stream.
//
// Exhausted is true when every input has degraded and no failover candidate
// remains — the stream is effectively offline at the source.
type RuntimeStatus struct {
	Status                domain.StreamStatus       `json:"status"`
	PipelineActive        bool                      `json:"pipeline_active"`
	ActiveInputPriority   int                       `json:"active_input_priority"`
	OverrideInputPriority *int                      `json:"override_input_priority,omitempty"`
	Exhausted             bool                      `json:"exhausted"`
	Inputs                []InputHealthSnapshot     `json:"inputs"`
	Transcoder            *transcoder.RuntimeStatus `json:"transcoder,omitempty"`
	Publisher             *publisher.RuntimeStatus  `json:"publisher,omitempty"`
}

// InputHealthSnapshot is a serialisable copy of one input's health.
// Errors is a bounded rolling history (newest first, max maxInputErrorHistory)
// of human-readable failure reasons (packet timeout, ingestor error, …).
// History persists for the lifetime of the manager registration and is only
// cleared when the stream pipeline is stopped. Frontend should treat each
// message as a diagnostic string, not a code.
type InputHealthSnapshot struct {
	InputPriority int                 `json:"input_priority"`
	LastPacketAt  time.Time           `json:"last_packet_at"`
	BitrateKbps   float64             `json:"bitrate_kbps"`
	PacketLoss    float64             `json:"packet_loss"`
	Status        domain.StreamStatus `json:"status"`
	Errors        []domain.ErrorEntry `json:"errors,omitempty"`
}

// probeTask carries the arguments for a background probe goroutine.
// All fields are immutable value copies so the goroutine is safe after state.mu is released.
type probeTask struct {
	priority int
	h        *InputHealth // pointer is stable for the stream's lifetime
	input    domain.Input // value copy of the input (URL, headers, etc.)
}

// ingestorDep is the slice of ingestor.Service that manager actually uses.
// Defined as an interface so unit tests can substitute a stub without
// constructing a full pull/push ingest stack — same pattern coordinator uses
// for its mgrDep / tcDep / pubDep deps.
type ingestorDep interface {
	Start(ctx context.Context, streamID domain.StreamCode, input domain.Input, bufferWriteID domain.StreamCode) error
	Probe(ctx context.Context, input domain.Input) error
	SetPacketObserver(fn func(streamID domain.StreamCode, inputPriority int))
	SetInputErrorObserver(fn func(streamID domain.StreamCode, inputPriority int, err error))
	Stop(streamID domain.StreamCode)
}

// Service monitors all streams and orchestrates source failover.
type Service struct {
	bus           events.Bus
	ingestor      ingestorDep
	m             *metrics.Metrics
	packetTimeout time.Duration

	mu      sync.RWMutex
	streams map[domain.StreamCode]*streamState

	onExhausted func(streamCode domain.StreamCode) // all inputs degraded — no ingest possible
	onRestored  func(streamCode domain.StreamCode) // at least one input active again after exhaustion
}

// SetExhaustedCallback registers a function called when all inputs for a stream are
// degraded and no failover candidate is available.
func (s *Service) SetExhaustedCallback(fn func(domain.StreamCode)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onExhausted = fn
}

// SetRestoredCallback registers a function called when a failover succeeds after
// a period where all inputs were exhausted.
func (s *Service) SetRestoredCallback(fn func(domain.StreamCode)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onRestored = fn
}

// NewForTesting builds a Service from pre-constructed deps. Used by unit
// tests that stub the ingestor (no real pull workers needed) and skip the
// DI plumbing. packetTimeoutSec mirrors ManagerConfig.InputPacketTimeoutSec
// (defaults to 30s when ≤ 0).
func NewForTesting(bus events.Bus, ing ingestorDep, m *metrics.Metrics, packetTimeoutSec int) *Service {
	if packetTimeoutSec <= 0 {
		packetTimeoutSec = 30
	}
	svc := &Service{
		bus:           bus,
		ingestor:      ing,
		m:             m,
		packetTimeout: time.Duration(packetTimeoutSec) * time.Second,
		streams:       make(map[domain.StreamCode]*streamState),
	}
	ing.SetPacketObserver(svc.RecordPacket)
	ing.SetInputErrorObserver(svc.ReportInputError)
	return svc
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[config.ManagerConfig](i)
	bus := do.MustInvoke[events.Bus](i)
	ing := do.MustInvoke[*ingestor.Service](i)
	m := do.MustInvoke[*metrics.Metrics](i)
	sec := cfg.InputPacketTimeoutSec
	if sec <= 0 {
		sec = 30
	}
	svc := &Service{
		bus:           bus,
		ingestor:      ing,
		m:             m,
		packetTimeout: time.Duration(sec) * time.Second,
		streams:       make(map[domain.StreamCode]*streamState),
	}
	ing.SetPacketObserver(svc.RecordPacket)
	ing.SetInputErrorObserver(svc.ReportInputError)
	return svc, nil
}

// Register starts health monitoring for a stream and begins ingesting on the best input.
// bufferWriteID is the Buffer Hub slot for ingest writes; empty defaults to stream.Code.
func (s *Service) Register(ctx context.Context, stream *domain.Stream, bufferWriteID domain.StreamCode) error {
	if bufferWriteID == "" {
		bufferWriteID = stream.Code
	}

	monCtx, cancel := context.WithCancel(ctx)
	state := &streamState{
		inputs:        make(map[int]*InputHealth, len(stream.Inputs)),
		bufferWriteID: bufferWriteID,
		degradedAt:    make(map[int]time.Time),
		probing:       make(map[int]bool),
		lastSwitchAt:  time.Now(),
		monCtx:        monCtx,
		cancel:        cancel,
	}
	for _, inp := range stream.Inputs {
		state.inputs[inp.Priority] = &InputHealth{
			Input:  inp,
			Status: domain.StatusIdle,
		}
	}

	s.mu.Lock()
	s.streams[stream.Code] = state
	s.mu.Unlock()

	go s.monitor(monCtx, stream.Code)

	// Start ingesting on the best available input.
	state.mu.Lock()
	best := selectBest(state)
	if best != nil {
		state.active = best.Input.Priority
	}
	state.mu.Unlock()

	if best != nil {
		if err := s.ingestor.Start(monCtx, stream.Code, best.Input, bufferWriteID); err != nil {
			slog.Error("manager: initial ingest start failed",
				"stream_code", stream.Code,
				"input_priority", best.Input.Priority,
				"err", err,
			)
		}
	}
	return nil
}

// Unregister stops ingest and health monitoring for streamID.
func (s *Service) Unregister(streamID domain.StreamCode) {
	s.mu.Lock()
	state, ok := s.streams[streamID]
	if !ok {
		s.mu.Unlock()
		return
	}
	// Mark dead while holding Service.mu so any goroutine that reads dead under
	// state.mu (after releasing Service.mu) will see it immediately.
	state.mu.Lock()
	state.dead = true
	state.mu.Unlock()
	state.cancel() // cancels monCtx → stops monitor + aborts in-flight probes
	delete(s.streams, streamID)
	s.mu.Unlock()

	s.ingestor.Stop(streamID)
}

// IsRegistered reports whether streamID is under manager supervision.
func (s *Service) IsRegistered(streamID domain.StreamCode) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.streams[streamID]
	return ok
}

// RuntimeStatus returns a snapshot of runtime input health, or ok=false if not registered.
func (s *Service) RuntimeStatus(streamID domain.StreamCode) (RuntimeStatus, bool) {
	s.mu.RLock()
	state, ok := s.streams[streamID]
	s.mu.RUnlock()
	if !ok {
		return RuntimeStatus{}, false
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	out := RuntimeStatus{
		ActiveInputPriority:   state.active,
		OverrideInputPriority: state.overridePriority,
		Exhausted:             state.exhausted,
		Inputs:                make([]InputHealthSnapshot, 0, len(state.inputs)),
	}
	for _, h := range state.inputs {
		snap := InputHealthSnapshot{
			InputPriority: h.Input.Priority,
			LastPacketAt:  h.LastPacketAt,
			BitrateKbps:   h.Bitrate,
			PacketLoss:    h.PacketLoss,
			Status:        h.Status,
		}
		if len(h.Errors) > 0 {
			// Defensive copy — caller must not see future mutations under state.mu.
			snap.Errors = make([]domain.ErrorEntry, len(h.Errors))
			copy(snap.Errors, h.Errors)
		}
		out.Inputs = append(out.Inputs, snap)
	}
	return out, true
}

// RecordPacket updates the last-seen timestamp for an input.
// Called by the ingestor per packet — must be fast and contention-free.
func (s *Service) RecordPacket(streamID domain.StreamCode, inputPriority int) {
	s.mu.RLock()
	state, ok := s.streams[streamID]
	s.mu.RUnlock()
	if !ok {
		return
	}

	now := time.Now()
	state.mu.Lock()
	if !state.dead {
		if h, found := state.inputs[inputPriority]; found {
			h.LastPacketAt = now
			if h.Status != domain.StatusActive {
				h.Status = domain.StatusActive
				// Recovery wipes the error history. Without this, transient
				// faults (file-loop EOF gap, brief network blip) leave a
				// stale "1 error" badge in the UI on a now-healthy input —
				// users can't distinguish "currently broken" from "broke
				// 2 minutes ago, recovered". Diagnostic history of past
				// failures is still observable via hooks (EventInputDegraded
				// / EventInputFailover) and slog warn lines, which are the
				// right place for "what happened over time".
				h.Errors = nil
				delete(state.degradedAt, inputPriority)
				s.m.ManagerInputHealth.WithLabelValues(string(streamID), strconv.Itoa(inputPriority)).Set(1)
			}
		}
	}
	state.mu.Unlock()
}

// ReportInputError marks an input degraded immediately and triggers failover.
// Called by the ingestor on non-retriable source errors.
func (s *Service) ReportInputError(streamID domain.StreamCode, inputPriority int, err error) {
	s.mu.RLock()
	state, ok := s.streams[streamID]
	s.mu.RUnlock()
	if !ok {
		return
	}

	now := time.Now()
	state.mu.Lock()
	if state.dead {
		state.mu.Unlock()
		return
	}
	h, found := state.inputs[inputPriority]
	if !found || h.Status == domain.StatusDegraded || h.Status == domain.StatusStopped {
		state.mu.Unlock()
		return
	}
	h.Status = domain.StatusDegraded
	recordInputError(h, err.Error(), now)
	state.degradedAt[inputPriority] = now
	state.mu.Unlock()

	slog.Warn("manager: input degraded by ingestor error",
		"stream_code", streamID,
		"input_priority", inputPriority,
		"err", err,
	)
	s.m.ManagerInputHealth.WithLabelValues(string(streamID), strconv.Itoa(inputPriority)).Set(0)
	s.bus.Publish(state.monCtx, domain.Event{
		Type:       domain.EventInputDegraded,
		StreamCode: streamID,
		Payload:    map[string]any{"input_priority": inputPriority, "reason": err.Error()},
	})
	s.tryFailover(streamID, state)
}

func (s *Service) monitor(ctx context.Context, streamID domain.StreamCode) {
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkHealth(streamID) //nolint:contextcheck // checkHealth uses state.monCtx internally
		}
	}
}

// checkHealth detects active-input timeouts and schedules probes for degraded inputs.
// It holds state.mu only briefly to collect work items, then acts outside the lock.
func (s *Service) checkHealth(streamID domain.StreamCode) {
	s.mu.RLock()
	state, ok := s.streams[streamID]
	s.mu.RUnlock()
	if !ok {
		return
	}

	now := time.Now()
	timeout := s.packetTimeout
	timedOutPriority := -1
	var probeTasks []probeTask

	state.mu.Lock()
	if !state.dead {
		for priority, h := range state.inputs {
			s.collectTimeoutIfNeeded(state, h, priority, now, timeout, &timedOutPriority)
			s.collectProbeIfNeeded(state, h, priority, now, &probeTasks)
		}
	}
	state.mu.Unlock()

	if timedOutPriority >= 0 {
		slog.Warn("manager: active input timed out",
			"stream_code", streamID,
			"input_priority", timedOutPriority,
		)
		s.m.ManagerInputHealth.WithLabelValues(string(streamID), strconv.Itoa(timedOutPriority)).Set(0)
		s.bus.Publish(state.monCtx, domain.Event{
			Type:       domain.EventInputDegraded,
			StreamCode: streamID,
			Payload:    map[string]any{"input_priority": timedOutPriority},
		})
		s.tryFailover(streamID, state)
	}
	for _, task := range probeTasks {
		go s.runProbe(streamID, state, task) //nolint:contextcheck // probe uses state.monCtx internally
	}
}

// collectTimeoutIfNeeded sets active input as degraded if no packet arrived within the timeout.
// Caller must hold state.mu.
func (s *Service) collectTimeoutIfNeeded(
	state *streamState,
	h *InputHealth,
	priority int,
	now time.Time,
	timeout time.Duration,
	timedOut *int,
) {
	if priority != state.active {
		return
	}
	if h.Status != domain.StatusActive {
		return
	}
	if now.Sub(h.LastPacketAt) <= timeout {
		return
	}
	h.Status = domain.StatusDegraded
	recordInputError(h, fmt.Sprintf("no packet for %s (timeout %s)", now.Sub(h.LastPacketAt).Truncate(time.Second), timeout), now)
	state.degradedAt[priority] = now
	*timedOut = priority
}

// collectProbeIfNeeded queues a probe task for a degraded input past its cooldown.
// Caller must hold state.mu.
//
// We normally skip the active priority because the live ingestor worker is
// already trying to reconnect on its own. EXCEPTION: when the stream is
// exhausted, the active worker has stopped (handleReadError returned true on
// EOF / non-retriable error), so nobody is reconnecting. In that case the
// "active" priority is stale — we must probe it ourselves or single-input
// streams stay permanently exhausted.
func (s *Service) collectProbeIfNeeded(
	state *streamState,
	h *InputHealth,
	priority int,
	now time.Time,
	tasks *[]probeTask,
) {
	if h.Status != domain.StatusDegraded || state.probing[priority] {
		return
	}
	if priority == state.active && !state.exhausted {
		return
	}
	at, seen := state.degradedAt[priority]
	if !seen || now.Sub(at) < failbackProbeCooldown {
		return
	}
	state.probing[priority] = true
	*tasks = append(*tasks, probeTask{priority: priority, h: h, input: h.Input})
}

// tryFailover picks the best healthy input and seamlessly switches to it.
// It is safe to call concurrently; concurrent calls are idempotent.
func (s *Service) tryFailover(streamID domain.StreamCode, state *streamState) {
	state.mu.Lock()
	if state.dead {
		state.mu.Unlock()
		return
	}
	best := selectBest(state)
	clearOverrideIfNeeded(streamID, state, best)
	if best == nil {
		s.handleExhausted(streamID, state)
		return
	}
	// Skip when best is already the active worker — except when exhausted, in
	// which case the active worker has stopped (EOF / error) and we must
	// restart it. Common single-input case: priority 0 degrades, exhausts,
	// later probes clean → best.Priority == state.active == 0 → must restart.
	if best.Input.Priority == state.active && !state.exhausted {
		state.mu.Unlock()
		return
	}
	prevPriority := state.active
	wasExhausted := state.exhausted
	bestInput := best.Input
	bufID := state.bufferWriteID
	ctx := state.monCtx
	state.mu.Unlock()

	slog.Info("manager: switching input",
		"stream_code", streamID,
		"from", prevPriority,
		"to", bestInput.Priority,
	)

	if err := s.ingestor.Start(ctx, streamID, bestInput, bufID); err != nil {
		slog.Error("manager: failed to start new ingestor",
			"stream_code", streamID,
			"input_priority", bestInput.Priority,
			"err", err,
		)
		return
	}

	commitSwitch(state, prevPriority, bestInput)

	s.m.ManagerFailoversTotal.WithLabelValues(string(streamID)).Inc()
	s.bus.Publish(ctx, domain.Event{
		Type:       domain.EventInputFailover,
		StreamCode: streamID,
		Payload:    map[string]any{"from": prevPriority, "to": bestInput.Priority},
	})
	s.notifyRestored(wasExhausted, streamID)
}

// handleExhausted marks the stream as having no healthy inputs and fires the callback once.
// Caller must hold state.mu; releases it before returning.
func (s *Service) handleExhausted(streamID domain.StreamCode, state *streamState) {
	wasExhausted := state.exhausted
	state.exhausted = true
	state.mu.Unlock()

	slog.Error("manager: no healthy input available", "stream_code", streamID)
	if wasExhausted {
		return
	}
	s.mu.RLock()
	cb := s.onExhausted
	s.mu.RUnlock()
	if cb != nil {
		go cb(streamID)
	}
}

// commitSwitch atomically updates streamState after a successful ingestor start.
func commitSwitch(state *streamState, prevPriority int, bestInput domain.Input) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.dead || state.active != prevPriority {
		return
	}
	if prevH := state.inputs[prevPriority]; prevH != nil && prevH.Status == domain.StatusActive {
		// Only downgrade Active → Idle; never override StatusDegraded that triggered the switch.
		prevH.Status = domain.StatusIdle
	}
	now := time.Now()
	state.active = bestInput.Priority
	state.lastSwitchAt = now
	state.exhausted = false
	// Stamp LastPacketAt so the packet-timeout monitor doesn't fire on the new input
	// before it has had a chance to connect (pre-connect handoff window).
	if newH := state.inputs[bestInput.Priority]; newH != nil {
		newH.LastPacketAt = now
	}
}

// notifyRestored fires the onRestored callback when the stream recovers from exhaustion.
func (s *Service) notifyRestored(wasExhausted bool, streamID domain.StreamCode) {
	if !wasExhausted {
		return
	}
	s.mu.RLock()
	cb := s.onRestored
	s.mu.RUnlock()
	if cb != nil {
		go cb(streamID)
	}
}

// clearOverrideIfNeeded clears the manual input override when the overridden input is no longer
// the selected best (meaning it has degraded permanently). Caller must hold state.mu.
func clearOverrideIfNeeded(streamID domain.StreamCode, state *streamState, best *InputHealth) {
	if state.overridePriority == nil {
		return
	}
	if best != nil && best.Input.Priority == *state.overridePriority {
		return
	}
	slog.Info("manager: manual input override cleared due to permanent failure",
		"stream_code", streamID,
		"override_priority", *state.overridePriority,
	)
	state.overridePriority = nil
}

// runProbe verifies whether a degraded input has recovered.
// On success it promotes the input to Idle and triggers a failback if warranted.
func (s *Service) runProbe(streamID domain.StreamCode, state *streamState, task probeTask) {
	defer func() {
		state.mu.Lock()
		state.probing[task.priority] = false
		state.mu.Unlock()
	}()

	// Use monCtx as parent so the probe is automatically cancelled when the stream is unregistered.
	probeCtx, cancel := context.WithTimeout(state.monCtx, probeTimeout)
	defer cancel()

	if err := s.ingestor.Probe(probeCtx, task.input); err != nil {
		return // stays StatusDegraded; probe is retried after failbackProbeCooldown
	}

	state.mu.Lock()
	if state.dead {
		state.mu.Unlock()
		return
	}
	task.h.Status = domain.StatusIdle
	delete(state.degradedAt, task.priority)
	currentActive := state.active
	wasExhausted := state.exhausted
	sinceSwitch := time.Since(state.lastSwitchAt)
	state.mu.Unlock()

	slog.Info("manager: degraded input recovered via probe",
		"stream_code", streamID,
		"input_priority", task.priority,
		"was_exhausted", wasExhausted,
	)

	// Two reasons to failover after a successful probe:
	//   1. Exhausted recovery — the active worker died (EOF / non-retriable
	//      error) so there's nothing running. Restart on whatever input just
	//      probed clean, regardless of priority comparison (might be the same
	//      priority that was active before).
	//   2. Failback — recovered input has higher priority than the fallback
	//      we're currently running on; switch back when cooldown elapsed.
	if wasExhausted || (task.priority < currentActive && sinceSwitch >= failbackSwitchCooldown) {
		s.tryFailover(streamID, state)
	}
}

// SwitchInput forces the stream to use the given input priority regardless of its
// configured priority value. The override persists until the input degrades permanently,
// at which point the manager reverts to normal priority-based selection.
// Switching to a different priority replaces any previous override.
func (s *Service) SwitchInput(streamID domain.StreamCode, priority int) error {
	s.mu.RLock()
	state, ok := s.streams[streamID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("manager: stream %s is not active", streamID)
	}

	state.mu.Lock()
	if state.dead {
		state.mu.Unlock()
		return fmt.Errorf("manager: stream %s is not active", streamID)
	}
	h, exists := state.inputs[priority]
	if !exists {
		state.mu.Unlock()
		return fmt.Errorf("manager: input priority %d not found in stream %s", priority, streamID)
	}
	if h.Status == domain.StatusDegraded || h.Status == domain.StatusStopped {
		state.mu.Unlock()
		return fmt.Errorf("manager: input priority %d is degraded and cannot be switched to", priority)
	}
	state.overridePriority = &priority
	state.mu.Unlock()

	slog.Info("manager: manual input switch requested",
		"stream_code", streamID,
		"priority", priority,
	)
	s.tryFailover(streamID, state)
	return nil
}

// UpdateInputs patches the live input routing table while the monitor is running.
//   - removed: deleted from state.inputs; if the active input is removed, failover is triggered.
//   - added: inserted as StatusIdle; if a higher-priority input is added, failover is triggered.
//   - updated: Input field replaced; if the active input is updated, the ingestor is restarted.
func (s *Service) UpdateInputs(
	streamID domain.StreamCode,
	added, removed, updated []domain.Input,
) {
	s.mu.RLock()
	state, ok := s.streams[streamID]
	s.mu.RUnlock()
	if !ok {
		return
	}

	var needFailover bool
	var restartInput *domain.Input

	state.mu.Lock()
	if state.dead {
		state.mu.Unlock()
		return
	}

	for _, inp := range removed {
		delete(state.inputs, inp.Priority)
		delete(state.degradedAt, inp.Priority)
		delete(state.probing, inp.Priority)
		if inp.Priority == state.active {
			needFailover = true
		}
		slog.Info("manager: input removed", "stream_code", streamID, "priority", inp.Priority)
	}

	for _, inp := range added {
		state.inputs[inp.Priority] = &InputHealth{
			Input:  inp,
			Status: domain.StatusIdle,
		}
		if inp.Priority < state.active {
			needFailover = true
		}
		slog.Info("manager: input added", "stream_code", streamID, "priority", inp.Priority)
	}

	for _, inp := range updated {
		if h, ok := state.inputs[inp.Priority]; ok {
			h.Input = inp
			if inp.Priority == state.active {
				ri := inp
				restartInput = &ri
			}
			slog.Info("manager: input updated", "stream_code", streamID, "priority", inp.Priority)
		}
	}
	bufID := state.bufferWriteID
	ctx := state.monCtx
	state.mu.Unlock()

	if needFailover {
		s.tryFailover(streamID, state)
	} else if restartInput != nil {
		if err := s.ingestor.Start(ctx, streamID, *restartInput, bufID); err != nil {
			slog.Error("manager: restart active input failed",
				"stream_code", streamID,
				"input_priority", restartInput.Priority,
				"err", err,
			)
		}
	}
}

// UpdateBufferWriteID changes the buffer slot where the ingestor writes packets.
// Used when adding/removing the transcoder (buffer topology change).
// The active ingestor is restarted to write to the new buffer.
func (s *Service) UpdateBufferWriteID(streamID domain.StreamCode, newBufID domain.StreamCode) {
	s.mu.RLock()
	state, ok := s.streams[streamID]
	s.mu.RUnlock()
	if !ok {
		return
	}

	state.mu.Lock()
	if state.dead {
		state.mu.Unlock()
		return
	}
	state.bufferWriteID = newBufID
	activeH := state.inputs[state.active]
	ctx := state.monCtx
	state.mu.Unlock()

	if activeH == nil {
		return
	}

	slog.Info("manager: buffer write ID updated",
		"stream_code", streamID,
		"new_buffer_id", newBufID,
	)

	if err := s.ingestor.Start(ctx, streamID, activeH.Input, newBufID); err != nil {
		slog.Error("manager: restart ingestor for new buffer failed",
			"stream_code", streamID,
			"err", err,
		)
	}
}

// selectBest returns the input to activate next. Caller must hold state.mu.
// If a manual override is set and the overridden input is healthy, it always wins
// regardless of its actual priority value. Otherwise the lowest priority value wins.
func selectBest(state *streamState) *InputHealth {
	if state.overridePriority != nil {
		if h, ok := state.inputs[*state.overridePriority]; ok &&
			h.Status != domain.StatusDegraded && h.Status != domain.StatusStopped {
			return h
		}
	}
	var best *InputHealth
	for _, h := range state.inputs {
		if h.Status == domain.StatusDegraded || h.Status == domain.StatusStopped {
			continue
		}
		if best == nil || h.Input.Priority < best.Input.Priority {
			best = h
		}
	}
	return best
}
