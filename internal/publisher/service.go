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
	"path/filepath"
	"strings"
	"sync"

	"github.com/bluenviron/gortsplib/v5"

	"github.com/ntthuan060102github/open-streamer/config"
	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/events"
	"github.com/samber/do/v2"
)

// Compile-time assertion: rtspHandler implements the ServerHandler interfaces it claims.
var (
	_ gortsplib.ServerHandlerOnDescribe = (*rtspHandler)(nil)
	_ gortsplib.ServerHandlerOnSetup    = (*rtspHandler)(nil)
)

// Service manages all output workers for active streams.
type Service struct {
	cfg        config.PublisherConfig
	buf        *buffer.Service
	bus        events.Bus
	ffmpegPath string
	// hlsFailoverGen: incremented on each input.failover so every ABR variant segmenter can tag
	// exactly one EXT-X-DISCONTINUITY on its next flush (bool map would only let the first variant win).
	hlsFailoverMu  sync.Mutex
	hlsFailoverGen map[domain.StreamCode]uint64
	mu             sync.Mutex
	workers        map[domain.StreamCode]context.CancelFunc
	// streamWorkCtx is the per-stream publisher worker context (same lifetime as workers).
	streamWorkCtx map[domain.StreamCode]context.Context
	// mediaBuffer is the Buffer Hub id for single-rendition outputs (RTSP, RTMP, SRT, push, DVR via API).
	// When transcoding produces an ABR ladder, this is the best rung ($r$...), not the logical stream code.
	mediaBuffer map[domain.StreamCode]domain.StreamCode

	rtspMounts   map[string]*gortsplib.ServerStream // path -> stream
	rtspSrv      *gortsplib.Server                  // set by RunRTSPPlayServer
	rtspSrvReady chan struct{}                      // closed when server is ready or disabled
	rtmpActive   map[domain.StreamCode]struct{}
	srtActive    map[domain.StreamCode]struct{}
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[*config.Config](i)
	buf := do.MustInvoke[*buffer.Service](i)
	bus := do.MustInvoke[events.Bus](i)
	pub := &cfg.Publisher

	ffmpegPath := cfg.Transcoder.FFmpegPath
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}

	svc := &Service{
		cfg:            *pub,
		buf:            buf,
		bus:            bus,
		ffmpegPath:     ffmpegPath,
		hlsFailoverGen: make(map[domain.StreamCode]uint64),
		workers:        make(map[domain.StreamCode]context.CancelFunc),
		streamWorkCtx:  make(map[domain.StreamCode]context.Context),
		mediaBuffer:    make(map[domain.StreamCode]domain.StreamCode),
		rtspMounts:     make(map[string]*gortsplib.ServerStream),
		rtspSrvReady:   make(chan struct{}),
		rtmpActive:     make(map[domain.StreamCode]struct{}),
		srtActive:      make(map[domain.StreamCode]struct{}),
	}
	bus.Subscribe(domain.EventInputFailover, func(_ context.Context, e domain.Event) error {
		svc.hlsFailoverMu.Lock()
		svc.hlsFailoverGen[e.StreamCode]++
		svc.hlsFailoverMu.Unlock()
		return nil
	})
	return svc, nil
}

// hlsFailoverGenSnapshot returns the current failover generation for streamID (for segmenter local state).
func (s *Service) hlsFailoverGenSnapshot(streamID domain.StreamCode) uint64 {
	s.hlsFailoverMu.Lock()
	defer s.hlsFailoverMu.Unlock()
	return s.hlsFailoverGen[streamID]
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
	s.mediaBuffer[stream.Code] = buffer.PlaybackBufferID(stream.Code, stream.Transcoder)

	s.spawnOutputs(workerCtx, stream, p)

	return nil
}

func (s *Service) spawnOutputs(
	workerCtx context.Context,
	stream *domain.Stream,
	p domain.OutputProtocols,
) {
	code := stream.Code
	if p.HLS {
		if publisherABRActive(stream) {
			go s.serveHLSAdaptive(workerCtx, stream)
		} else {
			go s.serveHLS(workerCtx, s.mediaBufferForLocked(code))
		}
	}
	if p.DASH {
		if publisherABRActive(stream) {
			go s.serveDASHAdaptive(workerCtx, stream)
		} else {
			go s.serveDASH(workerCtx, s.mediaBufferForLocked(code))
		}
	}

	mediaBuf := s.mediaBufferForLocked(code)

	if p.RTSP {
		go s.serveRTSP(workerCtx, code, mediaBuf)
	}

	for _, dest := range stream.Push {
		if !dest.Enabled {
			continue
		}
		d := dest // capture for goroutine
		go s.serveRTMPPush(workerCtx, code, mediaBuf, d)
	}
}

func publisherABRActive(stream *domain.Stream) bool {
	if stream == nil {
		return false
	}
	return len(buffer.RenditionsForTranscoder(stream.Code, stream.Transcoder)) > 0
}

// mediaBufferForLocked returns the buffer id for MPEG-TS playout; caller must hold s.mu.
func (s *Service) mediaBufferForLocked(code domain.StreamCode) domain.StreamCode {
	if id, ok := s.mediaBuffer[code]; ok {
		return id
	}
	return code
}

// mediaBufferFor returns the buffer id for logical stream code (RTMP play path, etc.).
func (s *Service) mediaBufferFor(code domain.StreamCode) domain.StreamCode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mediaBufferForLocked(code)
}

// Stop cancels all output workers for a stream.
func (s *Service) Stop(streamID domain.StreamCode) {
	s.mu.Lock()
	if cancel, ok := s.workers[streamID]; ok {
		cancel()
		delete(s.workers, streamID)
	}
	delete(s.streamWorkCtx, streamID)
	delete(s.mediaBuffer, streamID)
	s.mu.Unlock()

	s.hlsFailoverMu.Lock()
	delete(s.hlsFailoverGen, streamID)
	s.hlsFailoverMu.Unlock()
}
