package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// An empty file, which stays where a namespace was unmounted, is no pin.
func TestAFileIsNoPinOfANamespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pco")

	require.EqualError(t, netnsPinned(path), path+" does not exist")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	require.EqualError(t, netnsPinned(path), path+" is no namespace")
}
