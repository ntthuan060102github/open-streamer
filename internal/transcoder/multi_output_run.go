// multi_output_run.go — single FFmpeg per stream, N output pipes (one per
// rendition). Default mode; opt out per-stream via Stream.Transcoder.Mode="per_profile".
//
// Architecture (vs legacy per-profile mode in worker_run.go):
//
//	Legacy:                       Multi-output:
//	┌──────────┐                  ┌──────────────────────────────┐
//	│ ffmpeg#0 │ video[0]         │ ffmpeg                       │
//	│ in→dec→  │─→ outBuf[0]      │  in → decode (1×) →          │
//	│  enc[0]  │                  │   ├─ scale[0] → enc[0] → fd3 │─→ outBuf[0]
//	└──────────┘                  │   ├─ scale[1] → enc[1] → fd4 │─→ outBuf[1]
//	┌──────────┐                  │   └─ ...                     │
//	│ ffmpeg#1 │ video[1]         └──────────────────────────────┘
//	│ in→dec→  │─→ outBuf[1]
//	│  enc[1]  │
//	└──────────┘
//
// Wins:
//   - decode shared once → ~50% NVDEC + container demux saved per ABR stream
//   - process baseline shared → ~40% RAM saved (1 FFmpeg vs N)
//   - 1 CUDA context vs N
//
// Trade-offs (caller already accepted via the config flag):
//   - 1 input glitch crashes the whole pipeline (all profiles down ~2-3s
//     for restart, vs 1 profile in legacy mode)
//   - per-profile bitrate tweak restarts the whole pipeline (legacy mode
//     restarts only the affected profile)

package transcoder

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
)

// runStreamEncoder is the multi-output equivalent of runProfileEncoder. It
// holds ONE FFmpeg subprocess that emits all profiles via separate output
// pipes (fd 3, 4, 5…). Restarts the whole pipeline on any crash.
//
// Subscribes to rawIngestID once. Each output pipe drains into its
// corresponding outBufferID.
func (s *Service) runStreamEncoder(
	ctx context.Context,
	logStream domain.StreamCode,
	rawIngestID domain.StreamCode,
	tc *domain.TranscoderConfig,
	targets []RenditionTarget,
) {
	if len(targets) == 0 {
		slog.Error("transcoder: multi-output: no targets", "stream_code", logStream)
		return
	}
	profiles := make([]Profile, len(targets))
	for i, t := range targets {
		profiles[i] = t.Profile
	}

	args, err := buildMultiOutputArgs(profiles, tc)
	if err != nil {
		slog.Error("transcoder: multi-output: build args failed",
			"stream_code", logStream, "err", err)
		return
	}

	// Hold the buffer subscription across restarts (same rationale as
	// legacy mode: avoid losing packets to subscribe/unsubscribe cycles).
	sub, err := s.buf.Subscribe(rawIngestID)
	if err != nil {
		slog.Error("transcoder: multi-output: subscribe failed",
			"stream_code", logStream, "err", err)
		return
	}
	defer s.buf.Unsubscribe(rawIngestID, sub)

	delay := restartBaseDelay
	attempt := 0
	var lastErrMsg string
	consecutiveSame := 0
	const visibleConsecutiveCap = 3
	// Multi-output runs ONE FFmpeg per stream so health is reported
	// against profile index 0 (the synthetic "stream encoder" entry —
	// same index spawnMultiOutput uses in the streamWorker map).
	consecutiveFastCrashes := 0
	const healthProfileIdx = 0

	for {
		startedAt := time.Now()
		crashed, runErr := s.runOnceMultiOutput(ctx, logStream, sub, args, targets)
		runDur := time.Since(startedAt)
		if ctx.Err() != nil {
			return
		}
		if !crashed {
			s.fireHealthyIfTransitioned(logStream, healthProfileIdx)
			return
		}

		// Sustained run before this crash → reset fast-crash counter
		// and clear unhealthy flag if previously set.
		if runDur >= healthSustainDur {
			consecutiveFastCrashes = 0
			s.fireHealthyIfTransitioned(logStream, healthProfileIdx)
		}

		attempt++
		errMsg := "ffmpeg crashed"
		if runErr != nil {
			errMsg = runErr.Error()
		}
		// Multi-output crash affects every profile — record the same error
		// against each so the per-profile error history accurately shows
		// "all rungs went down together".
		for i := range targets {
			s.recordProfileError(logStream, i, errMsg)
		}
		s.m.TranscoderRestartsTotal.WithLabelValues(string(logStream)).Inc()

		if runDur < healthSustainDur {
			consecutiveFastCrashes++
			if consecutiveFastCrashes >= healthDegradeThreshold {
				s.fireUnhealthyIfTransitioned(logStream, healthProfileIdx, errMsg)
			}
		}

		if errMsg == lastErrMsg {
			consecutiveSame++
		} else {
			consecutiveSame = 1
			lastErrMsg = errMsg
		}
		isPowerOf2 := attempt&(attempt-1) == 0
		visible := consecutiveSame <= visibleConsecutiveCap || isPowerOf2

		if visible {
			slog.Warn("transcoder: ffmpeg multi-output crashed, restarting",
				"stream_code", logStream,
				"attempt", attempt,
				"consecutive_same_err", consecutiveSame,
				"restart_in", delay,
				"profiles", len(targets),
			)
			//nolint:contextcheck // ctx may be cancelled after crash; publish must outlive it
			s.bus.Publish(context.Background(), domain.Event{
				Type:       domain.EventTranscoderError,
				StreamCode: logStream,
				Payload: map[string]any{
					"mode":           "multi_output",
					"attempt":        attempt,
					"restart_in_sec": delay.Seconds(),
					"error":          errMsg,
					"profiles":       len(targets),
				},
			})
		} else {
			slog.Debug("transcoder: ffmpeg multi-output crashed (suppressed)",
				"stream_code", logStream,
				"attempt", attempt,
				"consecutive_same_err", consecutiveSame,
				"restart_in", delay,
				"err", errMsg,
			)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = minDuration(delay*2, restartMaxDelay)
	}
}

// runOnceMultiOutput spawns one FFmpeg with N extra output pipes (one per
// target via cmd.ExtraFiles). Blocks until FFmpeg exits or all reader
// goroutines finish. Returns crashed=true on unexpected exit.
//
//nolint:gocognit // multi-output lifecycle has irreducible per-output bookkeeping
func (s *Service) runOnceMultiOutput(
	ctx context.Context,
	logStream domain.StreamCode,
	sub *buffer.Subscriber,
	args []string,
	targets []RenditionTarget,
) (crashed bool, err error) {
	cmd := exec.CommandContext(ctx, s.cfg.FFmpegPath, args...)
	slog.Debug("transcoder: ffmpeg multi-output cmdline",
		"stream_code", logStream,
		"cmd", formatFFmpegCmd(s.cfg.FFmpegPath, args),
	)

	// Create N output pipes. Parent reads from rN; child sees fd 3, 4, …
	// (whatever multiOutputBaseFD points at, see ffmpeg_args.go).
	readers := make([]*os.File, len(targets))
	writers := make([]*os.File, len(targets))
	cleanupPipes := func() {
		for _, r := range readers {
			if r != nil {
				_ = r.Close()
			}
		}
		for _, w := range writers {
			if w != nil {
				_ = w.Close()
			}
		}
	}
	for i := range targets {
		r, w, perr := os.Pipe()
		if perr != nil {
			cleanupPipes()
			return true, fmt.Errorf("os.Pipe[%d]: %w", i, perr)
		}
		readers[i], writers[i] = r, w
	}
	cmd.ExtraFiles = writers

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cleanupPipes()
		return true, fmt.Errorf("stdin pipe: %w", err)
	}
	// Bump the kernel pipe buffer past Linux's 64 KiB default. At high
	// ingest bitrates (>20 Mbps) the default fills in tens of ms and our
	// stdin.Write goroutine blocks → buffer-hub fan-out for the $raw$<code>
	// subscriber drops packets silently → ffmpeg sees corrupted TS.
	setFFmpegStdinPipeSize(stdin)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cleanupPipes()
		return true, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cleanupPipes()
		return true, fmt.Errorf("ffmpeg start: %w", err)
	}
	// Once started, the child holds its own copies of the writer fds. Close
	// our parent-side write ends so EOF propagates correctly when the child
	// exits — otherwise the parent reader goroutines would block forever.
	for _, w := range writers {
		_ = w.Close()
	}

	slog.Info("transcoder: stream encoder started (multi-output)",
		"stream_code", logStream,
		"profiles", len(targets),
	)

	tail := newStderrTail(stderrTailCap)
	go s.logStderr(logStream, "stream", stderr, tail)

	// stdin pump: feed raw ingest TS bytes (or AVPackets muxed on the fly)
	// to FFmpeg's stdin. Identical logic to legacy runOnce.
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

	// One reader per output pipe → corresponding rendition buffer.
	var readerWG sync.WaitGroup
	for i, target := range targets {
		readerWG.Add(1)
		go func(profileIndex int, r *os.File, outBufID domain.StreamCode) {
			defer readerWG.Done()
			defer func() { _ = r.Close() }()

			track := buffer.VideoTrackSlug(profileIndex)
			readBuf := make([]byte, 256*1024)
			for {
				n, rerr := r.Read(readBuf)
				if n > 0 {
					out := make([]byte, n)
					copy(out, readBuf[:n])
					if werr := s.buf.Write(outBufID, buffer.TSPacket(out)); werr != nil {
						slog.Error("transcoder: multi-output buffer write failed",
							"stream_code", logStream,
							"profile", track,
							"err", werr)
						return
					}
				}
				if rerr != nil {
					if rerr != io.EOF {
						slog.Debug("transcoder: multi-output pipe read ended",
							"stream_code", logStream,
							"profile", track,
							"err", rerr)
					}
					return
				}
			}
		}(i, readers[i], target.BufferID)
	}

	// Wait for FFmpeg to exit (which closes all output pipes → readers EOF).
	waitErr := cmd.Wait()
	_ = stdin.Close()
	stdinWG.Wait()
	readerWG.Wait()

	if waitErr != nil && ctx.Err() == nil {
		stderrCtx := formatStderrTail(tail.snapshot())
		slog.Error("transcoder: ffmpeg multi-output exited with error",
			"stream_code", logStream,
			"err", waitErr,
			"stderr_tail", stderrCtx,
		)
		if stderrCtx != "" {
			return true, fmt.Errorf("ffmpeg exit: %w; stderr: %s", waitErr, stderrCtx)
		}
		return true, fmt.Errorf("ffmpeg exit: %w", waitErr)
	}
	return false, nil
}
