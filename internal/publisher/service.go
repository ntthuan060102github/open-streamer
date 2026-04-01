// Package publisher delivers transcoded streams to all outputs.
//
// It handles two distinct output types:
//   - Serve endpoints (HLS, DASH, RTSP, RTMP listen, SRT listen, RTS/WebRTC): the server listens,
//     clients connect and pull. HLS uses MPEG-TS segments + m3u8; DASH uses fMP4 (init + .m4s) + dynamic MPD (Eyevinn mp4ff); RTSP/RTMP/SRT use gortsplib/gomedia/gosrt — no FFmpeg in this package.
//     RTSP, RTMP play, and SRT listen each use one shared TCP/UDP port (publisher.*.port_min); streams are selected by path (/live/<code>), RTMP app "live", or SRT streamid (live/<code>).
//   - Push destinations: currently rtmp:// via gomedia client; other schemes return a clear error.
package publisher

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bluenviron/gortsplib/v5"
	srt "github.com/datarhei/gosrt"

	"github.com/open-streamer/open-streamer/config"
	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/samber/do/v2"
)

// Service manages all output workers for active streams.
type Service struct {
	cfg     config.PublisherConfig
	buf     *buffer.Service
	mu      sync.Mutex
	workers map[domain.StreamCode]context.CancelFunc
	// streamWorkCtx is the per-stream publisher worker context (same lifetime as workers).
	streamWorkCtx map[domain.StreamCode]context.Context

	rtspMu     sync.RWMutex
	rtspSrv    *gortsplib.Server
	rtspMounts map[string]*gortsplib.ServerStream // path -> stream (see publisherLiveMountPath)

	rtmpMu     sync.Mutex
	rtmpLn     net.Listener
	rtmpActive map[domain.StreamCode]struct{}

	srtMu     sync.Mutex
	srtLn     srt.Listener
	srtActive map[domain.StreamCode]struct{}
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[*config.Config](i)
	buf := do.MustInvoke[*buffer.Service](i)
	pub := &cfg.Publisher

	return &Service{
		cfg:           *pub,
		buf:           buf,
		workers:       make(map[domain.StreamCode]context.CancelFunc),
		streamWorkCtx: make(map[domain.StreamCode]context.Context),
		rtspMounts:    make(map[string]*gortsplib.ServerStream),
		rtmpActive:    make(map[domain.StreamCode]struct{}),
		srtActive:     make(map[domain.StreamCode]struct{}),
	}, nil
}

// Start launches all serve-endpoints and push-destination workers for a stream.
func (s *Service) Start(ctx context.Context, stream *domain.Stream) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.workers[stream.Code]; ok {
		return fmt.Errorf("publisher: stream %s already running", stream.Code)
	}

	p := stream.Protocols
	if p.DASH && strings.TrimSpace(s.cfg.DASH.Dir) == "" {
		return fmt.Errorf("publisher: dash.dir is required when DASH output is enabled")
	}
	if p.HLS && p.DASH {
		h := filepath.Clean(s.cfg.HLS.Dir)
		d := filepath.Clean(strings.TrimSpace(s.cfg.DASH.Dir))
		if h == d {
			return fmt.Errorf("publisher: hls.dir and dash.dir must differ when both are enabled (both are %q)", h)
		}
	}

	workerCtx, cancel := context.WithCancel(ctx)
	s.workers[stream.Code] = cancel
	s.streamWorkCtx[stream.Code] = workerCtx

	s.spawnOutputs(workerCtx, stream.Code, stream.Push, p)

	return nil
}

func (s *Service) spawnOutputs(
	workerCtx context.Context,
	code domain.StreamCode,
	push []domain.PushDestination,
	p domain.OutputProtocols,
) {
	if p.HLS {
		go s.serveHLS(workerCtx, code)
	}
	if p.DASH {
		go s.serveDASH(workerCtx, code)
	}

	if p.RTSP {
		go s.serveRTSP(workerCtx, code)
	}
	if p.RTMP {
		go s.serveRTMP(workerCtx, code)
	}
	if p.SRT {
		go s.serveSRT(workerCtx, code)
	}
	if p.RTS {
		go s.serveRTS(workerCtx, code)
	}
	for _, dest := range push {
		if dest.Enabled {
			go s.pushToDestination(workerCtx, code, dest)
		}
	}
}

// Stop cancels all output workers for a stream.
func (s *Service) Stop(streamID domain.StreamCode) {
	s.mu.Lock()
	if cancel, ok := s.workers[streamID]; ok {
		cancel()
		delete(s.workers, streamID)
	}
	delete(s.streamWorkCtx, streamID)
	s.mu.Unlock()
}
