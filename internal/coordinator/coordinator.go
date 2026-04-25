// Package coordinator wires buffer, stream manager, transcoder, and publisher
// for a single stream lifecycle. Used by the HTTP API and server bootstrap.
package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/dvr"
	"github.com/ntt0601zcoder/open-streamer/internal/events"
	"github.com/ntt0601zcoder/open-streamer/internal/manager"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/ntt0601zcoder/open-streamer/internal/publisher"
	"github.com/ntt0601zcoder/open-streamer/internal/store"
	"github.com/ntt0601zcoder/open-streamer/internal/transcoder"
	"github.com/samber/do/v2"
)

// Coordinator starts and stops the full per-stream pipeline.
type Coordinator struct {
	buf        *buffer.Service
	mgr        mgrDep
	tc         tcDep
	pub        pubDep
	dvr        dvrDep
	bus        events.Bus
	m          *metrics.Metrics
	streamRepo store.StreamRepository

	// upstreamLookup resolves another stream by code; used by detectABRCopy
	// to inspect the upstream's transcoder ladder shape before deciding
	// which pipeline branch to wire. Nil in unit tests that don't exercise
	// ABR copy — detectABRCopy short-circuits to the normal path.
	upstreamLookup func(domain.StreamCode) (*domain.Stream, bool)

	rendMu     sync.Mutex
	renditions map[domain.StreamCode][]string // ABR rendition slugs per stream (for buffer teardown)

	abrMu     sync.RWMutex
	abrCopies map[domain.StreamCode]*abrCopyEntry  // streams currently running as ABR copy
	abrMixers map[domain.StreamCode]*abrMixerEntry // streams currently running as ABR mixer (mirror video ladder + audio fan-out)

	statusMu sync.RWMutex
	status   map[domain.StreamCode]domain.StreamStatus // runtime status per running stream
}

// New registers a Coordinator with the DI injector.
func New(i do.Injector) (*Coordinator, error) {
	repo := do.MustInvoke[store.StreamRepository](i)
	c := &Coordinator{
		buf:        do.MustInvoke[*buffer.Service](i),
		mgr:        do.MustInvoke[*manager.Service](i),
		tc:         do.MustInvoke[*transcoder.Service](i),
		pub:        do.MustInvoke[*publisher.Service](i),
		dvr:        do.MustInvoke[*dvr.Service](i),
		bus:        do.MustInvoke[events.Bus](i),
		m:          do.MustInvoke[*metrics.Metrics](i),
		streamRepo: repo,
		upstreamLookup: func(code domain.StreamCode) (*domain.Stream, bool) {
			s, err := repo.FindByCode(context.Background(), code)
			if err != nil {
				return nil, false
			}
			return s, true
		},
		renditions: make(map[domain.StreamCode][]string),
		abrCopies:  make(map[domain.StreamCode]*abrCopyEntry),
		abrMixers:  make(map[domain.StreamCode]*abrMixerEntry),
		status:     make(map[domain.StreamCode]domain.StreamStatus),
	}
	c.mgr.SetExhaustedCallback(c.handleAllInputsExhausted)
	c.mgr.SetRestoredCallback(c.handleInputRestored)
	return c, nil
}

// newForTesting builds a Coordinator from pre-constructed deps, for use in tests.
func newForTesting(
	buf *buffer.Service,
	mgr mgrDep,
	tc tcDep,
	pub pubDep,
	dvr dvrDep,
	bus events.Bus,
	m *metrics.Metrics,
) *Coordinator {
	c := &Coordinator{
		buf:        buf,
		mgr:        mgr,
		tc:         tc,
		pub:        pub,
		dvr:        dvr,
		bus:        bus,
		m:          m,
		renditions: make(map[domain.StreamCode][]string),
		abrCopies:  make(map[domain.StreamCode]*abrCopyEntry),
		abrMixers:  make(map[domain.StreamCode]*abrMixerEntry),
		status:     make(map[domain.StreamCode]domain.StreamStatus),
	}
	c.mgr.SetExhaustedCallback(c.handleAllInputsExhausted)
	c.mgr.SetRestoredCallback(c.handleInputRestored)
	return c
}

// SetUpstreamLookupForTesting injects an upstream stream resolver — used by
// tests that exercise the ABR-copy branch without spinning up a store backend.
func (c *Coordinator) SetUpstreamLookupForTesting(fn func(domain.StreamCode) (*domain.Stream, bool)) {
	c.upstreamLookup = fn
}

// StreamStatus returns the runtime status of a stream pipeline.
// It is derived purely from in-memory state — never read from the store.
//
//   - StatusStopped  — pipeline is not registered (never started, or was stopped)
//   - StatusActive   — pipeline is running and at least one input is live
//   - StatusDegraded — pipeline is running but all inputs are currently exhausted
func (c *Coordinator) StreamStatus(code domain.StreamCode) domain.StreamStatus {
	if !c.IsRunning(code) {
		return domain.StatusStopped
	}
	c.statusMu.RLock()
	st := c.status[code]
	c.statusMu.RUnlock()
	if st == "" {
		return domain.StatusActive
	}
	return st
}

func (c *Coordinator) setStatus(code domain.StreamCode, st domain.StreamStatus) {
	c.statusMu.Lock()
	if st == "" || st == domain.StatusActive {
		delete(c.status, code) // active is the default; no need to store it explicitly
	} else {
		c.status[code] = st
	}
	c.statusMu.Unlock()
}

// Start creates the buffer, registers the stream with the manager (ingest + failover),
// starts publisher outputs, and optionally a transcoder worker.
//
// When the stream's first input is `copy://X` and X has an ABR ladder the
// pipeline is wired through startABRCopy instead — no ingest worker, no
// transcoder, just N tap goroutines re-publishing upstream rendition packets.
func (c *Coordinator) Start(ctx context.Context, stream *domain.Stream) error {
	if stream == nil {
		return fmt.Errorf("coordinator: nil stream")
	}
	if c.IsRunning(stream.Code) {
		return nil
	}
	if stream.Disabled {
		return fmt.Errorf("coordinator: stream %q is disabled", stream.Code)
	}
	if len(stream.Inputs) == 0 {
		return fmt.Errorf("coordinator: stream %q has no inputs", stream.Code)
	}

	if upstream := c.detectABRCopy(stream); upstream != nil {
		return c.startABRCopy(ctx, stream, upstream)
	}
	if videoUp, audioUp, ok := c.detectABRMixer(stream); ok {
		return c.startABRMixer(ctx, stream, videoUp, audioUp)
	}

	c.buf.Create(stream.Code)

	ingestWriteID := stream.Code
	if shouldRunTranscoder(stream) {
		ingestWriteID = buffer.RawIngestBufferID(stream.Code)
		c.buf.Create(ingestWriteID)
	}

	// Build the rendition targets and create their buffers BEFORE starting the
	// publisher. The publisher goroutines (serveHLSAdaptive, serveDASHAdaptive)
	// subscribe to rendition buffers synchronously on a different goroutine; if
	// those buffers don't exist yet the subscribe fails and the rendition is
	// silently dropped from the master playlist.
	var (
		transcoderTargets []transcoder.RenditionTarget
		renditionSlugs    []string
	)
	if shouldRunTranscoder(stream) {
		profiles := transcoderProfilesFromDomain(&stream.Transcoder.Video)
		for i := range profiles {
			slug := buffer.VideoTrackSlug(i)
			bid := buffer.RenditionBufferID(stream.Code, slug)
			c.buf.Create(bid)
			renditionSlugs = append(renditionSlugs, slug)
			transcoderTargets = append(transcoderTargets, transcoder.RenditionTarget{
				BufferID: bid,
				Profile:  profiles[i],
			})
		}
	}

	if err := c.mgr.Register(ctx, stream, ingestWriteID); err != nil {
		for _, slug := range renditionSlugs {
			c.buf.Delete(buffer.RenditionBufferID(stream.Code, slug))
		}
		c.buf.Delete(stream.Code)
		if ingestWriteID != stream.Code {
			c.buf.Delete(ingestWriteID)
		}
		return fmt.Errorf("coordinator: register manager: %w", err)
	}

	if err := c.pub.Start(ctx, stream); err != nil {
		c.mgr.Unregister(stream.Code)
		for _, slug := range renditionSlugs {
			c.buf.Delete(buffer.RenditionBufferID(stream.Code, slug))
		}
		c.buf.Delete(stream.Code)
		if ingestWriteID != stream.Code {
			c.buf.Delete(ingestWriteID)
		}
		return fmt.Errorf("coordinator: publisher: %w", err)
	}

	if len(transcoderTargets) > 0 {
		rawID := buffer.RawIngestBufferID(stream.Code)
		if err := c.tc.Start(ctx, stream.Code, rawID, stream.Transcoder, transcoderTargets); err != nil {
			for _, slug := range renditionSlugs {
				c.buf.Delete(buffer.RenditionBufferID(stream.Code, slug))
			}
			c.pub.Stop(stream.Code)
			c.mgr.Unregister(stream.Code)
			c.buf.Delete(stream.Code)
			c.buf.Delete(rawID)
			return fmt.Errorf("coordinator: transcoder: %w", err)
		}
		c.rendMu.Lock()
		c.renditions[stream.Code] = renditionSlugs
		c.rendMu.Unlock()
	}

	if stream.DVR != nil && stream.DVR.Enabled {
		mediaBuf := buffer.PlaybackBufferID(stream.Code, stream.Transcoder)
		if _, err := c.dvr.StartRecording(ctx, stream.Code, mediaBuf, stream.DVR); err != nil {
			slog.Warn("coordinator: dvr start failed", "stream_code", stream.Code, "err", err)
		}
	}

	if c.m != nil {
		c.m.StreamStartTimeSeconds.WithLabelValues(string(stream.Code)).Set(float64(time.Now().Unix()))
	}

	c.setStatus(stream.Code, domain.StatusActive)

	c.bus.Publish(ctx, domain.Event{
		Type:       domain.EventStreamStarted,
		StreamCode: stream.Code,
	})

	slog.Info("coordinator: stream pipeline started",
		"stream_code", stream.Code,
		"inputs", len(stream.Inputs),
		"transcoder", shouldRunTranscoder(stream),
		"renditions", len(renditionSlugs),
		"hls", stream.Protocols.HLS,
		"dash", stream.Protocols.DASH,
		"rtsp", stream.Protocols.RTSP,
		"push_targets", len(stream.Push),
		"dvr", stream.DVR != nil && stream.DVR.Enabled,
	)
	return nil
}

// IsRunning reports whether the stream pipeline is active in memory.
// Covers the normal pipeline (manager-registered) plus the in-process bypass
// paths (ABR copy, ABR mixer) that don't go through the manager.
func (c *Coordinator) IsRunning(streamID domain.StreamCode) bool {
	if c.mgr.IsRegistered(streamID) {
		return true
	}
	c.abrMu.RLock()
	_, copyOK := c.abrCopies[streamID]
	_, mixerOK := c.abrMixers[streamID]
	c.abrMu.RUnlock()
	return copyOK || mixerOK
}

// Stop tears down publisher, transcoder, manager (ingest), and the buffer.
// ctx is used for the DVR stop and the EventStreamStopped publish; cleanup
// of in-memory state always proceeds even if ctx is cancelled.
func (c *Coordinator) Stop(ctx context.Context, streamID domain.StreamCode) {
	c.abrMu.Lock()
	abr, isABR := c.abrCopies[streamID]
	if isABR {
		delete(c.abrCopies, streamID)
	}
	mix, isMix := c.abrMixers[streamID]
	if isMix {
		delete(c.abrMixers, streamID)
	}
	c.abrMu.Unlock()
	if isABR {
		slog.Info("coordinator: stopping abr copy pipeline", "stream_code", streamID)
		c.stopABRCopy(ctx, streamID, abr)
		return
	}
	if isMix {
		slog.Info("coordinator: stopping abr mixer pipeline", "stream_code", streamID)
		c.stopABRMixer(ctx, streamID, mix)
		return
	}

	slog.Info("coordinator: stopping stream pipeline", "stream_code", streamID)
	if c.dvr.IsRecording(streamID) {
		if err := c.dvr.StopRecording(ctx, streamID); err != nil {
			slog.Warn("coordinator: dvr stop failed", "stream_code", streamID, "err", err)
		}
	}
	c.pub.Stop(streamID)
	c.tc.Stop(streamID)
	c.mgr.Unregister(streamID)

	c.rendMu.Lock()
	slugs := c.renditions[streamID]
	delete(c.renditions, streamID)
	c.rendMu.Unlock()
	for _, slug := range slugs {
		c.buf.Delete(buffer.RenditionBufferID(streamID, slug))
	}

	c.buf.Delete(buffer.RawIngestBufferID(streamID))
	c.buf.Delete(streamID)

	if c.m != nil {
		c.m.StreamStartTimeSeconds.DeleteLabelValues(string(streamID))
	}

	c.setStatus(streamID, domain.StatusActive)

	c.bus.Publish(ctx, domain.Event{
		Type:       domain.EventStreamStopped,
		StreamCode: streamID,
	})
}

// Update hot-reloads only the components that changed between old and new stream configs.
// The caller must persist the new config to the store BEFORE calling Update.
// Returns early with c.Stop() when the stream transitions to Disabled.
func (c *Coordinator) Update(ctx context.Context, old, new *domain.Stream) error {
	if old == nil || new == nil {
		return fmt.Errorf("coordinator: Update requires both old and new stream")
	}

	// ABR copy / ABR mixer use custom pipelines (no ingestor / transcoder),
	// so the per-component diff routing doesn't apply. Whenever either side
	// is on one of these paths we full-cycle: stop whatever is running, then
	// start fresh.
	c.abrMu.RLock()
	wasABRCopy := c.abrCopies[old.Code] != nil
	wasABRMixer := c.abrMixers[old.Code] != nil
	c.abrMu.RUnlock()
	willBeABRCopy := !new.Disabled && c.detectABRCopy(new) != nil
	_, _, willBeABRMixer := c.detectABRMixer(new)
	willBeABRMixer = willBeABRMixer && !new.Disabled
	if wasABRCopy || wasABRMixer || willBeABRCopy || willBeABRMixer {
		c.Stop(ctx, old.Code)
		if new.Disabled || len(new.Inputs) == 0 {
			return nil
		}
		return c.Start(ctx, new)
	}

	diff := ComputeDiff(old, new)

	slog.Info("coordinator: applying stream update",
		"stream_code", new.Code,
		"now_disabled", diff.NowDisabled,
		"transcoder_topology_changed", diff.TranscoderTopologyChanged,
		"transcoder_changed", diff.TranscoderChanged,
		"inputs_changed", diff.InputsChanged,
		"protocols_changed", diff.ProtocolsChanged,
		"push_changed", diff.PushChanged,
		"dvr_changed", diff.DVRChanged,
	)

	if diff.NowDisabled {
		c.Stop(ctx, new.Code)
		return nil
	}

	// Transcoder topology changes (nil↔non-nil, mode change, video.copy change)
	// require rebuilding the whole buffer layout → full pipeline reload.
	if diff.TranscoderTopologyChanged {
		return c.reloadTranscoderFull(ctx, old, new)
	}

	// Per-profile transcoder changes: stop/start only affected FFmpeg processes.
	if diff.TranscoderChanged && diff.ProfilesDiff != nil {
		//nolint:contextcheck // StartProfile derives from streamWorker.baseCtx; by design
		if err := c.reloadProfiles(new, diff.ProfilesDiff); err != nil {
			return fmt.Errorf("coordinator: reload profiles: %w", err)
		}
		// Push updated profile metadata to the live HLS master playlist writer so
		// resolution/bandwidth info reflects the new profile without a publisher restart.
		if len(diff.ProfilesDiff.Updated) > 0 {
			c.pub.UpdateABRMasterMeta(new.Code, abrMetaFromUpdated(new, diff.ProfilesDiff.Updated))
		}
	}

	if diff.InputsChanged {
		c.mgr.UpdateInputs(new.Code, diff.AddedInputs, diff.RemovedInputs, diff.UpdatedInputs)
	}

	// Profile add/remove: restart only HLS+DASH (RTSP/SRT viewers unaffected).
	if diff.ProfilesDiff != nil && diff.ProfilesDiff.HasAddedOrRemoved() {
		if err := c.pub.RestartHLSDASH(ctx, new); err != nil {
			return fmt.Errorf("coordinator: restart hls/dash: %w", err)
		}
	}

	// Protocol/push changes: surgically stop/start only affected goroutines.
	if diff.ProtocolsChanged || diff.PushChanged {
		if err := c.pub.UpdateProtocols(ctx, old, new); err != nil {
			return fmt.Errorf("coordinator: update protocols: %w", err)
		}
	}

	if diff.DVRChanged {
		c.reloadDVR(ctx, new)
	} else if diff.ProfilesDiff != nil && diff.ProfilesDiff.HasAddedOrRemoved() {
		// Best rendition may have changed (profile added/removed); re-evaluate DVR buffer.
		c.reloadDVRIfBufferChanged(ctx, old, new)
	}

	return nil
}

// reloadTranscoderFull rebuilds the entire pipeline when transcoder topology changes.
// This is the fallback for changes that affect buffer layout (nil↔non-nil, mode change).
func (c *Coordinator) reloadTranscoderFull(ctx context.Context, old, new *domain.Stream) error {
	slog.Info("coordinator: full transcoder reload", "stream_code", new.Code)

	if c.dvr.IsRecording(new.Code) {
		if err := c.dvr.StopRecording(ctx, new.Code); err != nil {
			slog.Warn("coordinator: dvr stop during reload failed", "stream_code", new.Code, "err", err)
		}
	}
	c.pub.Stop(new.Code)
	//nolint:contextcheck // tc.Stop uses its own baseCtx from Start; by design
	c.tc.Stop(new.Code)

	// Tear down old rendition buffers.
	c.rendMu.Lock()
	oldSlugs := c.renditions[new.Code]
	delete(c.renditions, new.Code)
	c.rendMu.Unlock()
	for _, slug := range oldSlugs {
		c.buf.Delete(buffer.RenditionBufferID(new.Code, slug))
	}

	// Rebuild buffers based on new config.
	oldIngestID := new.Code
	if shouldRunTranscoder(old) {
		oldIngestID = buffer.RawIngestBufferID(new.Code)
	}

	newIngestID := new.Code
	var transcoderTargets []transcoder.RenditionTarget
	var renditionSlugs []string

	if shouldRunTranscoder(new) {
		newIngestID = buffer.RawIngestBufferID(new.Code)
		if oldIngestID != newIngestID {
			c.buf.Create(newIngestID)
		}
		profiles := transcoderProfilesFromDomain(&new.Transcoder.Video)
		for i := range profiles {
			slug := buffer.VideoTrackSlug(i)
			bid := buffer.RenditionBufferID(new.Code, slug)
			c.buf.Create(bid)
			renditionSlugs = append(renditionSlugs, slug)
			transcoderTargets = append(transcoderTargets, transcoder.RenditionTarget{
				BufferID: bid,
				Profile:  profiles[i],
			})
		}
	} else if oldIngestID != newIngestID {
		// Old had raw buffer, new doesn't → delete it.
		c.buf.Delete(oldIngestID)
	}

	// Swap buffer write target on the active ingestor.
	c.mgr.UpdateBufferWriteID(new.Code, newIngestID)

	if len(transcoderTargets) > 0 {
		if err := c.tc.Start(ctx, new.Code, newIngestID, new.Transcoder, transcoderTargets); err != nil {
			return fmt.Errorf("transcoder start: %w", err)
		}
		c.rendMu.Lock()
		c.renditions[new.Code] = renditionSlugs
		c.rendMu.Unlock()
	}

	if err := c.pub.Start(ctx, new); err != nil {
		return fmt.Errorf("publisher start: %w", err)
	}

	c.reloadDVR(ctx, new)
	return nil
}

// reloadProfiles applies per-profile transcoder changes without touching unchanged profiles.
// Updated profiles: stop+start the corresponding FFmpeg process.
// Added profiles: create rendition buffer + start FFmpeg process.
// Removed profiles: stop FFmpeg process + delete rendition buffer.
func (c *Coordinator) reloadProfiles(new *domain.Stream, pd *ProfilesDiff) error {
	newProfiles := transcoderProfilesFromDomain(&new.Transcoder.Video)

	// Removed: stop encoders and delete their buffers.
	for _, ch := range pd.Removed {
		c.tc.StopProfile(new.Code, ch.Index)
		slug := buffer.VideoTrackSlug(ch.Index)
		c.buf.Delete(buffer.RenditionBufferID(new.Code, slug))

		c.rendMu.Lock()
		slugs := c.renditions[new.Code]
		for i, s := range slugs {
			if s == slug {
				c.renditions[new.Code] = append(slugs[:i], slugs[i+1:]...)
				break
			}
		}
		c.rendMu.Unlock()
	}

	// Updated: stop + restart each changed encoder.
	for _, ch := range pd.Updated {
		if ch.Index >= len(newProfiles) {
			continue
		}
		c.tc.StopProfile(new.Code, ch.Index)
		slug := buffer.VideoTrackSlug(ch.Index)
		bid := buffer.RenditionBufferID(new.Code, slug)
		if err := c.tc.StartProfile(new.Code, ch.Index, transcoder.RenditionTarget{
			BufferID: bid,
			Profile:  newProfiles[ch.Index],
		}); err != nil {
			return fmt.Errorf("restart profile %d: %w", ch.Index, err)
		}
	}

	// Added: create buffer + start new encoder.
	for _, ch := range pd.Added {
		if ch.Index >= len(newProfiles) {
			continue
		}
		slug := buffer.VideoTrackSlug(ch.Index)
		bid := buffer.RenditionBufferID(new.Code, slug)
		c.buf.Create(bid)
		if err := c.tc.StartProfile(new.Code, ch.Index, transcoder.RenditionTarget{
			BufferID: bid,
			Profile:  newProfiles[ch.Index],
		}); err != nil {
			c.buf.Delete(bid)
			return fmt.Errorf("start added profile %d: %w", ch.Index, err)
		}

		c.rendMu.Lock()
		c.renditions[new.Code] = append(c.renditions[new.Code], slug)
		c.rendMu.Unlock()
	}

	return nil
}

// abrMetaFromUpdated converts ProfilesDiff.Updated entries to publisher.ABRRepMeta slices
// so the live HLS master playlist can be rewritten without restarting the publisher.
func abrMetaFromUpdated(stream *domain.Stream, updated []ProfileChange) []publisher.ABRRepMeta {
	profiles := transcoderProfilesFromDomain(&stream.Transcoder.Video)
	out := make([]publisher.ABRRepMeta, 0, len(updated))
	for _, ch := range updated {
		if ch.Index >= len(profiles) {
			continue
		}
		p := profiles[ch.Index]
		bwBps := 0
		if ch.New != nil {
			bwBps = ch.New.Bitrate * 1000
		}
		out = append(out, publisher.ABRRepMeta{
			Slug:   buffer.VideoTrackSlug(ch.Index),
			BwBps:  bwBps,
			Width:  p.Width,
			Height: p.Height,
		})
	}
	return out
}

// reloadDVR stops any active recording and starts a new one when DVR is enabled.
func (c *Coordinator) reloadDVR(ctx context.Context, new *domain.Stream) {
	if c.dvr.IsRecording(new.Code) {
		if err := c.dvr.StopRecording(ctx, new.Code); err != nil {
			slog.Warn("coordinator: dvr stop failed", "stream_code", new.Code, "err", err)
		}
	}
	if new.DVR != nil && new.DVR.Enabled {
		mediaBuf := buffer.PlaybackBufferID(new.Code, new.Transcoder)
		if _, err := c.dvr.StartRecording(ctx, new.Code, mediaBuf, new.DVR); err != nil {
			slog.Warn("coordinator: dvr start failed", "stream_code", new.Code, "err", err)
		}
	}
}

// reloadDVRIfBufferChanged reloads DVR only when the playback buffer changed
// (e.g. best rendition shifted when a higher-resolution profile was added/removed).
func (c *Coordinator) reloadDVRIfBufferChanged(ctx context.Context, old, new *domain.Stream) {
	oldBuf := buffer.PlaybackBufferID(old.Code, old.Transcoder)
	newBuf := buffer.PlaybackBufferID(new.Code, new.Transcoder)
	if oldBuf != newBuf {
		c.reloadDVR(ctx, new)
	}
}

// handleAllInputsExhausted is called by the manager when all inputs are degraded.
func (c *Coordinator) handleAllInputsExhausted(streamCode domain.StreamCode) {
	slog.Warn("coordinator: all inputs exhausted, stream degraded", "stream_code", streamCode)
	c.setStatus(streamCode, domain.StatusDegraded)
	c.bus.Publish(context.Background(), domain.Event{
		Type:       domain.EventInputDegraded,
		StreamCode: streamCode,
		Payload:    map[string]any{"reason": "all_inputs_exhausted"},
	})
}

// handleInputRestored is called by the manager when failover succeeds after all inputs
// were previously exhausted.
func (c *Coordinator) handleInputRestored(streamCode domain.StreamCode) {
	slog.Info("coordinator: input restored, stream active", "stream_code", streamCode)
	c.setStatus(streamCode, domain.StatusActive)
}

// BootstrapPersistedStreams starts the pipeline for every non-disabled stream that
// has at least one input configured.  Stream status is never persisted, so all
// eligible streams are started fresh on every boot regardless of their last
// known runtime state.
func BootstrapPersistedStreams(ctx context.Context, log *slog.Logger, repo store.StreamRepository, coord *Coordinator) {
	streams, err := repo.List(ctx, store.StreamFilter{})
	if err != nil {
		log.Error("bootstrap: list streams failed", "err", err)
		return
	}
	for _, st := range streams {
		if st == nil {
			continue
		}
		if st.Disabled {
			log.Debug("bootstrap: skip stream (disabled)", "stream_code", st.Code)
			continue
		}
		if len(st.Inputs) == 0 {
			log.Debug("bootstrap: skip stream (no inputs)", "stream_code", st.Code)
			continue
		}
		if err := coord.Start(ctx, st); err != nil {
			log.Warn("bootstrap: stream start failed", "stream_code", st.Code, "err", err)
			continue
		}
		log.Info("bootstrap: stream pipeline started", "stream_code", st.Code)
	}
}

func shouldRunTranscoder(stream *domain.Stream) bool {
	if stream == nil || stream.Transcoder == nil {
		return false
	}
	tc := stream.Transcoder
	// Both video and audio copy → raw MPEG-TS passes through without FFmpeg.
	return !tc.Video.Copy || !tc.Audio.Copy
}

// transcoderProfilesFromDomain maps stream video settings to FFmpeg ladder profiles.
// When video.copy is true or profiles is empty, returns a single passthrough profile
// (mpegts copy — one worker, no ABR). Explicit non-empty profiles with copy false
// build a multi-rendition encode ladder.
func transcoderProfilesFromDomain(video *domain.VideoTranscodeConfig) []transcoder.Profile {
	if video == nil {
		return singleOriginCopyProfile()
	}
	if video.Copy || len(video.Profiles) == 0 {
		return singleOriginCopyProfile()
	}
	out := make([]transcoder.Profile, 0, len(video.Profiles))
	for _, p := range video.Profiles {
		// Leave codec/preset empty when not set — buildFFmpegArgs +
		// normalizeVideoEncoder route on Global.HW (e.g. "" → h264_nvenc when
		// hw=nvenc, libx264 when hw=none). Defaulting to "libx264"/"fast" here
		// hardcodes the CPU encoder and silently ignores the HW config.
		codec := string(p.Codec)
		if codec == string(domain.VideoCodecCopy) {
			codec = ""
		}
		br := strconv.Itoa(p.Bitrate) + "k"
		if p.Bitrate <= 0 {
			br = strconv.Itoa(domain.DefaultVideoBitrateK) + "k"
		}
		out = append(out, transcoder.Profile{
			Width:            p.Width,
			Height:           p.Height,
			Bitrate:          br,
			Codec:            codec,
			Preset:           p.Preset,
			CodecProfile:     p.Profile,
			CodecLevel:       p.Level,
			MaxBitrate:       p.MaxBitrate,
			Framerate:        p.Framerate,
			KeyframeInterval: p.KeyframeInterval,
			Bframes:          p.Bframes,
			Refs:             p.Refs,
			SAR:              p.SAR,
			ResizeMode:       string(p.ResizeMode),
		})
	}
	return out
}

func singleOriginCopyProfile() []transcoder.Profile {
	// Empty codec/preset → buildFFmpegArgs picks based on Global.HW.
	return []transcoder.Profile{{
		Bitrate: strconv.Itoa(domain.DefaultVideoBitrateK) + "k",
	}}
}
