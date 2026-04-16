package publisher

// serve_srt.go — SRT play server (listener mode, publisher-side).
//
// Architecture:
//
//	Buffer Hub → subscriber → tsmux.FeedWirePacket → DrainTS188Aligned → srt.Conn.Write
//	                                                                            │
//	                                                                       SRT client
//
// SRT is a transport protocol — no codec conversion needed; raw MPEG-TS is
// written directly to each subscribing connection.
//
// Client URL: srt://host:port?streamid=live/<stream_code>
// The bare <stream_code> (without "live/" prefix) is also accepted.
//
// Each client gets its own goroutine and an independent Buffer Hub subscriber.
// When the server context is cancelled, all active subscriber connections are
// closed via a per-connection watcher goroutine.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	srt "github.com/datarhei/gosrt"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
)

// RunSRTPlayServer starts the SRT play listener.
// Returns nil immediately when publisher.srt.port is 0 (disabled).
func (s *Service) RunSRTPlayServer(ctx context.Context) error {
	port := s.cfg.SRT.Port
	if port == 0 {
		return nil
	}
	host := s.cfg.SRT.ListenHost
	if host == "" {
		host = "0.0.0.0"
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	latency := time.Duration(s.cfg.SRT.LatencyMS) * time.Millisecond
	if latency <= 0 {
		latency = 120 * time.Millisecond
	}
	cfg := srt.DefaultConfig()
	cfg.ReceiverLatency = latency
	cfg.PeerLatency = latency

	srv := &srt.Server{
		Addr:   addr,
		Config: &cfg,
		HandleConnect: func(req srt.ConnRequest) srt.ConnType {
			return s.srtHandleConnect(req)
		},
		HandleSubscribe: func(conn srt.Conn) {
			s.srtHandleSubscribe(ctx, conn)
		},
	}

	if err := srv.Listen(); err != nil {
		return fmt.Errorf("publisher srt: listen %q: %w", addr, err)
	}

	slog.Info("publisher: SRT play server listening", "addr", addr)

	go func() {
		<-ctx.Done()
		srv.Shutdown()
	}()

	if err := srv.Serve(); err != nil && err != srt.ErrServerClosed {
		return fmt.Errorf("publisher srt: serve: %w", err)
	}
	return nil
}

// srtHandleConnect validates an incoming SRT connection request.
// Returns SUBSCRIBE if the requested stream is active, REJECT otherwise.
func (s *Service) srtHandleConnect(req srt.ConnRequest) srt.ConnType {
	code := srtStreamCode(req.StreamId())
	if code == "" {
		slog.Warn("publisher: SRT connect rejected: empty streamid")
		return srt.REJECT
	}

	_, ok := s.mediaBufferFor(domain.StreamCode(code))
	if !ok {
		slog.Warn("publisher: SRT connect rejected: stream not active", "streamid", req.StreamId())
		return srt.REJECT
	}
	return srt.SUBSCRIBE
}

// srtHandleSubscribe serves raw MPEG-TS to one SRT subscriber.
// It is called in a goroutine by gosrt's Serve loop.
func (s *Service) srtHandleSubscribe(ctx context.Context, conn srt.Conn) {
	defer func() { _ = conn.Close() }()

	code := srtStreamCode(conn.StreamId())
	if code == "" {
		return
	}
	streamCode := domain.StreamCode(code)

	bufID, ok2 := s.mediaBufferFor(streamCode)
	if !ok2 {
		slog.Warn("publisher: SRT stream no longer active", "stream_code", streamCode)
		return
	}

	sub, err := s.buf.Subscribe(bufID)
	if err != nil {
		slog.Warn("publisher: SRT subscribe failed", "stream_code", streamCode, "err", err)
		return
	}
	defer s.buf.Unsubscribe(bufID, sub)

	slog.Info("publisher: SRT play session started", "stream_code", streamCode, "remote", conn.RemoteAddr())
	defer slog.Info("publisher: SRT play session ended", "stream_code", streamCode)

	// Close the connection when the server context is cancelled so the write
	// loop below unblocks.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-watchDone:
		}
	}()

	var tsCarry []byte
	var avMux *tsmux.FromAV

	for {
		select {
		case <-ctx.Done():
			return
		case pkt, ok := <-sub.Recv():
			if !ok {
				return
			}
			if pkt.AV != nil && pkt.AV.Discontinuity {
				avMux = nil
				tsCarry = nil
			}
			var writeErr error
			tsmux.FeedWirePacket(pkt.TS, pkt.AV, &avMux, func(b []byte) {
				if writeErr != nil {
					return
				}
				tsmux.DrainTS188Aligned(&tsCarry, b, func(aligned []byte) {
					if writeErr != nil {
						return
					}
					if _, err := conn.Write(aligned); err != nil {
						writeErr = err
					}
				})
			})
			if writeErr != nil {
				slog.Debug("publisher: SRT write error", "stream_code", streamCode, "err", writeErr)
				return
			}
		}
	}
}

// srtStreamCode extracts the stream code from an SRT streamid.
// Accepts "live/<code>" or bare "<code>".
func srtStreamCode(streamid string) string {
	code := strings.TrimPrefix(streamid, "live/")
	return strings.TrimSpace(code)
}
