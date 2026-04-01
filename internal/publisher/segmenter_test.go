package publisher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWindowTailStrings(t *testing.T) {
	t.Parallel()
	names := []string{"a.ts", "b.ts", "c.ts", "d.ts"}

	t.Run("n<=0", func(t *testing.T) {
		t.Parallel()
		require.Nil(t, windowTail(names, 0))
		require.Nil(t, windowTail(names, -1))
	})

	t.Run("empty input", func(t *testing.T) {
		t.Parallel()
		require.Nil(t, windowTail(nil, 3))
		require.Nil(t, windowTail([]string{}, 2))
	})

	t.Run("tail two", func(t *testing.T) {
		t.Parallel()
		got := windowTail(names, 2)
		require.Equal(t, []string{"c.ts", "d.ts"}, got)
		require.NotSame(t, &names[0], &got[0], "should be a copy")
	})

	t.Run("tail exceeds len", func(t *testing.T) {
		t.Parallel()
		got := windowTail(names, 10)
		require.Equal(t, names, got)
	})
}

func TestWriteFileAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	data := []byte("hello atomic")

	require.NoError(t, writeFileAtomic(path, data))
	read, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, data, read)

	// overwrite
	require.NoError(t, writeFileAtomic(path, []byte("v2")))
	read2, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte("v2"), read2)
}
