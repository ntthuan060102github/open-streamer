package publisher

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"

	"github.com/yapingcat/gomedia/go-codec"
	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"
	rtmpmedia "github.com/yapingcat/gomedia/go-rtmp"

	"github.com/open-streamer/open-streamer/internal/domain"
)

func (s *Service) rtmpListenPort() int {
	p := s.cfg.RTMP.PortMin
	if p <= 0 {
		return 1936
	}
	return p
}

func (s *Service) serveRTMP(ctx context.Context, streamID domain.StreamCode) {
	s.rtmpMu.Lock()
	if s.rtmpLn == nil {
		host := trimListenHost(s.cfg.RTMP.ListenHost)
		port := s.rtmpListenPort()
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		lc := net.ListenConfig{}
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			s.rtmpMu.Unlock()
			slog.Error("publisher: RTMP listen failed", "stream_code", streamID, "addr", addr, "err", err)
			return
		}
		s.rtmpLn = ln
		slog.Info("publisher: RTMP shared listener (gomedia play)",
			"listen_addr", addr,
			"client_hint", fmt.Sprintf("rtmp://<this-host>:%d/live/<stream_code>", port),
		)
		go s.rtmpAcceptLoop(ln)
	}
	s.rtmpActive[streamID] = struct{}{}
	s.rtmpMu.Unlock()

	<-ctx.Done()

	s.rtmpMu.Lock()
	delete(s.rtmpActive, streamID)
	shouldClose := len(s.rtmpActive) == 0 && s.rtmpLn != nil
	ln := s.rtmpLn
	if shouldClose {
		s.rtmpLn = nil
	}
	s.rtmpMu.Unlock()

	if shouldClose && ln != nil {
		_ = ln.Close()
	}
}

func (s *Service) rtmpAcceptLoop(ln net.Listener) {
	for {
		conn, aerr := ln.Accept()
		if aerr != nil {
			s.rtmpMu.Lock()
			stopped := s.rtmpLn == nil
			s.rtmpMu.Unlock()
			if stopped {
				return
			}
			slog.Warn("publisher: RTMP accept error", "err", aerr)
			continue
		}
		go s.handleRTMPPlayConn(conn)
	}
}

func (s *Service) handleRTMPPlayConn(conn net.Conn) {
	connCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { _ = conn.Close() }()

	var playCode domain.StreamCode
	var playOk bool
	var playMu sync.Mutex

	handle := rtmpmedia.NewRtmpServerHandle()
	ready := make(chan struct{})
	handle.OnPlay(func(app, streamName string, _, _ float64, _ bool) rtmpmedia.StatusCode {
		if app != publisherRTMPApp {
			return rtmpmedia.NETSTREAM_PLAY_NOTFOUND
		}
		code := domain.StreamCode(streamName)
		if err := domain.ValidateStreamCode(string(code)); err != nil {
			return rtmpmedia.NETSTREAM_PLAY_NOTFOUND
		}
		s.rtmpMu.Lock()
		_, allowed := s.rtmpActive[code]
		s.rtmpMu.Unlock()
		if !allowed {
			return rtmpmedia.NETSTREAM_PLAY_NOTFOUND
		}
		playMu.Lock()
		playCode = code
		playOk = true
		playMu.Unlock()
		return rtmpmedia.NETSTREAM_PLAY_START
	})
	handle.OnStateChange(func(st rtmpmedia.RtmpState) {
		if st == rtmpmedia.STATE_RTMP_PLAY_START {
			select {
			case <-ready:
			default:
				close(ready)
			}
		}
	})
	handle.SetOutput(func(b []byte) error {
		_, werr := conn.Write(b)
		return werr
	})

	go func() {
		select {
		case <-ready:
		case <-connCtx.Done():
			return
		}
		playMu.Lock()
		code, ok := playCode, playOk
		playMu.Unlock()
		if !ok {
			return
		}
		s.mu.Lock()
		wctx, has := s.streamWorkCtx[code]
		s.mu.Unlock()
		if !has {
			return
		}
		s.runRTMPMediaPump(wctx, code, handle)
	}()

	buf := make([]byte, 64*1024)
	for {
		n, rerr := conn.Read(buf)
		if rerr != nil {
			return
		}
		if err := handle.Input(buf[:n]); err != nil {
			return
		}
	}
}

func (s *Service) runRTMPMediaPump(ctx context.Context, streamID domain.StreamCode, handle *rtmpmedia.RtmpServerHandle) {
	sub, err := s.buf.Subscribe(streamID)
	if err != nil {
		slog.Error("publisher: RTMP play subscribe failed", "stream_code", streamID, "err", err)
		return
	}
	defer s.buf.Unsubscribe(streamID, sub)

	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()

	go func() {
		defer func() { _ = pw.Close() }()
		for {
			select {
			case <-ctx.Done():
				return
			case pkt, ok := <-sub.Recv():
				if !ok {
					return
				}
				if _, werr := pw.Write(pkt); werr != nil {
					return
				}
			}
		}
	}()

	demux := mpeg2.NewTSDemuxer()
	demux.OnFrame = func(cid mpeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
		pt := uint32(pts)
		dt := uint32(dts)
		switch cid {
		case mpeg2.TS_STREAM_H264:
			_ = handle.WriteVideo(codec.CODECID_VIDEO_H264, frame, pt, dt)
		case mpeg2.TS_STREAM_H265:
			_ = handle.WriteVideo(codec.CODECID_VIDEO_H265, frame, pt, dt)
		case mpeg2.TS_STREAM_AAC:
			_ = handle.WriteAudio(codec.CODECID_AUDIO_AAC, frame, pt, dt)
		case mpeg2.TS_STREAM_AUDIO_MPEG1, mpeg2.TS_STREAM_AUDIO_MPEG2:
			_ = handle.WriteAudio(codec.CODECID_AUDIO_MP3, frame, pt, dt)
		}
	}
	_ = demux.Input(pr)
}
