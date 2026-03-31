// Package json provides a JSON file-based implementation of the store repositories.
// Intended for development and lightweight single-node deployments.
// Writes are atomic: data is written to a .tmp file then renamed into place.
package json

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/store"
)

// Store is a JSON-backed implementation of all repositories.
type Store struct {
	dir string
	mu  sync.RWMutex
}

// New creates a Store that persists data under dir.
// The directory is created if it does not exist.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("json store: mkdir %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Streams returns a StreamRepository backed by this Store.
func (s *Store) Streams() store.StreamRepository { return &streamRepo{s} }

// Recordings returns a RecordingRepository backed by this Store.
func (s *Store) Recordings() store.RecordingRepository { return &recordingRepo{s} }

// Hooks returns a HookRepository backed by this Store.
func (s *Store) Hooks() store.HookRepository { return &hookRepo{s} }

// --- helpers ---

func (s *Store) readAll(name string, dst any) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("json store: read %s: %w", name, err)
	}
	return json.Unmarshal(data, dst)
}

func (s *Store) writeAll(name string, src any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(src, "", "  ")
	if err != nil {
		return fmt.Errorf("json store: marshal %s: %w", name, err)
	}

	tmp := filepath.Join(s.dir, name+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("json store: write tmp %s: %w", name, err)
	}

	if err := os.Rename(tmp, filepath.Join(s.dir, name)); err != nil {
		return fmt.Errorf("json store: rename %s: %w", name, err)
	}
	return nil
}

// --- StreamRepository ---

type streamRepo struct{ s *Store }

func (r *streamRepo) Save(_ context.Context, stream *domain.Stream) error {
	streams, err := r.load()
	if err != nil {
		return err
	}
	streams[string(stream.Code)] = stream
	return r.s.writeAll("streams.json", streams)
}

func (r *streamRepo) FindByCode(_ context.Context, code domain.StreamCode) (*domain.Stream, error) {
	streams, err := r.load()
	if err != nil {
		return nil, err
	}
	s, ok := streams[string(code)]
	if !ok {
		return nil, fmt.Errorf("stream %s: %w", code, store.ErrNotFound)
	}
	return s, nil
}

func (r *streamRepo) List(_ context.Context, filter store.StreamFilter) ([]*domain.Stream, error) {
	streams, err := r.load()
	if err != nil {
		return nil, err
	}
	result := make([]*domain.Stream, 0, len(streams))
	for _, s := range streams {
		if filter.Status != nil && s.Status != *filter.Status {
			continue
		}
		result = append(result, s)
	}
	return result, nil
}

func (r *streamRepo) Delete(_ context.Context, code domain.StreamCode) error {
	streams, err := r.load()
	if err != nil {
		return err
	}
	delete(streams, string(code))
	return r.s.writeAll("streams.json", streams)
}

func (r *streamRepo) load() (map[string]*domain.Stream, error) {
	m := make(map[string]*domain.Stream)
	return m, r.s.readAll("streams.json", &m)
}

// --- RecordingRepository ---

type recordingRepo struct{ s *Store }

func (r *recordingRepo) Save(_ context.Context, rec *domain.Recording) error {
	recs, err := r.load()
	if err != nil {
		return err
	}
	recs[string(rec.ID)] = rec
	return r.s.writeAll("recordings.json", recs)
}

func (r *recordingRepo) FindByID(_ context.Context, id domain.RecordingID) (*domain.Recording, error) {
	recs, err := r.load()
	if err != nil {
		return nil, err
	}
	rec, ok := recs[string(id)]
	if !ok {
		return nil, fmt.Errorf("recording %s: %w", id, store.ErrNotFound)
	}
	return rec, nil
}

func (r *recordingRepo) ListByStream(_ context.Context, streamCode domain.StreamCode) ([]*domain.Recording, error) {
	recs, err := r.load()
	if err != nil {
		return nil, err
	}
	result := make([]*domain.Recording, 0)
	for _, rec := range recs {
		if rec.StreamCode == streamCode {
			result = append(result, rec)
		}
	}
	return result, nil
}

func (r *recordingRepo) Delete(_ context.Context, id domain.RecordingID) error {
	recs, err := r.load()
	if err != nil {
		return err
	}
	delete(recs, string(id))
	return r.s.writeAll("recordings.json", recs)
}

func (r *recordingRepo) load() (map[string]*domain.Recording, error) {
	m := make(map[string]*domain.Recording)
	return m, r.s.readAll("recordings.json", &m)
}

// --- HookRepository ---

type hookRepo struct{ s *Store }

func (r *hookRepo) Save(_ context.Context, hook *domain.Hook) error {
	hooks, err := r.load()
	if err != nil {
		return err
	}
	hooks[string(hook.ID)] = hook
	return r.s.writeAll("hooks.json", hooks)
}

func (r *hookRepo) FindByID(_ context.Context, id domain.HookID) (*domain.Hook, error) {
	hooks, err := r.load()
	if err != nil {
		return nil, err
	}
	h, ok := hooks[string(id)]
	if !ok {
		return nil, fmt.Errorf("hook %s: %w", id, store.ErrNotFound)
	}
	return h, nil
}

func (r *hookRepo) List(_ context.Context) ([]*domain.Hook, error) {
	hooks, err := r.load()
	if err != nil {
		return nil, err
	}
	result := make([]*domain.Hook, 0, len(hooks))
	for _, h := range hooks {
		result = append(result, h)
	}
	return result, nil
}

func (r *hookRepo) Delete(_ context.Context, id domain.HookID) error {
	hooks, err := r.load()
	if err != nil {
		return err
	}
	delete(hooks, string(id))
	return r.s.writeAll("hooks.json", hooks)
}

func (r *hookRepo) load() (map[string]*domain.Hook, error) {
	m := make(map[string]*domain.Hook)
	return m, r.s.readAll("hooks.json", &m)
}
