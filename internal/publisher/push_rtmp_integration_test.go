package publisher_test

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yapingcat/gomedia/go-codec"
	gomediartmp "github.com/yapingcat/gomedia/go-rtmp"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/events"
	"github.com/ntt0601zcoder/open-streamer/internal/publisher"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available:", err)
	}
}

// rtmpTestServer is a minimal RTMP server built on gomedia/go-rtmp,
// used solely as the receive sink for our publisher's RTMP push tests.
// Each accepted connection runs an RtmpServerHandle that completes the
// RTMP handshake, accepts the publish command, and counts every video /
// audio frame the server delivers via OnFrame.
//
// Replaced an earlier joy4-based server when the rest of the codebase
// migrated off nareix/joy4 (see internal/ingestor/pull/rtmp.go header).
type rtmpTestServer struct {
	port    int
	packets atomic.Int64

	sessionStarted chan struct{}
	startOnce      sync.Once
	secondSession  chan struct{}
	secondOnce     sync.Once

	mu       sync.Mutex
	sessions int32

	listener net.Listener
}

func newRTMPTestServer(t *testing.T) *rtmpTestServer {
	t.Helper()

	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port

	s := &rtmpTestServer{
		port:           port,
		listener:       ln,
		sessionStarted: make(chan struct{}),
		secondSession:  make(chan struct{}),
	}

	go s.acceptLoop()

	t.Cleanup(func() {
		_ = ln.Close()
	})
	return s
}

// acceptLoop accepts TCP connections and spawns one handler goroutine per
// connection. Exits cleanly when the listener is closed (test teardown).
func (s *rtmpTestServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

// handleConn drives one publish session: RTMP handshake → publish accept →
// frame loop. Session counting fires from inside OnPublish so the "second
// session connected" channel reflects the RTMP-publish-state, not just
// TCP accept (which would race on a partially-established handshake).
func (s *rtmpTestServer) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	handle := gomediartmp.NewRtmpServerHandle()
	handle.OnPublish(func(_, _ string) gomediartmp.StatusCode {
		s.mu.Lock()
		s.sessions++
		n := s.sessions
		s.mu.Unlock()

		if n == 1 {
			s.startOnce.Do(func() { close(s.sessionStarted) })
		}
		if n >= 2 {
			s.secondOnce.Do(func() { close(s.secondSession) })
		}
		return gomediartmp.NETSTREAM_PUBLISH_START
	})
	handle.SetOutput(func(b []byte) error {
		_, err := conn.Write(b)
		return err
	})
	handle.OnFrame(func(_ codec.CodecID, _, _ uint32, _ []byte) {
		s.packets.Add(1)
	})

	buf := make([]byte, 64*1024)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		if err := handle.Input(buf[:n]); err != nil {
			return
		}
	}
}

func (s *rtmpTestServer) rtmpURL(path string) string {
	return fmt.Sprintf("rtmp://127.0.0.1:%d/live/%s", s.port, path)
}

func (s *rtmpTestServer) waitSession(t *testing.T, ch <-chan struct{}, name string, timeout time.Duration) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %s", name)
	}
}

// feedFFmpegTS starts ffmpeg producing TS to stdout and writes raw 188-byte
// TS packets into the buffer. Returns a cancel function.
func feedFFmpegTS(
	t *testing.T,
	streamCode domain.StreamCode,
	buf *buffer.Service,
	markDiscontinuity bool,
) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-re",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=44100",
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-g", "25", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "64k",
		"-f", "mpegts", "pipe:1",
	)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	go func() {
		pktBuf := make([]byte, 188)
		first := markDiscontinuity
		for ctx.Err() == nil {
			if _, err := readFull(stdout, pktBuf); err != nil {
				return
			}
			if first {
				_ = buf.Write(streamCode, buffer.Packet{
					AV: &domain.AVPacket{
						Codec:         domain.AVCodecH264,
						Data:          []byte{0},
						Discontinuity: true,
					},
				})
				first = false
			}
			ts := make([]byte, 188)
			copy(ts, pktBuf)
			_ = buf.Write(streamCode, buffer.Packet{TS: ts})
		}
	}()

	return cancel
}

// readFull reads exactly len(buf) bytes from r.
func readFull(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// TestRTMPPushInputSwitch verifies that the RTMP push session tears down on
// input discontinuity and reconnects with fresh codec probing.
func TestRTMPPushInputSwitch(t *testing.T) {
	requireFFmpeg(t)

	// ── 1. Start RTMP test server (in-process, no Docker) ───────────────────
	srv := newRTMPTestServer(t)
	pushURL := srv.rtmpURL("output")
	t.Logf("RTMP test server on %s", pushURL)

	// ── 2. Create buffer + publisher ────────────────────────────────────────
	streamCode := domain.StreamCode("test_push")
	buf := buffer.NewServiceForTesting(2000)
	buf.Create(streamCode)

	bus := events.New(4, 64)
	pubCfg := config.PublisherConfig{
		HLS:  config.PublisherHLSConfig{Dir: t.TempDir()},
		DASH: config.PublisherDASHConfig{Dir: t.TempDir()},
	}
	pub := publisher.NewServiceForTesting(pubCfg, buf, bus)

	stream := &domain.Stream{
		Code: streamCode,
		Push: []domain.PushDestination{
			{
				URL:             pushURL,
				Enabled:         true,
				TimeoutSec:      10,
				RetryTimeoutSec: 2,
			},
		},
		Protocols: domain.OutputProtocols{},
	}

	// ── 3. Start push publisher ─────────────────────────────────────────────
	require.NoError(t, pub.Start(context.Background(), stream))
	t.Cleanup(func() { pub.Stop(streamCode) })

	// ── 4. Feed source-1 ────────────────────────────────────────────────────
	t.Log("Starting feed-1...")
	cancelFeed1 := feedFFmpegTS(t, streamCode, buf, false)

	// ── 5. Wait for session-1 ───────────────────────────────────────────────
	srv.waitSession(t, srv.sessionStarted, "session-1", 15*time.Second)
	t.Log("Session-1 established!")

	time.Sleep(3 * time.Second)
	pkts1 := srv.packets.Load()
	t.Logf("Packets received before switch: %d", pkts1)
	assert.Greater(t, pkts1, int64(0), "expected packets before switch")

	// ── 6. Simulate input switch ────────────────────────────────────────────
	t.Log("Simulating input switch...")
	cancelFeed1()

	// Discontinuity marker.
	_ = buf.Write(streamCode, buffer.Packet{
		AV: &domain.AVPacket{
			Codec:         domain.AVCodecH264,
			Data:          []byte{0},
			Discontinuity: true,
		},
	})

	// ── 7. Feed source-2 ────────────────────────────────────────────────────
	t.Log("Starting feed-2...")
	cancelFeed2 := feedFFmpegTS(t, streamCode, buf, true)
	defer cancelFeed2()

	// ── 8. Wait for session-2 (push reconnected) ────────────────────────────
	srv.waitSession(t, srv.secondSession, "session-2", 20*time.Second)
	t.Log("Session-2 established — push reconnected after discontinuity!")

	time.Sleep(3 * time.Second)
	pkts2 := srv.packets.Load()
	t.Logf("Total packets received after switch: %d", pkts2)
	assert.Greater(t, pkts2, pkts1, "expected more packets after switch")

	t.Log("RTMP push survived input switch successfully!")
}
