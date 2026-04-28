// Package publisher delivers transcoded streams to all outputs.
//
// It handles two distinct output types:
//   - Serve endpoints (HLS, DASH, RTSP, RTMP listen, SRT listen): the server listens,
//     clients connect and pull. HLS uses MPEG-TS segments + m3u8; DASH uses fMP4 (init + .m4s) + dynamic MPD (Eyevinn mp4ff); RTSP/RTMP/SRT use gortsplib/gomedia/gosrt — no FFmpeg in this package.
//     RTSP, RTMP play, and SRT listen each use one shared TCP/UDP port (configured under listeners.{rtsp,rtmp,srt}.port — same port serves both ingest and play); streams are selected by path (/live/<code>), RTMP app "live", or SRT streamid (live/<code>).
//   - Push destinations: rtmp:// (plain TCP) and rtmps:// (TLS, default port 443) via gomedia go-rtmp client; other schemes return a clear error.
package publisher

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/do/v2"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/events"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/ntt0601zcoder/open-streamer/internal/sessions"
)

// Compile-time assertion: rtspHandler implements the ServerHandler interfaces it claims.
var (
	_ gortsplib.ServerHandlerOnDescribe     = (*rtspHandler)(nil)
	_ gortsplib.ServerHandlerOnSetup        = (*rtspHandler)(nil)
	_ gortsplib.ServerHandlerOnPlay         = (*rtspHandler)(nil)
	_ gortsplib.ServerHandlerOnSessionClose = (*rtspHandler)(nil)
)

// ABRRepMeta carries updated metadata for one ABR rendition used in in-place master
// playlist rewrites (e.g. after a profile bitrate/resolution change without an ABR
// ladder structure change).
type ABRRepMeta struct {
	Slug   string
	BwBps  int
	Width  int
	Height int
}

// streamState holds per-stream publisher lifecycle state.
// Each output protocol has its own context so it can be stopped independently.
type streamState struct {
	baseCtx    context.Context
	baseCancel context.CancelFunc

	// code is the StreamCode this state belongs to — duplicated here so log lines
	// emitted by spawn/stop helpers can include it without an extra map lookup.
	code domain.StreamCode

	// mediaBuf is the Buffer Hub ID for single-rendition outputs (RTSP, RTMP push, SRT).
	// When ABR is active this is the best rendition; otherwise it is the stream code.
	mediaBuf domain.StreamCode

	// wg tracks all protocol goroutines so Stop can wait for them to finish
	// before cleaning up on-disk segments.
	wg sync.WaitGroup

	mu sync.Mutex
	// protocols maps per-protocol keys ("hls", "dash", "rtsp", "push:<url>") to their
	// cancel funcs. Cancelling one key stops only that output goroutine.
	protocols map[string]context.CancelFunc

	// hlsMaster is set by serveHLSAdaptive so UpdateABRMasterMeta can push in-place
	// metadata updates to the running master playlist writer.
	hlsMaster *hlsABRMaster
}

// Service manages all output workers for active streams.
type Service struct {
	cfg        config.PublisherConfig
	listeners  config.ListenersConfig
	buf        *buffer.Service
	bus        events.Bus
	tracker    sessions.Tracker
	m          *metrics.Metrics
	ffmpegPath string

	// hlsFailoverGen: incremented on each input.failover so every ABR variant segmenter
	// can tag exactly one EXT-X-DISCONTINUITY on its next flush.
	hlsFailoverMu  sync.Mutex
	hlsFailoverGen map[domain.StreamCode]uint64

	mu      sync.Mutex
	streams map[domain.StreamCode]*streamState

	// mediaBuffer mirrors streamState.mediaBuf for fast lookup from RTMP/SRT play
	// handlers that hold s.mu but cannot access streamState fields directly.
	mediaBuffer map[domain.StreamCode]domain.StreamCode

	rtspMounts   map[string]*gortsplib.ServerStream // path -> stream
	rtspSrv      *gortsplib.Server                  // set by RunRTSPPlayServer
	rtspSrvReady chan struct{}                      // closed when server is ready or disabled
	rtmpActive   map[domain.StreamCode]struct{}
	srtActive    map[domain.StreamCode]struct{}

	// rtspSessions maps each gortsplib *ServerSession to its tracker session
	// adapter. Populated in OnPlay, drained in OnSessionClose. Distinct mutex
	// because RTSP session callbacks fire from gortsplib's worker goroutines
	// and must not contend on the broader s.mu used during stream lifecycle.
	rtspSessionsMu sync.Mutex
	rtspSessions   map[*gortsplib.ServerSession]*playSession

	// pushStates is the per-(stream, url) push destination runtime state used
	// by RuntimeStatus. Updated by serveRTMPPush at session boundaries.
	// Separate mutex from s.mu to avoid blocking output orchestration when
	// the API polls runtime status.
	pushMu     sync.Mutex
	pushStates map[domain.StreamCode]map[string]*pushState
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	pub := do.MustInvoke[config.PublisherConfig](i)
	listeners := do.MustInvoke[config.ListenersConfig](i)
	tc := do.MustInvoke[config.TranscoderConfig](i)
	buf := do.MustInvoke[*buffer.Service](i)
	bus := do.MustInvoke[events.Bus](i)

	// Sessions tracker is optional — if no provider is registered, the
	// publisher silently skips tracking. Lets the feature ship without
	// forcing every test harness to wire it.
	var tracker sessions.Tracker
	if t, err := do.Invoke[*sessions.Service](i); err == nil {
		tracker = t
	}

	// Metrics is also optional so unit tests can wire only the publisher.
	var m *metrics.Metrics
	if mm, err := do.Invoke[*metrics.Metrics](i); err == nil {
		m = mm
	}

	ffmpegPath := tc.FFmpegPath
	if ffmpegPath == "" {
		ffmpegPath = domain.DefaultFFmpegPath
	}

	svc := &Service{
		cfg:            pub,
		listeners:      listeners,
		buf:            buf,
		bus:            bus,
		tracker:        tracker,
		m:              m,
		ffmpegPath:     ffmpegPath,
		hlsFailoverGen: make(map[domain.StreamCode]uint64),
		streams:        make(map[domain.StreamCode]*streamState),
		mediaBuffer:    make(map[domain.StreamCode]domain.StreamCode),
		rtspMounts:     make(map[string]*gortsplib.ServerStream),
		rtspSrvReady:   make(chan struct{}),
		rtmpActive:     make(map[domain.StreamCode]struct{}),
		srtActive:      make(map[domain.StreamCode]struct{}),
		pushStates:     make(map[domain.StreamCode]map[string]*pushState),
		rtspSessions:   make(map[*gortsplib.ServerSession]*playSession),
	}
	bus.Subscribe(domain.EventInputFailover, func(_ context.Context, e domain.Event) error {
		svc.hlsFailoverMu.Lock()
		svc.hlsFailoverGen[e.StreamCode]++
		svc.hlsFailoverMu.Unlock()
		return nil
	})

	svc.cleanupAllOutputDirs()

	return svc, nil
}

// NewServiceForTesting creates a Service without DI, for use in integration tests.
func NewServiceForTesting(cfg config.PublisherConfig, buf *buffer.Service, bus events.Bus) *Service {
	return &Service{
		cfg:            cfg,
		buf:            buf,
		bus:            bus,
		ffmpegPath:     "ffmpeg",
		hlsFailoverGen: make(map[domain.StreamCode]uint64),
		streams:        make(map[domain.StreamCode]*streamState),
		mediaBuffer:    make(map[domain.StreamCode]domain.StreamCode),
		rtspMounts:     make(map[string]*gortsplib.ServerStream),
		rtspSrvReady:   make(chan struct{}),
		rtmpActive:     make(map[domain.StreamCode]struct{}),
		srtActive:      make(map[domain.StreamCode]struct{}),
		pushStates:     make(map[domain.StreamCode]map[string]*pushState),
		rtspSessions:   make(map[*gortsplib.ServerSession]*playSession),
	}
}

// SetListenersForTesting overrides the shared listeners config without going
// through DI. Use only from tests that exercise RunRTSPPlayServer / RunSRTPlayServer.
func (s *Service) SetListenersForTesting(l config.ListenersConfig) {
	s.listeners = l
}

// cleanupAllOutputDirs wipes the HLS and DASH root directories on startup so
// stale segments from a previous run are never served to clients.
func (s *Service) cleanupAllOutputDirs() {
	for _, dir := range []string{
		strings.TrimSpace(s.cfg.HLS.Dir),
		strings.TrimSpace(s.cfg.DASH.Dir),
	} {
		if dir == "" {
			continue
		}
		if err := resetOutputDir(dir); err != nil {
			slog.Warn("publisher: startup cleanup failed", "dir", dir, "err", err)
		}
	}
}

// hlsFailoverGenSnapshot returns the current failover generation for streamID.
func (s *Service) hlsFailoverGenSnapshot(streamID domain.StreamCode) uint64 {
	s.hlsFailoverMu.Lock()
	defer s.hlsFailoverMu.Unlock()
	return s.hlsFailoverGen[streamID]
}

// Start launches all serve-endpoints and push-destination workers for a stream.
func (s *Service) Start(ctx context.Context, stream *domain.Stream) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.streams[stream.Code]; ok {
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

	baseCtx, baseCancel := context.WithCancel(ctx)
	ss := &streamState{
		baseCtx:    baseCtx,
		baseCancel: baseCancel,
		code:       stream.Code,
		mediaBuf:   buffer.PlaybackBufferID(stream.Code, stream.Transcoder),
		protocols:  make(map[string]context.CancelFunc),
	}
	s.streams[stream.Code] = ss
	s.mediaBuffer[stream.Code] = ss.mediaBuf

	//nolint:contextcheck // goroutines derive from ss.baseCtx (stream lifetime), not request ctx; by design
	s.spawnOutputsLocked(ss, stream, p)

	return nil
}

// spawnOutputsLocked launches goroutines for each enabled protocol.
// Caller must hold s.mu.
func (s *Service) spawnOutputsLocked(ss *streamState, stream *domain.Stream, p domain.OutputProtocols) {
	if p.HLS {
		s.spawnProtocolLocked(ss, "hls", s.hlsFunc(ss, stream))
	}
	if p.DASH {
		s.spawnProtocolLocked(ss, "dash", s.dashFunc(stream))
	}
	if p.RTSP {
		mediaBuf := ss.mediaBuf
		code := stream.Code
		s.spawnProtocolLocked(ss, "rtsp", func(ctx context.Context) {
			s.serveRTSP(ctx, code, mediaBuf)
		})
	}
	for _, dest := range stream.Push {
		if !dest.Enabled {
			continue
		}
		d := dest
		mediaBuf := ss.mediaBuf
		code := stream.Code
		s.spawnProtocolLocked(ss, "push:"+d.URL, func(ctx context.Context) {
			s.serveRTMPPush(ctx, code, mediaBuf, d)
		})
	}
}

// spawnProtocolLocked starts fn in a new goroutine with a child context derived from
// ss.baseCtx. If a goroutine with the same key is already running, it is cancelled first.
// Caller must hold s.mu (which serialises modifications to ss.protocols).
func (s *Service) spawnProtocolLocked(ss *streamState, key string, fn func(context.Context)) {
	ss.mu.Lock()
	respawn := false
	if cancel, ok := ss.protocols[key]; ok {
		cancel()
		respawn = true
	}
	ctx, cancel := context.WithCancel(ss.baseCtx)
	ss.protocols[key] = cancel
	ss.mu.Unlock()

	slog.Info("publisher: protocol started",
		"stream_code", ss.code, "protocol", key, "respawn", respawn)

	ss.wg.Add(1)
	go func() {
		defer ss.wg.Done()
		fn(ctx)
		slog.Info("publisher: protocol exited",
			"stream_code", ss.code, "protocol", key)
	}()
}

// stopProtocol cancels the goroutine for a single protocol key without affecting others.
func (s *Service) stopProtocol(streamID domain.StreamCode, key string) {
	s.mu.Lock()
	ss := s.streams[streamID]
	s.mu.Unlock()
	if ss == nil {
		return
	}
	ss.mu.Lock()
	stopped := false
	if cancel, ok := ss.protocols[key]; ok {
		cancel()
		delete(ss.protocols, key)
		stopped = true
	}
	ss.mu.Unlock()
	if stopped {
		slog.Info("publisher: protocol stopped",
			"stream_code", streamID, "protocol", key)
	}
}

// hlsFunc returns a closure that runs the HLS output for stream, wiring hlsMaster
// back into ss so UpdateABRMasterMeta can reach it.
func (s *Service) hlsFunc(ss *streamState, stream *domain.Stream) func(context.Context) {
	return func(ctx context.Context) {
		if publisherABRActive(stream) {
			s.serveHLSAdaptive(ctx, stream, ss)
		} else {
			// Single-rendition: mediaBuf is stable; read without lock.
			s.serveHLS(ctx, ss.mediaBuf)
		}
	}
}

// dashFunc returns a closure that runs the DASH output for stream.
func (s *Service) dashFunc(stream *domain.Stream) func(context.Context) {
	return func(ctx context.Context) {
		if publisherABRActive(stream) {
			s.serveDASHAdaptive(ctx, stream)
		} else {
			s.serveDASH(ctx, stream.Code)
		}
	}
}

// Stop cancels all output workers for a stream, waits for them to finish,
// and removes the on-disk segment directories (HLS/DASH).
func (s *Service) Stop(streamID domain.StreamCode) {
	s.mu.Lock()
	ss, ok := s.streams[streamID]
	if ok {
		ss.baseCancel()
		delete(s.streams, streamID)
		delete(s.mediaBuffer, streamID)
	}
	s.mu.Unlock()

	s.hlsFailoverMu.Lock()
	delete(s.hlsFailoverGen, streamID)
	s.hlsFailoverMu.Unlock()

	if ok {
		// Wait for all protocol goroutines to finish so no writer races with
		// the directory removal below.
		ss.wg.Wait()
		s.cleanupStreamDirs(streamID)
	}
}

// cleanupStreamDirs removes the HLS and DASH output directories for a stream.
func (s *Service) cleanupStreamDirs(streamID domain.StreamCode) {
	if dir := strings.TrimSpace(s.cfg.HLS.Dir); dir != "" {
		p := filepath.Join(dir, string(streamID))
		if err := os.RemoveAll(p); err != nil && !os.IsNotExist(err) {
			slog.Warn("publisher: cleanup HLS dir failed",
				"stream_code", streamID, "dir", p, "err", err)
		}
	}
	if dir := strings.TrimSpace(s.cfg.DASH.Dir); dir != "" {
		p := filepath.Join(dir, string(streamID))
		if err := os.RemoveAll(p); err != nil && !os.IsNotExist(err) {
			slog.Warn("publisher: cleanup DASH dir failed",
				"stream_code", streamID, "dir", p, "err", err)
		}
	}
}

// UpdateProtocols surgically stops/starts only the protocol goroutines that changed
// between old and new stream configs. Goroutines for unchanged protocols keep running.
// Call this when diff.ProtocolsChanged || diff.PushChanged.
func (s *Service) UpdateProtocols(ctx context.Context, old, new *domain.Stream) error {
	s.mu.Lock()
	ss, ok := s.streams[new.Code]
	if ok {
		newBuf := buffer.PlaybackBufferID(new.Code, new.Transcoder)
		if ss.mediaBuf != newBuf {
			ss.mediaBuf = newBuf
			s.mediaBuffer[new.Code] = newBuf
		}
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("publisher: stream %s not running", new.Code)
	}

	op, np := old.Protocols, new.Protocols

	// HLS: only act on OFF→ON or ON→OFF transitions.
	// HLS+DASH restarts due to ABR ladder changes are handled by RestartHLSDASH.
	if op.HLS && !np.HLS {
		s.stopProtocol(new.Code, "hls")
	} else if !op.HLS && np.HLS {
		s.mu.Lock()
		//nolint:contextcheck // goroutine derives from ss.baseCtx (stream lifetime); by design
		s.spawnProtocolLocked(ss, "hls", s.hlsFunc(ss, new))
		s.mu.Unlock()
	}

	// DASH
	if op.DASH && !np.DASH {
		s.stopProtocol(new.Code, "dash")
	} else if !op.DASH && np.DASH {
		s.mu.Lock()
		//nolint:contextcheck // goroutine derives from ss.baseCtx (stream lifetime); by design
		s.spawnProtocolLocked(ss, "dash", s.dashFunc(new))
		s.mu.Unlock()
	}

	// RTSP
	if op.RTSP && !np.RTSP {
		s.stopProtocol(new.Code, "rtsp")
	} else if !op.RTSP && np.RTSP {
		mediaBuf := ss.mediaBuf
		code := new.Code
		s.mu.Lock()
		//nolint:contextcheck // goroutine derives from ss.baseCtx (stream lifetime); by design
		s.spawnProtocolLocked(ss, "rtsp", func(ctx context.Context) {
			s.serveRTSP(ctx, code, mediaBuf)
		})
		s.mu.Unlock()
	}

	// Push destinations: diff by URL.
	//nolint:contextcheck // goroutines derive from ss.baseCtx (stream lifetime); by design
	s.updatePushDestinations(old.Push, new.Push, new.Code, ss)

	return nil
}

// updatePushDestinations stops removed push goroutines and starts added ones.
func (s *Service) updatePushDestinations(
	oldPush, newPush []domain.PushDestination,
	code domain.StreamCode,
	ss *streamState,
) {
	oldByURL := make(map[string]domain.PushDestination, len(oldPush))
	for _, d := range oldPush {
		oldByURL[d.URL] = d
	}
	newByURL := make(map[string]domain.PushDestination, len(newPush))
	for _, d := range newPush {
		newByURL[d.URL] = d
	}

	// Stop removed or disabled destinations.
	for _, od := range oldPush {
		nd, stillExists := newByURL[od.URL]
		if !stillExists || (!nd.Enabled && od.Enabled) {
			s.stopProtocol(code, "push:"+od.URL)
		}
	}

	// Start new or re-enabled destinations.
	for _, nd := range newPush {
		if !nd.Enabled {
			continue
		}
		od, existed := oldByURL[nd.URL]
		if !existed || (!od.Enabled && nd.Enabled) {
			d := nd
			mediaBuf := ss.mediaBuf
			s.mu.Lock()
			s.spawnProtocolLocked(ss, "push:"+d.URL, func(ctx context.Context) {
				s.serveRTMPPush(ctx, code, mediaBuf, d)
			})
			s.mu.Unlock()
		}
	}
}

// RestartHLSDASH stops and restarts only the HLS and DASH goroutines with the new
// stream config. Used when the ABR ladder count changes (profile added/removed) so the
// master playlist and per-shard segmenters reflect the new rendition set.
// RTSP, RTMP, and SRT goroutines are unaffected.
func (s *Service) RestartHLSDASH(ctx context.Context, stream *domain.Stream) error {
	s.mu.Lock()
	ss, ok := s.streams[stream.Code]
	if ok {
		newBuf := buffer.PlaybackBufferID(stream.Code, stream.Transcoder)
		ss.mediaBuf = newBuf
		s.mediaBuffer[stream.Code] = newBuf
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("publisher: stream %s not running", stream.Code)
	}

	if stream.Protocols.HLS {
		s.mu.Lock()
		//nolint:contextcheck // goroutine derives from ss.baseCtx (stream lifetime); by design
		s.spawnProtocolLocked(ss, "hls", s.hlsFunc(ss, stream))
		s.mu.Unlock()
	} else {
		s.stopProtocol(stream.Code, "hls")
	}
	if stream.Protocols.DASH {
		s.mu.Lock()
		//nolint:contextcheck // goroutine derives from ss.baseCtx (stream lifetime); by design
		s.spawnProtocolLocked(ss, "dash", s.dashFunc(stream))
		s.mu.Unlock()
	} else {
		s.stopProtocol(stream.Code, "dash")
	}
	return nil
}

// UpdateABRMasterMeta applies updated bandwidth/resolution metadata to the running HLS
// ABR master playlist writer. This avoids a full publisher restart when only a profile's
// bitrate or resolution changes (but the ladder count stays the same).
func (s *Service) UpdateABRMasterMeta(streamCode domain.StreamCode, updates []ABRRepMeta) {
	s.mu.Lock()
	ss, ok := s.streams[streamCode]
	s.mu.Unlock()
	if !ok {
		return
	}
	ss.mu.Lock()
	m := ss.hlsMaster
	ss.mu.Unlock()
	if m == nil {
		return
	}
	for _, u := range updates {
		m.SetRepOverride(u.Slug, u.BwBps, u.Width, u.Height)
	}
}

func publisherABRActive(stream *domain.Stream) bool {
	if stream == nil {
		return false
	}
	return len(buffer.RenditionsForTranscoder(stream.Code, stream.Transcoder)) > 0
}

// mediaBufferFor returns the buffer id for logical stream code (RTMP/SRT play handlers).
// Returns ("", false) when the stream is not active.
func (s *Service) mediaBufferFor(code domain.StreamCode) (domain.StreamCode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.mediaBuffer[code]
	return id, ok
}

// hlsSegCounter pre-binds the per-(stream, profile) HLS segment counter. The
// returned counter is nil-safe at the call site so segmenters work in tests
// that don't wire metrics.
func (s *Service) hlsSegCounter(streamID domain.StreamCode, profile string) prometheus.Counter {
	if s.m == nil {
		return nil
	}
	return s.m.PublisherSegmentsTotal.WithLabelValues(string(streamID), "hls", profile)
}

// dashSegCounter is the DASH counterpart to hlsSegCounter.
func (s *Service) dashSegCounter(streamID domain.StreamCode, profile string) prometheus.Counter {
	if s.m == nil {
		return nil
	}
	return s.m.PublisherSegmentsTotal.WithLabelValues(string(streamID), "dash", profile)
}

// pushStateGauge pre-binds the per-(stream, dest_url) push state gauge.
// Nil-safe — push workers tolerate a nil gauge in tests.
func (s *Service) pushStateGauge(streamID domain.StreamCode, destURL string) prometheus.Gauge {
	if s.m == nil {
		return nil
	}
	return s.m.PublisherPushState.WithLabelValues(string(streamID), destURL)
}
