package publisher

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

// serveDASH starts a single-rendition MPEG-DASH publisher for streamID.
//
// The stream is read from the Buffer Hub via a new Subscriber (TS or AV packets),
// segmented into fMP4 fragments, and an MPD playlist is kept up to date on disk.
// The output directory (cfg.DASH.Dir/<streamID>/) is wiped clean on every start
// so stale segments from a previous run are never served.
func (s *Service) serveDASH(ctx context.Context, streamID domain.StreamCode) {
	cfg := s.cfg.DASH
	streamDir := filepath.Join(cfg.Dir, string(streamID))

	if err := resetOutputDir(streamDir); err != nil {
		slog.Error("publisher: DASH reset output dir failed",
			"stream_code", streamID, "dir", streamDir, "err", err)
		return
	}

	manifestPath := filepath.Join(streamDir, "index.mpd")

	sub, err := s.buf.Subscribe(streamID)
	if err != nil {
		slog.Error("publisher: DASH subscribe failed",
			"stream_code", streamID, "err", err)
		return
	}
	defer s.buf.Unsubscribe(streamID, sub)

	slog.Info("publisher: DASH serve started (fMP4 + dynamic MPD)",
		"stream_code", streamID, "dash_dir", streamDir)

	runDASHFMP4Packager(
		ctx,
		streamID,
		sub,
		streamDir,
		manifestPath,
		cfg.LiveSegmentSec,
		cfg.LiveWindow,
		cfg.LiveHistory,
		cfg.LiveEphemeral,
		nil,
	)

	slog.Info("publisher: DASH serve stopped", "stream_code", streamID)
}

