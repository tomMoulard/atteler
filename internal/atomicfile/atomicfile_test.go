package atomicfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteFile_ReplacesExistingFileAndAppliesMode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "store.json")
	require.NoError(t, WriteFile(path, []byte("first\n"), 0o600, ".test-*.tmp"))

	require.NoError(t, os.Chmod(path, 0o644)) //nolint:gosec // Test starts loose to prove WriteFile tightens permissions.
	require.NoError(t, WriteFile(path, []byte("second\n"), 0o600, ".test-*.tmp"))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "second\n", string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestWriteFile_CleansTempFileOnWriteError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	blockingDir := filepath.Join(dir, "blocking")
	require.NoError(t, os.Mkdir(blockingDir, 0o700))

	err := WriteFile(blockingDir, []byte("not a file"), 0o600, ".test-*.tmp")
	require.Error(t, err)

	matches, globErr := filepath.Glob(filepath.Join(dir, ".test-*.tmp"))
	require.NoError(t, globErr)
	assert.Empty(t, matches)
}
