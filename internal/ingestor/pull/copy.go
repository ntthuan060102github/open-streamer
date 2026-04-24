// Package pull — copy.go: in-process copy:// PacketReader.
//
// CopyReader subscribes to another in-process stream's published buffer
// and forwards each AVPacket as if it had been read from a real network
// source. From the manager / coordinator's point of view a copy:// input
// behaves like any other PacketReader: Open / ReadPackets / Close. From
// the system's point of view there is no network IO at all — the data
// flows through the in-memory Buffer Hub.
//
// Scope (v1 / single-stream): the upstream MUST be single-stream
// (no transcoder, or transcoder with video.copy=true). ABR upstreams use
// a separate coordinator path that creates N parallel taps; this reader
// only handles the 1-buffer case. ValidateCopyShape rejects the mismatch
// at write time so we never reach Open with an ABR upstream.

package pull

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// StreamLookup resolves an upstream stream by code. Plumbed from the API
// layer (or any layer holding the repo) so this package stays free of the
// store dependency.
type StreamLookup func(domain.StreamCode) (*domain.Stream, bool)

// CopyReader implements ingestor.PacketReader for `copy://<upstream>` URLs.
type CopyReader struct {
	target   domain.StreamCode
	bufID    domain.StreamCode // resolved at construction (= upstream's main buffer)
	bufSvc   *buffer.Service
	sub      *buffer.Subscriber
	closed   atomic.Bool
	closeErr error
}

// NewCopyReader constructs a CopyReader from a copy:// input. Errors:
//   - URL malformed (`copy://` grammar violation) — propagates protocol error
//   - upstream missing in lookup — runtime error; coordinator surfaces it
//     as input degraded so the manager can switch to a fallback
//   - upstream has ABR transcoder — copy:// from ABR routes via the
//     coordinator's ABR path, never through this single-stream reader;
//     reaching here with ABR upstream is a configuration bug
func NewCopyReader(input domain.Input, bufSvc *buffer.Service, lookup StreamLookup) (*CopyReader, error) {
	if bufSvc == nil {
		return nil, errors.New("copy reader: nil buffer service")
	}
	if lookup == nil {
		return nil, errors.New("copy reader: nil stream lookup")
	}

	target, err := domain.CopyInputTarget(input)
	if err != nil {
		return nil, fmt.Errorf("copy reader: %w", err)
	}

	upstream, ok := lookup(target)
	if !ok {
		return nil, fmt.Errorf("copy reader: upstream stream %q not found", target)
	}

	if streamHasRenditions(upstream) {
		return nil, fmt.Errorf(
			"copy reader: upstream %q has an ABR ladder — single-stream copy reader cannot tap an ABR upstream (must be routed via coordinator's ABR path)",
			target,
		)
	}

	// Single-stream upstream → main buffer is the stream code itself
	// (PlaybackBufferID with nil/copy transcoder returns code).
	bufID := buffer.PlaybackBufferID(target, upstream.Transcoder)

	return &CopyReader{
		target: target,
		bufID:  bufID,
		bufSvc: bufSvc,
	}, nil
}

// Open subscribes to the upstream buffer. Returns an error when the
// buffer doesn't exist yet (upstream not started, or torn down).
func (r *CopyReader) Open(ctx context.Context) error {
	_ = ctx // subscription is sync; ctx is honoured by ReadPackets only
	if r.closed.Load() {
		return errors.New("copy reader: open after close")
	}
	sub, err := r.bufSvc.Subscribe(r.bufID)
	if err != nil {
		return fmt.Errorf("copy reader: subscribe %q: %w", r.bufID, err)
	}
	r.sub = sub
	return nil
}

// ReadPackets blocks until one upstream packet arrives or ctx is done.
// Returns io.EOF when upstream tears down (subscriber channel closes).
//
// Packets without an AV payload (raw TS bytes from a transcoded
// rendition buffer) are skipped silently — single-stream upstreams write
// AV packets exclusively. A TS-only packet here means the upstream
// shape changed mid-flight (e.g. transcoder added) and the downstream
// stream needs to be restarted; we return an empty slice and let the
// next call block again.
func (r *CopyReader) ReadPackets(ctx context.Context) ([]domain.AVPacket, error) {
	if r.sub == nil {
		return nil, errors.New("copy reader: ReadPackets called before Open")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case pkt, ok := <-r.sub.Recv():
		if !ok {
			return nil, io.EOF
		}
		if pkt.AV == nil {
			// Upstream wrote a TS-only packet — single-stream copy doesn't
			// support that shape (ABR path would). Drop and wait for next.
			return nil, nil
		}
		return []domain.AVPacket{*pkt.AV}, nil
	}
}

// Close unsubscribes. Idempotent — safe to call from multiple goroutines.
func (r *CopyReader) Close() error {
	if r.closed.Swap(true) {
		return r.closeErr
	}
	if r.sub != nil {
		r.bufSvc.Unsubscribe(r.bufID, r.sub)
	}
	return nil
}

// streamHasRenditions mirrors domain.streamHasRenditions but kept inline
// here so the pull package doesn't have to expose a domain accessor for
// this single use. Returns true when the stream has a re-encoding
// transcoder ladder (= rendition buffers exist, distinct from main).
func streamHasRenditions(s *domain.Stream) bool {
	if s == nil || s.Transcoder == nil {
		return false
	}
	if s.Transcoder.Video.Copy {
		return false
	}
	return len(s.Transcoder.Video.Profiles) > 0
}
