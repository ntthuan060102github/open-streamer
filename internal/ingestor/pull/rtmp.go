package pull

// rtmp.go — RTMP pull ingestor using gomedia/go-rtmp.
//
// Migrated from nareix/joy4 (last commit 2020, no Enhanced RTMP support)
// to gomedia/go-rtmp so the reader recognises modern codecs over RTMP:
//
//   - H.264 (codec ID 7, original RTMP)
//   - H.265 / HEVC (codec ID 12, Enhanced RTMP "hvc1" / "hev1")
//   - AAC (codec ID 10, original RTMP)
//
// Frame format guarantees from gomedia (see go-flv/flv-demuxer.go):
//
//   - H.264 / H.265 access units arrive in Annex-B form with SPS/PPS
//     (and VPS for H.265) already prepended on every IDR. We do NOT need
//     to extract codec config from a Streams() probe — gomedia tracks
//     the sequence header internally and re-emits it on each keyframe.
//   - AAC frames arrive with the 7-byte ADTS header already prepended
//     (gomedia stores the AudioSpecificConfig from the sequence header
//     and wraps every subsequent frame).
//
// This eliminates the manual AVCC→Annex-B conversion, manual SPS/PPS
// prefix tracking, and manual ADTS wrapping the joy4 path used to do.
//
// I/O architecture differs from joy4: gomedia is push-driven — the user
// owns the TCP socket and feeds bytes via cli.Input(). We therefore run a
// single goroutine that reads the TCP socket and forwards bytes to the
// client; the client invokes OnFrame on that same goroutine for each
// decoded access unit, which we wrap into a domain.AVPacket and send to
// a buffered channel that ReadPackets drains.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/yapingcat/gomedia/go-codec"
	gomediartmp "github.com/yapingcat/gomedia/go-rtmp"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// rtmpChanSize is the buffered AVPacket channel depth between the network
// read goroutine and ReadPackets. 16384 packets ≈ several seconds at the
// highest bitrates we expect — gives ReadPackets ample slack across GC
// pauses while bounding memory at ~16 MB worst-case.
const rtmpChanSize = 16384

// rtmpReadBufSize is the per-syscall TCP read buffer fed into the gomedia
// client. 32 KiB matches the upstream test suite's value and is a sweet
// spot for reasonable RTMP bitrates without too many syscalls.
const rtmpReadBufSize = 32 * 1024

// rtmpDefaultDialTimeout caps the TCP dial + RTMP handshake + play-start
// negotiation. Once Open returns the connection runs unbounded; this only
// guards the initial setup.
const rtmpDefaultDialTimeout = 10 * time.Second

// rtmpPlayStartCode is the gomedia OnStatus event that signals the play
// command was accepted by the server and frames are about to start
// flowing. Open blocks until either this fires or the negotiation errors.
const rtmpPlayStartCode = "NetStream.Play.Start"

// RTMPReader pulls a remote RTMP play stream and emits domain.AVPacket.
//
// Lifecycle: NewRTMPReader → Open (TCP dial + RTMP handshake + play
// negotiation, blocks until first frame is imminent) → ReadPackets loop →
// Close. Open errors out cleanly without leaking goroutines on dial /
// handshake / play-start failures. Close is idempotent; concurrent calls
// from the worker (failover) and Read loop are safe.
type RTMPReader struct {
	input domain.Input

	mu   sync.Mutex
	conn net.Conn
	cli  *gomediartmp.RtmpClient

	pkts    chan domain.AVPacket
	done    chan struct{}
	pumpWg  sync.WaitGroup
	closing bool
}

// NewRTMPReader constructs a reader for the given input without opening
// any connections. URL parse / dial happens in Open.
func NewRTMPReader(input domain.Input) *RTMPReader {
	return &RTMPReader{input: input}
}

// Open dials the upstream, completes the RTMP handshake, sends the play
// command, and waits for NetStream.Play.Start before returning. After this
// returns nil, ReadPackets is ready to drain frames.
func (r *RTMPReader) Open(ctx context.Context) error {
	hostPort, err := rtmpHostPort(r.input.URL)
	if err != nil {
		return err
	}

	dialTimeout := rtmpDefaultDialTimeout
	if r.input.Net.TimeoutSec > 0 {
		dialTimeout = time.Duration(r.input.Net.TimeoutSec) * time.Second
	}

	dialer := &net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		return fmt.Errorf("rtmp reader: dial %q: %w", hostPort, err)
	}

	pkts := make(chan domain.AVPacket, rtmpChanSize)
	done := make(chan struct{})
	playStart := make(chan struct{}, 1)
	openErr := make(chan error, 1)

	cli := gomediartmp.NewRtmpClient(gomediartmp.WithComplexHandshake())

	cli.OnError(func(code, describe string) {
		slog.Warn("rtmp reader: protocol error",
			"url", r.input.URL, "code", code, "describe", describe)
		select {
		case openErr <- fmt.Errorf("rtmp reader: %s: %s", code, describe):
		default:
		}
	})

	cli.OnStatus(func(code, level, describe string) {
		slog.Debug("rtmp reader: status",
			"url", r.input.URL, "code", code, "level", level, "describe", describe)
		if code == rtmpPlayStartCode && level == "status" {
			select {
			case playStart <- struct{}{}:
			default:
			}
		}
	})

	cli.OnFrame(func(cid codec.CodecID, pts, dts uint32, frame []byte) {
		emitRTMPFrame(cid, frame, pts, dts, pkts, done)
	})

	cli.SetOutput(func(data []byte) error {
		_, werr := conn.Write(data)
		return werr
	})

	r.mu.Lock()
	r.conn = conn
	r.cli = cli
	r.pkts = pkts
	r.done = done
	r.closing = false
	r.mu.Unlock()

	// Spawn the network read pump BEFORE Start so RTMP control responses
	// reach the client and trigger the play-start status event.
	r.pumpWg.Add(1)
	go r.readPump(pkts, done)

	cli.Start(r.input.URL)

	select {
	case <-playStart:
		return nil
	case err := <-openErr:
		r.abortOpen()
		return err
	case <-ctx.Done():
		r.abortOpen()
		return ctx.Err()
	case <-time.After(dialTimeout):
		r.abortOpen()
		return fmt.Errorf("rtmp reader: play-start timeout after %s", dialTimeout)
	}
}

// readPump reads bytes from the TCP connection and feeds them into the
// gomedia client. Exits when the connection closes (EOF, network error,
// or Close-driven local close). The frame callback registered via
// cli.OnFrame runs on this goroutine.
func (r *RTMPReader) readPump(pkts chan<- domain.AVPacket, done <-chan struct{}) {
	defer r.pumpWg.Done()
	defer func() {
		// Drain-by-close so ReadPackets unblocks with io.EOF.
		r.mu.Lock()
		// Only close if we haven't already done so via Close — closing a
		// closed channel panics.
		if !r.closing {
			r.closing = true
			close(pkts)
		}
		r.mu.Unlock()
	}()

	buf := make([]byte, rtmpReadBufSize)
	for {
		select {
		case <-done:
			return
		default:
		}
		n, err := r.conn.Read(buf)
		if n > 0 {
			if inputErr := r.cli.Input(buf[:n]); inputErr != nil {
				slog.Debug("rtmp reader: client input error",
					"url", r.input.URL, "err", inputErr)
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !isClosedConn(err) {
				slog.Debug("rtmp reader: tcp read ended",
					"url", r.input.URL, "err", err)
			}
			return
		}
		_ = pkts // silence unused if select branch didn't run
	}
}

// abortOpen tears down a partial Open — used by Open's error paths so the
// caller doesn't have to track which side effects happened. Safe even
// when the read pump has already exited.
func (r *RTMPReader) abortOpen() {
	r.mu.Lock()
	conn := r.conn
	r.conn = nil
	r.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	r.pumpWg.Wait()
}

// ReadPackets blocks until at least one AVPacket is available, then drains
// up to 256 more so the caller's per-call overhead amortises across a
// burst. Returns (nil, io.EOF) on stream end and (nil, ctx.Err()) on
// caller cancellation.
func (r *RTMPReader) ReadPackets(ctx context.Context) ([]domain.AVPacket, error) {
	r.mu.Lock()
	ch := r.pkts
	r.mu.Unlock()
	if ch == nil {
		return nil, io.EOF
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case p, ok := <-ch:
		if !ok {
			return nil, io.EOF
		}
		batch := []domain.AVPacket{p}
		for len(batch) < 256 {
			select {
			case p2, ok2 := <-ch:
				if !ok2 {
					return batch, nil
				}
				batch = append(batch, p2)
			default:
				return batch, nil
			}
		}
		return batch, nil
	}
}

// Close tears down the RTMP connection and waits for the read pump to
// exit. Idempotent.
func (r *RTMPReader) Close() error {
	r.mu.Lock()
	conn := r.conn
	r.conn = nil
	closing := r.closing
	r.closing = true
	r.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
	r.pumpWg.Wait()

	r.mu.Lock()
	pkts := r.pkts
	r.pkts = nil
	r.mu.Unlock()
	if !closing && pkts != nil {
		// Read pump's deferred close didn't run yet (rare race) — close
		// here to unblock any waiting ReadPackets.
		safeClosePktChan(pkts)
	}
	return nil
}

// emitRTMPFrame translates a gomedia OnFrame callback into a domain.AVPacket
// and pushes it onto the buffered channel. Drops silently when done is
// closed (rather than blocking the network read goroutine forever).
func emitRTMPFrame(
	cid codec.CodecID,
	frame []byte,
	pts, dts uint32,
	pkts chan<- domain.AVPacket,
	done <-chan struct{},
) {
	if len(frame) == 0 {
		return
	}
	p, ok := buildRTMPAVPacket(cid, frame, pts, dts)
	if !ok {
		return
	}
	select {
	case pkts <- p:
	case <-done:
	}
}

// buildRTMPAVPacket maps a gomedia codec ID to a domain.AVPacket. Returns
// ok=false for codecs we don't surface (legacy RTMP audio formats, future
// Enhanced RTMP additions we haven't wired yet).
//
//nolint:exhaustive // VP8 / G711 / Opus / MP3 over RTMP are intentionally dropped — see roadmap in docs/CODEC.md.
func buildRTMPAVPacket(
	cid codec.CodecID,
	frame []byte,
	pts, dts uint32,
) (domain.AVPacket, bool) {
	switch cid {
	case codec.CODECID_VIDEO_H264:
		return domain.AVPacket{
			Codec:    domain.AVCodecH264,
			Data:     frame,
			PTSms:    uint64(pts),
			DTSms:    uint64(dts),
			KeyFrame: isH264IDRAnnexB(frame),
		}, true
	case codec.CODECID_VIDEO_H265:
		return domain.AVPacket{
			Codec:    domain.AVCodecH265,
			Data:     frame,
			PTSms:    uint64(pts),
			DTSms:    uint64(dts),
			KeyFrame: isH265IRAPAnnexB(frame),
		}, true
	case codec.CODECID_AUDIO_AAC:
		return domain.AVPacket{
			Codec: domain.AVCodecAAC,
			Data:  frame,
			PTSms: uint64(pts),
			DTSms: uint64(dts),
		}, true
	}
	return domain.AVPacket{}, false
}

// h264NALTypeIDR is the H.264 NAL unit type for IDR slices (per ISO 14496-10).
// Spelled here instead of using codec.H264_NAL_I_SLICE so the byte-level
// comparison stays well-typed.
const h264NALTypeIDR = 5

// isH264IDRAnnexB returns true when the Annex-B byte sequence contains any
// NAL unit of type 5 (IDR). Walks start codes only — no full NAL parsing.
func isH264IDRAnnexB(b []byte) bool {
	for _, t := range walkAnnexBNALTypesH264(b) {
		if t&0x1F == h264NALTypeIDR {
			return true
		}
	}
	return false
}

// isH265IRAPAnnexB returns true when the Annex-B byte sequence contains
// any IRAP NAL unit (type 16-23) — IDR / CRA / BLA, the access points a
// HEVC decoder can resync on.
func isH265IRAPAnnexB(b []byte) bool {
	for _, t := range walkAnnexBNALTypesH265(b) {
		if t >= 16 && t <= 23 {
			return true
		}
	}
	return false
}

// walkAnnexBNALTypesH264 yields the NAL header byte of each NAL unit in b.
// Recognises both 3-byte (00 00 01) and 4-byte (00 00 00 01) start codes.
func walkAnnexBNALTypesH264(b []byte) []byte {
	out := make([]byte, 0, 4)
	i := 0
	for i+3 < len(b) {
		switch {
		case b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1:
			if i+4 < len(b) {
				out = append(out, b[i+4])
			}
			i += 4
		case b[i] == 0 && b[i+1] == 0 && b[i+2] == 1:
			if i+3 < len(b) {
				out = append(out, b[i+3])
			}
			i += 3
		default:
			i++
		}
	}
	return out
}

// walkAnnexBNALTypesH265 yields the H.265 NAL type field (top 6 bits of
// the first NAL header byte after the start code). Distinct from H.264
// NAL type bits because HEVC uses a 2-byte NAL header where the type is
// in bits 1-6 of byte 0.
func walkAnnexBNALTypesH265(b []byte) []byte {
	out := make([]byte, 0, 4)
	i := 0
	for i+3 < len(b) {
		switch {
		case b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1:
			if i+4 < len(b) {
				out = append(out, (b[i+4]>>1)&0x3F)
			}
			i += 4
		case b[i] == 0 && b[i+1] == 0 && b[i+2] == 1:
			if i+3 < len(b) {
				out = append(out, (b[i+3]>>1)&0x3F)
			}
			i += 3
		default:
			i++
		}
	}
	return out
}

// rtmpHostPort extracts "host:port" from an rtmp:// URL, defaulting the
// port to 1935 when omitted. Used because the gomedia client doesn't dial
// the socket itself — we own it.
func rtmpHostPort(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("rtmp reader: parse url %q: %w", rawURL, err)
	}
	host := u.Host
	if host == "" {
		return "", fmt.Errorf("rtmp reader: url %q has no host", rawURL)
	}
	if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
		host = net.JoinHostPort(host, "1935")
	}
	return host, nil
}

// isClosedConn reports whether err is a "use of closed network connection"
// — the standard-library error returned by net.Conn.Read after Close.
// Logged at debug level rather than warn because it's the expected exit
// path on shutdown.
func isClosedConn(err error) bool {
	return err != nil && errors.Is(err, net.ErrClosed)
}

// safeClosePktChan closes the channel if it isn't already closed. Used by
// Close as a defensive double-close guard for a rare race with the read
// pump's deferred close.
func safeClosePktChan(ch chan domain.AVPacket) {
	defer func() { _ = recover() }()
	close(ch)
}
