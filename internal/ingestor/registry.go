package ingestor

import (
	"fmt"
	"sync"

	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
)

// Registry maps ingest credentials (stream key, SRT stream ID) to the
// destination buffer for a specific stream. Push servers (RTMP, SRT) use this
// to route incoming connections to the correct Buffer Hub slot.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]registryEntry
}

type registryEntry struct {
	streamID domain.StreamCode
	buf      *buffer.Service
}

// NewRegistry constructs an empty Registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]registryEntry)}
}

// Register maps key (stream key or SRT stream ID) to a stream's buffer.
// Registering the same key twice overwrites the previous entry.
func (r *Registry) Register(key string, streamID domain.StreamCode, buf *buffer.Service) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[key] = registryEntry{
		streamID: streamID,
		buf:      buf,
	}
}

// Unregister removes a key from the registry.
func (r *Registry) Unregister(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, key)
}

// Lookup returns the buffer associated with key, or an error if not found.
func (r *Registry) Lookup(key string) (domain.StreamCode, *buffer.Service, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[key]
	if !ok {
		return "", nil, fmt.Errorf("ingestor: no stream registered for key %q", key)
	}
	return e.streamID, e.buf, nil
}
