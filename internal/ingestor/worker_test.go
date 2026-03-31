package ingestor

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
)

// ---- mock Reader ----

type mockReader struct {
	mu      sync.Mutex
	opens   int
	closes  int
	openErr error
	packets [][]byte
	readErr error
}

func (m *mockReader) Open(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opens++
	return m.openErr
}

func (m *mockReader) Read(_ context.Context) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.packets) > 0 {
		pkt := m.packets[0]
		m.packets = m.packets[1:]
		return pkt, nil
	}
	if m.readErr != nil {
		return nil, m.readErr
	}
	return nil, io.EOF
}

func (m *mockReader) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closes++
	return nil
}

func (m *mockReader) openCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opens
}

func (m *mockReader) closeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closes
}

// ---- waitBackoff ----

func TestWaitBackoff_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	ok := waitBackoff(ctx, 10*time.Second)

	assert.False(t, ok)
	assert.Less(t, time.Since(start), time.Second, "should return immediately on cancelled ctx")
}

func TestWaitBackoff_TimerFires(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	start := time.Now()
	ok := waitBackoff(ctx, 20*time.Millisecond)

	assert.True(t, ok)
	assert.GreaterOrEqual(t, time.Since(start), 20*time.Millisecond)
}

// ---- readLoop ----

func TestReadLoop_WritesPacketsToBuffer(t *testing.T) {
	t.Parallel()

	streamID := domain.StreamCode("test-stream")
	buf := buffer.NewServiceForTesting(128)
	buf.Create(streamID)

	sub, err := buf.Subscribe(streamID)
	require.NoError(t, err)
	defer buf.Unsubscribe(streamID, sub)

	pkt1 := []byte("pkt-one-188-bytes-padded-to-length-xxxxxxxxxxxxxxxxxxxxxxxx")
	pkt2 := []byte("pkt-two-188-bytes-padded-to-length-xxxxxxxxxxxxxxxxxxxxxxxx")

	r := &mockReader{packets: [][]byte{pkt1, pkt2}}

	errCh := make(chan error, 1)
	go func() {
		errCh <- readLoop(context.Background(), streamID, domain.Input{}, r, buf, nil)
	}()

	var received [][]byte
	timeout := time.After(2 * time.Second)
	for len(received) < 2 {
		select {
		case p := <-sub.Recv():
			received = append(received, []byte(p))
		case <-timeout:
			t.Fatal("timed out waiting for packets")
		}
	}

	assert.Equal(t, pkt1, received[0])
	assert.Equal(t, pkt2, received[1])

	// After exhausting packets, readLoop returns io.EOF.
	err = <-errCh
	assert.ErrorIs(t, err, io.EOF)
}

func TestReadLoop_ContextCancelled(t *testing.T) {
	t.Parallel()

	streamID := domain.StreamCode("stream-ctx")
	buf := buffer.NewServiceForTesting(32)
	buf.Create(streamID)

	ctx, cancel := context.WithCancel(context.Background())

	// Reader that blocks until context is cancelled.
	r := &mockReader{readErr: context.Canceled}

	errCh := make(chan error, 1)
	go func() {
		cancel()
		errCh <- readLoop(ctx, streamID, domain.Input{}, r, buf, nil)
	}()

	select {
	case err := <-errCh:
		assert.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop did not return after context cancel")
	}
}

func TestReadLoop_SkipsEmptyPackets(t *testing.T) {
	t.Parallel()

	streamID := domain.StreamCode("stream-empty")
	buf := buffer.NewServiceForTesting(32)
	buf.Create(streamID)

	sub, err := buf.Subscribe(streamID)
	require.NoError(t, err)
	defer buf.Unsubscribe(streamID, sub)

	realPkt := []byte("real-packet")
	// Reader returns: empty, real, EOF
	r := &mockReader{packets: [][]byte{{}, realPkt}}

	done := make(chan error, 1)
	go func() {
		done <- readLoop(context.Background(), streamID, domain.Input{}, r, buf, nil)
	}()

	select {
	case p := <-sub.Recv():
		assert.Equal(t, realPkt, []byte(p))
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
	<-done
}

// ---- runPullWorker ----

func TestRunPullWorker_ReadsAndWritesToBuffer(t *testing.T) {
	t.Parallel()

	streamID := domain.StreamCode("worker-stream")
	buf := buffer.NewServiceForTesting(128)
	buf.Create(streamID)

	sub, err := buf.Subscribe(streamID)
	require.NoError(t, err)
	defer buf.Unsubscribe(streamID, sub)

	pkt := []byte("ts-packet-data")
	r := &mockReader{packets: [][]byte{pkt}}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Worker exits naturally on EOF.
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPullWorker(ctx, streamID, domain.Input{Priority: 0}, r, buf, nil, nil)
	}()

	select {
	case received := <-sub.Recv():
		assert.Equal(t, pkt, []byte(received))
	case <-time.After(2 * time.Second):
		t.Fatal("no packet received")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not exit")
	}
}

func TestRunPullWorker_StopsOnContextCancel(t *testing.T) {
	t.Parallel()

	streamID := domain.StreamCode("cancel-stream")
	buf := buffer.NewServiceForTesting(32)
	buf.Create(streamID)

	// Reader that never gives EOF: simulates infinite stream.
	endlessPkt := make([]byte, 188)
	r := &mockReader{readErr: errors.New("connection reset")}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.packets = [][]byte{endlessPkt, endlessPkt, endlessPkt}
		runPullWorker(ctx, streamID, domain.Input{Priority: 0}, r, buf, nil, nil)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop after ctx cancel")
	}
}

func TestRunPullWorker_ReconnectsAfterOpenError(t *testing.T) {
	t.Parallel()

	streamID := domain.StreamCode("reconnect-stream")
	buf := buffer.NewServiceForTesting(64)
	buf.Create(streamID)

	sub, err := buf.Subscribe(streamID)
	require.NoError(t, err)
	defer buf.Unsubscribe(streamID, sub)

	pkt := []byte("success-packet")
	r := &mockReader{}

	callCount := 0
	openErrOnFirst := &mockReader{
		openErr: errors.New("transient error"),
	}
	_ = openErrOnFirst

	// First Open fails, second succeeds and returns a packet.
	var mu sync.Mutex
	r.openErr = errors.New("first open fails")

	origReader := &controlledReader{
		openFn: func() error {
			mu.Lock()
			defer mu.Unlock()
			callCount++
			if callCount == 1 {
				return errors.New("transient dial error")
			}
			r.openErr = nil
			r.packets = [][]byte{pkt}
			return nil
		},
		readFn: func() ([]byte, error) {
			return r.Read(context.Background())
		},
		closeFn: func() error { return nil },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		runPullWorker(ctx, streamID, domain.Input{Priority: 0}, origReader, buf, nil, nil)
	}()

	select {
	case received := <-sub.Recv():
		assert.Equal(t, pkt, []byte(received))
	case <-time.After(4 * time.Second):
		t.Fatal("worker never recovered after transient open error")
	}
}

// controlledReader is a Reader whose behavior is governed by function fields.
type controlledReader struct {
	openFn  func() error
	readFn  func() ([]byte, error)
	closeFn func() error
}

func (c *controlledReader) Open(_ context.Context) error           { return c.openFn() }
func (c *controlledReader) Read(_ context.Context) ([]byte, error) { return c.readFn() }
func (c *controlledReader) Close() error                           { return c.closeFn() }
