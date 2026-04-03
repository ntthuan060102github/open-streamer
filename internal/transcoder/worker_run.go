package transcoder

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"sync"

	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/tsmux"
)

// runProfileEncoder is one FFmpeg process: raw MPEG-TS in → one profile MPEG-TS out → buffer.
// profileIndex is the 0-based ladder position (log label = buffer.VideoTrackSlug(profileIndex)).
func (s *Service) runProfileEncoder(
	ctx context.Context,
	logStream domain.StreamCode,
	rawIngestID, outBufferID domain.StreamCode,
	tc *domain.TranscoderConfig,
	profileIndex int,
	prof Profile,
) {
	track := buffer.VideoTrackSlug(profileIndex)
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

	sub, err := s.buf.Subscribe(rawIngestID)
	if err != nil {
		slog.Error("transcoder: subscribe failed", "stream_code", logStream, "profile", track, "err", err)
		return
	}
	defer s.buf.Unsubscribe(rawIngestID, sub)

	cmd := exec.CommandContext(ctx, s.cfg.FFmpegPath, args...)
	slog.Info("transcoder: ffmpeg command",
		"stream_code", logStream,
		"profile", track,
		"cmd", cmd.String(),
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		slog.Error("transcoder: stdin pipe failed", "stream_code", logStream, "profile", track, "err", err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		slog.Error("transcoder: stdout pipe failed", "stream_code", logStream, "profile", track, "err", err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		slog.Error("transcoder: stderr pipe failed", "stream_code", logStream, "profile", track, "err", err)
		return
	}

	if err := cmd.Start(); err != nil {
		slog.Error("transcoder: ffmpeg start failed", "stream_code", logStream, "profile", track, "err", err)
		return
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
			_, err := stdin.Write(b)
			return err
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
	}
}
