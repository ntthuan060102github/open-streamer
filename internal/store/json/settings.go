package json

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ntt0601zcoder/open-streamer/internal/store"
)

type settingsRepo struct{ s *Store }

// Get implements store.SettingsRepository.
func (r *settingsRepo) Get(_ context.Context, key string) (json.RawMessage, error) {
	var result json.RawMessage
	err := r.s.readAll(func(d db) error {
		v, ok := d.Global[key]
		if !ok {
			return fmt.Errorf("settings %s: %w", key, store.ErrNotFound)
		}
		result = v
		return nil
	})
	return result, err
}

// Set implements store.SettingsRepository.
func (r *settingsRepo) Set(_ context.Context, key string, value json.RawMessage) error {
	return r.s.modify(func(d *db) error {
		if d.Global == nil {
			d.Global = make(map[string]json.RawMessage)
		}
		d.Global[key] = value
		return nil
	})
}
