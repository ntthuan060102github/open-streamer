package handler

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// Happy path: directory doesn't exist → MkdirAll creates it → write probe
// succeeds → no probe file left behind.
func TestEnsureMountStorageWritable_CreatesMissingDirAndProbes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "new", "vod")

	require.NoError(t, ensureMountStorageWritable(target))

	info, err := os.Stat(target)
	require.NoError(t, err, "directory must exist after helper runs")
	require.True(t, info.IsDir())

	// No probe file should remain.
	entries, err := os.ReadDir(target)
	require.NoError(t, err)
	require.Empty(t, entries, "probe file must be cleaned up")
}

// Idempotent: calling on an existing writable directory is fine.
func TestEnsureMountStorageWritable_ExistingDirIsNoOp(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	require.NoError(t, ensureMountStorageWritable(target))
	require.NoError(t, ensureMountStorageWritable(target), "second call must also succeed")
}

// The probe catches the case MkdirAll silently swallows: directory exists
// but is not writable by the service. Skipped on Windows (perm semantics
// differ) and when the test runs as root (root bypasses mode bits).
func TestEnsureMountStorageWritable_FailsOnReadOnlyDir(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission semantics required")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses mode-bit checks; cannot meaningfully test")
	}

	target := t.TempDir()
	require.NoError(t, os.Chmod(target, 0o555)) // r-x for owner, no write
	t.Cleanup(func() { _ = os.Chmod(target, 0o755) })

	err := ensureMountStorageWritable(target)
	require.Error(t, err, "must reject read-only directory")
	require.Contains(t, err.Error(), "write probe", "error must name the failing step")
}
