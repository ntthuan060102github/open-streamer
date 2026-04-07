package transcoder

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/tsmux"
)

const (
	restartBaseDelay = 2 * time.Second
	restartMaxDelay  = 30 * time.Second
)

// runProfileEncoder is one FFmpeg process: raw MPEG-TS in → one profile MPEG-TS out → buffer.
// profileIndex is the 0-based ladder position (log label = buffer.VideoTrackSlug(profileIndex)).
// On unexpected crash the process is restarted with exponential backoff until ctx is cancelled.
func (s *Service) runProfileEncoder(
	ctx context.Context,
	logStream domain.StreamCode,
	rawIngestID, outBufferID domain.StreamCode,
	tc *domain.TranscoderConfig,
	profileIndex int,
	prof Profile,
) {
	track := buffer.VideoTrackSlug(profileIndex)

	// Hold the semaphore slot for the full lifetime of this profile (including retries).
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return
	}

	args, err := buildFFmpegArgs([]Profile{prof}, tc)
	if err != nil {
		slog.Error("transcoder: build ffmpeg args", "stream_code", logStream, "profile", track, "err", err)
		return
	}

	// Hold the buffer subscription across restarts so ingest packets are not lost
	// to a subscribe/unsubscribe cycle. Packets that arrive during the backoff window
	// accumulate in the subscriber channel; any overflow is silently dropped by the
	// fan-out (write-never-blocks invariant), resulting in a discontinuity on restart.
	sub, err := s.buf.Subscribe(rawIngestID)
	if err != nil {
		slog.Error("transcoder: subscribe failed", "stream_code", logStream, "profile", track, "err", err)
		return
	}
	defer s.buf.Unsubscribe(rawIngestID, sub)

	maxRestarts := s.cfg.MaxRestarts
	delay := restartBaseDelay
	attempt := 0

	for {
		crashed := s.runOnce(ctx, logStream, outBufferID, track, sub, args)
		if ctx.Err() != nil {
			return // clean shutdown
		}
		if !crashed {
			return // graceful exit (buffer closed, etc.)
		}

		attempt++
		fatal := maxRestarts > 0 && attempt >= maxRestarts
		s.bus.Publish(context.Background(), domain.Event{
			Type:       domain.EventTranscoderError,
			StreamCode: logStream,
			Payload: map[string]any{
				"profile":        track,
				"attempt":        attempt,
				"fatal":          fatal,
				"restart_in_sec": delay.Seconds(),
			},
		})

		if fatal {
			slog.Error("transcoder: ffmpeg exceeded max restarts, giving up",
				"stream_code", logStream,
				"profile", track,
				"max_restarts", maxRestarts,
			)
			s.mu.Lock()
			cb := s.onFatal
			s.mu.Unlock()
			if cb != nil {
				go cb(logStream)
			}
			return
		}

		slog.Warn("transcoder: ffmpeg crashed, restarting",
			"stream_code", logStream,
			"profile", track,
			"attempt", attempt,
			"restart_in", delay,
		)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = minDuration(delay*2, restartMaxDelay)
	}
}

// runOnce spawns one FFmpeg process and blocks until it exits.
// Returns true if FFmpeg exited unexpectedly (crash), false on a clean/expected exit.
func (s *Service) runOnce(
	ctx context.Context,
	logStream domain.StreamCode,
	outBufferID domain.StreamCode,
	track string,
	sub *buffer.Subscriber,
	args []string,
) (crashed bool) {
	cmd := exec.CommandContext(ctx, s.cfg.FFmpegPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		slog.Error("transcoder: stdin pipe failed", "stream_code", logStream, "profile", track, "err", err)
		return true
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		slog.Error("transcoder: stdout pipe failed", "stream_code", logStream, "profile", track, "err", err)
		return true
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		slog.Error("transcoder: stderr pipe failed", "stream_code", logStream, "profile", track, "err", err)
		return true
	}

	if err := cmd.Start(); err != nil {
		slog.Error("transcoder: ffmpeg start failed", "stream_code", logStream, "profile", track, "err", err)
		return true
	}

	slog.Info("transcoder: profile encoder started",
		"stream_code", logStream,
		"profile", track,
		"write_to", outBufferID,
	)

	go s.logStderr(logStream, track, stderr)

	var stdinWG sync.WaitGroup
	stdinWG.Add(1)
	go func() {
		defer stdinWG.Done()
		defer func() { _ = stdin.Close() }()
		var avMux *tsmux.FromAV
		var tsCarry []byte
		write := func(b []byte) error {
			_, werr := stdin.Write(b)
			return werr
		}
		for {
			select {
			case <-ctx.Done():
				return
			case pkt, ok := <-sub.Recv():
				if !ok {
					return
				}
				var werr error
				if len(pkt.TS) > 0 {
					tsmux.DrainTS188Aligned(&tsCarry, pkt.TS, func(b []byte) {
						if werr != nil {
							return
						}
						werr = write(b)
					})
				} else {
					tsmux.FeedWirePacket(nil, pkt.AV, &avMux, func(b []byte) {
						if werr != nil {
							return
						}
						werr = write(b)
					})
				}
				if werr != nil {
					return
				}
			}
		}
	}()

	readBuf := make([]byte, 256*1024)
	for {
		n, rerr := stdout.Read(readBuf)
		if n > 0 {
			out := make([]byte, n)
			copy(out, readBuf[:n])
			if werr := s.buf.Write(outBufferID, buffer.TSPacket(out)); werr != nil {
				slog.Error("transcoder: buffer write failed", "stream_code", logStream, "profile", track, "err", werr)
				break
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				slog.Debug("transcoder: stdout read ended", "stream_code", logStream, "profile", track, "err", rerr)
			}
			break
		}
	}

	_ = stdin.Close()
	stdinWG.Wait()

	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		slog.Error("transcoder: ffmpeg exited with error", "stream_code", logStream, "profile", track, "err", err)
		return true // crashed
	}
	return false
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
