// Package transcoder manages a bounded pool of FFmpeg worker processes.
// Each stream may run one FFmpeg process per ladder rung (passthrough copy or ABR encode);
// all read the same raw ingest.
// GPU acceleration (NVENC) is used when configured on profiles / global HW.
package transcoder

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/ntthuan060102github/open-streamer/config"
	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/events"
	"github.com/ntthuan060102github/open-streamer/internal/metrics"
	"github.com/samber/do/v2"
)

// Profile defines a single transcoding output rendition.
// Rendition label in logs and URLs is track_<n> from ladder order (see buffer.VideoTrackSlug).
type Profile struct {
	Width            int
	Height           int
	Bitrate          string // e.g. "4000k"
	Codec            string // e.g. "h264_nvenc", "libx264"
	Preset           string // e.g. "p5" (NVENC), "fast" (libx264)
	CodecProfile     string // H.264/HEVC profile (baseline, main, high)
	CodecLevel       string // e.g. 4.1
	MaxBitrate       int    // kbps peak (0 = omit -maxrate)
	Framerate        float64
	KeyframeInterval int // GOP target in seconds (0 = encoder default)
}

// profileWorker tracks a single FFmpeg encoder process (one ABR ladder rung).
type profileWorker struct {
	cancel context.CancelFunc
	done   chan struct{} // closed when the goroutine exits
}

// streamWorker holds all profile encoders for one stream.
type streamWorker struct {
	baseCtx    context.Context
	baseCancel context.CancelFunc // cancels all profiles at once (Stop)
	rawIngest  domain.StreamCode
	tc         *domain.TranscoderConfig
	mu         sync.Mutex
	profiles   map[int]*profileWorker // key = profile index (0-based)
}

// Service manages the FFmpeg worker pool.
type Service struct {
	cfg     config.TranscoderConfig
	buf     *buffer.Service
	bus     events.Bus
	m       *metrics.Metrics
	sem     chan struct{} // bounded semaphore to cap concurrent FFmpeg processes
	mu      sync.Mutex
	workers map[domain.StreamCode]*streamWorker
	onFatal func(streamCode domain.StreamCode) // called when a profile exceeds MaxRestarts
}

// SetFatalCallback registers a function called when a transcoder profile exceeds its
// maximum restart count. The callback fires once per profile that gives up.
// It is safe to call this before or after Start.
func (s *Service) SetFatalCallback(fn func(streamCode domain.StreamCode)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onFatal = fn
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[*config.Config](i)
	buf := do.MustInvoke[*buffer.Service](i)
	bus := do.MustInvoke[events.Bus](i)
	m := do.MustInvoke[*metrics.Metrics](i)

	return &Service{
		cfg:     cfg.Transcoder,
		buf:     buf,
		bus:     bus,
		m:       m,
		sem:     make(chan struct{}, cfg.Transcoder.MaxWorkers),
		workers: make(map[domain.StreamCode]*streamWorker),
	}, nil
}

// Start launches one FFmpeg encoder per RenditionTarget (same raw ingest, different output buffers).
func (s *Service) Start(
	ctx context.Context,
	logStreamCode domain.StreamCode,
	rawIngestID domain.StreamCode,
	tc *domain.TranscoderConfig,
	targets []RenditionTarget,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.workers[logStreamCode]; ok {
		return fmt.Errorf("transcoder: stream %s already running", logStreamCode)
	}

	if len(targets) == 0 {
		return fmt.Errorf("transcoder: no rendition targets")
	}

	baseCtx, baseCancel := context.WithCancel(ctx)
	sw := &streamWorker{
		baseCtx:    baseCtx,
		baseCancel: baseCancel,
		rawIngest:  rawIngestID,
		tc:         tc,
		profiles:   make(map[int]*profileWorker, len(targets)),
	}
	s.workers[logStreamCode] = sw

	slog.Info("transcoder: stream job started",
		"stream_code", logStreamCode,
		"profiles", len(targets),
		"read_from", rawIngestID,
	)

	s.m.TranscoderWorkersActive.WithLabelValues(string(logStreamCode)).Set(float64(len(targets)))
	s.m.TranscoderQualitiesActive.WithLabelValues(string(logStreamCode)).Set(float64(len(targets)))
	s.bus.Publish(ctx, domain.Event{
		Type:       domain.EventTranscoderStarted,
		StreamCode: logStreamCode,
		Payload:    map[string]any{"profiles": len(targets), "raw_ingest_id": string(rawIngestID)},
	})

	for i, t := range targets {
		s.spawnProfile(logStreamCode, sw, i, t)
	}
	return nil
}

// Stop cancels all FFmpeg encoders for a stream and waits for them to exit.
func (s *Service) Stop(streamID domain.StreamCode) {
	s.mu.Lock()
	sw, ok := s.workers[streamID]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.workers, streamID)
	s.mu.Unlock()

	sw.baseCancel()

	// Wait for all profile goroutines to finish.
	sw.mu.Lock()
	profiles := make(map[int]*profileWorker, len(sw.profiles))
	for k, v := range sw.profiles {
		profiles[k] = v
	}
	sw.mu.Unlock()
	for _, pw := range profiles {
		<-pw.done
	}

	s.m.TranscoderWorkersActive.WithLabelValues(string(streamID)).Set(0)
	s.m.TranscoderQualitiesActive.WithLabelValues(string(streamID)).Set(0)
	//nolint:contextcheck // baseCtx is cancelled; publish must outlive it
	s.bus.Publish(context.Background(), domain.Event{
		Type:       domain.EventTranscoderStopped,
		StreamCode: streamID,
	})
}

// StopProfile stops a single FFmpeg encoder for one profile index.
func (s *Service) StopProfile(streamID domain.StreamCode, profileIndex int) {
	s.mu.Lock()
	sw, ok := s.workers[streamID]
	s.mu.Unlock()
	if !ok {
		return
	}

	sw.mu.Lock()
	pw, ok := sw.profiles[profileIndex]
	if !ok {
		sw.mu.Unlock()
		return
	}
	delete(sw.profiles, profileIndex)
	sw.mu.Unlock()

	pw.cancel()
	<-pw.done

	slog.Info("transcoder: profile stopped",
		"stream_code", streamID,
		"profile", buffer.VideoTrackSlug(profileIndex),
	)
	s.updateMetrics(streamID, sw)
}

// StartProfile starts a single FFmpeg encoder for one profile index on an existing stream worker.
func (s *Service) StartProfile(streamID domain.StreamCode, profileIndex int, target RenditionTarget) error {
	s.mu.Lock()
	sw, ok := s.workers[streamID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("transcoder: stream %s not running", streamID)
	}

	sw.mu.Lock()
	if _, exists := sw.profiles[profileIndex]; exists {
		sw.mu.Unlock()
		return fmt.Errorf("transcoder: profile %d already running for stream %s", profileIndex, streamID)
	}
	sw.mu.Unlock()

	s.spawnProfile(streamID, sw, profileIndex, target)

	slog.Info("transcoder: profile started",
		"stream_code", streamID,
		"profile", buffer.VideoTrackSlug(profileIndex),
		"write_to", target.BufferID,
	)
	s.updateMetrics(streamID, sw)
	return nil
}

// spawnProfile creates a per-profile context and launches the encoder goroutine.
func (s *Service) spawnProfile(streamID domain.StreamCode, sw *streamWorker, profileIndex int, target RenditionTarget) {
	profileCtx, profileCancel := context.WithCancel(sw.baseCtx)
	pw := &profileWorker{
		cancel: profileCancel,
		done:   make(chan struct{}),
	}

	sw.mu.Lock()
	sw.profiles[profileIndex] = pw
	sw.mu.Unlock()

	go func() {
		defer close(pw.done)
		s.runProfileEncoder(profileCtx, streamID, sw.rawIngest, target.BufferID, sw.tc, profileIndex, target.Profile)
	}()
}

// updateMetrics refreshes the active worker/quality gauge for a stream.
func (s *Service) updateMetrics(streamID domain.StreamCode, sw *streamWorker) {
	sw.mu.Lock()
	n := float64(len(sw.profiles))
	sw.mu.Unlock()
	s.m.TranscoderWorkersActive.WithLabelValues(string(streamID)).Set(n)
	s.m.TranscoderQualitiesActive.WithLabelValues(string(streamID)).Set(n)
}

func (s *Service) logStderr(streamID domain.StreamCode, profile string, r io.Reader) {
	sc := bufio.NewScanner(r)
	const maxLine = 64 * 1024
	buf := make([]byte, maxLine)
	sc.Buffer(buf, maxLine)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		// FFmpeg compensates PTS/DTS jumps (common when ingest fails over between HLS variants).
		if strings.Contains(line, "timestamp discontinuity") && strings.Contains(line, "new offset") {
			slog.Debug("transcoder: ffmpeg timestamp resync", "stream_code", streamID, "profile", profile, "msg", line)
			continue
		}
		trim := strings.TrimSpace(line)
		if strings.Contains(line, "Packet corrupt") ||
			strings.Contains(line, "PES packet size mismatch") ||
			strings.HasPrefix(trim, "Last message repeated") {
			slog.Debug("transcoder: ffmpeg stderr", "stream_code", streamID, "profile", profile, "msg", line)
			continue
		}
		// H.264 decoder / demuxer noise: probe gaps, segment joins, B-frame reorder, MMCO.
		if strings.Contains(line, "non-existing PPS") ||
			strings.Contains(line, "no frame!") ||
			strings.Contains(line, "error while decoding MB") ||
			strings.Contains(line, "Could not find codec parameters") ||
			strings.Contains(line, "Consider increasing the value for the 'analyzeduration'") ||
			strings.Contains(line, "unspecified pixel format") ||
			strings.Contains(line, "co located POCs unavailable") ||
			strings.Contains(line, "mmco: unref short failure") ||
			strings.Contains(line, "reference picture missing during reorder") ||
			strings.Contains(line, "Missing reference picture") {
			slog.Debug("transcoder: ffmpeg stderr", "stream_code", streamID, "profile", profile, "msg", line)
			continue
		}
		slog.Warn("transcoder: ffmpeg stderr", "stream_code", streamID, "profile", profile, "msg", line)
	}
}
