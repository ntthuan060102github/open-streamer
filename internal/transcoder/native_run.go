// native_run.go — multi-output runner using the native (libavcodec
// in-process) backend instead of an FFmpeg CLI subprocess.
//
// One Pipeline per stream — same "share one decode, fan out to N
// encoder chains" architecture as the previous multi_output mode, but
// the encoder chain is goroutine-local instead of forked into a child
// process. Subscribes once to the raw-ingest buffer + publishes per
// profile into the matching rendition buffer.

package transcoder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/transcoder/native"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
)

// Crash/restart pacing knobs. Carried forward from the previous
// FFmpeg-CLI runner so health tracking behaves identically across the
// migration. healthSustainDur defines "ran long enough to count as
// recovered" — runs shorter than this are fast crashes; once
// healthDegradeThreshold of them stack consecutively the stream flips
// to unhealthy in the coordinator.
const (
	restartBaseDelay       = 500 * time.Millisecond
	restartMaxDelay        = 30 * time.Second
	healthSustainDur       = 30 * time.Second
	healthDegradeThreshold = 3
)

// minDuration returns the smaller of two durations. Used to clamp the
// exponential restart-backoff at restartMaxDelay.
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// runStreamPipelineNative is the native-backend counterpart of
// runStreamEncoder (multi_output_run.go). Same crash-restart shape so
// the existing health tracking / metrics infrastructure stays
// compatible — only the inner runOnce changes.
func (s *Service) runStreamPipelineNative(
	ctx context.Context,
	logStream domain.StreamCode,
	rawIngestID domain.StreamCode,
	tc *domain.TranscoderConfig,
	targets []RenditionTarget,
) {
	if len(targets) == 0 {
		slog.Error("transcoder: native: no targets", "stream_code", logStream)
		return
	}

	// Subscribe once across restarts — losing the subscription would
	// drop packets during the restart backoff window.
	sub, err := s.buf.Subscribe(rawIngestID)
	if err != nil {
		slog.Error("transcoder: native: subscribe failed",
			"stream_code", logStream, "err", err)
		return
	}
	defer s.buf.Unsubscribe(rawIngestID, sub)

	delay := restartBaseDelay
	attempt := 0
	var lastErrMsg string
	consecutiveSame := 0
	const visibleConsecutiveCap = 3
	consecutiveFastCrashes := 0
	const healthProfileIdx = 0

	for {
		startedAt := time.Now()
		crashed, runErr := s.runOncePipelineNative(ctx, logStream, sub, tc, targets)
		runDur := time.Since(startedAt)
		if ctx.Err() != nil {
			return
		}
		if !crashed {
			s.fireHealthyIfTransitioned(logStream, healthProfileIdx)
			return
		}

		if runDur >= healthSustainDur {
			consecutiveFastCrashes = 0
			s.fireHealthyIfTransitioned(logStream, healthProfileIdx)
		}

		attempt++
		errMsg := "native pipeline crashed"
		if runErr != nil {
			errMsg = runErr.Error()
		}
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
			slog.Warn("transcoder: native pipeline crashed, restarting",
				"stream_code", logStream,
				"attempt", attempt,
				"consecutive_same_err", consecutiveSame,
				"restart_in", delay,
				"profiles", len(targets),
			)
			//nolint:contextcheck // outlives the crashed pipeline context
			s.bus.Publish(context.Background(), domain.Event{
				Type:       domain.EventTranscoderError,
				StreamCode: logStream,
				Payload: map[string]any{
					"mode":           "native",
					"attempt":        attempt,
					"restart_in_sec": delay.Seconds(),
					"error":          errMsg,
					"profiles":       len(targets),
				},
			})
		} else {
			slog.Debug("transcoder: native pipeline crashed (suppressed)",
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

// runOncePipelineNative builds a single native.Pipeline that fans the
// subscribed input out to one writer per RenditionTarget, runs it
// until input EOF / context cancel / fatal error, and returns whether
// the run crashed (so the caller knows to back off + retry).
//
// Buffer-hub plumbing:
//
//   - Input: a goroutine drains sub.Recv() and writes the bytes into
//     an io.Pipe. The pipe's read end feeds native.Pipeline.
//   - Outputs: one io.Pipe per target. The native pipeline writes
//     encoded MPEG-TS into the writer end; a reader goroutine pulls
//     blocks out and publishes them to target.BufferID via
//     s.buf.Write — same shape downstream publishers expect.
func (s *Service) runOncePipelineNative(
	ctx context.Context,
	logStream domain.StreamCode,
	sub *buffer.Subscriber,
	tc *domain.TranscoderConfig,
	targets []RenditionTarget,
) (crashed bool, err error) {
	// Per-run context so the input pump + output readers exit cleanly
	// when the pipeline returns (either gracefully or via crash).
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	// Input plumbing: subscriber → io.Pipe → native.Config.Input.
	inputReader, inputWriter := io.Pipe()
	defer inputReader.Close()
	defer inputWriter.Close()

	// Output plumbing: one io.Pipe per profile. native writes to the
	// writer end; we drain the reader end into the matching rendition
	// buffer. Mirrors what multi_output_run.go's os.Pipe + readerWG
	// did with FFmpeg's ExtraFiles.
	outReaders := make([]*io.PipeReader, len(targets))
	outWriters := make([]*io.PipeWriter, len(targets))
	for i := range targets {
		outReaders[i], outWriters[i] = io.Pipe()
	}
	// Defer-close in a closure so all writers / readers get closed even
	// if a later step in this function returns early.
	defer func() {
		for _, w := range outWriters {
			_ = w.Close()
		}
		for _, r := range outReaders {
			_ = r.Close()
		}
	}()

	// Build the native config. The video profile slice mirrors the
	// existing `targets` 1:1 so per-profile metric labels stay stable.
	// transcoder.Profile uses the FFmpeg-CLI-shaped string-bitrate
	// schema ("1500k") while native.ProfileOutput consumes the domain
	// VideoProfile (int kbps) — translate here.
	profileOuts := make([]native.ProfileOutput, len(targets))
	for i, t := range targets {
		profileOuts[i] = native.ProfileOutput{
			Profile: profileToDomain(t.Profile),
			Audio:   tc.Audio,
			Writer:  outWriters[i],
		}
	}

	pipeline, err := native.NewPipeline(native.Config{
		StreamID:  logStream,
		Input:     inputReader,
		HW:        tc.Global.HW,
		Interlace: tc.Video.Interlace,
		Watermark: tc.Watermark,
		Profiles:  profileOuts,
	})
	if err != nil {
		return true, fmt.Errorf("native pipeline init: %w", err)
	}
	defer func() { _ = pipeline.Close() }()

	slog.Info("transcoder: native pipeline started",
		"stream_code", logStream,
		"profiles", len(targets),
	)

	// Mint a fresh StreamSession on every rendition buffer so the
	// publisher segmenter cuts cleanly on each pipeline restart.
	for _, target := range targets {
		_ = s.buf.SetSession(target.BufferID, domain.SessionStartMixerCycle, nil, nil)
	}

	// Input pump: subscriber → inputWriter. Same TS-aligned write
	// shape as the FFmpeg-CLI runner so the upstream demux in
	// native.Pipeline sees well-formed MPEG-TS chunks.
	var pumpWG sync.WaitGroup
	pumpWG.Add(1)
	go func() {
		defer pumpWG.Done()
		defer func() { _ = inputWriter.Close() }()

		var avMux *tsmux.FromAV
		var tsCarry []byte
		write := func(b []byte) error {
			_, werr := inputWriter.Write(b)
			return werr
		}
		for {
			select {
			case <-runCtx.Done():
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
					tsmux.FeedWirePacket(runCtx, nil, pkt.AV, &avMux, func(b []byte) {
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

	// Reader goroutines: one per output. Drains the io.Pipe into the
	// rendition buffer. EOF signals the pipeline finished writing for
	// that profile; an unexpected error gets logged.
	var readerWG sync.WaitGroup
	for i, target := range targets {
		readerWG.Add(1)
		go func(profileIndex int, r *io.PipeReader, outBufID domain.StreamCode) {
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
						slog.Error("transcoder: native output buffer write failed",
							"stream_code", logStream,
							"profile", track,
							"err", werr)
						return
					}
				}
				if rerr != nil {
					if !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrClosedPipe) {
						slog.Debug("transcoder: native pipe read ended",
							"stream_code", logStream,
							"profile", track,
							"err", rerr)
					}
					return
				}
			}
		}(i, outReaders[i], target.BufferID)
	}

	// Run the pipeline. Blocks until input EOF, ctx cancel, or fatal
	// error. Errors here are crash-classifying — caller decides whether
	// to retry based on the time-since-start heuristic.
	runErr := pipeline.Run(runCtx)

	// Close writers to unblock readers, drain.
	for _, w := range outWriters {
		_ = w.Close()
	}
	_ = inputWriter.Close()
	pumpWG.Wait()
	readerWG.Wait()

	if runErr != nil && ctx.Err() == nil {
		return true, fmt.Errorf("native pipeline run: %w", runErr)
	}
	return false, nil
}
