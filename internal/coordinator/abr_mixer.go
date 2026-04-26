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
func (c *Coordinator) startABRMixer(ctx context.Context, downstream, videoUp, audioUp *domain.Stream) error {
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
	}

	c.abrMu.Lock()
	c.abrMixers[downstream.Code] = entry
	c.abrMu.Unlock()

	// N video tap goroutines.
	for i := range downRends {
		wg.Add(1)
		//nolint:contextcheck // tapCtx detached from request; cancelled by Stop()
		go c.runABRMixerVideoTap(tapCtx, wg, entry, downstream.Code, upRends[i].BufferID, downBufIDs[i], downRends[i].Slug)
	}
	// 1 audio fan-out goroutine. Audio source is upstream B's PlaybackBufferID
	// (main buffer for single-stream, best rendition for ABR).
	audioBufID := buffer.PlaybackBufferID(audioUp.Code, audioUp.Transcoder)
	audioIsABR := upstreamHasABRLadder(audioUp)
	wg.Add(1)
	//nolint:contextcheck // tapCtx detached from request; cancelled by Stop()
	go c.runABRMixerAudioFanOut(tapCtx, wg, entry, downstream.Code, audioBufID, audioIsABR, downBufIDs)

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
		"audio_is_abr", audioIsABR,
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
func (c *Coordinator) abrMixerVideoForward(ctx context.Context, entry *abrMixerEntry, upBufID, downBufID domain.StreamCode) bool {
	demux := pull.NewBufferTSDemuxReader(c.buf, upBufID)
	if err := demux.Open(ctx); err != nil {
		return false
	}
	defer func() { _ = demux.Close() }()

	for {
		batch, err := demux.ReadPackets(ctx)
		if err != nil {
			return ctx.Err() != nil
		}
		for _, p := range batch {
			if !p.Codec.IsVideo() {
				continue
			}
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
	audioIsABR bool,
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
		if c.abrMixerAudioForward(ctx, entry, audioBufID, audioIsABR, downBufIDs) {
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
func (c *Coordinator) abrMixerAudioForward(
	ctx context.Context,
	entry *abrMixerEntry,
	audioBufID domain.StreamCode,
	audioIsABR bool,
	downBufIDs []domain.StreamCode,
) bool {
	if audioIsABR {
		return c.abrMixerAudioForwardABR(ctx, entry, audioBufID, downBufIDs)
	}
	return c.abrMixerAudioForwardDirect(ctx, entry, audioBufID, downBufIDs)
}

// abrMixerAudioForwardABR demuxes the audio upstream's best-rendition TS
// stream into AVPackets and fans audio frames out to every downstream rung.
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

	for {
		batch, err := demux.ReadPackets(ctx)
		if err != nil {
			return ctx.Err() != nil
		}
		for _, p := range batch {
			if !p.Codec.IsAudio() {
				continue
			}
			pkt := buffer.Packet{AV: &p}
			c.fanOutToRenditions(downBufIDs, pkt, entry)
		}
	}
}

// abrMixerAudioForwardDirect subscribes to a single-stream audio upstream's
// main buffer (AVPackets) and fans audio frames out to every rung.
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
