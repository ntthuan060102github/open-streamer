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

// worker is a logical transcoding job for one stream (may spawn multiple FFmpeg processes).
type worker struct {
	cancel context.CancelFunc
}

// Service manages the FFmpeg worker pool.
type Service struct {
	cfg     config.TranscoderConfig
	buf     *buffer.Service
	sem     chan struct{} // bounded semaphore to cap concurrent FFmpeg processes
	mu      sync.Mutex
	workers map[domain.StreamCode]*worker
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[*config.Config](i)
	buf := do.MustInvoke[*buffer.Service](i)

	return &Service{
		cfg:     cfg.Transcoder,
		buf:     buf,
		sem:     make(chan struct{}, cfg.Transcoder.MaxWorkers),
		workers: make(map[domain.StreamCode]*worker),
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

	workerCtx, cancel := context.WithCancel(ctx)
	s.workers[logStreamCode] = &worker{cancel: cancel}

	go s.runStreamJob(workerCtx, logStreamCode, rawIngestID, tc, targets)
	return nil
}

// Stop cancels all FFmpeg encoders for a stream.
func (s *Service) Stop(streamID domain.StreamCode) {
	s.mu.Lock()
	if w, ok := s.workers[streamID]; ok {
		w.cancel()
		delete(s.workers, streamID)
	}
	s.mu.Unlock()
}

func (s *Service) runStreamJob(
	ctx context.Context,
	logStreamCode domain.StreamCode,
	rawIngestID domain.StreamCode,
	tc *domain.TranscoderConfig,
	targets []RenditionTarget,
) {
	defer func() {
		s.mu.Lock()
		delete(s.workers, logStreamCode)
		s.mu.Unlock()
	}()

	slog.Info("transcoder: stream job started",
		"stream_code", logStreamCode,
		"profiles", len(targets),
		"read_from", rawIngestID,
	)

	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(profileIndex int, t RenditionTarget) {
			defer wg.Done()
			s.runProfileEncoder(ctx, logStreamCode, rawIngestID, t.BufferID, tc, profileIndex, t.Profile)
		}(i, t)
	}
	wg.Wait()
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
