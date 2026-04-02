package pull

// tsdemux_packet_reader.go — TS chunk source → domain.AVPacket stream.
//
// Architecture:
//
//	TSChunkReader.Read ──► pumpChunks ──► chunks (chan []byte)
//	                                           │
//	                                      chanReader (io.Reader)
//	                                           │
//	                                    gompeg2.TSDemuxer.Input
//	                                           │ OnFrame callback
//	                                           ▼
//	                                     q (chan AVPacket)
//	                                           │
//	                                      ReadPackets ──► caller
//
// Using a buffered channel (chunks) instead of io.Pipe decouples the read goroutine
// from the demux goroutine: up to chunkQueueSize pre-fetched chunks can accumulate
// while the demuxer processes current data, eliminating per-write pipe mutex contention.

import (
	"bytes"
	"context"
	"io"
	"sync"

	gocodec "github.com/yapingcat/gomedia/go-codec"
	gompeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

const (
	// chunkQueueSize sets how many raw TS chunks can be buffered between pumpChunks and runDemux.
	// At 20 Mbps with 10 KB chunks this is ~256 ms of lookahead.
	chunkQueueSize = 64

	// avQueueSize is the maximum number of decoded AVPackets buffered before the caller drains them.
	avQueueSize = 16384
)

// TSChunkReader is an MPEG-TS byte source (UDP, HLS, file, SRT, …).
type TSChunkReader interface {
	Open(ctx context.Context) error
	Read(ctx context.Context) ([]byte, error)
	Close() error
}

// chanReader implements io.Reader over a channel of byte slices.
// It feeds the gompeg2.TSDemuxer without copying chunks into a pipe.
// rem holds the unconsumed portion of the current chunk.
type chanReader struct {
	ch  <-chan []byte
	rem []byte
}

// Read satisfies io.Reader. Blocks until a chunk arrives or the channel is closed (→ io.EOF).
func (r *chanReader) Read(p []byte) (int, error) {
	for len(r.rem) == 0 {
		chunk, ok := <-r.ch
		if !ok {
			return 0, io.EOF
		}
		r.rem = chunk
	}
	n := copy(p, r.rem)
	r.rem = r.rem[n:]
	return n, nil
}

// TSDemuxPacketReader wraps a TSChunkReader and emits domain.AVPacket values.
type TSDemuxPacketReader struct {
	r TSChunkReader

	mu      sync.Mutex
	started bool

	chunks chan []byte          // raw TS data from pumpChunks to chanReader/runDemux
	q      chan domain.AVPacket // decoded packets ready for ReadPackets
	done   chan struct{}        // closed by Close to unblock pumpChunks and OnFrame

	readLoopDone chan struct{}
	demuxDone    chan struct{}
}

// NewTSDemuxPacketReader wraps r and emits domain.AVPacket values.
func NewTSDemuxPacketReader(r TSChunkReader) *TSDemuxPacketReader {
	return &TSDemuxPacketReader{r: r}
}

// Open starts the pump and demux goroutines.
func (d *TSDemuxPacketReader) Open(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.started {
		return nil
	}
	if err := d.r.Open(ctx); err != nil {
		return err
	}

	d.chunks = make(chan []byte, chunkQueueSize)
	d.q = make(chan domain.AVPacket, avQueueSize)
	d.done = make(chan struct{})
	d.readLoopDone = make(chan struct{})
	d.demuxDone = make(chan struct{})
	d.started = true

	go d.pumpChunks(ctx)
	go d.runDemux()
	return nil
}

// pumpChunks reads raw TS chunks from the source and forwards them to chanReader via chunks.
// It exits when the source is exhausted, the context is cancelled, or Close is called.
func (d *TSDemuxPacketReader) pumpChunks(ctx context.Context) {
	defer close(d.readLoopDone)
	defer close(d.chunks) // EOF signal for chanReader → demux.Input returns

	for {
		if ctx.Err() != nil {
			return
		}
		chunk, rerr := d.r.Read(ctx)
		if len(chunk) > 0 {
			select {
			case d.chunks <- chunk:
			case <-d.done:
				return
			}
		}
		if rerr != nil {
			return
		}
	}
}

// runDemux feeds chanReader into the MPEG-TS demuxer and emits AVPackets onto q.
func (d *TSDemuxPacketReader) runDemux() {
	// Capture both channels locally to avoid closing nil if Close races.
	q := d.q
	done := d.done

	defer close(d.demuxDone)
	defer close(q)

	cr := &chanReader{ch: d.chunks}
	demux := gompeg2.NewTSDemuxer()
	demux.OnFrame = func(cid gompeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
		if len(frame) == 0 {
			return
		}
		p, ok := buildAVPacket(cid, frame, pts, dts)
		if !ok {
			return
		}
		select {
		case q <- p:
		case <-done:
		}
	}
	_ = demux.Input(cr)
}

// buildAVPacket constructs a domain.AVPacket from a TSDemuxer frame callback.
// Returns ok=false for unsupported stream types.
func buildAVPacket(cid gompeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) (domain.AVPacket, bool) {
	var cdc domain.AVCodec
	var keyFrame bool

	switch cid {
	case gompeg2.TS_STREAM_H264:
		cdc = domain.AVCodecH264
		keyFrame = gocodec.IsH264IDRFrame(frame)
	case gompeg2.TS_STREAM_H265:
		cdc = domain.AVCodecH265
		keyFrame = gocodec.IsH265IDRFrame(frame)
	case gompeg2.TS_STREAM_AAC:
		cdc = domain.AVCodecAAC
	default:
		return domain.AVPacket{}, false
	}

	return domain.AVPacket{
		Codec:    cdc,
		Data:     bytes.Clone(frame),
		PTSms:    pts,
		DTSms:    dts,
		KeyFrame: keyFrame,
	}, true
}

// ReadPackets blocks until at least one AVPacket is available, then drains as many
// as are ready (up to 256 at once) to amortise call overhead.
// Returns (nil, io.EOF) when the source is exhausted.
func (d *TSDemuxPacketReader) ReadPackets(ctx context.Context) ([]domain.AVPacket, error) {
	d.mu.Lock()
	q := d.q
	d.mu.Unlock()

	if q == nil {
		return nil, io.EOF
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case p, ok := <-q:
		if !ok {
			return nil, io.EOF
		}
		batch := []domain.AVPacket{p}
		for len(batch) < 256 {
			select {
			case p2, ok2 := <-q:
				if !ok2 {
					return batch, nil
				}
				batch = append(batch, p2)
			default:
				return batch, nil
			}
		}
		return batch, nil
	}
}

// Close tears down both goroutines and the underlying reader.
// It is safe to call Close concurrently or multiple times.
func (d *TSDemuxPacketReader) Close() error {
	d.mu.Lock()
	if !d.started {
		d.mu.Unlock()
		return d.r.Close()
	}
	d.started = false
	done := d.done
	readLoopDone := d.readLoopDone
	demuxDone := d.demuxDone
	d.q = nil // prevents ReadPackets from using a stale reference
	d.mu.Unlock()

	// Unblock pumpChunks if it is blocked on: (a) chunks send or (b) d.r.Read.
	// done must be closed before d.r.Close so the select in pumpChunks fires first.
	close(done)
	_ = d.r.Close()

	<-readLoopDone // wait for pumpChunks (it closes chunks on exit)
	<-demuxDone    // wait for runDemux   (it closes q on exit)
	return nil
}
