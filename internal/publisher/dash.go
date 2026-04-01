package publisher

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/open-streamer/open-streamer/internal/domain"
)

func (s *Service) serveDASH(ctx context.Context, streamID domain.StreamCode) {
	dashDir := strings.TrimSpace(s.cfg.DASH.Dir)
	if dashDir == "" {
		slog.Error("publisher: DASH disabled — publisher.dash.dir is empty (must differ from HLS dir)",
			"stream_code", streamID,
		)
		return
	}

	sub, err := s.buf.Subscribe(streamID)
	if err != nil {
		slog.Error("publisher: DASH subscribe failed", "stream_code", streamID, "err", err)
		return
	}
	defer s.buf.Unsubscribe(streamID, sub)

	slog.Info("publisher: DASH serve started (fMP4 + dynamic MPD)", "stream_code", streamID, "dash_dir", dashDir)

	streamDir := filepath.Join(dashDir, string(streamID))
	if err := resetOutputDir(streamDir); err != nil {
		slog.Error("publisher: DASH setup dir failed", "stream_code", streamID, "dir", streamDir, "err", err)
		return
	}

	manifest := filepath.Join(streamDir, "index.mpd")
	runDASHFMP4Packager(ctx, streamID, sub, streamDir, manifest,
		s.cfg.DASH.LiveSegmentSec,
		s.cfg.DASH.LiveWindow,
		s.cfg.DASH.LiveHistory,
		s.cfg.DASH.LiveEphemeral,
	)
}
