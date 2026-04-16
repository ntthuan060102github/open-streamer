// Package hooks implements the Hook dispatcher.
// It subscribes to the Event Bus and delivers events to registered external hooks
// via HTTP webhook or Kafka — asynchronously and with retry logic.
package hooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	kafka "github.com/segmentio/kafka-go"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/events"
	"github.com/ntt0601zcoder/open-streamer/internal/store"
	"github.com/samber/do/v2"
)

// ErrHookTestUnsupported is returned when the hook type cannot receive a synthetic test delivery.
var ErrHookTestUnsupported = errors.New("hooks: test delivery not supported for this hook type")

// Service subscribes to the event bus and dispatches events to registered hooks.
type Service struct {
	cfg            config.HooksConfig
	hookRepo       store.HookRepository
	bus            events.Bus
	client         *http.Client
	kafkaWritersMu sync.Mutex
	kafkaWriters   map[string]*kafka.Writer // topic → writer, lazy-initialized
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[*config.Config](i)
	hookRepo := do.MustInvoke[store.HookRepository](i)
	bus := do.MustInvoke[events.Bus](i)

	svc := &Service{
		cfg:          cfg.Hooks,
		hookRepo:     hookRepo,
		bus:          bus,
		kafkaWriters: make(map[string]*kafka.Writer),
		// No client-level timeout — each delivery applies its own per-hook timeout
		// via context.WithTimeout in deliverHTTP.
		client: &http.Client{},
	}
	return svc, nil
}

// DeliverTestEvent sends a single synthetic event to the hook using the same path as live delivery.
func (s *Service) DeliverTestEvent(ctx context.Context, id domain.HookID) error {
	h, err := s.hookRepo.FindByID(ctx, id)
	if err != nil {
		return err
	}
	ev := domain.Event{
		ID:         fmt.Sprintf("test-%d", time.Now().UnixNano()),
		Type:       domain.EventStreamCreated,
		StreamCode: "_open_streamer_test_",
		OccurredAt: time.Now(),
		Payload:    map[string]any{"test": true, "hook_id": string(h.ID)},
	}
	switch h.Type {
	case domain.HookTypeHTTP, domain.HookTypeKafka:
		return s.deliver(ctx, h, ev)
	default:
		return fmt.Errorf("%w: %s", ErrHookTestUnsupported, h.Type)
	}
}

// Start subscribes to all domain events and begins dispatching.
// It blocks until ctx is cancelled.
func (s *Service) Start(ctx context.Context) error {
	allEvents := []domain.EventType{
		domain.EventStreamCreated, domain.EventStreamStarted,
		domain.EventStreamStopped, domain.EventStreamDeleted,
		domain.EventInputConnected, domain.EventInputReconnecting,
		domain.EventInputDegraded, domain.EventInputFailed,
		domain.EventInputFailover, domain.EventRecordingStarted,
		domain.EventRecordingStopped, domain.EventRecordingFailed,
		domain.EventSegmentWritten,
		domain.EventTranscoderStarted, domain.EventTranscoderStopped,
		domain.EventTranscoderError,
	}

	unsubs := make([]func(), 0, len(allEvents))
	for _, et := range allEvents {
		et := et
		unsub := s.bus.Subscribe(et, func(ctx context.Context, event domain.Event) error {
			return s.dispatch(ctx, event)
		})
		unsubs = append(unsubs, unsub)
	}

	<-ctx.Done()

	for _, unsub := range unsubs {
		unsub()
	}

	// Close any open Kafka writers.
	s.kafkaWritersMu.Lock()
	defer s.kafkaWritersMu.Unlock()
	for _, w := range s.kafkaWriters {
		_ = w.Close()
	}
	return nil
}

func (s *Service) dispatch(ctx context.Context, event domain.Event) error {
	hooks, err := s.hookRepo.List(ctx)
	if err != nil {
		return fmt.Errorf("hooks: list: %w", err)
	}

	for _, h := range hooks {
		if !h.Enabled {
			continue
		}
		if !s.matches(h, event) {
			continue
		}

		if err := s.deliverWithRetry(ctx, h, event); err != nil {
			slog.Error("hooks: delivery failed",
				"hook_id", h.ID,
				"event_type", event.Type,
				"stream_code", event.StreamCode,
				"err", err,
			)
		}
	}
	return nil
}

func (s *Service) matches(h *domain.Hook, event domain.Event) bool {
	if len(h.EventTypes) > 0 {
		found := false
		for _, t := range h.EventTypes {
			if t == event.Type {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return h.StreamCodes.Matches(event.StreamCode)
}

func (s *Service) deliverWithRetry(ctx context.Context, h *domain.Hook, event domain.Event) error {
	backoffs := []time.Duration{1 * time.Second, 5 * time.Second, 30 * time.Second}
	maxRetries := h.MaxRetries
	if maxRetries == 0 {
		maxRetries = 3 // default when not set on the hook
	}
	if maxRetries > len(backoffs) {
		maxRetries = len(backoffs)
	}

	var lastErr error
	for attempt := range maxRetries + 1 {
		lastErr = s.deliver(ctx, h, event)
		if lastErr == nil {
			return nil
		}
		if attempt < maxRetries {
			slog.Warn("hooks: retrying delivery",
				"hook_id", h.ID,
				"attempt", attempt+1,
				"err", lastErr,
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoffs[attempt]):
			}
		}
	}
	return fmt.Errorf("hooks: all %d attempts failed: %w", maxRetries+1, lastErr)
}

func (s *Service) deliver(ctx context.Context, h *domain.Hook, event domain.Event) error {
	switch h.Type {
	case domain.HookTypeHTTP:
		return s.deliverHTTP(ctx, h, event)
	case domain.HookTypeKafka:
		return s.deliverKafka(ctx, h, event)
	default:
		return fmt.Errorf("unknown hook type: %s", h.Type)
	}
}

func (s *Service) deliverKafka(ctx context.Context, h *domain.Hook, event domain.Event) error {
	if len(s.cfg.KafkaBrokers) == 0 {
		return fmt.Errorf("kafka delivery: no brokers configured")
	}
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	s.kafkaWritersMu.Lock()
	w, ok := s.kafkaWriters[h.Target]
	if !ok {
		w = &kafka.Writer{
			Addr:     kafka.TCP(s.cfg.KafkaBrokers...),
			Topic:    h.Target,
			Balancer: &kafka.LeastBytes{},
		}
		s.kafkaWriters[h.Target] = w
	}
	s.kafkaWritersMu.Unlock()

	return w.WriteMessages(ctx, kafka.Message{
		Key:   []byte(event.StreamCode),
		Value: body,
	})
}

func (s *Service) deliverHTTP(ctx context.Context, h *domain.Hook, event domain.Event) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	timeoutSec := h.TimeoutSec
	if timeoutSec == 0 {
		timeoutSec = 10 // default when not set on the hook
	}
	deliverCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(deliverCtx, http.MethodPost, h.Target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if h.Secret != "" {
		mac := hmac.New(sha256.New, []byte(h.Secret))
		mac.Write(body)
		req.Header.Set("X-OpenStreamer-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("http post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}
	return nil
}
