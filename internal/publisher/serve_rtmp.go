package publisher

// serve_rtmp.go — RTMP play server (publisher-side).
//
// Two modes:
//
//  1. Shared port (recommended): the ingestor's RTMP server calls HandleRTMPPlay
//     when an external client connects with a "play" command. Same port as ingest
//     (default :1935). Wire via main.go: ing.SetRTMPPlayHandler(pub.HandleRTMPPlay).
//
//  2. Separate port (optional): RunRTMPPlayServer binds its own listener on
//     publisher.rtmp.port. Disabled when port = 0.
//
// Client URL: rtmp://host:port/live/<stream_code>
//
// Data flow per session:
//
//	Buffer Hub → subscriber → tsmux.FeedWirePacket → tsBuffer → mpeg2.TSDemuxer
//	                                                                   │
//	                                                    onTSFrame (H.264 Annex-B, AAC ADTS)
//	                                                                   │
//	                                                  writeFrame callback → RTMP client
//
// gomedia WriteFrame accepts Annex-B for H.264 and ADTS for AAC — the native
// output of mpeg2.TSDemuxer — so no conversion is needed.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"

	gocodec "github.com/yapingcat/gomedia/go-codec"
	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"
	gortmp "github.com/yapingcat/gomedia/go-rtmp"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
)

// HandleRTMPPlay is the play handler registered with the ingestor's RTMP server.
// It subscribes to the Buffer Hub for the given stream key, demuxes the TS stream,
// and calls writeFrame for each H.264/AAC frame until ctx is cancelled.
// Returns an error when the stream is not active (caller closes the connection).
func (s *Service) HandleRTMPPlay(
	ctx context.Context,
	key string,
	writeFrame func(cid gocodec.CodecID, data []byte, pts, dts uint32) error,
) error {
	code := domain.StreamCode(key)

	bufID, ok := s.mediaBufferFor(code)
	if !ok {
		return fmt.Errorf("stream %q not active", key)
	}

	sub, err := s.buf.Subscribe(bufID)
	if err != nil {
		return fmt.Errorf("subscribe %q: %w", key, err)
	}
	defer s.buf.Unsubscribe(bufID, sub)

	slog.Info("publisher: RTMP play session started", "stream_code", code)
	defer slog.Info("publisher: RTMP play session ended", "stream_code", code)

	runRTMPPlayPipeline(ctx, code, sub, writeFrame)
	return nil
}

// RunRTMPPlayServer starts an optional dedicated RTMP play listener.
// Returns nil immediately when publisher.rtmp.port is 0 (disabled).
func (s *Service) RunRTMPPlayServer(ctx context.Context) error {
	port := s.cfg.RTMP.Port
	if port == 0 {
		return nil
	}
	host := s.cfg.RTMP.ListenHost
	if host == "" {
		host = "0.0.0.0"
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("publisher rtmp: listen %q: %w", addr, err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	slog.Info("publisher: RTMP play server listening", "addr", addr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("publisher rtmp: accept: %w", err)
			}
		}
		go s.handleRTMPPlayConn(ctx, conn)
	}
}

// handleRTMPPlayConn drives one client on the dedicated play port.
func (s *Service) handleRTMPPlayConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	handle := gortmp.NewRtmpServerHandle()
	handle.SetOutput(func(b []byte) error {
		_, err := conn.Write(b)
		return err
	})

	handle.OnPlay(func(app, key string, start, duration float64, reset bool) gortmp.StatusCode {
		_, ok := s.mediaBufferFor(domain.StreamCode(key))
		if !ok {
			slog.Warn("publisher: RTMP play stream not found", "key", key)
			return gortmp.NETSTREAM_PLAY_NOTFOUND
		}
		return gortmp.NETSTREAM_PLAY_START
	})

	handle.OnStateChange(func(newState gortmp.RtmpState) {
		if newState != gortmp.STATE_RTMP_PLAY_START {
			return
		}
		key := handle.GetStreamName()
		go func() {
			err := s.HandleRTMPPlay(connCtx, key, func(cid gocodec.CodecID, data []byte, pts, dts uint32) error {
				return handle.WriteFrame(cid, data, pts, dts)
			})
			if err != nil {
				slog.Debug("publisher: RTMP play ended", "key", key, "err", err)
				_ = conn.Close()
			}
		}()
	})

	buf := make([]byte, 65536)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			break
		}
		if err := handle.Input(buf[:n]); err != nil {
			slog.Debug("publisher: RTMP play handle input", "err", err)
			break
		}
	}
}

// ─── pipeline ────────────────────────────────────────────────────────────────

// runRTMPPlayPipeline feeds TS from sub into a TSDemuxer and writes H.264/AAC
// frames via writeFrame until ctx is cancelled or sub closes.
func runRTMPPlayPipeline(
	ctx context.Context,
	streamCode domain.StreamCode,
	sub *buffer.Subscriber,
	writeFrame func(cid gocodec.CodecID, data []byte, pts, dts uint32) error,
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
					tsCarry = append(tsCarry, b...)
					for len(tsCarry) >= 188 {
						if tsCarry[0] != 0x47 {
							idx := bytes.IndexByte(tsCarry, 0x47)
							if idx < 0 {
								if len(tsCarry) > 187 {
									tsCarry = tsCarry[len(tsCarry)-187:]
								}
								return
							}
							tsCarry = tsCarry[idx:]
							if len(tsCarry) < 188 {
								return
							}
						}
						if len(tsCarry) >= 376 && tsCarry[188] != 0x47 {
							tsCarry = tsCarry[1:]
							continue
						}
						if _, err := tb.Write(tsCarry[:188]); err != nil {
							return
						}
						tsCarry = tsCarry[188:]
					}
				})
			}
		}
	}()

	ps := &rtmpPlaySession{
		streamCode: streamCode,
		writeFrame: writeFrame,
		firstVideo: true,
	}

	demux := mpeg2.NewTSDemuxer()
	demux.OnFrame = ps.onTSFrame

	demuxDone := make(chan error, 1)
	go func() { demuxDone <- demux.Input(tb) }()

	select {
	case <-ctx.Done():
		tb.Close()
		<-demuxDone
	case err := <-demuxDone:
		if err != nil {
			slog.Debug("publisher: RTMP play demux ended",
				"stream_code", streamCode, "err", err)
		}
	}
}

// ─── rtmpPlaySession ─────────────────────────────────────────────────────────

type rtmpPlaySession struct {
	streamCode  domain.StreamCode
	writeFrame  func(cid gocodec.CodecID, data []byte, pts, dts uint32) error
	baseDTS     uint64
	baseDTSSet  bool
	baseADTS    uint64
	baseADTSSet bool
	firstVideo  bool
}

func (ps *rtmpPlaySession) onTSFrame(cid mpeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
	if len(frame) == 0 {
		return
	}
	switch cid {
	case mpeg2.TS_STREAM_H264:
		ps.writeVideo(frame, pts, dts)
	case mpeg2.TS_STREAM_AAC:
		ps.writeAudio(frame, dts)
	case mpeg2.TS_STREAM_H265,
		mpeg2.TS_STREAM_AUDIO_MPEG1,
		mpeg2.TS_STREAM_AUDIO_MPEG2:
		return // not supported in standard RTMP
	default:
		return
	}
}

func (ps *rtmpPlaySession) writeVideo(frame []byte, pts, dts uint64) {
	if ps.firstVideo {
		if !tsmux.KeyFrameH264(frame) {
			return
		}
		ps.firstVideo = false
		ps.baseDTS = dts
		ps.baseDTSSet = true
	}
	if !ps.baseDTSSet {
		ps.baseDTS = dts
		ps.baseDTSSet = true
	}
	relDTS := uint32(dts - ps.baseDTS)
	relPTS := uint32(pts - ps.baseDTS)
	if err := ps.writeFrame(gocodec.CODECID_VIDEO_H264, frame, relPTS, relDTS); err != nil {
		slog.Debug("publisher: RTMP play write video error",
			"stream_code", ps.streamCode, "err", err)
	}
}

func (ps *rtmpPlaySession) writeAudio(frame []byte, dts uint64) {
	if ps.firstVideo {
		return // hold audio until first video keyframe
	}
	if !ps.baseADTSSet {
		ps.baseADTS = dts
		ps.baseADTSSet = true
	}
	relDTS := uint32(dts - ps.baseADTS)
	if err := ps.writeFrame(gocodec.CODECID_AUDIO_AAC, frame, relDTS, relDTS); err != nil {
		slog.Debug("publisher: RTMP play write audio error",
			"stream_code", ps.streamCode, "err", err)
	}
}
