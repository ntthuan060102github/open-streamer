package publisher

// push_rtmp.go — outbound RTMP re-stream (push to external RTMP endpoint).
//
// Data flow:
//
//	Buffer Hub → tsmux.FeedWirePacket → tsBuffer → mpeg2.TSDemuxer
//	                                                        │
//	                                        H.264 Annex-B + AAC ADTS
//	                                                        │
//	                         joy4 RTMP publish → remote RTMP server
//
// Codec probing: accumulates SPS+PPS bytes from H.264 frames and AAC config
// from the first ADTS frame. Once both are known, dials the destination and
// calls WriteHeader. Frames that arrive before the connection is ready are
// held in a bounded pending queue; the queue is flushed starting from the
// first keyframe after the connection is established.
//
// Reconnect: on any dial or write error the session ends; the caller
// (serveRTMPPush) waits RetryTimeoutSec and retries. Codec data is preserved
// across reconnects so the connection can be re-established quickly.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/avc"
	joyav "github.com/nareix/joy4/av"
	"github.com/nareix/joy4/codec/aacparser"
	joyh264 "github.com/nareix/joy4/codec/h264parser"
	joyrtmp "github.com/nareix/joy4/format/rtmp"
	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/tsmux"
)

const (
	// rtmpPushPendingMax caps the pending frame queue while waiting for codec init.
	rtmpPushPendingMax = 300
)

// rtmpPendingFrame is one queued frame before the connection is established.
type rtmpPendingFrame struct {
	pkt joyav.Packet
}

// rtmpPushPackager holds state for one outbound RTMP push session.
type rtmpPushPackager struct {
	streamID   domain.StreamCode
	url        string
	timeoutSec int

	// Codec state — set once and preserved across reconnects.
	videoPS []byte // accumulated Annex-B bytes for SPS/PPS detection
	h264CD  joyav.CodecData
	aacCfg  *aacparser.MPEG4AudioConfig
	aacCD   joyav.CodecData

	// Connection state — reset on each reconnect attempt.
	conn  *joyrtmp.Conn
	ready bool // WriteHeader has been called

	// Pending frames buffered before conn is ready.
	pending []rtmpPendingFrame

	// Base DTS for relative timestamps (reset on reconnect).
	baseDTS     uint64
	baseDTSSet  bool
	baseADTS    uint64
	baseADTSSet bool

	// connErr is set on first write failure; run() checks it to exit the session.
	connErr error
}

// run is the entry point: wires the TS pipe, demuxer, and drives the session.
// Returns when ctx is cancelled or a write/dial error occurs.
func (p *rtmpPushPackager) run(ctx context.Context, sub *buffer.Subscriber) error {
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
					tsCarry = tsCarry[:0]
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

	demux := mpeg2.NewTSDemuxer()
	demux.OnFrame = p.onTSFrame

	demuxDone := make(chan error, 1)
	go func() { demuxDone <- demux.Input(tb) }()

	select {
	case <-ctx.Done():
		tb.Close()
		<-demuxDone
	case err := <-demuxDone:
		if err != nil && ctx.Err() == nil {
			slog.Warn("publisher: RTMP push TS demux ended",
				"stream_code", p.streamID, "url", p.url, "err", err)
		}
	}

	p.closeConn()

	if p.connErr != nil {
		return p.connErr
	}
	return nil
}

// onTSFrame is the TSDemuxer callback; runs in the demuxer goroutine.
func (p *rtmpPushPackager) onTSFrame(cid mpeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
	if p.connErr != nil {
		return
	}

	switch cid {
	case mpeg2.TS_STREAM_H264:
		if len(frame) == 0 {
			return
		}
		p.handleVideoFrame(frame, pts, dts)

	case mpeg2.TS_STREAM_H265:
		// H.265 is not supported by the RTMP spec; skip silently.
		return

	case mpeg2.TS_STREAM_AAC:
		p.handleAudioFrame(frame, dts)

	case mpeg2.TS_STREAM_AUDIO_MPEG1, mpeg2.TS_STREAM_AUDIO_MPEG2:
		// MP3 audio in TS — not supported for RTMP push.
		return

	default:
		return
	}
}

func (p *rtmpPushPackager) handleVideoFrame(frame []byte, pts, dts uint64) {
	// Accumulate SPS/PPS until H.264 codec data is built.
	if p.h264CD == nil {
		p.videoPS = append(p.videoPS, frame...)
		p.tryBuildH264CD()
	}

	avcc := h264AnnexBToAVCC(frame)
	if len(avcc) == 0 {
		return
	}
	isKey := tsmux.KeyFrameH264(frame)

	if !p.baseDTSSet {
		p.baseDTS = dts
		p.baseDTSSet = true
	}
	elapsed := dts - p.baseDTS
	cto := int64(pts) - int64(dts)
	if cto < 0 {
		cto = 0
	}

	pkt := joyav.Packet{
		Idx:             0,
		IsKeyFrame:      isKey,
		Data:            avcc,
		Time:            time.Duration(elapsed) * time.Millisecond,
		CompositionTime: time.Duration(cto) * time.Millisecond,
	}

	p.enqueueOrWrite(pkt)
}

func (p *rtmpPushPackager) handleAudioFrame(frame []byte, dts uint64) {
	pos := 0
	for pos+7 <= len(frame) {
		hdr, hLen, err := aac.DecodeADTSHeader(bytes.NewReader(frame[pos:]))
		if err != nil {
			pos++
			continue
		}
		frameLen := int(hdr.HeaderLength) + int(hdr.PayloadLength)
		if pos+frameLen > len(frame) {
			break
		}
		rawAAC := frame[pos+hLen : pos+frameLen]

		// Build AAC codec data from first ADTS frame.
		if p.aacCD == nil && p.aacCfg == nil {
			joyCfg, joyErr := adtsHdrToJoyCfg(hdr)
			if joyErr == nil {
				p.aacCfg = &joyCfg
				cd, cdErr := aacparser.NewCodecDataFromMPEG4AudioConfig(joyCfg)
				if cdErr == nil {
					p.aacCD = cd
				}
			}
		}

		if !p.baseADTSSet {
			p.baseADTS = dts
			p.baseADTSSet = true
		}
		elapsed := dts - p.baseADTS

		pkt := joyav.Packet{
			Idx:        1,
			IsKeyFrame: true,
			Data:       append([]byte(nil), rawAAC...),
			Time:       time.Duration(elapsed) * time.Millisecond,
		}
		p.enqueueOrWrite(pkt)

		pos += frameLen
	}
}

// enqueueOrWrite either buffers the packet (during codec init / before connect)
// or writes it directly to the open connection.
func (p *rtmpPushPackager) enqueueOrWrite(pkt joyav.Packet) {
	if !p.ready {
		// Try to connect now that we may have codec data.
		if p.h264CD != nil && p.aacCD != nil {
			if err := p.connect(); err != nil {
				slog.Warn("publisher: RTMP push connect failed",
					"stream_code", p.streamID, "url", p.url, "err", err)
				p.connErr = err
				return
			}
			// Flush pending from first keyframe.
			p.flushPending()
		} else {
			// Buffer until codec is ready.
			if len(p.pending) >= rtmpPushPendingMax {
				p.pending = p.pending[1:] // drop oldest
			}
			p.pending = append(p.pending, rtmpPendingFrame{pkt: pkt})
			return
		}
	}

	if err := p.conn.WritePacket(pkt); err != nil {
		slog.Warn("publisher: RTMP push write error",
			"stream_code", p.streamID, "url", p.url, "err", err)
		p.connErr = err
		p.closeConn()
	}
}

// connect dials the RTMP URL and calls WriteHeader.
func (p *rtmpPushPackager) connect() error {
	timeout := time.Duration(p.timeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	slog.Info("publisher: RTMP push connecting", "stream_code", p.streamID, "url", p.url)

	conn, err := joyrtmp.DialTimeout(p.url, timeout)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	streams := []joyav.CodecData{p.h264CD, p.aacCD}
	if err := conn.WriteHeader(streams); err != nil {
		_ = conn.Close()
		return fmt.Errorf("write header: %w", err)
	}

	p.conn = conn
	p.ready = true
	slog.Info("publisher: RTMP push connected", "stream_code", p.streamID, "url", p.url)
	return nil
}

// flushPending writes queued frames starting from the first video keyframe.
func (p *rtmpPushPackager) flushPending() {
	// Find the first video keyframe in pending queue.
	start := 0
	for i, f := range p.pending {
		if f.pkt.Idx == 0 && f.pkt.IsKeyFrame {
			start = i
			break
		}
	}
	for _, f := range p.pending[start:] {
		if err := p.conn.WritePacket(f.pkt); err != nil {
			slog.Warn("publisher: RTMP push flush error",
				"stream_code", p.streamID, "url", p.url, "err", err)
			p.connErr = err
			p.closeConn()
			break
		}
	}
	p.pending = p.pending[:0]
}

func (p *rtmpPushPackager) closeConn() {
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn = nil
	}
	p.ready = false
}

// tryBuildH264CD scans accumulated videoPS for SPS+PPS and builds joy4 CodecData.
func (p *rtmpPushPackager) tryBuildH264CD() {
	if len(p.videoPS) < 20 {
		return
	}
	psBuf := annexB4To3ForPSExtract(p.videoPS)
	spss := avc.ExtractNalusOfTypeFromByteStream(avc.NALU_SPS, psBuf, false)
	ppss := avc.ExtractNalusOfTypeFromByteStream(avc.NALU_PPS, psBuf, false)
	if len(spss) == 0 || len(ppss) == 0 {
		return
	}
	cd, err := joyh264.NewCodecDataFromSPSAndPPS(spss[0], ppss[0])
	if err != nil {
		slog.Warn("publisher: RTMP push H.264 codec data error",
			"stream_code", p.streamID, "err", err)
		return
	}
	p.h264CD = cd
	p.videoPS = nil
}

// adtsHdrToJoyCfg converts an mp4ff ADTS header to a joy4 MPEG4AudioConfig.
func adtsHdrToJoyCfg(hdr *aac.ADTSHeader) (aacparser.MPEG4AudioConfig, error) {
	freq := int(hdr.Frequency())
	if freq <= 0 {
		return aacparser.MPEG4AudioConfig{}, fmt.Errorf("invalid AAC sample rate from ADTS")
	}
	// Map frequency to joy4 SampleRateIndex.
	srIdx := aacSampleRateIndex(freq)
	chCfg := uint(hdr.ChannelConfig)
	cfg := aacparser.MPEG4AudioConfig{
		ObjectType:      2, // AAC-LC
		SampleRate:      freq,
		SampleRateIndex: srIdx,
		ChannelConfig:   chCfg,
	}
	cfg.Complete()
	return cfg, nil
}

// aacSampleRateIndex returns the MPEG-4 Audio sample rate index for a given Hz value.
func aacSampleRateIndex(hz int) uint {
	table := []int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}
	for i, r := range table {
		if hz == r {
			return uint(i)
		}
	}
	return 4 // default to 44100
}

// serveRTMPPush is the publisher goroutine for one outbound PushDestination.
// It creates a fresh packager session per connection attempt and reconnects on error.
func (s *Service) serveRTMPPush(
	ctx context.Context,
	streamID domain.StreamCode,
	mediaBufferID domain.StreamCode,
	dest domain.PushDestination,
) {
	if !strings.HasPrefix(dest.URL, "rtmp://") {
		slog.Warn("publisher: RTMP push unsupported scheme (only rtmp:// supported)",
			"stream_code", streamID, "url", dest.URL)
		return
	}

	retryDelay := time.Duration(dest.RetryTimeoutSec) * time.Second
	if retryDelay <= 0 {
		retryDelay = 5 * time.Second
	}

	attempts := 0

	// Codec data (SPS/PPS, AAC config) is preserved across reconnects.
	var preserved *rtmpPushPackager

	for {
		if ctx.Err() != nil {
			return
		}
		if dest.Limit > 0 && attempts >= dest.Limit {
			slog.Warn("publisher: RTMP push reached retry limit",
				"stream_code", streamID, "url", dest.URL, "limit", dest.Limit)
			return
		}
		attempts++

		sub, err := s.buf.Subscribe(mediaBufferID)
		if err != nil {
			slog.Warn("publisher: RTMP push subscribe failed",
				"stream_code", streamID, "url", dest.URL, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay):
				continue
			}
		}

		p := &rtmpPushPackager{
			streamID:   streamID,
			url:        dest.URL,
			timeoutSec: dest.TimeoutSec,
		}
		// Reuse codec data from previous session so reconnect is fast.
		if preserved != nil {
			p.h264CD = preserved.h264CD
			p.aacCD = preserved.aacCD
			p.aacCfg = preserved.aacCfg
		}

		sessionErr := p.run(ctx, sub)
		s.buf.Unsubscribe(mediaBufferID, sub)

		// Preserve codec data for next session.
		preserved = p

		if ctx.Err() != nil {
			return
		}
		if sessionErr != nil {
			slog.Warn("publisher: RTMP push session ended, retrying",
				"stream_code", streamID, "url", dest.URL, "err", sessionErr,
				"retry_in", retryDelay)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay):
		}
	}
}
