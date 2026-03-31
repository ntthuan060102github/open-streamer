// Package mongo provides a MongoDB implementation of the store repositories.
// Each entity is stored as a document with _id (string) and a binary JSON payload
// in the "data" field, matching the PostgreSQL JSONB layout.
//
// Collection names: streams, recordings, hooks.
package mongo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Store is a MongoDB-backed implementation of all repositories.
type Store struct {
	client *mongo.Client
	db     *mongo.Database
}

// New opens a MongoDB connection and selects the target database.
func New(ctx context.Context, uri, database string) (*Store, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("mongo store: connect: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, fmt.Errorf("mongo store: ping: %w", err)
	}
	return &Store{
		client: client,
		db:     client.Database(database),
	}, nil
}

// Close disconnects from MongoDB.
func (s *Store) Close(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}

// EnsureIndexes creates required secondary indexes. Call once at startup.
func (s *Store) EnsureIndexes(ctx context.Context) error {
	_, err := s.db.Collection("recordings").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "stream_code", Value: 1}},
	})
	if err != nil {
		return fmt.Errorf("mongo store: recordings index: %w", err)
	}
	return nil
}

// Streams returns a StreamRepository backed by this Store.
func (s *Store) Streams() store.StreamRepository {
	return &streamRepo{col: s.db.Collection("streams")}
}

// Recordings returns a RecordingRepository backed by this Store.
func (s *Store) Recordings() store.RecordingRepository {
	return &recordingRepo{col: s.db.Collection("recordings")}
}

// Hooks returns a HookRepository backed by this Store.
func (s *Store) Hooks() store.HookRepository {
	return &hookRepo{col: s.db.Collection("hooks")}
}

// --- StreamRepository ---

type streamRepo struct{ col *mongo.Collection }

func (r *streamRepo) Save(ctx context.Context, stream *domain.Stream) error {
	payload, err := json.Marshal(stream)
	if err != nil {
		return fmt.Errorf("mongo streams.Save: marshal: %w", err)
	}
	filter := bson.M{"_id": string(stream.Code)}
	update := bson.M{
		"$set": bson.M{
			"data":       payload,
			"updated_at": time.Now().UTC(),
		},
	}
	opts := options.UpdateOne().SetUpsert(true)
	_, err = r.col.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return fmt.Errorf("mongo streams.Save: %w", err)
	}
	return nil
}

func (r *streamRepo) FindByCode(ctx context.Context, code domain.StreamCode) (*domain.Stream, error) {
	var doc struct {
		Data []byte `bson:"data"`
	}
	err := r.col.FindOne(ctx, bson.M{"_id": string(code)}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, fmt.Errorf("mongo streams.FindByCode: %w", store.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("mongo streams.FindByCode: %w", err)
	}
	var s domain.Stream
	if err := json.Unmarshal(doc.Data, &s); err != nil {
		return nil, fmt.Errorf("mongo streams.FindByCode: unmarshal: %w", err)
	}
	return &s, nil
}

func (r *streamRepo) List(ctx context.Context, filter store.StreamFilter) ([]*domain.Stream, error) {
	cur, err := r.col.Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("mongo streams.List: %w", err)
	}
	defer cur.Close(ctx)

	out := make([]*domain.Stream, 0)
	for cur.Next(ctx) {
		var doc struct {
			Data []byte `bson:"data"`
		}
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("mongo streams.List: decode: %w", err)
		}
		var s domain.Stream
		if err := json.Unmarshal(doc.Data, &s); err != nil {
			return nil, fmt.Errorf("mongo streams.List: unmarshal: %w", err)
		}
		if filter.Status != nil && s.Status != *filter.Status {
			continue
		}
		out = append(out, &s)
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("mongo streams.List: cursor: %w", err)
	}
	return out, nil
}

func (r *streamRepo) Delete(ctx context.Context, code domain.StreamCode) error {
	_, err := r.col.DeleteOne(ctx, bson.M{"_id": string(code)})
	if err != nil {
		return fmt.Errorf("mongo streams.Delete: %w", err)
	}
	return nil
}

// --- RecordingRepository ---

type recordingRepo struct{ col *mongo.Collection }

func (r *recordingRepo) Save(ctx context.Context, rec *domain.Recording) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("mongo recordings.Save: marshal: %w", err)
	}
	filter := bson.M{"_id": string(rec.ID)}
	update := bson.M{
		"$set": bson.M{
			"data":       payload,
			"stream_code": string(rec.StreamCode),
			"updated_at": time.Now().UTC(),
		},
	}
	opts := options.UpdateOne().SetUpsert(true)
	_, err = r.col.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return fmt.Errorf("mongo recordings.Save: %w", err)
	}
	return nil
}

func (r *recordingRepo) FindByID(ctx context.Context, id domain.RecordingID) (*domain.Recording, error) {
	var doc struct {
		Data []byte `bson:"data"`
	}
	err := r.col.FindOne(ctx, bson.M{"_id": string(id)}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, fmt.Errorf("mongo recordings.FindByID: %w", store.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("mongo recordings.FindByID: %w", err)
	}
	var rec domain.Recording
	if err := json.Unmarshal(doc.Data, &rec); err != nil {
		return nil, fmt.Errorf("mongo recordings.FindByID: unmarshal: %w", err)
	}
	return &rec, nil
}

func (r *recordingRepo) ListByStream(ctx context.Context, streamCode domain.StreamCode) ([]*domain.Recording, error) {
	cur, err := r.col.Find(ctx, bson.M{"stream_code": string(streamCode)}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("mongo recordings.ListByStream: %w", err)
	}
	defer cur.Close(ctx)

	out := make([]*domain.Recording, 0)
	for cur.Next(ctx) {
		var doc struct {
			Data []byte `bson:"data"`
		}
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("mongo recordings.ListByStream: decode: %w", err)
		}
		var rec domain.Recording
		if err := json.Unmarshal(doc.Data, &rec); err != nil {
			return nil, fmt.Errorf("mongo recordings.ListByStream: unmarshal: %w", err)
		}
		out = append(out, &rec)
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("mongo recordings.ListByStream: cursor: %w", err)
	}
	return out, nil
}

func (r *recordingRepo) Delete(ctx context.Context, id domain.RecordingID) error {
	_, err := r.col.DeleteOne(ctx, bson.M{"_id": string(id)})
	if err != nil {
		return fmt.Errorf("mongo recordings.Delete: %w", err)
	}
	return nil
}

// --- HookRepository ---

type hookRepo struct{ col *mongo.Collection }

func (r *hookRepo) Save(ctx context.Context, hook *domain.Hook) error {
	payload, err := json.Marshal(hook)
	if err != nil {
		return fmt.Errorf("mongo hooks.Save: marshal: %w", err)
	}
	filter := bson.M{"_id": string(hook.ID)}
	update := bson.M{
		"$set": bson.M{
			"data":       payload,
			"updated_at": time.Now().UTC(),
		},
	}
	opts := options.UpdateOne().SetUpsert(true)
	_, err = r.col.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return fmt.Errorf("mongo hooks.Save: %w", err)
	}
	return nil
}

func (r *hookRepo) FindByID(ctx context.Context, id domain.HookID) (*domain.Hook, error) {
	var doc struct {
		Data []byte `bson:"data"`
	}
	err := r.col.FindOne(ctx, bson.M{"_id": string(id)}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, fmt.Errorf("mongo hooks.FindByID: %w", store.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("mongo hooks.FindByID: %w", err)
	}
	var h domain.Hook
	if err := json.Unmarshal(doc.Data, &h); err != nil {
		return nil, fmt.Errorf("mongo hooks.FindByID: unmarshal: %w", err)
	}
	return &h, nil
}

func (r *hookRepo) List(ctx context.Context) ([]*domain.Hook, error) {
	cur, err := r.col.Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("mongo hooks.List: %w", err)
	}
	defer cur.Close(ctx)

	out := make([]*domain.Hook, 0)
	for cur.Next(ctx) {
		var doc struct {
			Data []byte `bson:"data"`
		}
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("mongo hooks.List: decode: %w", err)
		}
		var h domain.Hook
		if err := json.Unmarshal(doc.Data, &h); err != nil {
			return nil, fmt.Errorf("mongo hooks.List: unmarshal: %w", err)
		}
		out = append(out, &h)
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("mongo hooks.List: cursor: %w", err)
	}
	return out, nil
}

func (r *hookRepo) Delete(ctx context.Context, id domain.HookID) error {
	_, err := r.col.DeleteOne(ctx, bson.M{"_id": string(id)})
	if err != nil {
		return fmt.Errorf("mongo hooks.Delete: %w", err)
	}
	return nil
}
