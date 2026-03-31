// Package manager implements the Stream Manager — the failover engine.
// It monitors input health (bitrate, FPS, packet loss, timeout) and seamlessly
// switches to the best available input when the active one degrades or fails.
// Failover is handled entirely in Go — FFmpeg is never restarted for this purpose.
package manager

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/events"
	"github.com/open-streamer/open-streamer/internal/ingestor"
	"github.com/samber/do/v2"
)

// InputHealth tracks the runtime health state of a single input source.
type InputHealth struct {
	Input        domain.Input
	LastPacketAt time.Time
	Bitrate      float64 // kbps
	PacketLoss   float64 // percent
	Status       domain.StreamStatus
}

// streamState holds the monitoring context for a stream.
type streamState struct {
	inputs       map[int]*InputHealth // key = Input.Priority
	active       int                  // active Input.Priority
	degradedAt   map[int]time.Time
	probing      map[int]bool
	lastSwitchAt time.Time
	cancel       context.CancelFunc
}

// RuntimeStatus is a snapshot of manager state for one stream (for the HTTP API).
type RuntimeStatus struct {
	ActiveInputPriority int                   `json:"active_input_priority"`
	Inputs              []InputHealthSnapshot `json:"inputs"`
}

// InputHealthSnapshot is a copy of input health safe to serialize to JSON.
type InputHealthSnapshot struct {
	InputPriority int                 `json:"input_priority"`
	LastPacketAt  time.Time           `json:"last_packet_at"`
	BitrateKbps   float64             `json:"bitrate_kbps"`
	PacketLoss    float64             `json:"packet_loss"`
	Status        domain.StreamStatus `json:"status"`
}

// Service monitors all streams and orchestrates failover.
type Service struct {
	bus      events.Bus
	ingestor *ingestor.Service
	mu       sync.RWMutex
	streams  map[domain.StreamCode]*streamState
}

const (
	failbackProbeCooldown  = 8 * time.Second
	failbackSwitchCooldown = 12 * time.Second
	probeTimeout           = 3 * time.Second
)

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	bus := do.MustInvoke[events.Bus](i)
	ing := do.MustInvoke[*ingestor.Service](i)
	svc := &Service{
		bus:      bus,
		ingestor: ing,
		streams:  make(map[domain.StreamCode]*streamState),
	}
	ing.SetPacketObserver(svc.RecordPacket)
	ing.SetInputErrorObserver(svc.ReportInputError)
	return svc, nil
}

// Register starts health monitoring for a stream's inputs and begins ingest on the best input.
func (s *Service) Register(ctx context.Context, stream *domain.Stream) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	monCtx, cancel := context.WithCancel(ctx)

	state := &streamState{
		inputs:       make(map[int]*InputHealth),
		degradedAt:   make(map[int]time.Time),
		probing:      make(map[int]bool),
		lastSwitchAt: time.Now(),
		cancel:       cancel,
	}
	for _, input := range stream.Inputs {
		state.inputs[input.Priority] = &InputHealth{
			Input:  input,
			Status: domain.StatusIdle,
		}
	}

	s.streams[stream.Code] = state
	go s.monitor(monCtx, stream.Code)

	if best := s.selectBest(state); best != nil {
		state.active = best.Input.Priority
		if err := s.ingestor.Start(monCtx, stream.Code, best.Input); err != nil {
			slog.Error("manager: initial ingest start failed",
				"stream_code", stream.Code,
				"input_priority", best.Input.Priority,
				"err", err,
			)
		}
	}

	return nil
}

// Unregister stops ingest and health monitoring for a stream.
func (s *Service) Unregister(streamID domain.StreamCode) {
	s.mu.Lock()
	if state, ok := s.streams[streamID]; ok {
		state.cancel()
		delete(s.streams, streamID)
		s.mu.Unlock()
		s.ingestor.Stop(streamID)
		return
	}
	s.mu.Unlock()
}

// IsRegistered reports whether the stream is under manager supervision.
func (s *Service) IsRegistered(streamID domain.StreamCode) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.streams[streamID]
	return ok
}

// RuntimeStatus returns a snapshot of runtime input health, or ok=false if the stream is not registered.
func (s *Service) RuntimeStatus(streamID domain.StreamCode) (RuntimeStatus, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.streams[streamID]
	if !ok {
		return RuntimeStatus{}, false
	}
	out := RuntimeStatus{ActiveInputPriority: state.active}
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

// RecordPacket updates the last-seen timestamp for an input — called by the Ingestor.
func (s *Service) RecordPacket(streamID domain.StreamCode, inputPriority int) {
	s.mu.RLock()
	state, ok := s.streams[streamID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	h, ok := state.inputs[inputPriority]
	if !ok {
		return
	}
	h.LastPacketAt = time.Now()
	h.Status = domain.StatusActive
	delete(state.degradedAt, inputPriority)
}

// ReportInputError marks an input degraded immediately and triggers failover.
func (s *Service) ReportInputError(streamID domain.StreamCode, inputPriority int, err error) {
	s.mu.RLock()
	state, ok := s.streams[streamID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	h, ok := state.inputs[inputPriority]
	if !ok {
		return
	}
	if h.Status == domain.StatusDegraded || h.Status == domain.StatusStopped {
		return
	}

	h.Status = domain.StatusDegraded
	state.degradedAt[inputPriority] = time.Now()
	slog.Warn("manager: input degraded by ingestor error",
		"stream_code", streamID,
		"input_priority", inputPriority,
		"err", err,
	)
	s.bus.Publish(context.Background(), domain.Event{
		Type:       domain.EventInputDegraded,
		StreamCode: streamID,
		Payload: map[string]any{
			"input_priority": inputPriority,
			"reason":         err.Error(),
		},
	})
	s.tryFailover(context.Background(), streamID, state)
}

func (s *Service) monitor(ctx context.Context, streamID domain.StreamCode) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkHealth(ctx, streamID)
		}
	}
}

func (s *Service) checkHealth(ctx context.Context, streamID domain.StreamCode) {
	s.mu.RLock()
	state, ok := s.streams[streamID]
	s.mu.RUnlock()
	if !ok {
		return
	}

	const timeout = 5 * time.Second
	for priority, h := range state.inputs {
		if time.Since(h.LastPacketAt) > timeout && h.Status == domain.StatusActive {
			slog.Warn("manager: input timeout detected",
				"stream_code", streamID,
				"input_priority", priority,
			)
			h.Status = domain.StatusDegraded
			state.degradedAt[priority] = time.Now()
			s.bus.Publish(ctx, domain.Event{
				Type:       domain.EventInputDegraded,
				StreamCode: streamID,
				Payload:    map[string]any{"input_priority": priority},
			})
			s.tryFailover(ctx, streamID, state)
		}
		s.probeDegradedInput(streamID, state, priority, h)
	}
}

func (s *Service) probeDegradedInput(streamID domain.StreamCode, state *streamState, priority int, h *InputHealth) {
	if h.Status != domain.StatusDegraded || priority == state.active {
		return
	}
	if state.probing[priority] {
		return
	}
	at, ok := state.degradedAt[priority]
	if !ok || time.Since(at) < failbackProbeCooldown {
		return
	}

	state.probing[priority] = true
	input := h.Input
	go func() {
		defer func() {
			state.probing[priority] = false
		}()

		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		if err := s.ingestor.Probe(ctx, input); err != nil {
			return
		}

		h.Status = domain.StatusIdle
		delete(state.degradedAt, priority)
		slog.Info("manager: degraded input recovered by probe",
			"stream_code", streamID,
			"input_priority", priority,
		)

		if priority < state.active && time.Since(state.lastSwitchAt) >= failbackSwitchCooldown {
			s.tryFailover(context.Background(), streamID, state)
		}
	}()
}

func (s *Service) tryFailover(ctx context.Context, streamID domain.StreamCode, state *streamState) {
	best := s.selectBest(state)
	if best == nil {
		slog.Error("manager: no healthy input available", "stream_code", streamID)
		return
	}
	if best.Input.Priority == state.active {
		return
	}

	slog.Info("manager: switching input",
		"stream_code", streamID,
		"from", state.active,
		"to", best.Input.Priority,
	)

	if err := s.ingestor.Start(ctx, streamID, best.Input); err != nil {
		slog.Error("manager: failed to start new ingestor", "stream_code", streamID, "err", err)
		return
	}

	prev := state.active
	state.active = best.Input.Priority
	state.lastSwitchAt = time.Now()

	s.bus.Publish(ctx, domain.Event{
		Type:       domain.EventInputFailover,
		StreamCode: streamID,
		Payload:    map[string]any{"from": prev, "to": best.Input.Priority},
	})
}

// selectBest picks the highest-priority input (lower Priority value = higher priority).
func (s *Service) selectBest(state *streamState) *InputHealth {
	var best *InputHealth
	for _, h := range state.inputs {
		// Failover candidate must not be degraded/stopped.
		if h.Status == domain.StatusDegraded || h.Status == domain.StatusStopped {
			continue
		}
		if best == nil || h.Input.Priority < best.Input.Priority {
			best = h
		}
	}
	return best
}
