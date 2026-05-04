// abr_mixer.go — coordinator branch for `mixer://A,B` where the video
// upstream A has an ABR ladder and the downstream has no own transcoder.
//
//	Upstream A rendition_track_1  ──video tap[1]──┐
//	Upstream A rendition_track_2  ──video tap[2]──┤   (1 audio packet
//	... (one video tap per rung)                  │    written to ALL N
//	                                              ├──> Downstream rendition[i]
//	Upstream B (best rendition)   ──audio fan-out┘    rendition buffers)
//
// Each downstream rendition buffer ends up with: video AVPackets from the
// matching A rung + the same audio AVPackets fanned out from B. Publisher
// then segments each rendition into HLS / DASH variants — viewer gets ABR
// HLS with audio replaced.
//
// v1 limitations (mirror those of abr_copy):
//   - upstream profile changes after Start are NOT picked up (operator must
//     restart the downstream stream)
//   - chained ABR-mixer (downstream of an in-process ABR mixer) is not
//     supported because the in-memory ladder mirror is local to coordinator
//     and not visible through the StreamRepository
//   - failover is not applicable (mixer:// is the SOLE input by validator)

package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/ingestor/pull"
	"github.com/ntt0601zcoder/open-streamer/internal/manager"
)

// abrMixerEntry holds the runtime handles needed to tear down an ABR-mixer
// stream. Mirrors abrCopyEntry plus an audio-upstream code field for status
// reporting.
type abrMixerEntry struct {
	cancel context.CancelFunc
	wg     *sync.WaitGroup
	slugs  []string

	videoUpstream     domain.StreamCode
	audioUpstream     domain.StreamCode
	lastPacketAtNanos atomic.Int64

	// t0 is the shared wall-clock anchor used by ptsRebaser instances in the
	// forward goroutines. Both video taps and the audio fan-out reference the
	// same t0 so packets from the two unrelated upstream clocks land on a
	// common timeline — without this, the player gets PTS values diverging
	// by hours and renders black/silence.
	t0 time.Time
}

// ptsRebaserBaseMs is the headroom added to every rebased timestamp so that a
// packet whose DTS or PTS dips slightly below the anchor (out-of-order
// delivery, brief network reorder) doesn't underflow uint64 when the mixer
// has only just started and wallOffsetMs is near zero. 1 second is several
// orders of magnitude larger than any realistic skew.
const ptsRebaserBaseMs int64 = 1000

// ptsRebaserPauseGapMs is the wall-clock gap above which we suspect the
// source paused (silent audio source, encoder hiccup, network reorder buffer
// drained). When true, we re-anchor the rebaser to the current wall-clock so
// output PTS doesn't drift behind — without this, audio with frequent micro-
// pauses falls 10s+ behind video over a 10-minute session and the player
// either buffers indefinitely or refuses to A/V sync.
const ptsRebaserPauseGapMs int64 = 500

// ptsRebaserPauseSrcDeltaSlackMs is the headroom subtracted from the wall-
// clock gap when comparing against source PTS delta. A real pause means the
// source produced almost nothing during the wall-clock gap; a packet that
// merely arrived a bit late (network jitter) will have srcDelta close to
// wallDelta. This slack avoids re-anchoring on normal jitter.
const ptsRebaserPauseSrcDeltaSlackMs int64 = 200

// ptsRebaser maps an upstream's PTS/DTS sequence onto a shared wall-clock
// timeline. The first packet of each cycle anchors itself at the elapsed
// wall-clock time since t0; subsequent packets preserve their original
// inter-frame deltas. One instance per source per forward cycle — re-creating
// it on reconnect is what stitches a restarted upstream's new PCR base back
// into a continuous downstream timeline.
//
// The rebaser also re-anchors mid-cycle when it detects a long pause in the
// source — see apply() for the pause-detection rule. On every re-anchor (cycle
// start OR pause re-anchor) the packet is marked Discontinuity so the
// downstream HLS segmenter inserts #EXT-X-DISCONTINUITY and the player handles
// the PTS jump cleanly instead of stuttering.
type ptsRebaser struct {
	t0           time.Time
	has          bool
	firstPTS     uint64
	firstDTS     uint64
	wallOffsetMs int64
	lastSrcPTS   uint64 // for pause detection
	lastWallMs   int64
}

// apply rewrites p.PTSms / p.DTSms in place. Signed math + base offset
// tolerate B-frame DTS<PTS, out-of-order packets, and packets briefly
// preceding the anchor without uint underflow.
//
// Re-anchor conditions:
//   - First packet of cycle (cold start or post-reconnect)
//   - Wall-clock gap since last packet > ptsRebaserPauseGapMs AND source PTS
//     advanced by less than (gap - slack) — i.e., source paused while wall-
//     clock kept moving, so we slide the anchor forward to current wall-clock
//     to keep output PTS aligned with real time
//
// On any re-anchor the packet's Discontinuity flag is set so the segmenter
// emits an EXT-X-DISCONTINUITY for the resulting PTS jump.
func (r *ptsRebaser) apply(p *domain.AVPacket) {
	nowMs := time.Since(r.t0).Milliseconds()
	if !r.has {
		// First packet of cycle. Mark Discontinuity so reconnect-driven PTS
		// jumps surface as an EXT-X-DISCONTINUITY in the HLS playlist; for
		// the very first segment of life this is a harmless extra tag.
		p.Discontinuity = true
		r.firstPTS = p.PTSms
		r.firstDTS = p.DTSms
		r.wallOffsetMs = nowMs + ptsRebaserBaseMs
		r.lastSrcPTS = p.PTSms
		r.lastWallMs = nowMs
		r.has = true
	} else {
		wallDelta := nowMs - r.lastWallMs
		srcDelta := int64(p.PTSms) - int64(r.lastSrcPTS)
		if wallDelta > ptsRebaserPauseGapMs &&
			srcDelta < wallDelta-ptsRebaserPauseSrcDeltaSlackMs {
			// Source fell behind wall-clock — re-anchor + signal discontinuity.
			p.Discontinuity = true
			r.firstPTS = p.PTSms
			r.firstDTS = p.DTSms
			r.wallOffsetMs = nowMs + ptsRebaserBaseMs
		}
		r.lastSrcPTS = p.PTSms
		r.lastWallMs = nowMs
	}
	p.PTSms = uint64(r.wallOffsetMs + (int64(p.PTSms) - int64(r.firstPTS)))
	p.DTSms = uint64(r.wallOffsetMs + (int64(p.DTSms) - int64(r.firstDTS)))
}

// detectABRMixer reports whether `s` should run on the ABR-mixer mirror
// path. Returns the resolved (videoUpstream, audioUpstream, true) when:
//   - downstream has NO own transcoder
//   - the first input is `mixer://V,A` and parses cleanly
//   - V exists in the lookup AND has an ABR ladder
//   - A exists in the lookup (any shape — single or ABR; the audio fan-out
//     handles both)
//
// Otherwise returns (nil, nil, false). The caller falls back to the normal
// MixerReader pipeline (single-tap-best for both sources).
func (c *Coordinator) detectABRMixer(s *domain.Stream) (videoUp, audioUp *domain.Stream, ok bool) {
	if c.upstreamLookup == nil || s == nil || len(s.Inputs) == 0 {
		return nil, nil, false
	}
	if s.Transcoder != nil {
		// Downstream has own encoder → use the normal pipeline (MixerReader
		// taps best rendition + feeds the encoder, which builds its own
		// ladder per downstream config). Mirror is only meaningful when
		// "preserve upstream ladder" is the goal.
		return nil, nil, false
	}
	first := s.Inputs[0]
	if !domain.IsMixerInput(first) {
		return nil, nil, false
	}
	video, audio, _, err := domain.MixerInputSpec(first)
	if err != nil {
		return nil, nil, false
	}
	upV, vok := c.upstreamLookup(video)
	if !vok {
		return nil, nil, false
	}
	if !upstreamHasABRLadder(upV) {
		return nil, nil, false
	}
	upA, aok := c.upstreamLookup(audio)
	if !aok {
		return nil, nil, false
	}
	return upV, upA, true
}

// startABRMixer wires the ABR-mixer pipeline:
//  1. shallow-clone downstream and inject a mirrored transcoder so publisher
//     and buffer helpers see N rungs (same trick used by startABRCopy);
//  2. create one downstream rendition buffer per video rung;
//  3. start the publisher (it'll subscribe to the rendition buffers);
//  4. spawn N video tap goroutines (one per upstream rung) + 1 audio
//     fan-out goroutine.
//
// Concurrency: holds c.abrMu for the entire body so two callers racing on
// the same downstream.Code can't both reach pub.Start. The previous design
// stored the c.abrMixers entry only AFTER pub.Start returned, leaving a
// window where a concurrent Start would pass IsRunning, attempt pub.Start,
// fail with "already running", and then the rollback path would delete
// the rendition buffers the FIRST start's HLS goroutines were about to
// subscribe to — manifesting as both
// "publisher: stream X already running" and
// "publisher: HLS ABR subscribe failed: buffer ... not found".
//
// Holding the lock for the duration is acceptable because mixer pipeline
// setup is a config-time operation, not a hot path.
func (c *Coordinator) startABRMixer(ctx context.Context, downstream, videoUp, audioUp *domain.Stream) error {
	c.abrMu.Lock()
	defer c.abrMu.Unlock()

	// Re-check idempotency under the lock. Caller (Coordinator.Start) does
	// an unlocked IsRunning check; a concurrent caller could have stored
	// the entry between that check and us. Treat a concurrent successful
	// start as a no-op rather than retrying.
	if _, exists := c.abrMixers[downstream.Code]; exists {
		return nil
	}

	upRends := buffer.RenditionsForTranscoder(videoUp.Code, videoUp.Transcoder)
	if len(upRends) == 0 {
		return fmt.Errorf("coordinator: abr mixer: video upstream %q has no rendition ladder", videoUp.Code)
	}

	mirrored := *downstream // shallow copy — we only mutate Transcoder pointer
	mirrored.Transcoder = mirrorTranscoderForCopy(videoUp.Transcoder)
	downRends := buffer.RenditionsForTranscoder(mirrored.Code, mirrored.Transcoder)
	if len(downRends) != len(upRends) {
		return fmt.Errorf("coordinator: abr mixer: rung count mismatch (upstream=%d downstream=%d)",
			len(upRends), len(downRends))
	}

	slugs := make([]string, 0, len(downRends))
	downBufIDs := make([]domain.StreamCode, 0, len(downRends))
	for _, r := range downRends {
		c.buf.Create(r.BufferID)
		slugs = append(slugs, r.Slug)
		downBufIDs = append(downBufIDs, r.BufferID)
	}

	if err := c.pub.Start(ctx, &mirrored); err != nil {
		for _, bid := range downBufIDs {
			c.buf.Delete(bid)
		}
		return fmt.Errorf("coordinator: abr mixer publisher: %w", err)
	}

	tapCtx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	entry := &abrMixerEntry{
		cancel:        cancel,
		wg:            wg,
		slugs:         slugs,
		videoUpstream: videoUp.Code,
		audioUpstream: audioUp.Code,
		t0:            time.Now(),
	}

	c.abrMixers[downstream.Code] = entry

	// N video tap goroutines.
	for i := range downRends {
		wg.Add(1)
		//nolint:contextcheck // tapCtx detached from request; cancelled by Stop()
		go c.runABRMixerVideoTap(tapCtx, wg, entry, downstream.Code, upRends[i].BufferID, downBufIDs[i], downRends[i].Slug)
	}
	// 1 audio fan-out goroutine. Audio source is upstream B's PlaybackBufferID
	// (main buffer for single-stream, best rendition for ABR).
	//
	// audioBufferIsTS controls how the fan-out goroutine reads packets:
	//   - true  → wrap buffer with BufferTSDemuxReader (TS chunks → AVPackets)
	//   - false → direct subscriber Recv() reading Packet.AV
	//
	// True when the upstream's main/rendition buffer is fed with raw TS bytes,
	// which now covers ABR upstreams AND single-stream upstreams whose source
	// is a raw-TS protocol (UDP / HLS / SRT / File — ingestor uses
	// TSPassthroughPacketReader). Misclassifying as direct silently drops
	// audio: pkt.AV is nil for raw TS, and the codec filter rejects every
	// packet.
	audioBufID := buffer.PlaybackBufferID(audioUp.Code, audioUp.Transcoder)
	audioBufferIsTS := upstreamHasABRLadder(audioUp) || domain.StreamMainBufferIsTS(audioUp)
	wg.Add(1)
	//nolint:contextcheck // tapCtx detached from request; cancelled by Stop()
	go c.runABRMixerAudioFanOut(tapCtx, wg, entry, downstream.Code, audioBufID, audioBufferIsTS, downBufIDs)

	if c.m != nil {
		c.m.StreamStartTimeSeconds.WithLabelValues(string(downstream.Code)).Set(float64(time.Now().Unix()))
	}
	c.clearDegradation(downstream.Code)
	c.bus.Publish(ctx, domain.Event{
		Type:       domain.EventStreamStarted,
		StreamCode: downstream.Code,
	})

	slog.Info("coordinator: abr mixer pipeline started",
		"stream_code", downstream.Code,
		"video_upstream", videoUp.Code,
		"audio_upstream", audioUp.Code,
		"renditions", len(downRends),
		"audio_buffer_is_ts", audioBufferIsTS,
	)
	return nil
}

// stopABRMixer tears down an ABR-mixer pipeline. Caller is responsible for
// removing the entry from c.abrMixers BEFORE calling.
func (c *Coordinator) stopABRMixer(ctx context.Context, code domain.StreamCode, entry *abrMixerEntry) {
	entry.cancel()
	c.pub.Stop(code)
	entry.wg.Wait()

	for _, slug := range entry.slugs {
		c.buf.Delete(buffer.RenditionBufferID(code, slug))
	}
	if c.m != nil {
		c.m.StreamStartTimeSeconds.DeleteLabelValues(string(code))
	}
	c.clearDegradation(code)
	c.bus.Publish(ctx, domain.Event{
		Type:       domain.EventStreamStopped,
		StreamCode: code,
	})
	slog.Info("coordinator: abr mixer pipeline stopped",
		"stream_code", code,
		"renditions", len(entry.slugs),
	)
}

// runABRMixerVideoTap subscribes to one upstream video rendition (TS bytes),
// demuxes to AVPackets, filters video-only frames, and forwards them to the
// matching downstream rendition buffer. Retries on upstream tear-down.
func (c *Coordinator) runABRMixerVideoTap(
	ctx context.Context,
	wg *sync.WaitGroup,
	entry *abrMixerEntry,
	downstreamCode, upBufID, downBufID domain.StreamCode,
	slug string,
) {
	defer wg.Done()

	const initialBackoff = time.Second
	const maxBackoff = 10 * time.Second
	backoff := initialBackoff

	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if c.abrMixerVideoForward(ctx, entry, upBufID, downBufID) {
			return
		}
		slog.Info("coordinator: abr mixer video tap reconnecting",
			"stream_code", downstreamCode,
			"slug", slug,
			"upstream_buffer", upBufID,
			"retry_in", backoff,
		)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// abrMixerVideoForward runs one demux-and-forward cycle for a single rung.
// Returns true on ctx cancellation (caller should not retry).
//
// PTS/DTS are rebased onto entry.t0 via a per-cycle ptsRebaser so that video
// from upstream A and audio from upstream B share a common timeline. The
// rebaser is scoped to the cycle: when an upstream restart triggers a new
// forward cycle, a fresh rebaser captures the current wall-clock offset, so
// the new PCR base is stitched in continuously instead of jumping.
func (c *Coordinator) abrMixerVideoForward(ctx context.Context, entry *abrMixerEntry, upBufID, downBufID domain.StreamCode) bool {
	demux := pull.NewBufferTSDemuxReader(c.buf, upBufID)
	if err := demux.Open(ctx); err != nil {
		return false
	}
	defer func() { _ = demux.Close() }()

	rebaser := ptsRebaser{t0: entry.t0}
	for {
		batch, err := demux.ReadPackets(ctx)
		if err != nil {
			return ctx.Err() != nil
		}
		for _, p := range batch {
			if !p.Codec.IsVideo() {
				continue
			}
			rebaser.apply(&p)
			if err := c.buf.Write(downBufID, buffer.Packet{AV: &p}); err == nil {
				entry.lastPacketAtNanos.Store(time.Now().UnixNano())
			}
		}
	}
}

// runABRMixerAudioFanOut subscribes to upstream B's audio source and writes
// each audio AVPacket to ALL N downstream rendition buffers, so every rung
// shares the same audio. Retries on upstream tear-down.
func (c *Coordinator) runABRMixerAudioFanOut(
	ctx context.Context,
	wg *sync.WaitGroup,
	entry *abrMixerEntry,
	downstreamCode, audioBufID domain.StreamCode,
	audioBufferIsTS bool,
	downBufIDs []domain.StreamCode,
) {
	defer wg.Done()

	const initialBackoff = time.Second
	const maxBackoff = 10 * time.Second
	backoff := initialBackoff

	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if c.abrMixerAudioForward(ctx, entry, audioBufID, audioBufferIsTS, downBufIDs) {
			return
		}
		slog.Info("coordinator: abr mixer audio fan-out reconnecting",
			"stream_code", downstreamCode,
			"upstream_buffer", audioBufID,
			"retry_in", backoff,
		)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// abrMixerAudioForward runs one subscribe-or-demux cycle for the audio
// source and fans audio AVPackets out to all downstream rendition buffers.
// Returns true on ctx cancellation.
//
// audioBufferIsTS dispatches between two reader strategies. The "ABR" path is
// just the TS-demuxer-over-buffer path — it works for any source whose buffer
// holds raw TS bytes, not only ABR upstreams. The "Direct" path is the legacy
// pkt.AV subscriber, used only when the upstream is RTSP/RTMP with no
// transcoder (the only configuration where the main buffer carries
// AVPackets after the raw-TS-passthrough refactor).
func (c *Coordinator) abrMixerAudioForward(
	ctx context.Context,
	entry *abrMixerEntry,
	audioBufID domain.StreamCode,
	audioBufferIsTS bool,
	downBufIDs []domain.StreamCode,
) bool {
	if audioBufferIsTS {
		return c.abrMixerAudioForwardABR(ctx, entry, audioBufID, downBufIDs)
	}
	return c.abrMixerAudioForwardDirect(ctx, entry, audioBufID, downBufIDs)
}

// abrMixerAudioForwardABR demuxes the audio upstream's best-rendition TS
// stream into AVPackets and fans audio frames out to every downstream rung.
// PTS/DTS are rebased against entry.t0 — see abrMixerVideoForward for why.
func (c *Coordinator) abrMixerAudioForwardABR(
	ctx context.Context,
	entry *abrMixerEntry,
	audioBufID domain.StreamCode,
	downBufIDs []domain.StreamCode,
) bool {
	demux := pull.NewBufferTSDemuxReader(c.buf, audioBufID)
	if err := demux.Open(ctx); err != nil {
		return false
	}
	defer func() { _ = demux.Close() }()

	rebaser := ptsRebaser{t0: entry.t0}
	for {
		batch, err := demux.ReadPackets(ctx)
		if err != nil {
			return ctx.Err() != nil
		}
		for _, p := range batch {
			if !p.Codec.IsAudio() {
				continue
			}
			rebaser.apply(&p)
			pkt := buffer.Packet{AV: &p}
			c.fanOutToRenditions(downBufIDs, pkt, entry)
		}
	}
}

// abrMixerAudioForwardDirect subscribes to a single-stream audio upstream's
// main buffer (AVPackets) and fans audio frames out to every rung.
// PTS/DTS are rebased against entry.t0 — see abrMixerVideoForward for why.
func (c *Coordinator) abrMixerAudioForwardDirect(
	ctx context.Context,
	entry *abrMixerEntry,
	audioBufID domain.StreamCode,
	downBufIDs []domain.StreamCode,
) bool {
	sub, err := c.buf.Subscribe(audioBufID)
	if err != nil {
		return false
	}
	defer c.buf.Unsubscribe(audioBufID, sub)

	rebaser := ptsRebaser{t0: entry.t0}
	for {
		select {
		case <-ctx.Done():
			return true
		case pkt, ok := <-sub.Recv():
			if !ok {
				return false
			}
			if pkt.AV == nil || !pkt.AV.Codec.IsAudio() {
				continue
			}
			// Subscriber receives a clone of the original Packet, so mutating
			// AV in place here only affects this consumer's view — safe.
			rebaser.apply(pkt.AV)
			c.fanOutToRenditions(downBufIDs, pkt, entry)
		}
	}
}

// fanOutToRenditions writes pkt to every downstream rendition buffer and
// stamps the entry's lastPacketAtNanos on at least one successful write.
func (c *Coordinator) fanOutToRenditions(downBufIDs []domain.StreamCode, pkt buffer.Packet, entry *abrMixerEntry) {
	wrote := false
	for _, bid := range downBufIDs {
		if err := c.buf.Write(bid, pkt); err == nil {
			wrote = true
		}
	}
	if wrote {
		entry.lastPacketAtNanos.Store(time.Now().UnixNano())
	}
}

// ABRMixerRuntimeStatus mirrors ABRCopyRuntimeStatus for the mixer path.
// Returns ok=false when the stream isn't running as ABR-mixer.
func (c *Coordinator) ABRMixerRuntimeStatus(code domain.StreamCode) (manager.RuntimeStatus, bool) {
	c.abrMu.RLock()
	entry, ok := c.abrMixers[code]
	c.abrMu.RUnlock()
	if !ok {
		return manager.RuntimeStatus{}, false
	}

	lastNanos := entry.lastPacketAtNanos.Load()
	var lastPacketAt time.Time
	status := domain.StatusDegraded
	if lastNanos > 0 {
		lastPacketAt = time.Unix(0, lastNanos)
		if time.Since(lastPacketAt) <= abrCopyInputStaleAfter {
			status = domain.StatusActive
		}
	}

	return manager.RuntimeStatus{
		Status:              domain.StatusActive,
		PipelineActive:      true,
		ActiveInputPriority: 0,
		Inputs: []manager.InputHealthSnapshot{{
			InputPriority: 0,
			LastPacketAt:  lastPacketAt,
			Status:        status,
		}},
	}, true
}
