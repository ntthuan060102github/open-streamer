// Package ingestor handles raw stream ingestion.
//
// The ingestor accepts a plain URL and automatically derives everything from it:
//   - Protocol (RTMP, RTSP, SRT, UDP, HLS, file, …) → from URL scheme
//   - Mode (pull vs push-listen) → from URL host (wildcard = listen)
//
// Pull workers are created per-stream and reconnect automatically.
// Push servers (RTMP, SRT) are global, started once, and route by stream key.
package ingestor

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/open-streamer/open-streamer/config"
	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/events"
	"github.com/open-streamer/open-streamer/internal/ingestor/push"
	"github.com/open-streamer/open-streamer/pkg/protocol"
	"github.com/samber/do/v2"
)

type pullWorkerEntry struct {
	inputPriority int
	cancel        context.CancelFunc
}

// Service manages the full ingest layer.
//
//   - Pull inputs: one goroutine per active stream, auto-reconnects.
//   - Push inputs: stream key is registered in the push server routing table;
//     the next encoder connection for that key is accepted and routed here.
type Service struct {
	cfg          config.IngestorConfig
	buf          *buffer.Service
	bus          events.Bus
	registry     *Registry
	onPacket     func(streamID domain.StreamCode, inputPriority int)
	onInputError func(streamID domain.StreamCode, inputPriority int, err error)

	mu      sync.Mutex
	workers map[domain.StreamCode]*pullWorkerEntry
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[*config.Config](i)
	buf := do.MustInvoke[*buffer.Service](i)
	bus := do.MustInvoke[events.Bus](i)

	return &Service{
		cfg:      cfg.Ingestor,
		buf:      buf,
		bus:      bus,
		registry: NewRegistry(),
		workers:  make(map[domain.StreamCode]*pullWorkerEntry),
	}, nil
}

// SetPacketObserver configures a callback invoked for each packet read.
func (s *Service) SetPacketObserver(fn func(streamID domain.StreamCode, inputPriority int)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onPacket = fn
}

// SetInputErrorObserver configures a callback invoked on input read/open failures.
func (s *Service) SetInputErrorObserver(fn func(streamID domain.StreamCode, inputPriority int, err error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onInputError = fn
}

// Run starts the push servers (RTMP, SRT) and blocks until ctx is cancelled.
// Call this in a dedicated goroutine alongside the rest of the application.
func (s *Service) Run(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)

	if s.cfg.RTMPEnabled {
		srv := push.NewRTMPServer(s.cfg, s.registry)
		g.Go(func() error {
			return srv.ListenAndServe(gctx)
		})
	}

	if s.cfg.SRTEnabled {
		srv := push.NewSRTServer(s.cfg, s.registry)
		g.Go(func() error {
			return srv.ListenAndServe(gctx)
		})
	}

	return g.Wait()
}

// Start activates ingest for the given input on streamID.
//
// The mode is derived entirely from the URL:
//   - If the URL host is a wildcard/loopback address (e.g. rtmp://0.0.0.0:1935/live/key)
//     → push-listen: register the stream key in the push server routing table.
//   - Otherwise → pull: start a goroutine that connects to the URL and reads data.
//
// Calling Start again for the same stream replaces the previous worker/registration.
func (s *Service) Start(ctx context.Context, streamID domain.StreamCode, input domain.Input) error {
	if protocol.IsPushListen(input.URL) {
		return s.startPushRegistration(streamID, input)
	}
	return s.startPullWorker(ctx, streamID, input)
}

// Probe performs a short pull-read health probe for one input.
// It is used by the manager to verify degraded inputs before failback.
func (s *Service) Probe(ctx context.Context, input domain.Input) error {
	if protocol.IsPushListen(input.URL) {
		return fmt.Errorf("ingestor: probe unsupported for push-listen input %q", input.URL)
	}
	reader, err := NewReader(input, s.cfg)
	if err != nil {
		return err
	}
	defer reader.Close()

	if err := reader.Open(ctx); err != nil {
		return err
	}
	pkt, err := reader.Read(ctx)
	if err != nil {
		return err
	}
	if len(pkt) == 0 {
		return fmt.Errorf("ingestor: probe got empty packet")
	}
	return nil
}

// Stop cancels the pull worker or unregisters the push key for streamID.
func (s *Service) Stop(streamID domain.StreamCode) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if w, ok := s.workers[streamID]; ok {
		slog.Info("ingestor: stopping pull worker",
			"stream_code", streamID,
			"input_priority", w.inputPriority,
		)
		w.cancel()
		delete(s.workers, streamID)
	}
}

// ---- private ----

func (s *Service) startPullWorker(ctx context.Context, streamID domain.StreamCode, input domain.Input) error {
	reader, err := NewReader(input, s.cfg)
	if err != nil {
		return fmt.Errorf("ingestor: create reader: %w", err)
	}

	s.mu.Lock()
	if prev, ok := s.workers[streamID]; ok {
		prev.cancel() // seamless source switch
	}
	workerCtx, cancel := context.WithCancel(ctx)
	s.workers[streamID] = &pullWorkerEntry{inputPriority: input.Priority, cancel: cancel}
	s.mu.Unlock()

	slog.Info("ingestor: starting pull worker",
		"stream_code", streamID,
		"input_priority", input.Priority,
		"url", input.URL,
		"protocol", protocol.Detect(input.URL),
	)

	go func() {
		s.mu.Lock()
		observer := s.onPacket
		errObserver := s.onInputError
		s.mu.Unlock()
		runPullWorker(workerCtx, streamID, input, reader, s.buf, observer, errObserver)
		cancel()
		s.bus.Publish(context.Background(), domain.Event{
			Type:       domain.EventInputFailed,
			StreamCode: streamID,
		})
	}()

	return nil
}

func (s *Service) startPushRegistration(streamID domain.StreamCode, input domain.Input) error {
	key := pushStreamKey(input)
	if key == "" {
		return fmt.Errorf("ingestor: cannot determine stream key from URL %q (path must end in /<key>)", input.URL)
	}

	s.registry.Register(key, streamID, s.buf)

	slog.Info("ingestor: push slot registered",
		"stream_code", streamID,
		"input_priority", input.Priority,
		"url", input.URL,
		"stream_key", key,
	)
	return nil
}

// pushStreamKey extracts the routing key from the last path segment of the URL.
//
// Example: rtmp://0.0.0.0:1935/live/myshow → "myshow"
func pushStreamKey(input domain.Input) string {
	url := strings.TrimRight(input.URL, "/")
	if idx := strings.LastIndex(url, "/"); idx >= 0 {
		return url[idx+1:]
	}
	return ""
}
