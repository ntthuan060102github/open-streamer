// Package coordinator wires buffer, stream manager, transcoder, and publisher
// for a single stream lifecycle. Used by the HTTP API and server bootstrap.
package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/manager"
	"github.com/open-streamer/open-streamer/internal/publisher"
	"github.com/open-streamer/open-streamer/internal/store"
	"github.com/open-streamer/open-streamer/internal/transcoder"
	"github.com/samber/do/v2"
)

// Coordinator starts and stops the full per-stream pipeline.
type Coordinator struct {
	buf *buffer.Service
	mgr *manager.Service
	tc  *transcoder.Service
	pub *publisher.Service
}

// New registers a Coordinator with the DI injector.
func New(i do.Injector) (*Coordinator, error) {
	return &Coordinator{
		buf: do.MustInvoke[*buffer.Service](i),
		mgr: do.MustInvoke[*manager.Service](i),
		tc:  do.MustInvoke[*transcoder.Service](i),
		pub: do.MustInvoke[*publisher.Service](i),
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
	if len(stream.Inputs) == 0 {
		return fmt.Errorf("coordinator: stream %q has no inputs", stream.Code)
	}

	c.buf.Create(stream.Code)

	if err := c.mgr.Register(ctx, stream); err != nil {
		c.buf.Delete(stream.Code)
		return fmt.Errorf("coordinator: register manager: %w", err)
	}

	if err := c.pub.Start(ctx, stream); err != nil {
		c.mgr.Unregister(stream.Code)
		c.buf.Delete(stream.Code)
		return fmt.Errorf("coordinator: publisher: %w", err)
	}

	if shouldRunTranscoder(stream) {
		profiles := transcoderProfilesFromDomain(stream.Transcoder.Video.Profiles)
		if err := c.tc.Start(ctx, stream.Code, profiles); err != nil {
			c.pub.Stop(stream.Code)
			c.mgr.Unregister(stream.Code)
			c.buf.Delete(stream.Code)
			return fmt.Errorf("coordinator: transcoder: %w", err)
		}
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
	c.buf.Delete(streamID)
}

// BootstrapPersistedStreams loads every stream from the store (except those marked stopped)
// and starts the ingest + publish pipeline for each stream that has at least one input.
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

func transcoderProfilesFromDomain(profiles []domain.VideoProfile) []transcoder.Profile {
	if len(profiles) == 0 {
		return transcoder.DefaultProfiles
	}
	out := make([]transcoder.Profile, 0, len(profiles))
	for _, p := range profiles {
		codec := string(p.Codec)
		if codec == "" || codec == string(domain.VideoCodecCopy) {
			codec = "libx264"
		}
		br := strconv.Itoa(p.Bitrate) + "k"
		if p.Bitrate <= 0 {
			br = "2500k"
		}
		name := p.Name
		if name == "" {
			name = fmt.Sprintf("%dx%d", p.Width, p.Height)
		}
		preset := p.Preset
		if preset == "" {
			preset = "fast"
		}
		out = append(out, transcoder.Profile{
			Name:    name,
			Width:   p.Width,
			Height:  p.Height,
			Bitrate: br,
			Codec:   codec,
			Preset:  preset,
		})
	}
	return out
}
