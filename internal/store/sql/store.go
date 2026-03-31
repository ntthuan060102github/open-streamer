// Package sql provides a PostgreSQL implementation of the store repositories.
// Domain entities are stored as JSONB documents (one row per entity).
//
// Register the driver:
//
//	import _ "github.com/jackc/pgx/v5/stdlib"
//
// DSN example: postgres://user:pass@localhost:5432/dbname?sslmode=disable
//
// Apply schema from scripts/migrations/ before use.
package sql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/store"

	_ "github.com/jackc/pgx/v5/stdlib" // register "pgx" driver for database/sql
)

// Store is a SQL-backed implementation of all repositories.
type Store struct {
	db *sqlx.DB
}

// New opens a PostgreSQL connection using the pgx driver and the given DSN.
func New(dsn string) (*Store, error) {
	db, err := sqlx.Connect("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql store: connect: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	return &Store{db: db}, nil
}

// Close releases the underlying connection pool.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB exposes the underlying pool for health checks or transactions.
func (s *Store) DB() *sqlx.DB {
	return s.db
}

// Streams returns a StreamRepository backed by this Store.
func (s *Store) Streams() store.StreamRepository { return &streamRepo{db: s.db} }

// Recordings returns a RecordingRepository backed by this Store.
func (s *Store) Recordings() store.RecordingRepository { return &recordingRepo{db: s.db} }

// Hooks returns a HookRepository backed by this Store.
func (s *Store) Hooks() store.HookRepository { return &hookRepo{db: s.db} }

// --- StreamRepository ---

type streamRepo struct{ db *sqlx.DB }

const streamUpsert = `
INSERT INTO streams (id, data, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data, updated_at = NOW()
`

func (r *streamRepo) Save(ctx context.Context, stream *domain.Stream) error {
	payload, err := json.Marshal(stream)
	if err != nil {
		return fmt.Errorf("sql streams.Save: marshal: %w", err)
	}
	_, err = r.db.ExecContext(ctx, streamUpsert, string(stream.Code), payload)
	if err != nil {
		return fmt.Errorf("sql streams.Save: %w", err)
	}
	return nil
}

func (r *streamRepo) FindByCode(ctx context.Context, code domain.StreamCode) (*domain.Stream, error) {
	var row struct {
		Data []byte `db:"data"`
	}
	err := r.db.GetContext(ctx, &row, `SELECT data FROM streams WHERE id = $1`, string(code))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sql streams.FindByCode: %w", store.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("sql streams.FindByCode: %w", err)
	}
	payload := row.Data
	var s domain.Stream
	if err := json.Unmarshal(payload, &s); err != nil {
		return nil, fmt.Errorf("sql streams.FindByCode: unmarshal: %w", err)
	}
	return &s, nil
}

func (r *streamRepo) List(ctx context.Context, filter store.StreamFilter) ([]*domain.Stream, error) {
	q := `SELECT data FROM streams`
	var args []any
	if filter.Status != nil {
		q += ` WHERE data->>'status' = $1`
		args = append(args, string(*filter.Status))
	}
	q += ` ORDER BY id`

	var rows []struct {
		Data []byte `db:"data"`
	}
	if err := r.db.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("sql streams.List: %w", err)
	}
	out := make([]*domain.Stream, 0, len(rows))
	for _, row := range rows {
		payload := row.Data
		var s domain.Stream
		if err := json.Unmarshal(payload, &s); err != nil {
			return nil, fmt.Errorf("sql streams.List: unmarshal: %w", err)
		}
		out = append(out, &s)
	}
	return out, nil
}

func (r *streamRepo) Delete(ctx context.Context, code domain.StreamCode) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM streams WHERE id = $1`, string(code))
	if err != nil {
		return fmt.Errorf("sql streams.Delete: %w", err)
	}
	return nil
}

// --- RecordingRepository ---

type recordingRepo struct{ db *sqlx.DB }

const recordingUpsert = `
INSERT INTO recordings (id, stream_id, data, updated_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (id) DO UPDATE SET stream_id = EXCLUDED.stream_id, data = EXCLUDED.data, updated_at = NOW()
`

func (r *recordingRepo) Save(ctx context.Context, rec *domain.Recording) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("sql recordings.Save: marshal: %w", err)
	}
	_, err = r.db.ExecContext(ctx, recordingUpsert, string(rec.ID), string(rec.StreamCode), payload)
	if err != nil {
		return fmt.Errorf("sql recordings.Save: %w", err)
	}
	return nil
}

func (r *recordingRepo) FindByID(ctx context.Context, id domain.RecordingID) (*domain.Recording, error) {
	var row struct {
		Data []byte `db:"data"`
	}
	err := r.db.GetContext(ctx, &row, `SELECT data FROM recordings WHERE id = $1`, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sql recordings.FindByID: %w", store.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("sql recordings.FindByID: %w", err)
	}
	payload := row.Data
	var rec domain.Recording
	if err := json.Unmarshal(payload, &rec); err != nil {
		return nil, fmt.Errorf("sql recordings.FindByID: unmarshal: %w", err)
	}
	return &rec, nil
}

func (r *recordingRepo) ListByStream(ctx context.Context, streamCode domain.StreamCode) ([]*domain.Recording, error) {
	var rows []struct {
		Data []byte `db:"data"`
	}
	err := r.db.SelectContext(ctx, &rows,
		`SELECT data FROM recordings WHERE stream_id = $1 ORDER BY id`,
		string(streamCode),
	)
	if err != nil {
		return nil, fmt.Errorf("sql recordings.ListByStream: %w", err)
	}
	out := make([]*domain.Recording, 0, len(rows))
	for _, row := range rows {
		payload := row.Data
		var rec domain.Recording
		if err := json.Unmarshal(payload, &rec); err != nil {
			return nil, fmt.Errorf("sql recordings.ListByStream: unmarshal: %w", err)
		}
		out = append(out, &rec)
	}
	return out, nil
}

func (r *recordingRepo) Delete(ctx context.Context, id domain.RecordingID) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM recordings WHERE id = $1`, string(id))
	if err != nil {
		return fmt.Errorf("sql recordings.Delete: %w", err)
	}
	return nil
}

// --- HookRepository ---

type hookRepo struct{ db *sqlx.DB }

const hookUpsert = `
INSERT INTO hooks (id, data, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data, updated_at = NOW()
`

func (r *hookRepo) Save(ctx context.Context, hook *domain.Hook) error {
	payload, err := json.Marshal(hook)
	if err != nil {
		return fmt.Errorf("sql hooks.Save: marshal: %w", err)
	}
	_, err = r.db.ExecContext(ctx, hookUpsert, string(hook.ID), payload)
	if err != nil {
		return fmt.Errorf("sql hooks.Save: %w", err)
	}
	return nil
}

func (r *hookRepo) FindByID(ctx context.Context, id domain.HookID) (*domain.Hook, error) {
	var row struct {
		Data []byte `db:"data"`
	}
	err := r.db.GetContext(ctx, &row, `SELECT data FROM hooks WHERE id = $1`, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sql hooks.FindByID: %w", store.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("sql hooks.FindByID: %w", err)
	}
	payload := row.Data
	var h domain.Hook
	if err := json.Unmarshal(payload, &h); err != nil {
		return nil, fmt.Errorf("sql hooks.FindByID: unmarshal: %w", err)
	}
	return &h, nil
}

func (r *hookRepo) List(ctx context.Context) ([]*domain.Hook, error) {
	var rows []struct {
		Data []byte `db:"data"`
	}
	if err := r.db.SelectContext(ctx, &rows, `SELECT data FROM hooks ORDER BY id`); err != nil {
		return nil, fmt.Errorf("sql hooks.List: %w", err)
	}
	out := make([]*domain.Hook, 0, len(rows))
	for _, row := range rows {
		payload := row.Data
		var h domain.Hook
		if err := json.Unmarshal(payload, &h); err != nil {
			return nil, fmt.Errorf("sql hooks.List: unmarshal: %w", err)
		}
		out = append(out, &h)
	}
	return out, nil
}

func (r *hookRepo) Delete(ctx context.Context, id domain.HookID) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM hooks WHERE id = $1`, string(id))
	if err != nil {
		return fmt.Errorf("sql hooks.Delete: %w", err)
	}
	return nil
}
