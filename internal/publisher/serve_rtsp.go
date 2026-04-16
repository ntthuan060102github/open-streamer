package publisher

// serve_rtsp.go — RTSP play server (publisher-side).
//
// Architecture:
//
//	Buffer Hub → subscriber → tsmux.FeedWirePacket → tsBuffer → mpeg2.TSDemuxer
//	                                                                   │
//	                                                   onTSFrame (H.264 Annex-B, AAC ADTS)
//	                                                                   │
//	                                                  rtspSession (codec detection + RTP encoding)
//	                                                                   │
//	                                              gortsplib.ServerStream.WritePacketRTP
//	                                                                   │
//	                                                             RTSP clients
//
// Stream registration lifecycle:
//  1. serveRTSP waits for the gortsplib server to start, then subscribes to the Buffer Hub.
//  2. Frames are collected until SPS/PPS (H.264) and AAC AudioSpecificConfig are known
//     (or up to maxRTSPPendingFrames video frames if audio never arrives).
//  3. A ServerStream is created and mounted at /live/<stream_code>; the handler starts
//     returning it to DESCRIBE/SETUP requests.
//  4. On stream stop, the mount is removed and the ServerStream is closed.
//
// Client URL: rtsp://host:port_min/live/<stream_code>

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
)

// maxRTSPPendingFrames is the maximum number of video frames buffered while
// waiting for AAC codec info. After this limit, the stream is initialised with
// video-only (no audio track in SDP).
const maxRTSPPendingFrames = 90

func rtspLiveMountPath(code domain.StreamCode) string {
	return "live/" + string(code)
}

// RunRTSPPlayServer starts the RTSP play listener.
// Returns nil immediately when publisher.rtsp.port_min is 0 (disabled).
func (s *Service) RunRTSPPlayServer(ctx context.Context) error {
	port := s.cfg.RTSP.PortMin
	if port == 0 {
		close(s.rtspSrvReady)
		return nil
	}
	host := s.cfg.RTSP.ListenHost
	if host == "" {
		host = "0.0.0.0"
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	srv := &gortsplib.Server{
		RTSPAddress: addr,
		Handler:     &rtspHandler{svc: s},
	}
	if err := srv.Start(); err != nil {
		return fmt.Errorf("publisher rtsp: listen %q: %w", addr, err)
	}

	s.mu.Lock()
	s.rtspSrv = srv
	s.mu.Unlock()
	close(s.rtspSrvReady)

	slog.Info("publisher: RTSP play server listening", "addr", addr)

	<-ctx.Done()
	srv.Close()
	_ = srv.Wait()
	return nil
}

// ─── handler ─────────────────────────────────────────────────────────────────

type rtspHandler struct{ svc *Service }

// OnDescribe handles DESCRIBE — returns the ServerStream for the requested path.
func (h *rtspHandler) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	stream := h.lookupStream(ctx.Path)
	if stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, stream, nil
}

// OnSetup handles SETUP — associates the client session with the ServerStream.
func (h *rtspHandler) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	stream := h.lookupStream(ctx.Path)
	if stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, stream, nil
}

// lookupStream matches a request path to a registered ServerStream.
// The SETUP path may have a track suffix (e.g. /live/foo/trackID=0), so we
// match by exact path or by prefix followed by "/".
func (h *rtspHandler) lookupStream(path string) *gortsplib.ServerStream {
	path = strings.TrimPrefix(path, "/")
	h.svc.mu.Lock()
	defer h.svc.mu.Unlock()
	for mountPath, stream := range h.svc.rtspMounts {
		if path == mountPath || strings.HasPrefix(path, mountPath+"/") {
			return stream
		}
	}
	return nil
}

// ─── per-stream pipeline ─────────────────────────────────────────────────────

// serveRTSP is spawned by spawnOutputs and runs for the lifetime of the stream.
func (s *Service) serveRTSP(ctx context.Context, streamCode, bufID domain.StreamCode) {
	// Wait for RTSP server to start (or be disabled).
	select {
	case <-ctx.Done():
		return
	case <-s.rtspSrvReady:
	}

	s.mu.Lock()
	srv := s.rtspSrv
	s.mu.Unlock()
	if srv == nil {
		return // server disabled (port = 0)
	}

	sub, err := s.buf.Subscribe(bufID)
	if err != nil {
		slog.Warn("publisher: RTSP subscribe failed", "stream_code", streamCode, "err", err)
		return
	}
	defer s.buf.Unsubscribe(bufID, sub)

	slog.Info("publisher: RTSP session started", "stream_code", streamCode)
	defer slog.Info("publisher: RTSP session ended", "stream_code", streamCode)

	runRTSPPipeline(ctx, streamCode, srv, sub, s)
}

// runRTSPPipeline feeds TS from sub through a TSDemuxer and writes H.264/AAC RTP.
func runRTSPPipeline(
	ctx context.Context,
	streamCode domain.StreamCode,
	srv *gortsplib.Server,
	sub *buffer.Subscriber,
	svc *Service,
) {
	tb := newTSBuffer()

	go func() {
		defer tb.Close()
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
				tsmux.FeedWirePacket(pkt.TS, pkt.AV, &avMux, func(b []byte) {
					tsmux.DrainTS188Aligned(&tsCarry, b, func(aligned []byte) {
						_, _ = tb.Write(aligned)
					})
				})
			}
		}
	}()

	sess := &rtspSession{
		streamCode: streamCode,
		srv:        srv,
		svc:        svc,
		mountPath:  rtspLiveMountPath(streamCode),
		firstVideo: true,
	}

	demux := mpeg2.NewTSDemuxer()
	demux.OnFrame = sess.onTSFrame

	demuxDone := make(chan error, 1)
	go func() { demuxDone <- demux.Input(tb) }()

	select {
	case <-ctx.Done():
		tb.Close()
		<-demuxDone
	case err := <-demuxDone:
		if err != nil {
			slog.Debug("publisher: RTSP demux ended",
				"stream_code", streamCode, "err", err)
		}
	}
	sess.closeStream()
}

// ─── rtspSession ─────────────────────────────────────────────────────────────

type rtspSession struct {
	streamCode domain.StreamCode
	srv        *gortsplib.Server
	svc        *Service
	mountPath  string

	// codec detection
	sps    []byte
	pps    []byte
	aacCfg *mpeg4audio.AudioSpecificConfig

	// pending frames buffered before the stream is initialised
	pendingVideo []rtspPendingVideo
	pendingAudio []rtspPendingAudio

	// serving state (nil until initialised)
	stream     *gortsplib.ServerStream
	videoMedia *description.Media
	audioMedia *description.Media
	videoEnc   *rtph264.Encoder
	audioEnc   *rtpmpeg4audio.Encoder

	// timing (MPEG-TS 90 kHz)
	baseDTS      uint64
	baseDTSSet   bool
	baseAudio    uint64
	baseAudioSet bool
	firstVideo   bool
}

type rtspPendingVideo struct {
	frame    []byte
	pts, dts uint64
}

type rtspPendingAudio struct {
	frame []byte
	dts   uint64
}

func (sess *rtspSession) onTSFrame(cid mpeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
	if len(frame) == 0 {
		return
	}
	switch cid {
	case mpeg2.TS_STREAM_H264:
		sess.handleVideo(frame, pts, dts)
	case mpeg2.TS_STREAM_AAC:
		sess.handleAudio(frame, dts)
	case mpeg2.TS_STREAM_H265,
		mpeg2.TS_STREAM_AUDIO_MPEG1,
		mpeg2.TS_STREAM_AUDIO_MPEG2:
	default:
	}
}

func (sess *rtspSession) handleVideo(frame []byte, pts, dts uint64) {
	// Drop until the first IDR so decoders can initialise.
	if sess.firstVideo {
		if !tsmux.KeyFrameH264(frame) {
			return
		}
		sess.firstVideo = false
	}

	// Extract SPS/PPS on keyframes.
	if tsmux.KeyFrameH264(frame) {
		if sps, pps := extractSPSPPS(frame); len(sps) > 0 && len(pps) > 0 {
			sess.sps = sps
			sess.pps = pps
		}
	}

	if sess.stream == nil {
		// Queue frame while waiting for codec info.
		if len(sess.sps) > 0 && len(sess.pps) > 0 {
			cp := make([]byte, len(frame))
			copy(cp, frame)
			sess.pendingVideo = append(sess.pendingVideo, rtspPendingVideo{frame: cp, pts: pts, dts: dts})

			// Init if we also have audio config, or if we've waited long enough.
			if sess.aacCfg != nil || len(sess.pendingVideo) >= maxRTSPPendingFrames {
				if err := sess.initStream(); err != nil {
					slog.Warn("publisher: RTSP init stream failed",
						"stream_code", sess.streamCode, "err", err)
					sess.pendingVideo = sess.pendingVideo[:0]
					sess.pendingAudio = sess.pendingAudio[:0]
				}
			}
		}
		return
	}

	sess.writeVideoRTP(frame, pts, dts)
}

func (sess *rtspSession) handleAudio(frame []byte, dts uint64) {
	if sess.firstVideo {
		return // hold audio until first video keyframe
	}

	// Extract AAC config from ADTS on first audio frame.
	if sess.aacCfg == nil {
		var pkts mpeg4audio.ADTSPackets
		if err := pkts.Unmarshal(frame); err == nil && len(pkts) > 0 {
			p := pkts[0]
			sess.aacCfg = &mpeg4audio.AudioSpecificConfig{
				Type:          p.Type,
				SampleRate:    p.SampleRate,
				ChannelConfig: p.ChannelConfig,
			}
		}
	}

	if sess.stream == nil {
		// Queue frame while waiting for stream init.
		cp := make([]byte, len(frame))
		copy(cp, frame)
		sess.pendingAudio = append(sess.pendingAudio, rtspPendingAudio{frame: cp, dts: dts})

		// If video codec is ready, trigger init now that we have audio config.
		if sess.aacCfg != nil && len(sess.sps) > 0 && len(sess.pps) > 0 {
			if err := sess.initStream(); err != nil {
				slog.Warn("publisher: RTSP init stream failed",
					"stream_code", sess.streamCode, "err", err)
				sess.pendingVideo = sess.pendingVideo[:0]
				sess.pendingAudio = sess.pendingAudio[:0]
			}
		}
		return
	}

	sess.writeAudioRTP(frame, dts)
}

func (sess *rtspSession) initStream() error {
	h264Format := &format.H264{
		PayloadTyp:        96,
		SPS:               append([]byte(nil), sess.sps...),
		PPS:               append([]byte(nil), sess.pps...),
		PacketizationMode: 1,
	}
	videoMedia := &description.Media{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{h264Format},
	}

	medias := []*description.Media{videoMedia}

	var aacFormat *format.MPEG4Audio
	var audioMedia *description.Media
	if sess.aacCfg != nil {
		aacFormat = &format.MPEG4Audio{
			PayloadTyp: 97,
			Config:     sess.aacCfg,
		}
		audioMedia = &description.Media{
			Type:    description.MediaTypeAudio,
			Formats: []format.Format{aacFormat},
		}
		medias = append(medias, audioMedia)
	}

	stream := &gortsplib.ServerStream{
		Server: sess.srv,
		Desc:   &description.Session{Medias: medias},
	}
	if err := stream.Initialize(); err != nil {
		return fmt.Errorf("initialize server stream: %w", err)
	}

	videoEnc, err := h264Format.CreateEncoder()
	if err != nil {
		stream.Close()
		return fmt.Errorf("create H264 encoder: %w", err)
	}
	if err := videoEnc.Init(); err != nil {
		stream.Close()
		return fmt.Errorf("init H264 encoder: %w", err)
	}

	var audioEnc *rtpmpeg4audio.Encoder
	if aacFormat != nil {
		audioEnc, err = aacFormat.CreateEncoder()
		if err != nil {
			stream.Close()
			return fmt.Errorf("create AAC encoder: %w", err)
		}
		if err := audioEnc.Init(); err != nil {
			stream.Close()
			return fmt.Errorf("init AAC encoder: %w", err)
		}
	}

	sess.stream = stream
	sess.videoMedia = videoMedia
	sess.audioMedia = audioMedia
	sess.videoEnc = videoEnc
	sess.audioEnc = audioEnc

	// Register mount so the handler can serve DESCRIBE requests.
	sess.svc.mu.Lock()
	sess.svc.rtspMounts[sess.mountPath] = stream
	sess.svc.mu.Unlock()

	slog.Info("publisher: RTSP stream mounted",
		"stream_code", sess.streamCode,
		"path", sess.mountPath,
		"audio", audioMedia != nil,
	)

	// Replay buffered frames.
	for _, v := range sess.pendingVideo {
		sess.writeVideoRTP(v.frame, v.pts, v.dts)
	}
	for _, a := range sess.pendingAudio {
		sess.writeAudioRTP(a.frame, a.dts)
	}
	sess.pendingVideo = nil
	sess.pendingAudio = nil

	return nil
}

func (sess *rtspSession) writeVideoRTP(frame []byte, pts, dts uint64) {
	if !sess.baseDTSSet {
		sess.baseDTS = dts
		sess.baseDTSSet = true
	}
	// MPEG-TS DTS is already in 90 kHz — same clock as H.264 RTP.
	rtpDTS := uint32(dts - sess.baseDTS)
	rtpPTS := rtpDTS + uint32(pts-dts)

	nalus := annexBToNALUs(frame)
	if len(nalus) == 0 {
		return
	}

	pkts, err := sess.videoEnc.Encode(nalus)
	if err != nil {
		slog.Debug("publisher: RTSP H264 encode error", "stream_code", sess.streamCode, "err", err)
		return
	}
	for i, pkt := range pkts {
		// Last packet of the access unit carries the display timestamp.
		if i == len(pkts)-1 {
			pkt.Timestamp = rtpPTS
		} else {
			pkt.Timestamp = rtpDTS
		}
		if err := sess.stream.WritePacketRTP(sess.videoMedia, pkt); err != nil {
			slog.Debug("publisher: RTSP write video RTP", "stream_code", sess.streamCode, "err", err)
			return
		}
	}
}

func (sess *rtspSession) writeAudioRTP(frame []byte, dts uint64) {
	if sess.audioMedia == nil || sess.audioEnc == nil || sess.aacCfg == nil {
		return
	}

	if !sess.baseAudioSet {
		sess.baseAudio = dts
		sess.baseAudioSet = true
	}
	// Convert from 90 kHz MPEG-TS clock to AAC RTP clock (sample rate).
	relDTS := dts - sess.baseAudio
	rtpTS := uint32(relDTS * uint64(sess.aacCfg.SampleRate) / 90000)

	var adtsPkts mpeg4audio.ADTSPackets
	if err := adtsPkts.Unmarshal(frame); err != nil || len(adtsPkts) == 0 {
		return
	}
	aus := make([][]byte, 0, len(adtsPkts))
	for _, p := range adtsPkts {
		if len(p.AU) > 0 {
			aus = append(aus, p.AU)
		}
	}
	if len(aus) == 0 {
		return
	}

	pkts, err := sess.audioEnc.Encode(aus)
	if err != nil {
		slog.Debug("publisher: RTSP AAC encode error", "stream_code", sess.streamCode, "err", err)
		return
	}
	for _, pkt := range pkts {
		pkt.Timestamp = rtpTS
		if err := sess.stream.WritePacketRTP(sess.audioMedia, pkt); err != nil {
			slog.Debug("publisher: RTSP write audio RTP", "stream_code", sess.streamCode, "err", err)
			return
		}
	}
}

func (sess *rtspSession) closeStream() {
	if sess.stream == nil {
		return
	}
	sess.svc.mu.Lock()
	delete(sess.svc.rtspMounts, sess.mountPath)
	sess.svc.mu.Unlock()
	sess.stream.Close()
	slog.Info("publisher: RTSP stream unmounted", "stream_code", sess.streamCode)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// extractSPSPPS returns the raw SPS (type 7) and PPS (type 8) NAL units from
// an Annex-B H.264 byte stream. Returns nil slices if not found.
func extractSPSPPS(annexB []byte) (sps, pps []byte) {
	for _, nalu := range annexBToNALUs(annexB) {
		if len(nalu) == 0 {
			continue
		}
		switch nalu[0] & 0x1f {
		case 7:
			sps = append([]byte(nil), nalu...)
		case 8:
			pps = append([]byte(nil), nalu...)
		}
	}
	return sps, pps
}

// annexBToNALUs splits an Annex-B H.264 byte stream into individual raw NAL units
// (without start codes). Handles both 3-byte and 4-byte start codes.
func annexBToNALUs(annexB []byte) [][]byte {
	var result [][]byte
	start := -1
	i := 0
	for i < len(annexB) {
		if i+4 <= len(annexB) && annexB[i] == 0 && annexB[i+1] == 0 && annexB[i+2] == 0 && annexB[i+3] == 1 {
			if start >= 0 && i > start {
				result = append(result, annexB[start:i])
			}
			i += 4
			start = i
			continue
		}
		if i+3 <= len(annexB) && annexB[i] == 0 && annexB[i+1] == 0 && annexB[i+2] == 1 {
			if start >= 0 && i > start {
				result = append(result, annexB[start:i])
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(annexB) {
		result = append(result, annexB[start:])
	}
	// Filter empty NALUs.
	out := result[:0]
	for _, n := range result {
		if len(n) > 0 {
			out = append(out, n)
		}
	}
	return out
}
