package ingestor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
)

const (
	reconnectBaseDelay = time.Second
	reconnectMaxDelay  = 10 * time.Second
)

var httpStatusPattern = regexp.MustCompile(`\bhttp (\d{3})\b`)

// runPullWorker reads from reader in a loop, writing each chunk to the buffer.
// It reconnects automatically after transient failures using exponential backoff.
// The loop exits cleanly when ctx is cancelled.
func runPullWorker(
	ctx context.Context,
	streamID domain.StreamCode,
	input domain.Input,
	r Reader,
	buf *buffer.Service,
	onPacket func(streamID domain.StreamCode, inputPriority int),
	onInputError func(streamID domain.StreamCode, inputPriority int, err error),
) {
	delay := reconnectBaseDelay

	for {
		if ctx.Err() != nil {
			return
		}

		if err := r.Open(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("ingestor: open failed",
				"stream_code", streamID,
				"input_priority", input.Priority,
				"url", input.URL,
				"err", err,
			)
			if !waitBackoff(ctx, delay) {
				return
			}
			delay = min(delay*2, reconnectMaxDelay)
			continue
		}

		slog.Info("ingestor: source connected",
			"stream_code", streamID,
			"input_priority", input.Priority,
			"url", input.URL,
		)
		delay = reconnectBaseDelay // reset on successful open

		readErr := readLoop(ctx, streamID, input, r, buf, onPacket)
		r.Close()

		if ctx.Err() != nil {
			return
		}

		if errors.Is(readErr, io.EOF) {
			// Graceful end of source (e.g. file finished).
			// For files configured to loop, the reader itself handles it.
			slog.Info("ingestor: source ended (EOF)",
				"stream_code", streamID,
				"input_priority", input.Priority,
			)
			return
		}

		slog.Warn("ingestor: read error, reconnecting",
			"stream_code", streamID,
			"input_priority", input.Priority,
			"err", readErr,
			"backoff", delay,
		)
		if shouldFailoverImmediately(readErr) {
			slog.Warn("ingestor: non-retriable source error, stop and trigger failover",
				"stream_code", streamID,
				"input_priority", input.Priority,
				"err", readErr,
			)
			if onInputError != nil {
				onInputError(streamID, input.Priority, readErr)
			}
			return
		}
		if !waitBackoff(ctx, delay) {
			return
		}
		delay = min(delay*2, reconnectMaxDelay)
	}
}

// readLoop reads from reader until error or ctx cancellation.
func readLoop(
	ctx context.Context,
	streamID domain.StreamCode,
	input domain.Input,
	r Reader,
	buf *buffer.Service,
	onPacket func(streamID domain.StreamCode, inputPriority int),
) error {
	for {
		pkt, err := r.Read(ctx)
		if err != nil {
			return err
		}
		if len(pkt) == 0 {
			continue
		}
		if writeErr := buf.Write(streamID, buffer.Packet(pkt)); writeErr != nil {
			slog.Error("ingestor: buffer write failed",
				"stream_code", streamID,
				"input_priority", input.Priority,
				"err", writeErr,
			)
			return writeErr
		}
		if onPacket != nil {
			onPacket(streamID, input.Priority)
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

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func shouldFailoverImmediately(err error) bool {
	m := httpStatusPattern.FindStringSubmatch(err.Error())
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
