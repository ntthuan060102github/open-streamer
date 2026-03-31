// Package publisher delivers transcoded streams to all outputs.
//
// It handles two distinct output types:
//   - Serve endpoints (HLS, DASH, RTSP, SRT listen, RTS/WebRTC): the server listens,
//     clients connect and pull. Managed by startServe().
//   - Push destinations (RTMP, SRT push, RIST, RTP): the server connects out and pushes.
//     Managed by startPush().
package publisher

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/open-streamer/open-streamer/config"
	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/pkg/ffmpeg"
	"github.com/samber/do/v2"
)

// Service manages all output workers for active streams.
type Service struct {
	cfg     config.PublisherConfig
	buf     *buffer.Service
	mu      sync.Mutex
	workers map[domain.StreamCode]context.CancelFunc
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[*config.Config](i)
	buf := do.MustInvoke[*buffer.Service](i)
	pubCfg := cfg.Publisher
	if strings.TrimSpace(pubCfg.DASHDir) == "" {
		pubCfg.DASHDir = pubCfg.HLSDir
	}

	return &Service{
		cfg:     pubCfg,
		buf:     buf,
		workers: make(map[domain.StreamCode]context.CancelFunc),
	}, nil
}

// Start launches all serve-endpoints and push-destination workers for a stream.
func (s *Service) Start(ctx context.Context, stream *domain.Stream) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.workers[stream.Code]; ok {
		return fmt.Errorf("publisher: stream %s already running", stream.Code)
	}

	workerCtx, cancel := context.WithCancel(ctx)
	s.workers[stream.Code] = cancel

	// --- Serve endpoints (clients pull from us) ---
	p := stream.Protocols
	if p.HLS {
		go s.serveHLS(workerCtx, stream.Code)
	}
	if p.DASH {
		go s.serveDASH(workerCtx, stream.Code)
	}
	if p.RTSP {
		go s.serveRTSP(workerCtx, stream.Code)
	}
	if p.RTMP {
		go s.serveRTMP(workerCtx, stream.Code)
	}
	if p.SRT {
		go s.serveSRT(workerCtx, stream.Code)
	}
	if p.RTS {
		go s.serveRTS(workerCtx, stream.Code)
	}

	// --- Push destinations (we connect out and push) ---
	for _, dest := range stream.Push {
		if dest.Enabled {
			go s.pushToDestination(workerCtx, stream.Code, dest)
		}
	}

	return nil
}

// Stop cancels all output workers for a stream.
func (s *Service) Stop(streamID domain.StreamCode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel, ok := s.workers[streamID]; ok {
		cancel()
		delete(s.workers, streamID)
	}
}

// ---- Serve endpoints --------------------------------------------------------
// Each function reads protocol config from s.cfg (global server config).

func (s *Service) serveHLS(ctx context.Context, streamID domain.StreamCode) {
	sub, err := s.buf.Subscribe(streamID)
	if err != nil {
		slog.Error("publisher: HLS subscribe failed", "stream_code", streamID, "err", err)
		return
	}
	defer s.buf.Unsubscribe(streamID, sub)

	slog.Info("publisher: HLS serve started", "stream_code", streamID, "hls_dir", s.cfg.HLSDir)

	streamDir := filepath.Join(s.cfg.HLSDir, string(streamID))
	if err := os.RemoveAll(streamDir); err != nil {
		slog.Error("publisher: HLS cleanup failed", "stream_code", streamID, "dir", streamDir, "err", err)
		return
	}
	if err := os.MkdirAll(streamDir, 0o755); err != nil {
		slog.Error("publisher: HLS mkdir failed", "stream_code", streamID, "dir", streamDir, "err", err)
		return
	}

	playlist := filepath.Join(streamDir, "index.m3u8")
	for {
		proc, err := ffmpeg.Start(ctx, "ffmpeg", ffmpegHLSArgs(playlist, s.cfg))
		if err != nil {
			slog.Error("publisher: HLS ffmpeg start failed", "stream_code", streamID, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}

		restart := false
		for !restart {
			select {
			case <-ctx.Done():
				_ = proc.Close()
				return
			case pkt, ok := <-sub.Recv():
				if !ok {
					_ = proc.Close()
					return
				}
				if _, err := proc.Stdin.Write(pkt); err != nil {
					slog.Warn("publisher: HLS ffmpeg pipe broken, restarting",
						"stream_code", streamID,
						"err", err,
					)
					_ = proc.Close()
					restart = true
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *Service) serveDASH(ctx context.Context, streamID domain.StreamCode) {
	sub, err := s.buf.Subscribe(streamID)
	if err != nil {
		slog.Error("publisher: DASH subscribe failed", "stream_code", streamID, "err", err)
		return
	}
	defer s.buf.Unsubscribe(streamID, sub)

	slog.Info("publisher: DASH serve started", "stream_code", streamID, "dash_dir", s.cfg.DASHDir)

	streamDir := filepath.Join(s.cfg.DASHDir, string(streamID))
	if err := os.RemoveAll(streamDir); err != nil {
		slog.Error("publisher: DASH cleanup failed", "stream_code", streamID, "dir", streamDir, "err", err)
		return
	}
	if err := os.MkdirAll(streamDir, 0o755); err != nil {
		slog.Error("publisher: DASH mkdir failed", "stream_code", streamID, "dir", streamDir, "err", err)
		return
	}

	manifest := filepath.Join(streamDir, "index.mpd")
	for {
		proc, err := ffmpeg.Start(ctx, "ffmpeg", ffmpegDASHArgs(manifest, s.cfg))
		if err != nil {
			slog.Error("publisher: DASH ffmpeg start failed", "stream_code", streamID, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}

		restart := false
		for !restart {
			select {
			case <-ctx.Done():
				_ = proc.Close()
				return
			case pkt, ok := <-sub.Recv():
				if !ok {
					_ = proc.Close()
					return
				}
				if _, err := proc.Stdin.Write(pkt); err != nil {
					slog.Warn("publisher: DASH ffmpeg pipe broken, restarting",
						"stream_code", streamID,
						"err", err,
					)
					_ = proc.Close()
					restart = true
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *Service) serveRTSP(ctx context.Context, streamID domain.StreamCode) {
	slog.Info("publisher: RTSP serve started", "stream_id", streamID)
	slog.Warn("publisher: RTSP serve not implemented yet", "stream_id", streamID)
	<-ctx.Done()
}

func (s *Service) serveRTMP(ctx context.Context, streamID domain.StreamCode) {
	slog.Info("publisher: RTMP serve started", "stream_id", streamID)
	slog.Warn("publisher: RTMP serve not implemented yet", "stream_id", streamID)
	<-ctx.Done()
}

func (s *Service) serveSRT(ctx context.Context, streamID domain.StreamCode) {
	slog.Info("publisher: SRT listener started", "stream_id", streamID)
	slog.Warn("publisher: SRT serve not implemented yet", "stream_id", streamID)
	<-ctx.Done()
}

func (s *Service) serveRTS(ctx context.Context, streamID domain.StreamCode) {
	slog.Info("publisher: RTS (WebRTC) serve started", "stream_id", streamID)
	slog.Warn("publisher: RTS/WebRTC serve not implemented yet", "stream_id", streamID)
	<-ctx.Done()
}

// ---- Push destinations ------------------------------------------------------

func (s *Service) pushToDestination(ctx context.Context, streamID domain.StreamCode, dest domain.PushDestination) {
	sub, err := s.buf.Subscribe(streamID)
	if err != nil {
		slog.Error("publisher: push subscribe failed", "stream_code", streamID, "url", dest.URL, "err", err)
		return
	}
	defer s.buf.Unsubscribe(streamID, sub)

	slog.Info("publisher: push destination started", "stream_code", streamID, "url", dest.URL)

	s.pushViaFFmpeg(ctx, streamID, dest, sub)
}

func (s *Service) pushViaFFmpeg(ctx context.Context, streamID domain.StreamCode, dest domain.PushDestination, sub *buffer.Subscriber) {
	retryDelay := time.Duration(dest.RetryTimeoutSec) * time.Second
	if retryDelay <= 0 {
		retryDelay = time.Second
	}

	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if dest.Limit > 0 && attempt >= dest.Limit {
			slog.Warn("publisher: push retry limit reached",
				"stream_code", streamID,
				"url", dest.URL,
				"limit", dest.Limit,
			)
			return
		}
		attempt++

		runCtx := ctx
		cancel := func() {}
		if dest.TimeoutSec > 0 {
			runCtx, cancel = context.WithTimeout(ctx, time.Duration(dest.TimeoutSec)*time.Second)
		}

		err := s.runSinglePushAttempt(runCtx, streamID, dest, sub)
		cancel()
		if err == nil || ctx.Err() != nil {
			return
		}

		slog.Warn("publisher: push failed, retrying",
			"stream_code", streamID,
			"url", dest.URL,
			"attempt", attempt,
			"err", err,
			"delay", retryDelay,
		)

		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay):
		}
	}
}

func (s *Service) runSinglePushAttempt(
	ctx context.Context,
	streamID domain.StreamCode,
	dest domain.PushDestination,
	sub *buffer.Subscriber,
) error {
	proc, err := ffmpeg.Start(ctx, "ffmpeg", ffmpegPushArgs(dest.URL))
	if err != nil {
		return fmt.Errorf("publisher: start ffmpeg: %w", err)
	}
	defer func() {
		_ = proc.Close()
	}()

	slog.Info("publisher: ffmpeg push running",
		"stream_code", streamID,
		"url", dest.URL,
		"pid", proc.PID(),
	)

	for {
		select {
		case <-ctx.Done():
			_ = proc.Close()
			return nil
		case pkt, ok := <-sub.Recv():
			if !ok {
				_ = proc.Close()
				return nil
			}
			if _, err := proc.Stdin.Write(pkt); err != nil {
				_ = proc.Close()
				return fmt.Errorf("publisher: ffmpeg stdin write: %w", err)
			}
		}
	}
}

func ffmpegPushArgs(url string) []string {
	format := "mpegts"
	if strings.HasPrefix(strings.ToLower(url), "rtmp://") {
		format = "flv"
	}
	return []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-fflags", "nobuffer",
		"-f", "mpegts",
		"-i", "pipe:0",
		"-c", "copy",
		"-f", format,
		url,
	}
}

func ffmpegHLSArgs(playlistPath string, cfg config.PublisherConfig) []string {
	segmentSec := cfg.LiveSegmentSec
	if segmentSec <= 0 {
		segmentSec = 2
	}
	window := cfg.LiveWindow
	if window <= 0 {
		window = 6
	}
	history := cfg.LiveHistory
	if history < 0 {
		history = 0
	}
	flags := "delete_segments+append_list+independent_segments"
	if cfg.LiveEphemeral {
		flags = "delete_segments+append_list+omit_endlist+independent_segments"
	}
	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-fflags", "nobuffer",
		"-f", "mpegts",
		"-i", "pipe:0",
		"-c", "copy",
		"-f", "hls",
		"-hls_time", fmt.Sprintf("%d", segmentSec),
		"-hls_list_size", fmt.Sprintf("%d", window),
		"-hls_flags", flags,
		"-hls_segment_filename", filepath.Join(filepath.Dir(playlistPath), "seg_%06d.ts"),
	}
	if cfg.LiveEphemeral {
		// Keep a small safety buffer for lagging clients.
		args = append(args, "-hls_delete_threshold", fmt.Sprintf("%d", history+1))
	}
	args = append(args, playlistPath)
	return args
}

func ffmpegDASHArgs(manifestPath string, cfg config.PublisherConfig) []string {
	segmentSec := cfg.LiveSegmentSec
	if segmentSec <= 0 {
		segmentSec = 2
	}
	window := cfg.LiveWindow
	if window <= 0 {
		window = 6
	}
	history := cfg.LiveHistory
	if history < 0 {
		history = 0
	}
	extraWindow := 6
	removeAtExit := "0"
	if cfg.LiveEphemeral {
		extraWindow = history
		removeAtExit = "1"
	}
	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-fflags", "nobuffer",
		"-f", "mpegts",
		"-i", "pipe:0",
		"-map", "0:v:0?",
		"-map", "0:a:0?",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-tune", "zerolatency",
		"-g", "50",
		"-keyint_min", "50",
		"-sc_threshold", "0",
		"-c:a", "aac",
		"-b:a", "128k",
		"-ar", "48000",
		"-ac", "2",
		"-f", "dash",
		"-seg_duration", fmt.Sprintf("%d", segmentSec),
		"-window_size", fmt.Sprintf("%d", window),
		"-extra_window_size", fmt.Sprintf("%d", extraWindow),
		"-remove_at_exit", removeAtExit,
		"-use_template", "1",
		"-use_timeline", "1",
		"-init_seg_name", "init_$RepresentationID$.m4s",
		"-media_seg_name", "chunk_$RepresentationID$_$Number%06d$.m4s",
	}
	if cfg.LiveEphemeral {
		args = append(args, "-ldash", "1")
	}
	args = append(args, manifestPath)
	return args
}
