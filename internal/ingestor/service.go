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
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/events"
	"github.com/ntt0601zcoder/open-streamer/internal/ingestor/push"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/ntt0601zcoder/open-streamer/internal/vod"
	"github.com/ntt0601zcoder/open-streamer/pkg/protocol"
	"github.com/samber/do/v2"
)

var errNoPusherConnected = errors.New("ingestor: no pusher connected")

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
	m            *metrics.Metrics
	registry     *Registry
	vods         *vod.Registry
	onPacket     func(streamID domain.StreamCode, inputPriority int)
	onInputError func(streamID domain.StreamCode, inputPriority int, err error)

	mu              sync.Mutex
	workers         map[domain.StreamCode]*pullWorkerEntry
	rtmpSrv         *push.RTMPServer // set during Run if RTMPEnabled
	pendingPlayFunc push.PlayFunc    // stored before Run, wired in once server is created
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[config.IngestorConfig](i)
	buf := do.MustInvoke[*buffer.Service](i)
	bus := do.MustInvoke[events.Bus](i)
	m := do.MustInvoke[*metrics.Metrics](i)
	vods := do.MustInvoke[*vod.Registry](i)

	return &Service{
		cfg:      cfg,
		buf:      buf,
		bus:      bus,
		m:        m,
		vods:     vods,
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

// SetRTMPPlayHandler registers an external play handler on the RTMP server.
// Must be called before Run. If the RTMP server is not enabled this is a no-op.
func (s *Service) SetRTMPPlayHandler(fn push.PlayFunc) {
	s.mu.Lock()
	srv := s.rtmpSrv
	s.mu.Unlock()
	if srv != nil {
		srv.SetPlayFunc(fn)
		return
	}
	// Server not yet created (Run not started); store for deferred wiring.
	s.mu.Lock()
	s.pendingPlayFunc = fn
	s.mu.Unlock()
}

// Run starts the push servers (RTMP, SRT) and blocks until ctx is cancelled.
// Call this in a dedicated goroutine alongside the rest of the application.
func (s *Service) Run(ctx context.Context) error {
	g, _ := errgroup.WithContext(ctx)

	if s.cfg.RTMPEnabled {
		rtmpSrv, err := push.NewRTMPServer(
			s.cfg.RTMPAddr,
			s.registry,
			func(ctx context.Context, streamID, bufferWriteID domain.StreamCode, input domain.Input) error {
				return s.startPullWorker(ctx, streamID, input, bufferWriteID)
			},
		)
		if err != nil {
			return fmt.Errorf("ingestor: create rtmp server: %w", err)
		}
		s.mu.Lock()
		s.rtmpSrv = rtmpSrv
		if s.pendingPlayFunc != nil {
			rtmpSrv.SetPlayFunc(s.pendingPlayFunc)
			s.pendingPlayFunc = nil
		}
		s.mu.Unlock()
		g.Go(func() error { return rtmpSrv.Run(ctx) })
	}

	return g.Wait()
}

// Start activates ingest for the given input on streamID.
//
// The mode is derived from the URL:
//   - publish:// → push-listen: register the stream code in the push server
//     routing table so encoders can connect with just the stream code as key.
//   - rtmp://0.0.0.0:... / srt://0.0.0.0:... → legacy push-listen (same effect).
//   - Any other URL → pull: start a goroutine that connects to the URL.
//
// Calling Start again for the same stream replaces the previous worker/registration.
// bufferWriteID is the Buffer Hub slot for MPEG-TS writes; if empty, streamID is used.
func (s *Service) Start(ctx context.Context, streamID domain.StreamCode, input domain.Input, bufferWriteID domain.StreamCode) error {
	if bufferWriteID == "" {
		bufferWriteID = streamID
	}
	if protocol.IsPushListen(input.URL) {
		return s.startPushRegistration(streamID, input, bufferWriteID)
	}
	return s.startPullWorker(ctx, streamID, input, bufferWriteID)
}

// Probe performs a short pull-read health probe for one input.
// It is used by the manager to verify degraded inputs before failback.
func (s *Service) Probe(ctx context.Context, input domain.Input) error {
	if protocol.IsPushListen(input.URL) {
		return fmt.Errorf("ingestor: probe unsupported for push-listen input %q", input.URL)
	}
	reader, err := NewPacketReader(input, s.cfg, s.vods)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()

	if err := reader.Open(ctx); err != nil {
		return err
	}
	batch, err := reader.ReadPackets(ctx)
	if err != nil {
		return err
	}
	if len(batch) == 0 {
		return fmt.Errorf("ingestor: probe got empty batch")
	}
	for _, p := range batch {
		if len(p.Data) > 0 {
			return nil
		}
	}
	return fmt.Errorf("ingestor: probe got no payload")
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

func (s *Service) startPullWorker(ctx context.Context, streamID domain.StreamCode, input domain.Input, bufferWriteID domain.StreamCode) error {
	reader, err := NewPacketReader(input, s.cfg, s.vods)
	if err != nil {
		return fmt.Errorf("ingestor: create packet reader: %w", err)
	}

	// Register new worker immediately so Stop() can cancel it at any time.
	// The previous worker is NOT cancelled here; it is cancelled by onHandoff after the
	// new source connects successfully, preventing a buffer gap during source transitions.
	s.mu.Lock()
	var prevCancel context.CancelFunc
	if prev, ok := s.workers[streamID]; ok {
		prevCancel = prev.cancel
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

	proto := string(protocol.Detect(input.URL))

	go func() {
		s.mu.Lock()
		cb := pullWorkerCallbacks{
			onPacket:     s.onPacket,
			onInputError: s.onInputError,
			onConnect: func(id domain.StreamCode, priority int) { //nolint:contextcheck // workerCtx is cancelled on stop; publish must outlive it
				s.bus.Publish(context.Background(), domain.Event{
					Type:       domain.EventInputConnected,
					StreamCode: id,
					Payload:    map[string]any{"input_priority": priority, "url": input.URL},
				})
			},
			onReconnect: func(id domain.StreamCode, priority int, err error) { //nolint:contextcheck // workerCtx is cancelled on stop; publish must outlive it
				s.bus.Publish(context.Background(), domain.Event{
					Type:       domain.EventInputReconnecting,
					StreamCode: id,
					Payload:    map[string]any{"input_priority": priority, "error": err.Error()},
				})
				s.m.IngestorErrorsTotal.WithLabelValues(string(id), "reconnect").Inc()
			},
			onPacketBytes: func(id domain.StreamCode, _ int, n int) {
				s.m.IngestorBytesTotal.WithLabelValues(string(id), proto).Add(float64(n))
				s.m.IngestorPacketsTotal.WithLabelValues(string(id), proto).Inc()
			},
			onHandoff: func() {
				if prevCancel != nil {
					slog.Info("ingestor: pre-connect handoff — releasing previous source",
						"stream_code", streamID,
						"new_input_priority", input.Priority,
					)
					prevCancel()
				}
			},
		}
		s.mu.Unlock()
		runPullWorker(workerCtx, streamID, bufferWriteID, input, reader, s.buf, cb)
		// Ensure the previous worker is always released — handles the case where Stop()
		// is called during pre-connect before onHandoff had a chance to fire.
		if prevCancel != nil {
			prevCancel()
		}
		cancel()
		s.m.IngestorErrorsTotal.WithLabelValues(string(streamID), "failover").Inc()
		//nolint:contextcheck // worker ctx is cancelled; publish must outlive it for hooks/manager.
		s.bus.Publish(context.Background(), domain.Event{
			Type:       domain.EventInputFailed,
			StreamCode: streamID,
		})
	}()

	return nil
}

func (s *Service) startPushRegistration(streamID domain.StreamCode, input domain.Input, bufferWriteID domain.StreamCode) error {
	key := pushStreamKey(streamID, input)
	if key == "" {
		return fmt.Errorf("ingestor: cannot determine stream key from URL %q", input.URL)
	}

	s.registry.Register(key, streamID, s.buf, bufferWriteID)

	slog.Info("ingestor: push slot registered",
		"stream_code", streamID,
		"input_priority", input.Priority,
		"url", input.URL,
		"stream_key", key,
	)

	// No pusher is connected yet — notify the manager so it can fall back to
	// a lower-priority input immediately rather than waiting for the packet timeout.
	s.mu.Lock()
	errObserver := s.onInputError
	s.mu.Unlock()
	if errObserver != nil {
		errObserver(streamID, input.Priority, errNoPusherConnected)
	}
	return nil
}

// pushStreamKey returns the routing key that encoders must use when connecting.
//
// For publish:// URLs the key is the stream code itself — the encoder only
// needs to know the stream code, no app-name prefix is required.
//
// For legacy wildcard-host URLs (rtmp://0.0.0.0:1935/live/<key>) the last
// path segment is used, preserving backward compatibility.
func pushStreamKey(streamID domain.StreamCode, input domain.Input) string {
	if protocol.Detect(input.URL) == protocol.KindPublish {
		return string(streamID)
	}
	// Legacy: extract last path segment from rtmp://0.0.0.0:…/live/<key>
	raw := strings.TrimRight(input.URL, "/")
	if idx := strings.LastIndex(raw, "/"); idx >= 0 {
		return raw[idx+1:]
	}
	return ""
}
