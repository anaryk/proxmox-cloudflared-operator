package appliance

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// ExitNoVolume is the status pco daemon exits with when VolumeMounted fails;
// the appliance's pco.service drop-in names it in RestartPreventExitStatus=, so
// that a missing volume leaves the daemon failed and not restarted over and over.
const ExitNoVolume = 78

// The ways VolumeMounted fails besides a path that cannot be read.
var (
	ErrNotMountPoint = errors.New("is not a mount point of its own")
	ErrNoMarker      = errors.New("has no pco volume marker")
)

// VolumeMounted reports whether path is a mount point of its own (st_dev
// differs from its parent) and the marker file is on it.
func VolumeMounted(path, marker string) error {
	vol, err := os.Stat(path)
	if err != nil {
		return err
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if same, known := sameDevice(vol, parent); !known || same {
		return fmt.Errorf("%s %w", path, ErrNotMountPoint)
	}
	m, err := os.Lstat(filepath.Join(path, marker))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s %w", path, ErrNoMarker)
	case err != nil:
		return err
	}
	if same, _ := sameDevice(m, vol); !same || !m.Mode().IsRegular() {
		return fmt.Errorf("%s %w", path, ErrNoMarker)
	}
	return nil
}

// sameDevice reports whether two files are on one device; known is false
// where the platform does not say.
func sameDevice(a, b fs.FileInfo) (same, known bool) {
	sa, ok := a.Sys().(*syscall.Stat_t)
	if !ok {
		return false, false
	}
	sb, ok := b.Sys().(*syscall.Stat_t)
	if !ok {
		return false, false
	}
	return sa.Dev == sb.Dev, true
}

// VolumeLine is what the daemon, and pco status when the daemon does not
// answer, say about a VolumeMounted that failed for path: how to repair it,
// with the VMID of this container as far as its mounts tell it.
func VolumeLine(path string, err error, vmid string) string {
	repair := "run pco appliance repair --vmid " + vmid + " on the node"
	switch {
	case errors.Is(err, ErrNoMarker):
		return fmt.Sprintf("%s has no pco volume marker (restore, or a volume that is not pco's?): %s", path, repair)
	case errors.Is(err, ErrNotMountPoint):
		return fmt.Sprintf("%s is not a mount point of its own (a restore without the volume?): %s", path, repair)
	}
	return fmt.Sprintf("cannot read %s: %v", path, err)
}

// VMIDHint is the VMID of this container as its mounts tell it, for a line
// that names the command to run on the node: the one the volume at path
// names, or else the one of the root filesystem, or "<vmid>".
func (s System) VMIDHint(path string) string {
	for _, p := range []string{path, "/"} {
		if src, err := s.MountSource(p); err == nil && src.VMID > 0 {
			return strconv.Itoa(src.VMID)
		}
	}
	return "<vmid>"
}
