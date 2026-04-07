// Package coordinator wires buffer, stream manager, transcoder, and publisher
// for a single stream lifecycle. Used by the HTTP API and server bootstrap.
package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/dvr"
	"github.com/ntthuan060102github/open-streamer/internal/events"
	"github.com/ntthuan060102github/open-streamer/internal/manager"
	"github.com/ntthuan060102github/open-streamer/internal/publisher"
	"github.com/ntthuan060102github/open-streamer/internal/store"
	"github.com/ntthuan060102github/open-streamer/internal/transcoder"
	"github.com/samber/do/v2"
)

// Coordinator starts and stops the full per-stream pipeline.
type Coordinator struct {
	buf        *buffer.Service
	mgr        *manager.Service
	tc         *transcoder.Service
	pub        *publisher.Service
	dvr        *dvr.Service
	bus        events.Bus
	streamRepo store.StreamRepository

	rendMu     sync.Mutex
	renditions map[domain.StreamCode][]string // ABR rendition slugs per stream (for buffer teardown)
}

// New registers a Coordinator with the DI injector.
func New(i do.Injector) (*Coordinator, error) {
	c := &Coordinator{
		buf:        do.MustInvoke[*buffer.Service](i),
		mgr:        do.MustInvoke[*manager.Service](i),
		tc:         do.MustInvoke[*transcoder.Service](i),
		pub:        do.MustInvoke[*publisher.Service](i),
		dvr:        do.MustInvoke[*dvr.Service](i),
		bus:        do.MustInvoke[events.Bus](i),
		streamRepo: do.MustInvoke[store.StreamRepository](i),
		renditions: make(map[domain.StreamCode][]string),
	}
	c.tc.SetFatalCallback(c.handleTranscoderFatal)
	c.mgr.SetExhaustedCallback(c.handleAllInputsExhausted)
	c.mgr.SetRestoredCallback(c.handleInputRestored)
	return c, nil
}

// Start creates the buffer, registers the stream with the manager (ingest + failover),
// starts publisher outputs, and optionally a transcoder worker.
func (c *Coordinator) Start(ctx context.Context, stream *domain.Stream) error {
	if stream == nil {
		return fmt.Errorf("coordinator: nil stream")
	}
	if c.mgr.IsRegistered(stream.Code) {
		return nil
	}
	if stream.Disabled {
		return fmt.Errorf("coordinator: stream %q is disabled", stream.Code)
	}
	if len(stream.Inputs) == 0 {
		return fmt.Errorf("coordinator: stream %q has no inputs", stream.Code)
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

	c.bus.Publish(ctx, domain.Event{
		Type:       domain.EventStreamStarted,
		StreamCode: stream.Code,
	})

	return nil
}

// IsRunning reports whether the stream pipeline is active in memory.
func (c *Coordinator) IsRunning(streamID domain.StreamCode) bool {
	return c.mgr.IsRegistered(streamID)
}

// Stop tears down publisher, transcoder, manager (ingest), and the buffer.
func (c *Coordinator) Stop(streamID domain.StreamCode) {
	if c.dvr.IsRecording(streamID) {
		if err := c.dvr.StopRecording(context.Background(), streamID); err != nil {
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

	c.bus.Publish(context.Background(), domain.Event{
		Type:       domain.EventStreamStopped,
		StreamCode: streamID,
	})
}

// handleAllInputsExhausted is called by the manager when all inputs are degraded.
// It persists stream status as degraded so operators and hooks are notified.
func (c *Coordinator) handleAllInputsExhausted(streamCode domain.StreamCode) {
	slog.Warn("coordinator: all inputs exhausted, marking stream degraded",
		"stream_code", streamCode,
	)
	ctx := context.Background()
	st, err := c.streamRepo.FindByCode(ctx, streamCode)
	if err != nil {
		slog.Warn("coordinator: exhausted — could not load stream", "stream_code", streamCode, "err", err)
		return
	}
	st.Status = domain.StatusDegraded
	if err := c.streamRepo.Save(ctx, st); err != nil {
		slog.Warn("coordinator: exhausted — could not persist stream status", "stream_code", streamCode, "err", err)
	}
	c.bus.Publish(ctx, domain.Event{
		Type:       domain.EventInputDegraded,
		StreamCode: streamCode,
		Payload:    map[string]any{"reason": "all_inputs_exhausted"},
	})
}

// handleInputRestored is called by the manager when failover succeeds after all inputs
// were previously exhausted. Marks the stream active again.
func (c *Coordinator) handleInputRestored(streamCode domain.StreamCode) {
	slog.Info("coordinator: input restored, marking stream active",
		"stream_code", streamCode,
	)
	ctx := context.Background()
	st, err := c.streamRepo.FindByCode(ctx, streamCode)
	if err != nil {
		slog.Warn("coordinator: restored — could not load stream", "stream_code", streamCode, "err", err)
		return
	}
	st.Status = domain.StatusActive
	if err := c.streamRepo.Save(ctx, st); err != nil {
		slog.Warn("coordinator: restored — could not persist stream status", "stream_code", streamCode, "err", err)
	}
}

// handleTranscoderFatal is called by the transcoder when a profile exceeds MaxRestarts.
// It stops the full pipeline and marks the stream as stopped in the store.
func (c *Coordinator) handleTranscoderFatal(streamCode domain.StreamCode) {
	slog.Error("coordinator: transcoder fatal, stopping stream pipeline",
		"stream_code", streamCode,
	)
	c.Stop(streamCode)

	ctx := context.Background()
	st, err := c.streamRepo.FindByCode(ctx, streamCode)
	if err != nil {
		slog.Warn("coordinator: transcoder fatal — could not load stream to update status",
			"stream_code", streamCode, "err", err)
		return
	}
	st.Status = domain.StatusStopped
	if err := c.streamRepo.Save(ctx, st); err != nil {
		slog.Warn("coordinator: transcoder fatal — could not persist stream status",
			"stream_code", streamCode, "err", err)
	}
}

// BootstrapPersistedStreams loads every stream from the store (except those marked stopped
// or disabled) and starts the ingest + publish pipeline for each stream that has at least one input.
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
		if st.Status == domain.StatusStopped {
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
	switch stream.Transcoder.Mode {
	case domain.TranscodeModePassthrough, domain.TranscodeModeRemux:
		return false
	case domain.TranscodeModeFull, "":
		return true
	}
	return true
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
		codec := string(p.Codec)
		if codec == "" || codec == string(domain.VideoCodecCopy) {
			codec = "libx264"
		}
		br := strconv.Itoa(p.Bitrate) + "k"
		if p.Bitrate <= 0 {
			br = "2500k"
		}
		preset := p.Preset
		if preset == "" {
			preset = "fast"
		}
		out = append(out, transcoder.Profile{
			Width:            p.Width,
			Height:           p.Height,
			Bitrate:          br,
			Codec:            codec,
			Preset:           preset,
			CodecProfile:     p.Profile,
			CodecLevel:       p.Level,
			MaxBitrate:       p.MaxBitrate,
			Framerate:        p.Framerate,
			KeyframeInterval: p.KeyframeInterval,
		})
	}
	return out
}

func singleOriginCopyProfile() []transcoder.Profile {
	return []transcoder.Profile{{
		Width:   0,
		Height:  0,
		Bitrate: "2500k",
		Codec:   "libx264",
		Preset:  "fast",
	}}
}
