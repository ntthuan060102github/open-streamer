package pull

// stats_demuxer.go — side-channel demuxer that turns raw MPEG-TS chunks into
// AVPacket events for the SOLE purpose of populating the manager's input
// "tracks" panel (codec / bitrate / resolution).
//
// Why a side channel: the data path for raw-TS sources (UDP / HLS / SRT /
// File) intentionally bypasses demux/remux to preserve PCR / PID / PTS
// continuity end-to-end. That preserves correctness but leaves the stats
// channel — which is fed by `cb.onMedia(streamID, priority, *AVPacket)` —
// without any AVPackets to observe. As a result the UI shows "No tracks
// reported" for every UDP / HTTP-MPEG-TS / SRT input even when the data
// path is healthy.
//
// StatsDemuxer fixes this without disturbing the data path: it owns its own
// goroutine and gompeg2 demuxer instance, fed via a buffered channel of
// chunk copies. If the demuxer can't keep up (constrained CPU, GC pause)
// chunks are dropped silently — stats are best-effort.
//
// Lifecycle:
//
//	NewStatsDemuxer(onAV) → goroutine running gompeg2.TSDemuxer in a loop
//	Feed(chunk)           → non-blocking enqueue (drops on full)
//	Close()               → stops the goroutine, drains the channel
//
// Resync semantics mirror TSDemuxPacketReader: gompeg2 returns immediately
// on a parse error, so we restart the demuxer to consume the next sync
// byte from the chanReader's `rem` buffer. A run of consecutive parse
// errors with no successful frame eventually exits — same maxDemuxRestarts
// ceiling as the main path.

import (
	"log/slog"
	"sync"

	gompeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// statsChunkBuffer is the chunks-channel depth between Feed() and the
// demuxer goroutine. 32 chunks × ~1316 bytes ≈ 42 KiB ≈ ~28 ms at 12 Mbps.
// A backlog larger than that means the demuxer is genuinely behind; further
// chunks should be dropped rather than queued indefinitely.
const statsChunkBuffer = 32

// StatsDemuxer demultiplexes raw MPEG-TS bytes off the data path and surfaces
// per-frame AVPackets through onAV. Construct via NewStatsDemuxer; feed via
// Feed; tear down via Close. Safe to call Feed and Close concurrently.
//
// closeMu serialises send-vs-close on `ch`: without it, Close could close
// the channel between Feed's `closed` check and the send, panicking with
// "send on closed channel". Read-side (the demuxer goroutine) sees the
// close as a normal channel-closed signal and exits via chanReader → EOF.
type StatsDemuxer struct {
	onAV func(p *domain.AVPacket)

	ch chan []byte
	wg sync.WaitGroup

	closeMu sync.Mutex
	closed  bool
}

// NewStatsDemuxer starts a goroutine that demuxes chunks fed via Feed into
// AVPackets and invokes onAV for each. onAV is nil-checked so callers don't
// need to guard the construction site.
func NewStatsDemuxer(onAV func(p *domain.AVPacket)) *StatsDemuxer {
	if onAV == nil {
		// nil callback would still consume chunks but produce nothing —
		// reject to avoid the wasteful goroutine.
		return nil
	}
	sd := &StatsDemuxer{
		onAV: onAV,
		ch:   make(chan []byte, statsChunkBuffer),
	}
	sd.wg.Add(1)
	go sd.run()
	return sd
}

// Feed enqueues chunk for asynchronous demux. Non-blocking: if the channel
// is full the chunk is dropped (stats are best-effort). Empty chunks and
// post-Close calls are silently ignored. The slice is copied internally so
// the caller is free to reuse the underlying buffer.
func (sd *StatsDemuxer) Feed(chunk []byte) {
	if sd == nil || len(chunk) == 0 {
		return
	}
	sd.closeMu.Lock()
	defer sd.closeMu.Unlock()
	if sd.closed {
		return
	}
	cp := append([]byte(nil), chunk...)
	select {
	case sd.ch <- cp:
	default:
		// Demuxer is behind. Stats panel will skip a frame; the data path
		// is unaffected.
	}
}

// Close stops the demuxer goroutine and waits for it to exit. Safe to call
// multiple times and from any goroutine. Feed calls that race with Close
// drop silently rather than panicking on a send to a closed channel.
func (sd *StatsDemuxer) Close() {
	if sd == nil {
		return
	}
	sd.closeMu.Lock()
	if sd.closed {
		sd.closeMu.Unlock()
		return
	}
	sd.closed = true
	close(sd.ch)
	sd.closeMu.Unlock()
	sd.wg.Wait()
}

// drainStatsChannel discards every chunk remaining in ch. Used after the
// demuxer goroutine gives up (parse-error ceiling reached) so a subsequent
// Close() doesn't block on a full unread channel. Returns when ch is closed.
func drainStatsChannel(ch <-chan []byte) {
	for range ch {
		// Discard.
	}
}

// run is the demuxer goroutine. It reuses the existing chanReader +
// demuxInputSafe + buildAVPacket helpers from tsdemux_packet_reader.go so
// the parse-and-resync logic stays in one place.
func (sd *StatsDemuxer) run() {
	defer sd.wg.Done()
	cr := &chanReader{ch: sd.ch}

	consecutiveErrors := 0
	for {
		demux := gompeg2.NewTSDemuxer()
		demux.OnFrame = func(cid gompeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
			p, ok := buildAVPacket(cid, frame, pts, dts)
			if !ok {
				return
			}
			consecutiveErrors = 0
			sd.onAV(&p)
		}

		err := demuxInputSafe(demux, cr)
		if err == nil {
			// chanReader returned io.EOF (channel closed by Close).
			return
		}
		consecutiveErrors++
		if consecutiveErrors >= maxDemuxRestarts {
			slog.Warn("ingestor: stats demuxer giving up after parse errors",
				"consecutive_errors", consecutiveErrors,
			)
			// Drain remaining chunks so Close() doesn't block forever; the
			// next chunk that triggers a resync would have to come from a
			// fresh stream anyway.
			drainStatsChannel(sd.ch)
			return
		}
	}
}
