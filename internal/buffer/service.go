// Package buffer implements the Buffer Hub — the central in-memory ring buffer.
// It is the single data pipeline between Ingestor and all consumers (Transcoder, Publisher, DVR).
// Each stream has its own ring buffer; consumers subscribe and get an independent read cursor.
package buffer

import (
	"fmt"
	"sync"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/samber/do/v2"
)

// Subscriber is a read cursor into a stream's ring buffer.
type Subscriber struct {
	ch chan Packet
}

// Recv returns the channel from which the subscriber reads packets.
func (s *Subscriber) Recv() <-chan Packet { return s.ch }

// ringBuffer is a bounded in-memory queue for a single stream.
type ringBuffer struct {
	mu   sync.Mutex
	subs []*Subscriber
}

func (rb *ringBuffer) write(pkt Packet) {
	if pkt.empty() {
		return
	}
	rb.mu.Lock()
	subs := rb.subs
	rb.mu.Unlock()

	for _, s := range subs {
		// Independent copy per subscriber (consumers must not share backing slices).
		pc := clonePacket(pkt)
		select {
		case s.ch <- pc:
		default:
			// slow consumer: drop packet rather than block the writer
		}
	}
}

func (rb *ringBuffer) subscribe(chanSize int) *Subscriber {
	s := &Subscriber{ch: make(chan Packet, chanSize)}
	rb.mu.Lock()
	rb.subs = append(rb.subs, s)
	rb.mu.Unlock()
	return s
}

func (rb *ringBuffer) unsubscribe(s *Subscriber) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	for i, sub := range rb.subs {
		if sub == s {
			rb.subs = append(rb.subs[:i], rb.subs[i+1:]...)
			close(s.ch)
			return
		}
	}
}

// Service manages ring buffers for all active streams.
type Service struct {
	cfg     config.BufferConfig
	mu      sync.RWMutex
	buffers map[domain.StreamCode]*ringBuffer
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[config.BufferConfig](i)
	return &Service{
		cfg:     cfg,
		buffers: make(map[domain.StreamCode]*ringBuffer),
	}, nil
}

// NewServiceForTesting creates a Service without DI, for use in unit tests.
func NewServiceForTesting(capacity int) *Service {
	return &Service{
		cfg:     config.BufferConfig{Capacity: capacity},
		buffers: make(map[domain.StreamCode]*ringBuffer),
	}
}

// Create initialises a ring buffer for the given stream.
func (s *Service) Create(id domain.StreamCode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buffers[id]; !ok {
		s.buffers[id] = &ringBuffer{}
	}
}

// Write pushes a packet into the stream's ring buffer (deep-copied for subscribers).
// Only the active Ingestor goroutine for this stream should call Write.
func (s *Service) Write(id domain.StreamCode, pkt Packet) error {
	s.mu.RLock()
	rb, ok := s.buffers[id]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("buffer: stream %s not found", id)
	}
	rb.write(pkt)
	return nil
}

// Subscribe registers a new consumer for the stream's buffer.
// The caller must call Unsubscribe when done to avoid a goroutine/channel leak.
func (s *Service) Subscribe(id domain.StreamCode) (*Subscriber, error) {
	s.mu.RLock()
	rb, ok := s.buffers[id]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("buffer: stream %s not found", id)
	}
	return rb.subscribe(s.cfg.Capacity), nil
}

// Unsubscribe removes a consumer and closes its channel.
func (s *Service) Unsubscribe(id domain.StreamCode, sub *Subscriber) {
	s.mu.RLock()
	rb, ok := s.buffers[id]
	s.mu.RUnlock()
	if ok {
		rb.unsubscribe(sub)
	}
}

// Delete removes the ring buffer for a stream (call when stream is stopped).
func (s *Service) Delete(id domain.StreamCode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.buffers, id)
}
