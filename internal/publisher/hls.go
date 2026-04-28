package publisher

// hls.go — live HLS publisher (MPEG-TS segments + sliding m3u8 manifest).
//
// Segmenting strategy:
//   - AV path (pkt.AV): flush the current segment BEFORE writing an IDR frame so
//     every new segment starts with a keyframe.  Failover discontinuities are
//     detected via pkt.AV.Discontinuity.
//   - TS path (pkt.TS, transcoder output): the transcoder is configured with a
//     GOP size matching segSec, so a wall-clock ticker flush at segSec intervals
//     lands close to IDR boundaries without needing a demuxer.  Failover is
//     detected via the service-level hlsFailoverGen counter.
//
// In both modes, a 3/2 × segSec force-flush prevents runaway segments.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
)

// hlsRunOpts carries per-rendition metadata for ABR HLS; nil → single-rendition mode.
type hlsRunOpts struct {
	abrMaster   *hlsABRMaster
	abrSlug     string
	bwBps       int
	width       int
	height      int
	failoverGen func() uint64
	// segCount is incremented once per successful segment write. Pre-bound
	// to {stream_code,format=hls,profile} so the hot path skips the
	// WithLabelValues map lookup. Nil-safe.
	segCount prometheus.Counter
}

// hlsSegEntry holds per-segment metadata kept in the sliding window.
type hlsSegEntry struct {
	name string  // e.g. "seg_000001.ts"
	dur  float64 // seconds, measured wall-clock from flush
	disc bool    // emit EXT-X-DISCONTINUITY before this segment
}

// serveHLS is the single-rendition HLS output goroutine.
func (s *Service) serveHLS(ctx context.Context, streamID domain.StreamCode) {
	hlsDir := strings.TrimSpace(s.cfg.HLS.Dir)
	if hlsDir == "" {
		slog.Error("publisher: HLS disabled — publisher.hls.dir is empty", "stream_code", streamID)
		return
	}

	sub, err := s.buf.Subscribe(streamID)
	if err != nil {
		slog.Error("publisher: HLS subscribe failed", "stream_code", streamID, "err", err)
		return
	}
	defer s.buf.Unsubscribe(streamID, sub)

	slog.Info("publisher: HLS serve started", "stream_code", streamID, "hls_dir", hlsDir)

	streamDir := filepath.Join(hlsDir, string(streamID))
	if err := os.MkdirAll(streamDir, 0o755); err != nil {
		slog.Error("publisher: HLS setup dir failed", "stream_code", streamID, "dir", streamDir, "err", err)
		return
	}

	manifest := filepath.Join(streamDir, "index.m3u8")
	opts := &hlsRunOpts{
		failoverGen: func() uint64 { return s.hlsFailoverGenSnapshot(streamID) },
		segCount:    s.hlsSegCounter(streamID, "main"),
	}
	runHLSSegmenter(
		ctx, streamID, sub, streamDir, manifest,
		s.cfg.HLS.LiveSegmentSec,
		s.cfg.HLS.LiveWindow,
		s.cfg.HLS.LiveHistory,
		s.cfg.HLS.LiveEphemeral,
		opts,
	)
}

// runHLSSegmenter is the entry-point for both single-rendition and ABR per-shard segmenters.
func runHLSSegmenter(
	ctx context.Context,
	streamID domain.StreamCode,
	sub *buffer.Subscriber,
	streamDir string,
	manifestPath string,
	segSec, window, history int,
	ephemeral bool,
	opts *hlsRunOpts,
) {
	if segSec <= 0 {
		segSec = domain.DefaultLiveSegmentSec
	}
	if window <= 0 {
		window = domain.DefaultLiveWindow
	}
	if history < 0 {
		history = domain.DefaultLiveHistory
	}

	p := &hlsSegmenter{
		streamID:     streamID,
		streamDir:    streamDir,
		manifestPath: manifestPath,
		segSec:       segSec,
		window:       window,
		history:      history,
		ephemeral:    ephemeral,
		segBuf:       make([]byte, 0, 4<<20), // 4 MiB initial capacity
	}
	if opts != nil {
		p.abrMaster = opts.abrMaster
		p.abrSlug = opts.abrSlug
		p.bwBps = opts.bwBps
		p.width = opts.width
		p.height = opts.height
		p.failoverGen = opts.failoverGen
		p.segCount = opts.segCount
	}
	if p.failoverGen == nil {
		p.failoverGen = func() uint64 { return 0 }
	}
	p.knownGen = p.failoverGen()

	p.run(ctx, sub)
}

// hlsSegmenter manages the per-stream HLS segmentation state.
type hlsSegmenter struct {
	streamID     domain.StreamCode
	streamDir    string
	manifestPath string // empty when abrMaster handles the per-shard path
	segSec       int
	window       int
	history      int
	ephemeral    bool

	mu sync.Mutex

	// Current segment accumulator (aligned 188-byte MPEG-TS packets).
	segBuf   []byte
	segStart time.Time
	discNext bool // schedule EXT-X-DISCONTINUITY for the next flushed segment

	// Sliding-window manifest state.
	segN   uint64
	onDisk []hlsSegEntry

	// Failover generation tracking (TS path; AV path uses pkt.AV.Discontinuity).
	failoverGen func() uint64
	knownGen    uint64

	// ABR wiring.
	abrMaster *hlsABRMaster
	abrSlug   string
	bwBps     int
	width     int
	height    int

	// Pre-bound segment-write counter; nil-safe.
	segCount prometheus.Counter
}

func (p *hlsSegmenter) run(ctx context.Context, sub *buffer.Subscriber) {
	var tsCarry []byte
	var avMux *tsmux.FromAV

	segDur := time.Duration(p.segSec) * time.Second
	maxDur := segDur * 3 / 2 // force-flush deadline for TS path

	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			p.doFlush()
			return

		case pkt, ok := <-sub.Recv():
			if !ok {
				p.doFlush()
				return
			}
			if pkt.AV != nil {
				p.handleAVPacket(pkt.AV, segDur)
				// On source switch, discard stale muxer state so the new source
				// gets fresh PAT/PMT tables and clean continuity counters.
				// FeedWirePacket lazily allocates a new FromAV on the next call.
				if pkt.AV.Discontinuity {
					avMux = nil
					tsCarry = nil
				}
			}
			tsmux.FeedWirePacket(pkt.TS, pkt.AV, &avMux, func(b []byte) {
				alignedFeed(b, &tsCarry, func(pkt188 []byte) bool {
					p.mu.Lock()
					if p.segStart.IsZero() {
						p.segStart = time.Now()
					}
					p.segBuf = append(p.segBuf, pkt188...)
					p.mu.Unlock()
					return true
				})
			})

		case <-tick.C:
			p.tickFlush(maxDur)
		}
	}
}

// handleAVPacket processes AV-path control signals: discontinuity flushing and
// IDR-aligned segment splitting.  Must not be called with p.mu held.
func (p *hlsSegmenter) handleAVPacket(av *domain.AVPacket, segDur time.Duration) {
	if av.Discontinuity {
		p.mu.Lock()
		// Flush old-source data before mixing in new-source data.
		if len(p.segBuf) > 0 {
			p.flushLocked()
		}
		p.discNext = true
		p.mu.Unlock()
	}

	// IDR (keyframe): flush the previous segment BEFORE writing the IDR bytes
	// so the new segment starts at a clean keyframe boundary.
	if av.KeyFrame {
		p.mu.Lock()
		if len(p.segBuf) > 0 && !p.segStart.IsZero() &&
			time.Since(p.segStart) >= segDur {
			p.flushLocked()
		}
		p.mu.Unlock()
	}
}

// tickFlush is called every 50 ms by the ticker; it handles:
//   - TS-path time-based segmenting
//   - Fallback force-flush for stuck AV paths
//   - Failover generation detection (TS path)
func (p *hlsSegmenter) tickFlush(maxDur time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.segBuf) == 0 || p.segStart.IsZero() {
		return
	}

	// Detect failover via service-level generation counter (TS path).
	if gen := p.failoverGen(); gen != p.knownGen {
		p.knownGen = gen
		// Flush the current (mixed) segment and mark next as discontinuous.
		p.flushLocked()
		p.discNext = true
		return
	}

	elapsed := time.Since(p.segStart)
	if elapsed >= maxDur {
		// Force-flush: either TS path (no IDR pre-flush) or an AV path without
		// a keyframe for 1.5 × segSec (stream health issue).
		p.flushLocked()
	}
}

// doFlush flushes any remaining segment data on shutdown.
func (p *hlsSegmenter) doFlush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.segBuf) > 0 {
		p.flushLocked()
	}
}

// flushLocked writes the current segment to disk and updates the manifest.
// Caller must hold p.mu.
func (p *hlsSegmenter) flushLocked() {
	if len(p.segBuf) == 0 {
		return
	}

	dur := time.Since(p.segStart).Seconds()
	if dur <= 0 {
		dur = float64(p.segSec)
	}

	p.segN++
	name := fmt.Sprintf("seg_%06d.ts", p.segN)
	path := filepath.Join(p.streamDir, name)

	// Snapshot and reset the accumulator without reallocating.
	data := append(make([]byte, 0, len(p.segBuf)), p.segBuf...)
	p.segBuf = p.segBuf[:0]
	p.segStart = time.Time{}

	disc := p.discNext
	p.discNext = false

	if err := os.WriteFile(path, data, 0o644); err != nil {
		slog.Warn("publisher: HLS write segment failed",
			"stream_code", p.streamID, "segment", name, "err", err)
		return
	}

	if p.segCount != nil {
		p.segCount.Inc()
	}

	slog.Debug("publisher: HLS segment flushed",
		"stream_code", p.streamID, "segment", name,
		"dur_s", fmt.Sprintf("%.3f", dur),
		"bytes", len(data),
	)

	p.onDisk = append(p.onDisk, hlsSegEntry{name: name, dur: dur, disc: disc})
	p.trimDiskLocked()

	if err := p.writeManifestLocked(); err != nil {
		slog.Warn("publisher: HLS write manifest failed", "stream_code", p.streamID, "err", err)
	}
}

// trimDiskLocked removes old segments beyond the sliding window + history.
// Caller must hold p.mu.
func (p *hlsSegmenter) trimDiskLocked() {
	maxKeep := p.window + p.history
	if maxKeep < p.window {
		maxKeep = p.window
	}
	if !p.ephemeral {
		return
	}
	for len(p.onDisk) > maxKeep {
		old := p.onDisk[0]
		p.onDisk = p.onDisk[1:]
		_ = os.Remove(filepath.Join(p.streamDir, old.name))
	}
}

// writeManifestLocked serialises the sliding-window m3u8 and writes it atomically.
// Caller must hold p.mu.
func (p *hlsSegmenter) writeManifestLocked() error {
	if p.manifestPath == "" && p.abrMaster == nil {
		return nil
	}

	win := windowTailEntries(p.onDisk, p.window)

	// EXT-X-TARGETDURATION must be >= ceil of any segment duration in the manifest.
	targetDur := p.segSec + 1
	for _, e := range win {
		c := int(e.dur) + 1
		if c > targetDur {
			targetDur = c
		}
	}

	// Media sequence = segment counter of the first segment in the window.
	mediaSeq := int(p.segN) - len(win)
	if mediaSeq < 0 {
		mediaSeq = 0
	}

	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:3\n")
	fmt.Fprintf(&sb, "#EXT-X-TARGETDURATION:%d\n", targetDur)
	fmt.Fprintf(&sb, "#EXT-X-MEDIA-SEQUENCE:%d\n", mediaSeq)

	for _, e := range win {
		if e.disc {
			sb.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&sb, "#EXTINF:%.6f,\n%s\n", e.dur, e.name)
	}

	payload := []byte(sb.String())

	if p.abrMaster != nil {
		// Notify the ABR master so it can refresh the root master playlist.
		p.abrMaster.onShardUpdated(p.abrSlug, p.bwBps, p.width, p.height)
	}

	if p.manifestPath != "" {
		return writeFileAtomic(p.manifestPath, payload)
	}
	return nil
}

// windowTailEntries returns the last n entries of the sliding window.
func windowTailEntries(entries []hlsSegEntry, n int) []hlsSegEntry {
	if n <= 0 || len(entries) == 0 {
		return entries
	}
	if len(entries) <= n {
		return entries
	}
	return entries[len(entries)-n:]
}

// hlsCodecString returns a CODECS attribute value for EXT-X-STREAM-INF based on
// the rendition resolution.  These are representative H.264 profiles/levels; the
// exact string depends on the encoder configuration but this covers common setups.
func hlsCodecString(width, height int) string {
	pixels := width * height
	switch {
	case pixels >= 1920*1080:
		return "avc1.640028" // High@L4.0
	case pixels >= 1280*720:
		return "avc1.4d401f" // Main@L3.1
	default:
		return "avc1.42e01e" // Baseline@L3.0
	}
}
