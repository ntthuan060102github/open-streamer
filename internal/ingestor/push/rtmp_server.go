package push

// rtmp_server.go — RTMP push-ingest server using gomedia.
//
// Architecture: each encoder pushes to this server (publish mode). When a
// publisher is accepted, we start an internal RTMP pull worker that connects
// back to this server in play mode. The stable joy4-based RTMPReader handles
// all codec conversions and buffer writes — the same code path as a normal
// pull ingest.
//
//   Encoder ──RTMP push──► RTMPServer (gomedia)
//                                 │ dispatch goroutine
//                                 ▼
//                          rtmpSub.run() ──RTMP play──► joy4 RTMPReader
//                                                             │
//                                                             ▼
//                                                        Buffer Hub

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	gocodec "github.com/yapingcat/gomedia/go-codec"
	gortmp "github.com/yapingcat/gomedia/go-rtmp"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// ConnectFunc is invoked when an encoder starts publishing a registered stream.
// The implementation should call ingestor.Service.startPullWorker for the
// given input (which points to this server's play endpoint).
type ConnectFunc func(ctx context.Context, streamID, bufferWriteID domain.StreamCode, input domain.Input) error

// PlayFunc is invoked when an external play client connects for a key that has
// no active ingest relay. It should stream frames to the client via writeFrame
// until ctx is cancelled or an error occurs. Return a non-nil error only when
// the stream is not available (causes the client to receive NOTFOUND).
type PlayFunc func(ctx context.Context, key string, writeFrame func(cid gocodec.CodecID, data []byte, pts, dts uint32) error) error

// RTMPServer accepts RTMP push connections from encoders, validates them
// against the push Registry, and relays each live stream to an internal
// RTMP play subscriber (joy4 RTMPReader) via a loopback connection.
// External play clients (VLC, ffplay) are served via an optional PlayFunc.
type RTMPServer struct {
	addr      string
	port      string // port extracted from addr, used to build loopback pull URL
	registry  Registry
	onConnect ConnectFunc

	mu       sync.Mutex
	hub      map[string]*rtmpRelay // stream key → active relay
	playFunc PlayFunc              // optional; serves external play clients
}

// SetPlayFunc registers a handler for external RTMP play clients.
// Must be called before Run. Safe to call concurrently.
func (s *RTMPServer) SetPlayFunc(fn PlayFunc) {
	s.mu.Lock()
	s.playFunc = fn
	s.mu.Unlock()
}

// NewRTMPServer creates an RTMPServer. addr is the TCP bind address (e.g. ":1935").
func NewRTMPServer(addr string, registry Registry, onConnect ConnectFunc) (*RTMPServer, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("rtmp server: invalid addr %q: %w", addr, err)
	}
	return &RTMPServer{
		addr:      addr,
		port:      port,
		registry:  registry,
		onConnect: onConnect,
		hub:       make(map[string]*rtmpRelay),
	}, nil
}

// Run binds the TCP listener and accepts RTMP connections until ctx is cancelled.
func (s *RTMPServer) Run(ctx context.Context) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("rtmp server: listen %q: %w", s.addr, err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	slog.Info("rtmp server: listening", "addr", s.addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("rtmp server: accept: %w", err)
			}
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *RTMPServer) getRelay(key string) *rtmpRelay {
	s.mu.Lock()
	r := s.hub[key]
	s.mu.Unlock()
	return r
}

func (s *RTMPServer) setRelay(key string, r *rtmpRelay) {
	s.mu.Lock()
	s.hub[key] = r
	s.mu.Unlock()
}

func (s *RTMPServer) deleteRelay(key string) {
	s.mu.Lock()
	delete(s.hub, key)
	s.mu.Unlock()
}

// handleConn drives a single TCP connection. It can be either a publisher
// (encoder pushing a stream), an internal joy4 pull worker, or an external
// play client served by the registered PlayFunc.
//
// recover guard: gomedia's AMF/chunk parser panics on malformed input from
// untrusted RTMP peers (e.g. "unsupport amf type N"). One bad publisher would
// otherwise crash the entire server. We log + close the connection instead.
func (s *RTMPServer) handleConn(ctx context.Context, conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("rtmp server: connection handler panic recovered",
				"remote", conn.RemoteAddr().String(),
				"err", r,
			)
			_ = conn.Close()
		}
	}()
	// connCtx is cancelled when this connection closes, stopping any play session.
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	handle := gortmp.NewRtmpServerHandle()
	handle.SetOutput(func(b []byte) error {
		_, err := conn.Write(b)
		return err
	})

	// Track whether this connection ended up as a publisher.
	var (
		isPublish  bool
		publishKey string
		relay      *rtmpRelay
	)

	handle.OnPublish(func(app, key string) gortmp.StatusCode {
		bufWriteID, streamID, _, err := s.registry.Acquire(key)
		if err != nil {
			slog.Warn("rtmp server: rejected publisher", "key", key, "err", err)
			return gortmp.NETSTREAM_CONNECT_REJECTED
		}

		r := newRTMPRelay(key, streamID)
		s.setRelay(key, r)

		// Start the internal pull worker. It will dial this same server in play
		// mode and feed packets into the buffer via the stable RTMPReader path.
		pullURL := fmt.Sprintf("rtmp://localhost:%s/live/%s", s.port, key)
		if err := s.onConnect(ctx, streamID, bufWriteID, domain.Input{URL: pullURL}); err != nil {
			slog.Error("rtmp server: start pull worker failed", "key", key, "err", err)
			s.deleteRelay(key)
			s.registry.Release(key)
			return gortmp.NETSTREAM_CONNECT_REJECTED
		}

		r.start()

		// Wire incoming frames to the relay dispatch channel.
		handle.OnFrame(func(cid gocodec.CodecID, pts, dts uint32, frame []byte) {
			data := make([]byte, len(frame))
			copy(data, frame)
			select {
			case r.frames <- &rtmpFrame{cid: cid, pts: pts, dts: dts, data: data}:
			default:
				// Drop frame if the relay is falling behind.
			}
		})

		isPublish = true
		publishKey = key
		relay = r

		slog.Info("rtmp server: publisher connected", "key", key, "stream_id", streamID)
		return gortmp.NETSTREAM_PUBLISH_START
	})

	handle.OnPlay(func(app, key string, start, duration float64, reset bool) gortmp.StatusCode {
		// Accept play if there is an active ingest relay OR a play handler is registered.
		if s.getRelay(key) != nil {
			return gortmp.NETSTREAM_PLAY_START
		}
		s.mu.Lock()
		fn := s.playFunc
		s.mu.Unlock()
		if fn != nil {
			return gortmp.NETSTREAM_PLAY_START
		}
		slog.Warn("rtmp server: play rejected, stream not active", "key", key)
		return gortmp.NETSTREAM_PLAY_NOTFOUND
	})

	handle.OnStateChange(func(newState gortmp.RtmpState) {
		if newState != gortmp.STATE_RTMP_PLAY_START {
			return
		}
		key := handle.GetStreamName()

		// Internal joy4 loopback pull → serve via relay.
		r := s.getRelay(key)
		if r != nil {
			sub := newRTMPSub(conn, handle)
			r.addSub(sub)
			go func() {
				sub.run()
				r.removeSub(sub)
			}()
			return
		}

		// External play client → serve via PlayFunc.
		s.mu.Lock()
		fn := s.playFunc
		s.mu.Unlock()
		if fn == nil {
			_ = conn.Close()
			return
		}
		slog.Info("rtmp server: external play client connected", "key", key)
		go func() {
			err := fn(connCtx, key, func(cid gocodec.CodecID, data []byte, pts, dts uint32) error {
				return handle.WriteFrame(cid, data, pts, dts)
			})
			if err != nil {
				slog.Debug("rtmp server: play session ended", "key", key, "err", err)
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
			slog.Warn("rtmp server: handle input error", "err", err)
			break
		}
	}
	_ = conn.Close()

	if isPublish {
		if relay != nil {
			relay.stop() // closes subscriber conns so joy4 gets EOF
			s.deleteRelay(publishKey)
		}
		s.registry.Release(publishKey)
		slog.Info("rtmp server: publisher disconnected", "key", publishKey)
	}
}

// ─── rtmpFrame ───────────────────────────────────────────────────────────────

type rtmpFrame struct {
	cid  gocodec.CodecID
	data []byte
	pts  uint32
	dts  uint32
}

// ─── rtmpRelay ───────────────────────────────────────────────────────────────

// rtmpRelay fans out frames from one publisher to all connected subscribers.
type rtmpRelay struct {
	key      string
	streamID domain.StreamCode

	frames chan *rtmpFrame // publisher writes here

	mu   sync.Mutex
	subs []*rtmpSub

	quit chan struct{}
	die  sync.Once
}

func newRTMPRelay(key string, streamID domain.StreamCode) *rtmpRelay {
	return &rtmpRelay{
		key:      key,
		streamID: streamID,
		frames:   make(chan *rtmpFrame, 512),
		quit:     make(chan struct{}),
	}
}

func (r *rtmpRelay) start() { go r.dispatch() }

func (r *rtmpRelay) stop() {
	r.die.Do(func() {
		// Close subscriber connections first so joy4 gets EOF and the pull
		// worker exits cleanly before we signal the dispatch goroutine.
		r.mu.Lock()
		subs := make([]*rtmpSub, len(r.subs))
		copy(subs, r.subs)
		r.mu.Unlock()
		for _, sub := range subs {
			sub.stop()           // signal quit channel
			_ = sub.conn.Close() // unblock any pending TCP write
		}
		close(r.quit)
	})
}

func (r *rtmpRelay) addSub(sub *rtmpSub) {
	r.mu.Lock()
	r.subs = append(r.subs, sub)
	r.mu.Unlock()
}

func (r *rtmpRelay) removeSub(sub *rtmpSub) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, s := range r.subs {
		if s == sub {
			r.subs = append(r.subs[:i], r.subs[i+1:]...)
			return
		}
	}
}

func (r *rtmpRelay) dispatch() {
	for {
		select {
		case f := <-r.frames:
			r.mu.Lock()
			subs := make([]*rtmpSub, len(r.subs))
			copy(subs, r.subs)
			r.mu.Unlock()
			for _, sub := range subs {
				sub.enqueue(f)
			}
		case <-r.quit:
			return
		}
	}
}

// ─── rtmpSub ─────────────────────────────────────────────────────────────────

// rtmpSub serves frames to one play-mode client (our internal joy4 pull worker).
type rtmpSub struct {
	handle *gortmp.RtmpServerHandle
	conn   net.Conn

	mu        sync.Mutex
	pending   []*rtmpFrame
	frameCome chan struct{}

	quit chan struct{}
	die  sync.Once
}

func newRTMPSub(conn net.Conn, handle *gortmp.RtmpServerHandle) *rtmpSub {
	return &rtmpSub{
		handle:    handle,
		conn:      conn,
		frameCome: make(chan struct{}, 1),
		quit:      make(chan struct{}),
	}
}

func (sub *rtmpSub) stop() {
	sub.die.Do(func() { close(sub.quit) })
}

func (sub *rtmpSub) enqueue(f *rtmpFrame) {
	sub.mu.Lock()
	sub.pending = append(sub.pending, f)
	sub.mu.Unlock()
	select {
	case sub.frameCome <- struct{}{}:
	default:
	}
}

// run sends frames to the joy4 play client. It waits for the first H.264 IDR
// frame so that WriteFrame generates a fresh FLV sequence header (SPS+PPS)
// before any NALU data — joy4 needs this to build its codec config.
func (sub *rtmpSub) run() {
	defer sub.stop()
	firstVideo := true
	for {
		select {
		case <-sub.frameCome:
			sub.mu.Lock()
			frames := sub.pending
			sub.pending = nil
			sub.mu.Unlock()
			for _, f := range frames {
				if firstVideo {
					isIDR := f.cid == gocodec.CODECID_VIDEO_H264 && gocodec.IsH264IDRFrame(f.data)
					if !isIDR {
						continue // skip until keyframe so decoder can initialise
					}
					firstVideo = false
				}
				if err := sub.handle.WriteFrame(f.cid, f.data, f.pts, f.dts); err != nil {
					slog.Warn("rtmp server: write to subscriber failed", "err", err)
					return
				}
			}
		case <-sub.quit:
			return
		}
	}
}
