package publisher

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/open-streamer/open-streamer/internal/domain"
)

func (s *Service) serveHLS(ctx context.Context, streamID domain.StreamCode) {
	sub, err := s.buf.Subscribe(streamID)
	if err != nil {
		slog.Error("publisher: HLS subscribe failed", "stream_code", streamID, "err", err)
		return
	}
	defer s.buf.Unsubscribe(streamID, sub)

	slog.Info("publisher: HLS serve started (native TS segments)", "stream_code", streamID, "hls_dir", s.cfg.HLS.Dir)

	streamDir := filepath.Join(s.cfg.HLS.Dir, string(streamID))
	if err := resetOutputDir(streamDir); err != nil {
		slog.Error("publisher: HLS setup dir failed", "stream_code", streamID, "dir", streamDir, "err", err)
		return
	}

	playlist := filepath.Join(streamDir, "index.m3u8")
	runFilesystemSegmenter(ctx, streamID, sub, segmenterOpts{
		streamDir:   streamDir,
		hlsPlaylist: playlist,
		segmentSec:  s.cfg.HLS.LiveSegmentSec,
		window:      s.cfg.HLS.LiveWindow,
		history:     s.cfg.HLS.LiveHistory,
		ephemeral:   s.cfg.HLS.LiveEphemeral,
	})
}

func resetOutputDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}
