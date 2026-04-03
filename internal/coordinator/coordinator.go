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
	"github.com/ntthuan060102github/open-streamer/internal/manager"
	"github.com/ntthuan060102github/open-streamer/internal/publisher"
	"github.com/ntthuan060102github/open-streamer/internal/store"
	"github.com/ntthuan060102github/open-streamer/internal/transcoder"
	"github.com/samber/do/v2"
)

// Coordinator starts and stops the full per-stream pipeline.
type Coordinator struct {
	buf *buffer.Service
	mgr *manager.Service
	tc  *transcoder.Service
	pub *publisher.Service

	rendMu     sync.Mutex
	renditions map[domain.StreamCode][]string // ABR rendition slugs per stream (for buffer teardown)
}

// New registers a Coordinator with the DI injector.
func New(i do.Injector) (*Coordinator, error) {
	return &Coordinator{
		buf:        do.MustInvoke[*buffer.Service](i),
		mgr:        do.MustInvoke[*manager.Service](i),
		tc:         do.MustInvoke[*transcoder.Service](i),
		pub:        do.MustInvoke[*publisher.Service](i),
		renditions: make(map[domain.StreamCode][]string),
	}, nil
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

	if err := c.mgr.Register(ctx, stream, ingestWriteID); err != nil {
		c.buf.Delete(stream.Code)
		if ingestWriteID != stream.Code {
			c.buf.Delete(ingestWriteID)
		}
		return fmt.Errorf("coordinator: register manager: %w", err)
	}

	if err := c.pub.Start(ctx, stream); err != nil {
		c.mgr.Unregister(stream.Code)
		c.buf.Delete(stream.Code)
		if ingestWriteID != stream.Code {
			c.buf.Delete(ingestWriteID)
		}
		return fmt.Errorf("coordinator: publisher: %w", err)
	}

	if shouldRunTranscoder(stream) {
		profiles := transcoderProfilesFromDomain(&stream.Transcoder.Video)
		rawID := buffer.RawIngestBufferID(stream.Code)
		var slugs []string
		targets := make([]transcoder.RenditionTarget, 0, len(profiles))
		for i := range profiles {
			slug := buffer.VideoTrackSlug(i)
			bid := buffer.RenditionBufferID(stream.Code, slug)
			c.buf.Create(bid)
			slugs = append(slugs, slug)
			targets = append(targets, transcoder.RenditionTarget{BufferID: bid, Profile: profiles[i]})
		}
		if err := c.tc.Start(ctx, stream.Code, rawID, stream.Transcoder, targets); err != nil {
			for _, slug := range slugs {
				c.buf.Delete(buffer.RenditionBufferID(stream.Code, slug))
			}
			c.pub.Stop(stream.Code)
			c.mgr.Unregister(stream.Code)
			c.buf.Delete(stream.Code)
			c.buf.Delete(rawID)
			return fmt.Errorf("coordinator: transcoder: %w", err)
		}
		c.rendMu.Lock()
		c.renditions[stream.Code] = slugs
		c.rendMu.Unlock()
	}

	return nil
}

// IsRunning reports whether the stream pipeline is active in memory.
func (c *Coordinator) IsRunning(streamID domain.StreamCode) bool {
	return c.mgr.IsRegistered(streamID)
}

// Stop tears down publisher, transcoder, manager (ingest), and the buffer.
func (c *Coordinator) Stop(streamID domain.StreamCode) {
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
	return !stream.Transcoder.Global.External
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
