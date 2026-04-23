package publisher

// push_rtmp.go — outbound RTMP/RTMPS re-stream (push to external endpoint).
//
// Data flow:
//
//	Buffer Hub → tsmux.FeedWirePacket → tsBuffer → mpeg2.TSDemuxer
//	                                                        │
//	                                        H.264 Annex-B + AAC ADTS frames
//	                                                        │
//	                          lal.AvPacket2RtmpRemuxer → base.RtmpMsg
//	                                                        │
//	                          lal.PushSession.WriteMsg → remote RTMP(S) server
//
// Why lal (q191201771/lal) instead of yapingcat/gomedia:
//
// gomedia's RtmpClient panics on AMF marker bytes it doesn't recognise (we hit
// "unsupport amf type 98" pushing to Flussonic — the panic killed the whole
// service). lal's AMF parser returns errors instead of panicking, and its
// publish handshake is a strict synchronous Push() that returns only after the
// remote replies — no separate publish.start state machine to gate writes
// against, no pending-frame queue.
//
// Codec adapter: lal's PushSession transports already-FLV-encoded RtmpMsg
// (header + payload). Conversion from raw codec bytes (H.264 Annex-B NALU,
// AAC ADTS frame) to FLV tags is delegated to lal.AvPacket2RtmpRemuxer:
//   - Tracks SPS/PPS in the NALU stream and emits AVCDecoderConfigurationRecord
//     as the first video FLV tag automatically.
//   - Strips the 7-byte ADTS header from each AAC frame and emits
//     AudioSpecificConfig as the first audio FLV tag automatically.
//   - The OnRtmpMsg callback receives the wrapped FLV-tag base.RtmpMsg ready
//     to write.
//
// Reconnect: on any Push handshake failure, write error, or session close the
// caller (serveRTMPPush) waits RetryTimeoutSec and starts a fresh packager.
// Each packager builds a fresh remuxer, so SPS/PPS / ASC are re-emitted on
// every reconnect — no codec state needs to survive across sessions.
//
// Input switching: when the ingestor switches input source, the first packet
// of the new source is flagged Discontinuity=true. feedLoop signals the
// packager to end via errDiscontinuity. serveRTMPPush starts a fresh session
// with no retry delay so the new source can take over with minimum gap.
//
// RTMPS: rtmps:// URLs are handled natively by lal via PushSessionOption.TlsConfig.
// Default TLS config uses ServerName from the URL host.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/remux"
	"github.com/q191201771/lal/pkg/rtmp"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
)

const (
	// defaultPushTimeout is the publish handshake budget when dest.TimeoutSec is unset.
	defaultPushTimeout = 10 * time.Second
	// pushHandshakeBackstop adds a small grace period over the lal-internal
	// timeout so we only see our own deadline tick if lal itself wedges.
	pushHandshakeBackstop = 2 * time.Second
)

// errDiscontinuity signals that the input source changed (failover / manual
// switch). The session must be torn down and restarted because the new source
// may have different codec parameters.
var errDiscontinuity = errors.New("input discontinuity")

// rtmpPushPackager holds state for one outbound RTMP push session.
type rtmpPushPackager struct {
	streamID   domain.StreamCode
	url        string
	timeoutSec int

	session *rtmp.PushSession
	remuxer *remux.AvPacket2RtmpRemuxer

	// Base DTS for relative timestamps. TS demuxer emits 90 kHz ticks; lal
	// expects ms. baseDTS captures the first frame's DTS so the published
	// timeline starts at 0 regardless of source clock origin.
	baseDTS    int64
	baseDTSSet bool

	// connErr holds the first session failure. failOnce ensures the first
	// error wins so subsequent goroutines don't overwrite it.
	connErr   atomic.Pointer[error]
	failOnce  sync.Once
	closeOnce sync.Once

	// tsBuf is the TS buffer fed by feedLoop and read by the demuxer. Stored
	// on the packager so other goroutines (failConn) can close it on
	// connection failure to unblock the demuxer.
	tsBuf *tsBuffer

	// gotDiscontinuity is set by feedLoop when it sees Discontinuity=true on
	// an established session. run() then returns errDiscontinuity and
	// serveRTMPPush starts a fresh session with no backoff delay.
	gotDiscontinuity atomic.Bool
}

// failConn marks the session as failed and tears down the connection so the
// demuxer goroutine unblocks and run() can exit. Idempotent.
func (p *rtmpPushPackager) failConn(err error) {
	p.failOnce.Do(func() {
		p.connErr.Store(&err)
		p.closeConn()
		if p.tsBuf != nil {
			p.tsBuf.Close()
		}
	})
}

// run is the entry point: drives publish handshake, then pumps demuxed frames
// into the lal RTMP session until ctx is cancelled, the input ends, or any
// session error occurs.
func (p *rtmpPushPackager) run(ctx context.Context, sub *buffer.Subscriber) error {
	if err := p.connect(ctx); err != nil {
		return err
	}

	// Surface session-side errors (network drop, server close, write timeout)
	// asynchronously into our connErr so the demuxer loop unwinds promptly.
	go p.watchSessionExit()

	tb := newTSBuffer()
	p.tsBuf = tb
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
		if err != nil && ctx.Err() == nil && !p.gotDiscontinuity.Load() {
			slog.Warn("publisher: RTMP push TS demux ended",
				"stream_code", p.streamID, "url", p.url, "err", err)
		}
	}

	p.closeConn()

	if p.gotDiscontinuity.Load() {
		slog.Info("publisher: RTMP push session ending on discontinuity",
			"stream_code", p.streamID, "url", p.url)
		return errDiscontinuity
	}
	if errPtr := p.connErr.Load(); errPtr != nil {
		return *errPtr
	}
	return nil
}

// watchSessionExit converts an async lal session error into a packager fail
// so the demuxer / feedLoop chain unblocks instead of silently spinning on a
// dead connection.
func (p *rtmpPushPackager) watchSessionExit() {
	if p.session == nil {
		return
	}
	err, ok := <-p.session.WaitChan()
	if !ok {
		return
	}
	if err != nil {
		p.failConn(fmt.Errorf("rtmp session ended: %w", err))
	}
}

// connect drives the lal PushSession handshake. Push() blocks until the
// remote replies with the publish ack, so when it returns nil we are
// guaranteed the server is ready to accept media — no separate ready gate.
func (p *rtmpPushPackager) connect(ctx context.Context) error {
	u, err := url.Parse(p.url)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "rtmp" && u.Scheme != "rtmps" {
		return fmt.Errorf("unsupported scheme %q (only rtmp/rtmps)", u.Scheme)
	}

	timeout := time.Duration(p.timeoutSec) * time.Second
	if timeout <= 0 {
		timeout = defaultPushTimeout
	}

	slog.Info("publisher: RTMP push connecting",
		"stream_code", p.streamID, "url", p.url)

	p.session = rtmp.NewPushSession(func(option *rtmp.PushSessionOption) {
		option.PushTimeoutMs = int(timeout / time.Millisecond)
		// Complex handshake matches FFmpeg/OBS — required by most strict
		// peers (YouTube, Twitch, Flussonic). Plain handshake breaks
		// silently on those.
		option.HandshakeComplexFlag = true
		if u.Scheme == "rtmps" {
			option.TlsConfig = &tls.Config{
				ServerName: u.Hostname(),
				MinVersion: tls.VersionTLS12,
			}
		}
	})

	// Push() blocks. Run it in a goroutine so we can honour ctx cancellation
	// AND apply a backstop deadline (lal-internal PushTimeoutMs is the primary
	// timer; the backstop only fires if lal itself wedges).
	pushDone := make(chan error, 1)
	go func() { pushDone <- p.session.Push(p.url) }()

	select {
	case err := <-pushDone:
		if err != nil {
			_ = p.session.Dispose()
			return fmt.Errorf("push handshake: %w", err)
		}
	case <-ctx.Done():
		_ = p.session.Dispose()
		return ctx.Err()
	case <-time.After(timeout + pushHandshakeBackstop):
		_ = p.session.Dispose()
		return fmt.Errorf("push handshake timeout after %s", timeout)
	}

	slog.Info("publisher: RTMP push publish started",
		"stream_code", p.streamID, "url", p.url)

	// Wire the codec → FLV tag → RtmpMsg adapter. SPS/PPS and AAC ASC are
	// extracted from the input stream itself and emitted as sequence headers
	// the first time they appear.
	p.remuxer = remux.NewAvPacket2RtmpRemuxer().WithOnRtmpMsg(func(msg base.RtmpMsg) {
		if werr := p.session.WriteMsg(msg); werr != nil {
			p.failConn(fmt.Errorf("write rtmp msg: %w", werr))
		}
	})
	p.remuxer.WithOption(func(o *base.AvPacketStreamOption) {
		// TS demuxer emits Annex-B for video and ADTS-prefixed AAC for audio.
		o.VideoFormat = base.AvPacketStreamVideoFormatAnnexb
		o.AudioFormat = base.AvPacketStreamAudioFormatAdtsAac
	})

	return nil
}

// feedLoop reads packets from sub, reassembles 188-byte TS packets via
// alignedFeed, and pipes them into tb. Runs in its own goroutine.
//
// Discontinuity handling: the ingestor flags the first packet of every
// (re)connect with Discontinuity=true — it has no notion of source switch vs.
// initial start vs. transient reconnect, that's by design. Once we have an
// active push session (connErr nil), any Discontinuity must restart the
// session because remote codec params may have changed.
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
			if pkt.AV != nil && pkt.AV.Discontinuity && p.connErr.Load() == nil {
				p.gotDiscontinuity.Store(true)
				return
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
// Converts the demuxed frame into a base.AvPacket and feeds the lal remuxer.
func (p *rtmpPushPackager) onTSFrame(cid mpeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
	if p.connErr.Load() != nil || len(frame) == 0 {
		return
	}

	var pt base.AvPacketPt
	switch cid { //nolint:exhaustive // RTMP push only carries H.264 video and AAC audio; H.265 needs Enhanced RTMP, MP3 unsupported by most ingest servers
	case mpeg2.TS_STREAM_H264:
		pt = base.AvPacketPtAvc
	case mpeg2.TS_STREAM_AAC:
		pt = base.AvPacketPtAac
	default:
		return
	}

	if !p.baseDTSSet {
		p.baseDTS = int64(dts)
		p.baseDTSSet = true
	}
	// 90 kHz MPEG-TS ticks → milliseconds. Clamp negatives to 0 in case of
	// out-of-order frames or wrap-around near the base.
	relDTS := (int64(dts) - p.baseDTS) / 90
	if relDTS < 0 {
		relDTS = 0
	}
	relPTS := (int64(pts) - p.baseDTS) / 90
	if relPTS < 0 {
		relPTS = 0
	}

	p.remuxer.FeedAvPacket(base.AvPacket{
		PayloadType: pt,
		Timestamp:   relDTS,
		Pts:         relPTS,
		Payload:     frame,
	})
}

// closeConn disposes the lal session exactly once. Safe from multiple
// goroutines (failConn from various paths, plus run() at end).
func (p *rtmpPushPackager) closeConn() {
	p.closeOnce.Do(func() {
		if p.session != nil {
			_ = p.session.Dispose()
		}
	})
}

// serveRTMPPush is the publisher goroutine for one outbound PushDestination.
// It creates a fresh packager session per connection attempt and reconnects
// on error or input discontinuity.
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

		slog.Info("publisher: RTMP push session starting",
			"stream_code", streamID, "url", dest.URL, "attempt", attempts)

		sessionErr := s.runOnePushSession(ctx, streamID, mediaBufferID, dest)

		if ctx.Err() != nil {
			return
		}

		if errors.Is(sessionErr, errDiscontinuity) {
			slog.Info("publisher: RTMP push input discontinuity, restarting session",
				"stream_code", streamID, "url", dest.URL)
			attempts = 0
			continue
		}

		if sessionErr != nil {
			slog.Warn("publisher: RTMP push session ended, retrying",
				"stream_code", streamID, "url", dest.URL, "err", sessionErr,
				"retry_in", retryDelay)
		} else {
			slog.Info("publisher: RTMP push session ended cleanly, retrying",
				"stream_code", streamID, "url", dest.URL,
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
// then unsubscribes.
func (s *Service) runOnePushSession(
	ctx context.Context,
	streamID domain.StreamCode,
	mediaBufferID domain.StreamCode,
	dest domain.PushDestination,
) error {
	sub, err := s.buf.Subscribe(mediaBufferID)
	if err != nil {
		slog.Warn("publisher: RTMP push subscribe failed",
			"stream_code", streamID, "url", dest.URL, "err", err)
		return err
	}

	p := &rtmpPushPackager{
		streamID:   streamID,
		url:        dest.URL,
		timeoutSec: dest.TimeoutSec,
	}

	sessionErr := p.run(ctx, sub)
	s.buf.Unsubscribe(mediaBufferID, sub)
	return sessionErr
}
