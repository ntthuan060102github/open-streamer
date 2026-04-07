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
	"log/slog"
	"sync"
	"time"

	"github.com/ntthuan060102github/open-streamer/config"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/events"
	"github.com/ntthuan060102github/open-streamer/internal/ingestor"
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
}

// streamState holds all monitoring data for a single stream.
// Lock order: Service.mu (read or write) → state.mu — never in reverse.
type streamState struct {
	mu sync.Mutex

	inputs        map[int]*InputHealth // keyed by Input.Priority; populated at Register, never mutated
	active        int                  // active Input.Priority
	bufferWriteID domain.StreamCode
	degradedAt    map[int]time.Time // when each input was last marked degraded
	probing       map[int]bool      // true while a probe goroutine is in-flight for that priority
	lastSwitchAt  time.Time         // time of the most recent active-input switch

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

// RuntimeStatus is a JSON-safe snapshot of manager state for one stream.
type RuntimeStatus struct {
	ActiveInputPriority int                   `json:"active_input_priority"`
	Inputs              []InputHealthSnapshot `json:"inputs"`
}

// InputHealthSnapshot is a serialisable copy of one input's health.
type InputHealthSnapshot struct {
	InputPriority int                 `json:"input_priority"`
	LastPacketAt  time.Time           `json:"last_packet_at"`
	BitrateKbps   float64             `json:"bitrate_kbps"`
	PacketLoss    float64             `json:"packet_loss"`
	Status        domain.StreamStatus `json:"status"`
}

// probeTask carries the arguments for a background probe goroutine.
// All fields are immutable value copies so the goroutine is safe after state.mu is released.
type probeTask struct {
	priority int
	h        *InputHealth // pointer is stable for the stream's lifetime
	input    domain.Input // value copy of the input (URL, headers, etc.)
}

// Service monitors all streams and orchestrates source failover.
type Service struct {
	bus           events.Bus
	ingestor      *ingestor.Service
	packetTimeout time.Duration
	mu            sync.RWMutex
	streams       map[domain.StreamCode]*streamState
	onExhausted   func(streamCode domain.StreamCode) // all inputs degraded — no ingest possible
	onRestored     func(streamCode domain.StreamCode) // at least one input active again after exhaustion
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

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[*config.Config](i)
	bus := do.MustInvoke[events.Bus](i)
	ing := do.MustInvoke[*ingestor.Service](i)
	sec := cfg.Manager.InputPacketTimeoutSec
	if sec <= 0 {
		sec = 30
	}
	svc := &Service{
		bus:           bus,
		ingestor:      ing,
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
		ActiveInputPriority: state.active,
		Inputs:              make([]InputHealthSnapshot, 0, len(state.inputs)),
	}
	for _, h := range state.inputs {
		out.Inputs = append(out.Inputs, InputHealthSnapshot{
			InputPriority: h.Input.Priority,
			LastPacketAt:  h.LastPacketAt,
			BitrateKbps:   h.Bitrate,
			PacketLoss:    h.PacketLoss,
			Status:        h.Status,
		})
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
				delete(state.degradedAt, inputPriority)
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
	state.degradedAt[inputPriority] = now
	state.mu.Unlock()

	slog.Warn("manager: input degraded by ingestor error",
		"stream_code", streamID,
		"input_priority", inputPriority,
		"err", err,
	)
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
	state.degradedAt[priority] = now
	*timedOut = priority
}

// collectProbeIfNeeded queues a probe task for a degraded non-active input past its cooldown.
// Caller must hold state.mu.
func (s *Service) collectProbeIfNeeded(
	state *streamState,
	h *InputHealth,
	priority int,
	now time.Time,
	tasks *[]probeTask,
) {
	if h.Status != domain.StatusDegraded || priority == state.active || state.probing[priority] {
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
	if best == nil {
		wasExhausted := state.exhausted
		state.exhausted = true
		state.mu.Unlock()

		slog.Error("manager: no healthy input available", "stream_code", streamID)
		if !wasExhausted {
			s.mu.RLock()
			cb := s.onExhausted
			s.mu.RUnlock()
			if cb != nil {
				go cb(streamID)
			}
		}
		return
	}
	if best.Input.Priority == state.active {
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

	state.mu.Lock()
	if !state.dead && state.active == prevPriority {
		if prevH := state.inputs[prevPriority]; prevH != nil && prevH.Status == domain.StatusActive {
			// Only downgrade Active → Idle; never override StatusDegraded that triggered the switch.
			prevH.Status = domain.StatusIdle
		}
		state.active = bestInput.Priority
		state.lastSwitchAt = time.Now()
		state.exhausted = false
	}
	state.mu.Unlock()

	s.bus.Publish(ctx, domain.Event{
		Type:       domain.EventInputFailover,
		StreamCode: streamID,
		Payload:    map[string]any{"from": prevPriority, "to": bestInput.Priority},
	})

	if wasExhausted {
		s.mu.RLock()
		cb := s.onRestored
		s.mu.RUnlock()
		if cb != nil {
			go cb(streamID)
		}
	}
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
	sinceSwitch := time.Since(state.lastSwitchAt)
	state.mu.Unlock()

	slog.Info("manager: degraded input recovered via probe",
		"stream_code", streamID,
		"input_priority", task.priority,
	)

	// Failback: this input has higher priority (lower value) than the fallback we are
	// currently running on, and the switch cooldown has elapsed — switch back.
	if task.priority < currentActive && sinceSwitch >= failbackSwitchCooldown {
		s.tryFailover(streamID, state)
	}
}

// selectBest returns the highest-priority (lowest Priority value) input that is
// not degraded or stopped. Caller must hold state.mu.
func selectBest(state *streamState) *InputHealth {
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
