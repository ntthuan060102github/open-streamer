package publisher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	srt "github.com/datarhei/gosrt"

	"github.com/open-streamer/open-streamer/internal/domain"
)

func (s *Service) srtListenPort() int {
	p := s.cfg.SRT.PortMin
	if p <= 0 {
		return 10000
	}
	return p
}

// serveSRT exposes MPEG-TS from the buffer hub on one shared SRT listener; clients set streamid=live/<stream_code>.
func (s *Service) serveSRT(ctx context.Context, streamID domain.StreamCode) {
	srtCfg, err := buildPublisherSRTConfig(s.cfg.SRT.LatencyMS)
	if err != nil {
		slog.Error("publisher: SRT config invalid", "stream_code", streamID, "err", err)
		return
	}

	s.srtMu.Lock()
	if s.srtLn == nil {
		addr := srtPublisherBindAddr(s.cfg.SRT.ListenHost, s.srtListenPort())
		ln, lerr := srt.Listen("srt", addr, srtCfg)
		if lerr != nil {
			s.srtMu.Unlock()
			slog.Error("publisher: SRT listen failed", "stream_code", streamID, "addr", addr, "err", lerr)
			return
		}
		s.srtLn = ln
		host := trimListenHost(s.cfg.SRT.ListenHost)
		if host == defaultListenHost || host == "" {
			host = "<this-host>"
		}
		slog.Info("publisher: SRT shared listener (gosrt)",
			"bind_addr", addr,
			"client_hint_pattern", fmt.Sprintf("srt://%s:%d?mode=caller&transtype=live&latency=<ms>&streamid=live/<stream_code>",
				host, s.srtListenPort()),
		)
		go s.srtAcceptLoop(ln)
	}
	s.srtActive[streamID] = struct{}{}
	s.srtMu.Unlock()

	host := trimListenHost(s.cfg.SRT.ListenHost)
	if host == defaultListenHost || host == "" {
		host = "127.0.0.1"
	}
	slog.Info("publisher: SRT stream mount",
		"stream_code", streamID,
		"client_hint", srtCallerURL(host, s.srtListenPort(), s.cfg.SRT.LatencyMS, streamID),
	)

	<-ctx.Done()

	s.srtMu.Lock()
	delete(s.srtActive, streamID)
	shouldClose := len(s.srtActive) == 0 && s.srtLn != nil
	ln := s.srtLn
	if shouldClose {
		s.srtLn = nil
	}
	s.srtMu.Unlock()

	if shouldClose && ln != nil {
		ln.Close()
	}
}

func (s *Service) srtAcceptLoop(ln srt.Listener) {
	for {
		req, accErr := ln.Accept2()
		if accErr != nil {
			if errors.Is(accErr, srt.ErrListenerClosed) {
				return
			}
			s.srtMu.Lock()
			stopped := s.srtLn == nil
			s.srtMu.Unlock()
			if stopped {
				return
			}
			slog.Warn("publisher: SRT accept error", "err", accErr)
			continue
		}
		go s.handleSRTConnRequest(req)
	}
}

func (s *Service) handleSRTConnRequest(req srt.ConnRequest) {
	code, perr := streamCodeFromSRTStreamID(req.StreamId())
	if perr != nil {
		req.Reject(srt.REJX_BAD_REQUEST)
		return
	}
	s.srtMu.Lock()
	_, allowed := s.srtActive[code]
	s.srtMu.Unlock()
	if !allowed {
		req.Reject(srt.REJX_NOTFOUND)
		return
	}
	conn, aerr := req.Accept()
	if aerr != nil {
		slog.Warn("publisher: SRT handshake failed", "stream_code", code, "err", aerr)
		return
	}
	s.mu.Lock()
	wctx, ok := s.streamWorkCtx[code]
	s.mu.Unlock()
	if !ok {
		_ = conn.Close()
		return
	}
	s.runSRTSubscriber(wctx, code, conn)
}

func (s *Service) runSRTSubscriber(ctx context.Context, streamID domain.StreamCode, conn srt.Conn) {
	defer func() {
		_ = conn.Close()
	}()

	sub, err := s.buf.Subscribe(streamID)
	if err != nil {
		slog.Error("publisher: SRT subscriber buffer subscribe failed", "stream_code", streamID, "err", err)
		return
	}
	defer s.buf.Unsubscribe(streamID, sub)

	slog.Info("publisher: SRT playback client connected",
		"stream_code", streamID,
		"remote", conn.RemoteAddr(),
	)
	defer slog.Info("publisher: SRT playback client disconnected", "stream_code", streamID)

	for {
		select {
		case <-ctx.Done():
			return
		case pkt, ok := <-sub.Recv():
			if !ok {
				return
			}
			if _, werr := conn.Write(pkt); werr != nil {
				return
			}
		}
	}
}

func srtPublisherBindAddr(listenHost string, port int) string {
	h := trimListenHost(listenHost)
	if h == defaultListenHost || h == "" {
		return ":" + strconv.Itoa(port)
	}
	return net.JoinHostPort(h, strconv.Itoa(port))
}

func buildPublisherSRTConfig(latencyMS int) (srt.Config, error) {
	cfg := srt.DefaultConfig()
	if latencyMS > 0 {
		d := time.Duration(latencyMS) * time.Millisecond
		cfg.Latency = d
	}
	if err := cfg.Validate(); err != nil {
		return srt.Config{}, err
	}
	return cfg, nil
}
