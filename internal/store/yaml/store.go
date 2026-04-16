// Package yaml provides a single-file YAML implementation of the store repositories.
// All data (streams, recordings, hooks, global settings) is stored in one file
// with top-level keys:
//
//	streams:
//	  <code>: { ... }
//	recordings:
//	  <id>: { ... }
//	hooks:
//	  <id>: { ... }
//	global:
//	  <key>: <raw JSON string>
//
// Writes are atomic: data is marshalled to a .tmp file then renamed into place.
// Intended for development and lightweight single-node deployments.
package yaml

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/store"
	"gopkg.in/yaml.v3"
)

// dbFile is the name of the single data file inside the configured directory.
const dbFile = "open_streamer.yaml"

// db is the top-level structure persisted to disk.
type db struct {
	Streams    map[string]*domain.Stream    `yaml:"streams,omitempty"`
	Recordings map[string]*domain.Recording `yaml:"recordings,omitempty"`
	Hooks      map[string]*domain.Hook      `yaml:"hooks,omitempty"`
	Global     map[string]string            `yaml:"global,omitempty"` // raw JSON strings
}

// Store is a YAML-backed implementation of all repositories.
type Store struct {
	file string
	mu   sync.RWMutex
}

// New creates a Store that persists data to a single file inside dir.
// The directory is created if it does not exist.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("yaml store: mkdir %s: %w", dir, err)
	}
	return &Store{file: filepath.Join(dir, dbFile)}, nil
}

// Streams returns a StreamRepository backed by this Store.
func (s *Store) Streams() store.StreamRepository { return &streamRepo{s} }

// Recordings returns a RecordingRepository backed by this Store.
func (s *Store) Recordings() store.RecordingRepository { return &recordingRepo{s} }

// Hooks returns a HookRepository backed by this Store.
func (s *Store) Hooks() store.HookRepository { return &hookRepo{s} }

// Settings returns a SettingsRepository backed by this Store.
func (s *Store) Settings() store.SettingsRepository { return &settingsRepo{s} }

// --- internal helpers ---

// readDB reads and deserialises the data file. Nil maps are initialised.
// Caller must hold at least s.mu.RLock.
func (s *Store) readDB() (db, error) {
	data, err := os.ReadFile(s.file)
	if os.IsNotExist(err) {
		return emptyDB(), nil
	}
	if err != nil {
		return db{}, fmt.Errorf("yaml store: read: %w", err)
	}
	var d db
	if err := yaml.Unmarshal(data, &d); err != nil {
		return db{}, fmt.Errorf("yaml store: unmarshal: %w", err)
	}
	if d.Streams == nil {
		d.Streams = make(map[string]*domain.Stream)
	}
	if d.Recordings == nil {
		d.Recordings = make(map[string]*domain.Recording)
	}
	if d.Hooks == nil {
		d.Hooks = make(map[string]*domain.Hook)
	}
	return d, nil
}

// writeDB marshals and atomically writes the data file.
// Caller must hold s.mu.Lock.
func (s *Store) writeDB(d db) error {
	data, err := yaml.Marshal(d)
	if err != nil {
		return fmt.Errorf("yaml store: marshal: %w", err)
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("yaml store: write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.file); err != nil {
		return fmt.Errorf("yaml store: rename: %w", err)
	}
	return nil
}

// readAll reads the full database under a read lock and passes it to fn.
func (s *Store) readAll(fn func(db) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, err := s.readDB()
	if err != nil {
		return err
	}
	return fn(d)
}

// modify performs an atomic read-modify-write under a write lock.
func (s *Store) modify(fn func(*db) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.readDB()
	if err != nil {
		return err
	}
	if err := fn(&d); err != nil {
		return err
	}
	return s.writeDB(d)
}

func emptyDB() db {
	return db{
		Streams:    make(map[string]*domain.Stream),
		Recordings: make(map[string]*domain.Recording),
		Hooks:      make(map[string]*domain.Hook),
	}
}

// --- StreamRepository ---

type streamRepo struct{ s *Store }

// Save implements store.StreamRepository.
func (r *streamRepo) Save(_ context.Context, stream *domain.Stream) error {
	return r.s.modify(func(d *db) error {
		d.Streams[string(stream.Code)] = stream
		return nil
	})
}

// FindByCode implements store.StreamRepository.
func (r *streamRepo) FindByCode(_ context.Context, code domain.StreamCode) (*domain.Stream, error) {
	var result *domain.Stream
	err := r.s.readAll(func(d db) error {
		s, ok := d.Streams[string(code)]
		if !ok {
			return fmt.Errorf("stream %s: %w", code, store.ErrNotFound)
		}
		result = s
		return nil
	})
	return result, err
}

// List implements store.StreamRepository.
func (r *streamRepo) List(_ context.Context, _ store.StreamFilter) ([]*domain.Stream, error) {
	var result []*domain.Stream
	err := r.s.readAll(func(d db) error {
		result = make([]*domain.Stream, 0, len(d.Streams))
		for _, s := range d.Streams {
			result = append(result, s)
		}
		slices.SortFunc(result, func(a, b *domain.Stream) int {
			if a.Code < b.Code {
				return -1
			}
			if a.Code > b.Code {
				return 1
			}
			return 0
		})
		return nil
	})
	return result, err
}

// Delete implements store.StreamRepository.
func (r *streamRepo) Delete(_ context.Context, code domain.StreamCode) error {
	return r.s.modify(func(d *db) error {
		delete(d.Streams, string(code))
		return nil
	})
}

// --- RecordingRepository ---

type recordingRepo struct{ s *Store }

// Save implements store.RecordingRepository.
func (r *recordingRepo) Save(_ context.Context, rec *domain.Recording) error {
	return r.s.modify(func(d *db) error {
		d.Recordings[string(rec.ID)] = rec
		return nil
	})
}

// FindByID implements store.RecordingRepository.
func (r *recordingRepo) FindByID(_ context.Context, id domain.RecordingID) (*domain.Recording, error) {
	var result *domain.Recording
	err := r.s.readAll(func(d db) error {
		rec, ok := d.Recordings[string(id)]
		if !ok {
			return fmt.Errorf("recording %s: %w", id, store.ErrNotFound)
		}
		result = rec
		return nil
	})
	return result, err
}

// ListByStream implements store.RecordingRepository.
func (r *recordingRepo) ListByStream(_ context.Context, streamCode domain.StreamCode) ([]*domain.Recording, error) {
	var result []*domain.Recording
	err := r.s.readAll(func(d db) error {
		result = make([]*domain.Recording, 0)
		for _, rec := range d.Recordings {
			if rec.StreamCode == streamCode {
				result = append(result, rec)
			}
		}
		return nil
	})
	return result, err
}

// Delete implements store.RecordingRepository.
func (r *recordingRepo) Delete(_ context.Context, id domain.RecordingID) error {
	return r.s.modify(func(d *db) error {
		delete(d.Recordings, string(id))
		return nil
	})
}

// --- HookRepository ---

type hookRepo struct{ s *Store }

// Save implements store.HookRepository.
func (r *hookRepo) Save(_ context.Context, hook *domain.Hook) error {
	return r.s.modify(func(d *db) error {
		d.Hooks[string(hook.ID)] = hook
		return nil
	})
}

// FindByID implements store.HookRepository.
func (r *hookRepo) FindByID(_ context.Context, id domain.HookID) (*domain.Hook, error) {
	var result *domain.Hook
	err := r.s.readAll(func(d db) error {
		h, ok := d.Hooks[string(id)]
		if !ok {
			return fmt.Errorf("hook %s: %w", id, store.ErrNotFound)
		}
		result = h
		return nil
	})
	return result, err
}

// List implements store.HookRepository.
func (r *hookRepo) List(_ context.Context) ([]*domain.Hook, error) {
	var result []*domain.Hook
	err := r.s.readAll(func(d db) error {
		result = make([]*domain.Hook, 0, len(d.Hooks))
		for _, h := range d.Hooks {
			result = append(result, h)
		}
		return nil
	})
	return result, err
}

// Delete implements store.HookRepository.
func (r *hookRepo) Delete(_ context.Context, id domain.HookID) error {
	return r.s.modify(func(d *db) error {
		delete(d.Hooks, string(id))
		return nil
	})
}

// --- SettingsRepository ---

type settingsRepo struct{ s *Store }

// Get implements store.SettingsRepository.
func (r *settingsRepo) Get(_ context.Context, key string) (json.RawMessage, error) {
	var result json.RawMessage
	err := r.s.readAll(func(d db) error {
		v, ok := d.Global[key]
		if !ok {
			return fmt.Errorf("settings %s: %w", key, store.ErrNotFound)
		}
		result = json.RawMessage(v)
		return nil
	})
	return result, err
}

// Set implements store.SettingsRepository.
func (r *settingsRepo) Set(_ context.Context, key string, value json.RawMessage) error {
	return r.s.modify(func(d *db) error {
		if d.Global == nil {
			d.Global = make(map[string]string)
		}
		d.Global[key] = string(value)
		return nil
	})
}
