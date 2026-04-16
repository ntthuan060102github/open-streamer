package yaml_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/store"
	"github.com/ntt0601zcoder/open-streamer/internal/store/storetest"
	yamlstore "github.com/ntt0601zcoder/open-streamer/internal/store/yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) *yamlstore.Store {
	t.Helper()
	s, err := yamlstore.New(t.TempDir())
	require.NoError(t, err)
	return s
}

// --- StreamRepository ---

func TestYAMLStreamRepo_SaveAndFindByCode(t *testing.T) {
	ctx := context.Background()
	repo := newStore(t).Streams()

	want := storetest.NewFullStream("teststreamA")
	require.NoError(t, repo.Save(ctx, want))

	got, err := repo.FindByCode(ctx, "teststreamA")
	require.NoError(t, err)
	assert.Equal(t, want.Code, got.Code)
	assert.Equal(t, want.Name, got.Name)
}

func TestYAMLStreamRepo_FindByCode_NotFound(t *testing.T) {
	ctx := context.Background()
	repo := newStore(t).Streams()

	_, err := repo.FindByCode(ctx, "nonexistent")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrNotFound))
}

func TestYAMLStreamRepo_List(t *testing.T) {
	ctx := context.Background()
	repo := newStore(t).Streams()

	require.NoError(t, repo.Save(ctx, storetest.NewFullStream("stream1")))
	require.NoError(t, repo.Save(ctx, storetest.NewFullStream("stream2")))

	all, err := repo.List(ctx, store.StreamFilter{})
	require.NoError(t, err)
	assert.Len(t, all, 2)
}

func TestYAMLStreamRepo_Delete(t *testing.T) {
	ctx := context.Background()
	repo := newStore(t).Streams()

	require.NoError(t, repo.Save(ctx, storetest.NewFullStream("delete_me")))
	require.NoError(t, repo.Delete(ctx, "delete_me"))

	_, err := repo.FindByCode(ctx, "delete_me")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrNotFound))
}

// --- RecordingRepository ---

func TestYAMLRecordingRepo_SaveAndFindByID(t *testing.T) {
	ctx := context.Background()
	repo := newStore(t).Recordings()

	want := storetest.NewFullRecording("rec1", "stream1")
	require.NoError(t, repo.Save(ctx, want))

	got, err := repo.FindByID(ctx, "rec1")
	require.NoError(t, err)
	assert.Equal(t, want.ID, got.ID)
	assert.Equal(t, want.StreamCode, got.StreamCode)
}

func TestYAMLRecordingRepo_ListByStream(t *testing.T) {
	ctx := context.Background()
	repo := newStore(t).Recordings()

	require.NoError(t, repo.Save(ctx, storetest.NewFullRecording("recA", "stream_alpha")))
	require.NoError(t, repo.Save(ctx, storetest.NewFullRecording("recB", "stream_alpha")))
	require.NoError(t, repo.Save(ctx, storetest.NewFullRecording("recC", "stream_beta")))

	list, err := repo.ListByStream(ctx, "stream_alpha")
	require.NoError(t, err)
	assert.Len(t, list, 2)
}

// --- HookRepository ---

func TestYAMLHookRepo_SaveAndFindByID(t *testing.T) {
	ctx := context.Background()
	repo := newStore(t).Hooks()

	want := storetest.NewFullHook("hook1")
	require.NoError(t, repo.Save(ctx, want))

	got, err := repo.FindByID(ctx, "hook1")
	require.NoError(t, err)
	assert.Equal(t, want.ID, got.ID)
	assert.Equal(t, want.Name, got.Name)
}

func TestYAMLHookRepo_Delete(t *testing.T) {
	ctx := context.Background()
	repo := newStore(t).Hooks()

	require.NoError(t, repo.Save(ctx, storetest.NewFullHook("del_hook")))
	require.NoError(t, repo.Delete(ctx, "del_hook"))

	_, err := repo.FindByID(ctx, "del_hook")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrNotFound))
}

// --- SettingsRepository ---

func TestYAMLSettingsRepo_GetSet(t *testing.T) {
	ctx := context.Background()
	repo := newStore(t).Settings()

	_, err := repo.Get(ctx, "missing")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrNotFound))

	val := json.RawMessage(`{"foo":"bar"}`)
	require.NoError(t, repo.Set(ctx, "test_key", val))

	got, err := repo.Get(ctx, "test_key")
	require.NoError(t, err)
	assert.JSONEq(t, `{"foo":"bar"}`, string(got))
}

// --- Concurrent access ---

func TestYAMLStreamRepo_ConcurrentSaveAndFind(t *testing.T) {
	ctx := context.Background()
	repo := newStore(t).Streams()

	const workers = 10
	var wg sync.WaitGroup

	for i := range workers {
		code := domain.StreamCode(fmt.Sprintf("concurrent_%d", i))
		s := storetest.NewFullStream(code)
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, repo.Save(ctx, s))
			got, err := repo.FindByCode(ctx, code)
			require.NoError(t, err)
			assert.Equal(t, code, got.Code)
		}()
	}
	wg.Wait()

	all, err := repo.List(ctx, store.StreamFilter{})
	require.NoError(t, err)
	assert.Len(t, all, workers)
}
