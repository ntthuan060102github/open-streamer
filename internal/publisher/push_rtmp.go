package publisher

// push_rtmp.go — outbound RTMP/RTMPS re-stream (push to external endpoint).
//
// Data flow:
//
//	Buffer Hub → tsmux.FeedWirePacket → tsBuffer → mpeg2.TSDemuxer
//	                                                        │
//	                                        H.264 Annex-B + AAC ADTS
//	                                                        │
//	                         joy4 RTMP publish → remote RTMP(S) server
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
//
// RTMPS: rtmps:// URLs use a TLS-wrapped connection (default port 443).
// joy4's NewConn accepts any net.Conn, so the RTMP handshake and framing
// are identical — only the transport layer changes.

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/avc"
	joyav "github.com/nareix/joy4/av"
	"github.com/nareix/joy4/codec/aacparser"
	joyh264 "github.com/nareix/joy4/codec/h264parser"
	joyrtmp "github.com/nareix/joy4/format/rtmp"
	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
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
	go p.feedLoop(ctx, sub, tb)

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

// feedLoop reads packets from sub, reassembles 188-byte TS packets via
// alignedFeed, and pipes them into tb.  Runs in its own goroutine.
func (p *rtmpPushPackager) feedLoop(ctx context.Context, sub *buffer.Subscriber, tb *tsBuffer) {
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
				alignedFeed(b, &tsCarry, func(pkt188 []byte) bool {
					_, err := tb.Write(pkt188)
					return err == nil
				})
			})
		}
	}
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

// rtmpDial opens a joy4 RTMP connection for both rtmp:// and rtmps:// URLs.
//
//   - rtmp://  — plain TCP, default port 1935.
//   - rtmps:// — TLS over TCP, default port 443.  joy4's NewConn wraps any
//     net.Conn, so the RTMP framing layer is identical in both cases.
func rtmpDial(rawURL string, timeout time.Duration) (*joyrtmp.Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}

	// Apply scheme-specific default port when none is present.
	host := u.Host
	if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
		switch u.Scheme {
		case "rtmps":
			host += ":443"
		default:
			host += ":1935"
		}
		u.Host = host
	}

	dialer := &net.Dialer{Timeout: timeout}

	var netconn net.Conn
	switch u.Scheme {
	case "rtmps":
		tlsDialer := &tls.Dialer{
			NetDialer: dialer,
			Config:    &tls.Config{ServerName: u.Hostname()},
		}
		netconn, err = tlsDialer.DialContext(context.Background(), "tcp", host)
		if err != nil {
			return nil, fmt.Errorf("tls dial: %w", err)
		}
	default: // "rtmp"
		netconn, err = dialer.Dial("tcp", host)
		if err != nil {
			return nil, fmt.Errorf("tcp dial: %w", err)
		}
	}

	conn := joyrtmp.NewConn(netconn)
	conn.URL = u
	return conn, nil
}

// connect dials the RTMP/RTMPS URL and calls WriteHeader.
func (p *rtmpPushPackager) connect() error {
	timeout := time.Duration(p.timeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	slog.Info("publisher: RTMP push connecting", "stream_code", p.streamID, "url", p.url)

	conn, err := rtmpDial(p.url, timeout)
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
	if !strings.HasPrefix(dest.URL, "rtmp://") && !strings.HasPrefix(dest.URL, "rtmps://") {
		slog.Warn("publisher: RTMP push unsupported scheme (only rtmp:// and rtmps:// supported)",
			"stream_code", streamID, "url", dest.URL)
		return
	}

	retryDelay := time.Duration(dest.RetryTimeoutSec) * time.Second
	if retryDelay <= 0 {
		retryDelay = 5 * time.Second
	}

	attempts := 0
	var preserved *rtmpPushPackager // codec data reused across reconnects

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

		p, sessionErr := s.runOnePushSession(ctx, streamID, mediaBufferID, dest, preserved)
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

// runOnePushSession subscribes to the media buffer, runs one packager session,
// then unsubscribes.  Returns the packager (for codec reuse on the next attempt)
// and any session error.
func (s *Service) runOnePushSession(
	ctx context.Context,
	streamID domain.StreamCode,
	mediaBufferID domain.StreamCode,
	dest domain.PushDestination,
	preserved *rtmpPushPackager,
) (*rtmpPushPackager, error) {
	sub, err := s.buf.Subscribe(mediaBufferID)
	if err != nil {
		slog.Warn("publisher: RTMP push subscribe failed",
			"stream_code", streamID, "url", dest.URL, "err", err)
		return preserved, err
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
	return p, sessionErr
}
