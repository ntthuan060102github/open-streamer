package publisher

import (
	"context"
	"log/slog"

	"github.com/open-streamer/open-streamer/internal/domain"
)

func (s *Service) serveRTS(ctx context.Context, streamID domain.StreamCode) {
	slog.Info("publisher: RTS (WebRTC/WHEP) requested", "stream_code", streamID)
	slog.Warn("publisher: RTS/WebRTC is not implemented — WHEP requires a WebRTC stack (e.g. Pion) or an external SFU; use HLS/DASH with a short live_segment_sec for browser playback",
		"stream_code", streamID,
		"hls_base_url", s.cfg.HLS.BaseURL,
	)
	<-ctx.Done()
}
