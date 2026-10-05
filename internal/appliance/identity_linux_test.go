package appliance

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// A volume of its own is a mount: a tmpfs here, which only root may mount.
func TestVolumeMountedOnAMountOfItsOwn(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("mounting a tmpfs needs root")
	}
	dir := filepath.Join(t.TempDir(), "pco")
	require.NoError(t, os.Mkdir(dir, 0o700))
	if err := syscall.Mount("tmpfs", dir, "tmpfs", 0, "size=1m"); err != nil {
		t.Skipf("cannot mount a tmpfs here: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Unmount(dir, 0) })

	require.ErrorIs(t, VolumeMounted(dir, store.VolumeMarker), ErrNoMarker, "a new, empty volume")

	require.NoError(t, os.Mkdir(filepath.Join(dir, store.VolumeMarker), 0o700))
	require.ErrorIs(t, VolumeMounted(dir, store.VolumeMarker), ErrNoMarker, "a directory is no marker")
	require.NoError(t, os.Remove(filepath.Join(dir, store.VolumeMarker)))

	require.NoError(t, os.WriteFile(filepath.Join(dir, store.VolumeMarker), nil, 0o600))
	require.NoError(t, VolumeMounted(dir, store.VolumeMarker))

	require.NoError(t, syscall.Unmount(dir, 0))
	require.ErrorIs(t, VolumeMounted(dir, store.VolumeMarker), ErrNotMountPoint, "the directory under the volume")
}
