package ingestor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

const (
	reconnectBaseDelay = time.Second
	reconnectMaxDelay  = 10 * time.Second
)

// httpStatusPattern matches HTTP status codes in error strings, case-insensitively,
// to cover both "HTTP 404" (typical ingestor errors) and "http 404".
var httpStatusPattern = regexp.MustCompile(`(?i)\bhttp (\d{3})\b`)

// pullWorkerCallbacks groups optional observer callbacks for runPullWorker.
type pullWorkerCallbacks struct {
	onPacket      func(streamID domain.StreamCode, inputPriority int)
	onInputError  func(streamID domain.StreamCode, inputPriority int, err error)
	onConnect     func(streamID domain.StreamCode, inputPriority int)
	onReconnect   func(streamID domain.StreamCode, inputPriority int, err error)
	onPacketBytes func(streamID domain.StreamCode, inputPriority, bytes int)
	// onHandoff is called exactly once, after the very first successful Open.
	// It is used by startPullWorker to cancel the previous source worker only after
	// the new source has connected — eliminating the buffer gap during source transitions.
	onHandoff func()
}

// runPullWorker reads from reader in a loop, writing each chunk to the buffer.
// It reconnects automatically after transient failures using exponential backoff.
// The loop exits cleanly when ctx is cancelled.
func runPullWorker(
	ctx context.Context,
	streamID domain.StreamCode,
	bufferWriteID domain.StreamCode,
	input domain.Input,
	r PacketReader,
	buf *buffer.Service,
	cb pullWorkerCallbacks,
) {
	delay := reconnectBaseDelay
	handedOff := false // onHandoff fires at most once (first successful Open)

	for {
		if !openSource(ctx, streamID, input, r, cb, &delay, &handedOff) {
			return
		}

		slog.Info("ingestor: source connected",
			"stream_code", streamID,
			"input_priority", input.Priority,
			"url", input.URL,
		)
		delay = reconnectBaseDelay // reset on successful open
		if cb.onConnect != nil {
			cb.onConnect(streamID, input.Priority)
		}

		readErr := readLoop(ctx, streamID, bufferWriteID, input, r, buf, &cb)
		_ = r.Close()

		if ctx.Err() != nil {
			return
		}

		if done := handleReadError(ctx, streamID, input, readErr, cb, &delay); done {
			return
		}
	}
}

// openSource tries r.Open in a backoff loop until it succeeds or ctx is cancelled.
// On the very first successful open it fires cb.onHandoff exactly once — this is the
// pre-connect handoff that releases the previous ingestor without a buffer gap.
// Returns false when ctx is done (caller must return).
func openSource(
	ctx context.Context,
	streamID domain.StreamCode,
	input domain.Input,
	r PacketReader,
	cb pullWorkerCallbacks,
	delay *time.Duration,
	handedOff *bool,
) bool {
	for {
		// r.Open is context-aware; if ctx is already done it returns immediately.
		if err := r.Open(ctx); err != nil {
			if ctx.Err() != nil {
				return false
			}
			slog.Error("ingestor: open failed",
				"stream_code", streamID,
				"input_priority", input.Priority,
				"url", input.URL,
				"err", err,
			)
			if !waitBackoff(ctx, *delay) {
				return false
			}
			*delay = minDur(*delay*2, reconnectMaxDelay)
			continue
		}
		// First successful open — hand off from the old source.
		if !*handedOff {
			*handedOff = true
			if cb.onHandoff != nil {
				cb.onHandoff()
			}
		}
		return true
	}
}

// handleReadError inspects the result of readLoop and decides whether to retry.
// Returns true when the worker should stop entirely, false when it should reconnect.
func handleReadError(
	ctx context.Context,
	streamID domain.StreamCode,
	input domain.Input,
	readErr error,
	cb pullWorkerCallbacks,
	delay *time.Duration,
) bool {
	if errors.Is(readErr, io.EOF) {
		// Source ended cleanly (file finished, live stream closed by server).
		// Notify the manager so it can immediately failover to a backup input
		// rather than waiting for the packet-timeout to expire.
		slog.Info("ingestor: source ended (EOF)",
			"stream_code", streamID,
			"input_priority", input.Priority,
		)
		if cb.onInputError != nil {
			cb.onInputError(streamID, input.Priority, io.EOF)
		}
		return true
	}

	slog.Warn("ingestor: read error, reconnecting",
		"stream_code", streamID,
		"input_priority", input.Priority,
		"err", readErr,
		"backoff", *delay,
	)
	if shouldFailoverImmediately(readErr) {
		slog.Warn("ingestor: non-retriable source error, stop and trigger failover",
			"stream_code", streamID,
			"input_priority", input.Priority,
			"err", readErr,
		)
		if cb.onInputError != nil {
			cb.onInputError(streamID, input.Priority, readErr)
		}
		return true
	}

	if cb.onReconnect != nil {
		cb.onReconnect(streamID, input.Priority, readErr)
	}
	if !waitBackoff(ctx, *delay) {
		return true
	}
	*delay = minDur(*delay*2, reconnectMaxDelay)
	return false
}

// readLoop reads from reader until error or ctx cancellation.
// The first packet written in each call is marked Discontinuity=true so that
// downstream consumers (HLS, DASH packager) know the source has just switched.
func readLoop(
	ctx context.Context,
	streamID domain.StreamCode,
	bufferWriteID domain.StreamCode,
	input domain.Input,
	r PacketReader,
	buf *buffer.Service,
	cb *pullWorkerCallbacks,
) error {
	firstPacket := true
	for {
		batch, err := r.ReadPackets(ctx)
		if err != nil {
			return err
		}
		for _, p := range batch {
			if len(p.Data) == 0 {
				continue
			}
			cl := p.Clone()
			if firstPacket {
				cl.Discontinuity = true
				firstPacket = false
			}
			if writeErr := buf.Write(bufferWriteID, buffer.Packet{AV: cl}); writeErr != nil {
				slog.Error("ingestor: buffer write failed",
					"stream_code", streamID,
					"input_priority", input.Priority,
					"err", writeErr,
				)
				return writeErr
			}
			if cb != nil && cb.onPacket != nil {
				cb.onPacket(streamID, input.Priority)
			}
			if cb != nil && cb.onPacketBytes != nil {
				cb.onPacketBytes(streamID, input.Priority, len(p.Data))
			}
		}
	}
}

// waitBackoff sleeps for d, returning false if ctx is cancelled first.
func waitBackoff(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func shouldFailoverImmediately(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// TLS / x509 / DNS — source-side configuration errors that won't
	// recover by reconnect. Without this branch the worker spins in the
	// reconnect loop forever, manager never sees the input as degraded,
	// and failover to a backup input never fires (e.g. an HLS source with
	// an untrusted CA cert: previously cycled "fetch failed, retrying"
	// every few seconds without surfacing as an input error).
	if strings.Contains(msg, "x509:") ||
		strings.Contains(msg, "tls:") ||
		strings.Contains(msg, "no such host") {
		return true
	}
	m := httpStatusPattern.FindStringSubmatch(msg)
	if len(m) != 2 {
		return false
	}
	code, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return false
	}
	switch code {
	case 401, 403, 404, 410, 429, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}
