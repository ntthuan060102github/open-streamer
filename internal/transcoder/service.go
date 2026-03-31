// Package transcoder manages a bounded pool of FFmpeg worker processes.
// Each worker reads from the Buffer Hub and produces one or more output profiles (ABR).
// GPU acceleration (NVENC) is used when available.
package transcoder

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"

	"github.com/open-streamer/open-streamer/config"
	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/samber/do/v2"
)

// Profile defines a single transcoding output rendition.
type Profile struct {
	Name    string // e.g. "1080p", "720p", "480p"
	Width   int
	Height  int
	Bitrate string // e.g. "4000k"
	Codec   string // e.g. "h264_nvenc", "libx264"
	Preset  string // e.g. "p5" (NVENC), "fast" (libx264)
}

// DefaultProfiles are the standard ABR output renditions.
var DefaultProfiles = []Profile{
	{Name: "1080p", Width: 1920, Height: 1080, Bitrate: "5000k", Codec: "h264_nvenc", Preset: "p5"},
	{Name: "720p", Width: 1280, Height: 720, Bitrate: "2500k", Codec: "h264_nvenc", Preset: "p5"},
	{Name: "480p", Width: 854, Height: 480, Bitrate: "1000k", Codec: "h264_nvenc", Preset: "p5"},
}

// worker is a single FFmpeg process handling one stream.
type worker struct {
	streamID domain.StreamCode
	cancel   context.CancelFunc
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

// Start subscribes to the buffer and launches an FFmpeg worker for the stream.
func (s *Service) Start(ctx context.Context, streamID domain.StreamCode, profiles []Profile) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.workers[streamID]; ok {
		return fmt.Errorf("transcoder: stream %s already running", streamID)
	}

	workerCtx, cancel := context.WithCancel(ctx)
	s.workers[streamID] = &worker{streamID: streamID, cancel: cancel}

	go s.runWorker(workerCtx, streamID, profiles)
	return nil
}

// Stop cancels the FFmpeg worker for a stream.
func (s *Service) Stop(streamID domain.StreamCode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.workers[streamID]; ok {
		w.cancel()
		delete(s.workers, streamID)
	}
}

func (s *Service) runWorker(ctx context.Context, streamID domain.StreamCode, profiles []Profile) {
	// acquire semaphore slot
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return
	}

	sub, err := s.buf.Subscribe(streamID)
	if err != nil {
		slog.Error("transcoder: subscribe failed", "stream_code", streamID, "err", err)
		return
	}
	defer s.buf.Unsubscribe(streamID, sub)

	cmd := s.buildFFmpegCmd(ctx, profiles)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		slog.Error("transcoder: stdin pipe failed", "stream_code", streamID, "err", err)
		return
	}

	if err := cmd.Start(); err != nil {
		slog.Error("transcoder: ffmpeg start failed", "stream_code", streamID, "err", err)
		return
	}

	slog.Info("transcoder: worker started", "stream_code", streamID, "profiles", len(profiles))

	go func() {
		defer stdin.Close()
		for pkt := range sub.Recv() {
			if _, err := stdin.Write(pkt); err != nil {
				return
			}
		}
	}()

	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		slog.Error("transcoder: ffmpeg exited with error", "stream_code", streamID, "err", err)
	}
}

func (s *Service) buildFFmpegCmd(ctx context.Context, _ []Profile) *exec.Cmd {
	// TODO: build multi-output FFmpeg command from profiles
	return exec.CommandContext(ctx, s.cfg.FFmpegPath,
		"-i", "pipe:0",
		"-c:v", "h264_nvenc",
		"-preset", "p5",
		"-f", "mpegts", "pipe:1",
	)
}
