package publisher

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"

	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
)

func (s *Service) rtspListenPort() int {
	p := s.cfg.RTSP.PortMin
	if p <= 0 {
		return 18554
	}
	return p
}

// rtspEnsureStartedLocked starts the shared RTSP server (caller must hold s.rtspMu).
func (s *Service) rtspEnsureStartedLocked() error {
	if s.rtspSrv != nil {
		return nil
	}
	host := trimListenHost(s.cfg.RTSP.ListenHost)
	port := s.rtspListenPort()
	rtspAddr := net.JoinHostPort(host, strconv.Itoa(port))
	srv := &gortsplib.Server{
		Handler:     s,
		RTSPAddress: rtspAddr,
	}
	if err := srv.Start(); err != nil {
		return err
	}
	s.rtspSrv = srv
	slog.Info("publisher: RTSP shared listener (gortsplib, MPEG-TS in RTP)",
		"rtsp_addr", rtspAddr,
		"client_hint", fmt.Sprintf("rtsp://<this-host>:%d/live/<stream_code>", port),
	)
	return nil
}

func (s *Service) OnDescribe(
	ctx *gortsplib.ServerHandlerOnDescribeCtx,
) (*base.Response, *gortsplib.ServerStream, error) {
	code, err := streamCodeFromLivePath(ctx.Path)
	if err != nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	s.rtspMu.RLock()
	st := s.rtspMounts[publisherLiveMountPath(code)]
	s.rtspMu.RUnlock()
	if st == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, st, nil
}

func (s *Service) OnSetup(
	ctx *gortsplib.ServerHandlerOnSetupCtx,
) (*base.Response, *gortsplib.ServerStream, error) {
	code, err := streamCodeFromLivePath(ctx.Path)
	if err != nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	s.rtspMu.RLock()
	st := s.rtspMounts[publisherLiveMountPath(code)]
	s.rtspMu.RUnlock()
	if st == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, st, nil
}

func (s *Service) OnPlay(_ *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (s *Service) serveRTSP(ctx context.Context, streamID domain.StreamCode) {
	sub, err := s.buf.Subscribe(streamID)
	if err != nil {
		slog.Error("publisher: RTSP subscribe failed", "stream_code", streamID, "err", err)
		return
	}
	defer s.buf.Unsubscribe(streamID, sub)

	mount := publisherLiveMountPath(streamID)

	s.rtspMu.Lock()
	if err := s.rtspEnsureStartedLocked(); err != nil {
		s.rtspMu.Unlock()
		slog.Error("publisher: RTSP server start failed", "stream_code", streamID, "err", err)
		return
	}

	desc := &description.Session{
		Medias: []*description.Media{{
			Type:    description.MediaTypeVideo,
			Formats: []format.Format{&format.MPEGTS{}},
		}},
	}
	stream := &gortsplib.ServerStream{
		Server: s.rtspSrv,
		Desc:   desc,
	}
	if err := stream.Initialize(); err != nil {
		s.rtspMu.Unlock()
		slog.Error("publisher: RTSP stream init failed", "stream_code", streamID, "err", err)
		return
	}
	s.rtspMounts[mount] = stream
	s.rtspMu.Unlock()

	host := trimListenHost(s.cfg.RTSP.ListenHost)
	if host == defaultListenHost || host == "" {
		host = "<this-host>"
	}
	slog.Info("publisher: RTSP mount active",
		"stream_code", streamID,
		"url", fmt.Sprintf("rtsp://%s:%d%s", host, s.rtspListenPort(), mount),
	)

	pumpCtx, pumpCancel := context.WithCancel(ctx)
	go pumpRTSPMPEGTS(pumpCtx, sub, stream, desc.Medias[0])

	<-ctx.Done()
	pumpCancel()

	s.rtspMu.Lock()
	stream.Close()
	delete(s.rtspMounts, mount)
	needCloseSrv := len(s.rtspMounts) == 0 && s.rtspSrv != nil
	srv := s.rtspSrv
	if needCloseSrv {
		s.rtspSrv = nil
	}
	s.rtspMu.Unlock()

	if needCloseSrv && srv != nil {
		srv.Close()
		_ = srv.Wait()
	}
}

func pumpRTSPMPEGTS(ctx context.Context, sub *buffer.Subscriber, stream *gortsplib.ServerStream, medi *description.Media) {
	enc, err := (&format.MPEGTS{}).CreateEncoder()
	if err != nil {
		return
	}
	start := time.Now()
	var carry tsPacketBuf
	var pending [][]byte

	flush := func() {
		if len(pending) == 0 {
			return
		}
		rtpTS := uint32(time.Since(start).Seconds() * 90000)
		batch := pending
		pending = nil
		rpcks, err := enc.Encode(batch)
		if err != nil {
			pending = append(pending, batch...)
			return
		}
		for _, rp := range rpcks {
			rp.Timestamp = rtpTS
			_ = stream.WritePacketRTP(medi, rp)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case pkt, ok := <-sub.Recv():
			if !ok {
				return
			}
			for _, tsb := range carry.Feed(pkt) {
				pending = append(pending, tsb)
				if len(pending) >= 7 {
					flush()
				}
			}
			if len(pending) > 0 {
				flush()
			}
		}
	}
}
