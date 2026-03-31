// Package push contains push-mode server implementations.
// Each server listens on a port and routes incoming connections to the
// correct stream's Buffer Hub slot via the Registry interface.
package push

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	gocodec "github.com/yapingcat/gomedia/go-codec"
	gompeg2 "github.com/yapingcat/gomedia/go-mpeg2"
	gortmp "github.com/yapingcat/gomedia/go-rtmp"

	"github.com/open-streamer/open-streamer/config"
	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
)

// Registry maps a stream key to the stream's buffer hub slot.
// Implemented by ingestor.Registry and injected at construction time.
type Registry interface {
	Lookup(key string) (domain.StreamCode, *buffer.Service, error)
}

// RTMPServer is a single-port RTMP push server.
// External encoders (OBS, hardware encoders) connect to:
//
//	rtmp://<host>:<port>/<app>/<stream_key>
//
// The incoming FLV data is converted to MPEG-TS natively via
// yapingcat/gomedia — no external process is spawned.
type RTMPServer struct {
	cfg      config.IngestorConfig
	registry Registry
	listener net.Listener
}

// NewRTMPServer constructs an RTMPServer.
func NewRTMPServer(cfg config.IngestorConfig, registry Registry) *RTMPServer {
	return &RTMPServer{cfg: cfg, registry: registry}
}

// ListenAndServe starts listening and blocks until ctx is cancelled.
func (s *RTMPServer) ListenAndServe(ctx context.Context) error {
	addr := s.cfg.RTMPAddr
	if addr == "" {
		addr = ":1935"
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("rtmp server: listen %s: %w", addr, err)
	}
	s.listener = ln
	slog.Info("rtmp server: listening", "addr", addr)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Error("rtmp server: accept error", "err", err)
			continue
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *RTMPServer) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	session := &rtmpSession{
		conn:     conn,
		registry: s.registry,
		serverCtx: ctx,
	}
	session.run()
}

// rtmpSession manages one RTMP connection using gomedia's RtmpServerHandle.
type rtmpSession struct {
	conn      net.Conn
	registry  Registry
	serverCtx context.Context

	handle   *gortmp.RtmpServerHandle
	mux      *gompeg2.TSMuxer
	mu       sync.Mutex
	videoPid uint16
	audioPid uint16
	hasVideo bool
	hasAudio bool
	streamID domain.StreamCode
	buf      *buffer.Service
}

func (s *rtmpSession) run() {
	s.mux = gompeg2.NewTSMuxer()
	s.mux.OnPacket = func(pkg []byte) {
		if s.buf == nil {
			return
		}
		pkt := make([]byte, len(pkg))
		copy(pkt, pkg)
		if err := s.buf.Write(s.streamID, buffer.Packet(pkt)); err != nil {
			slog.Error("rtmp: buffer write failed", "stream_code", s.streamID, "err", err)
		}
	}

	s.handle = gortmp.NewRtmpServerHandle()
	s.handle.SetOutput(func(b []byte) error {
		_, err := s.conn.Write(b)
		return err
	})

	s.handle.OnPublish(func(app, streamName string) gortmp.StatusCode {
		streamID, buf, err := s.registry.Lookup(streamName)
		if err != nil {
			slog.Warn("rtmp: rejected unknown stream key", "key", streamName)
			return gortmp.NETCONNECT_CONNECT_REJECTED
		}
		s.mu.Lock()
		s.streamID = streamID
		s.buf = buf
		s.mu.Unlock()
		slog.Info("rtmp: publisher connected", "stream_key", streamName, "stream_code", streamID)
		return gortmp.NETSTREAM_PUBLISH_START
	})

	s.handle.OnFrame(func(cid gocodec.CodecID, pts, dts uint32, frame []byte) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch cid {
		case gocodec.CODECID_VIDEO_H264:
			if !s.hasVideo {
				s.videoPid = s.mux.AddStream(gompeg2.TS_STREAM_H264)
				s.hasVideo = true
			}
			s.mux.Write(s.videoPid, frame, uint64(pts), uint64(dts)) //nolint:errcheck
		case gocodec.CODECID_VIDEO_H265:
			if !s.hasVideo {
				s.videoPid = s.mux.AddStream(gompeg2.TS_STREAM_H265)
				s.hasVideo = true
			}
			s.mux.Write(s.videoPid, frame, uint64(pts), uint64(dts)) //nolint:errcheck
		case gocodec.CODECID_AUDIO_AAC:
			if !s.hasAudio {
				s.audioPid = s.mux.AddStream(gompeg2.TS_STREAM_AAC)
				s.hasAudio = true
			}
			s.mux.Write(s.audioPid, frame, uint64(pts), uint64(pts)) //nolint:errcheck
		}
	})

	defer func() {
		s.mu.Lock()
		if s.buf != nil {
			slog.Info("rtmp: publisher disconnected", "stream_id", s.streamID)
		}
		s.mu.Unlock()
	}()

	buf := make([]byte, 65536)
	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			return
		}
		if err := s.handle.Input(buf[:n]); err != nil {
			slog.Warn("rtmp: input error", "err", err)
			return
		}
	}
}
