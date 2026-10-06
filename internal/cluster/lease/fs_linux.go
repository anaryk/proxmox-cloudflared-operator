package lease

import (
	"io/fs"
	"time"

	"golang.org/x/sys/unix"
)

// SystemFS is the cluster filesystem of this node.
func SystemFS() FS { return systemFS{} }

type systemFS struct{}

func (systemFS) Mkdir(path string) error {
	if err := unix.Mkdir(path, 0o755); err != nil {
		return &fs.PathError{Op: "mkdir", Path: path, Err: err}
	}
	return nil
}

func (systemFS) Refresh(path string, mtime time.Time) error {
	return utimes(path, unix.Timespec{Sec: mtime.Unix()})
}

func (systemFS) UnlockRequest(path string) error {
	return utimes(path, unix.Timespec{})
}

func (systemFS) Rmdir(path string) error {
	if err := unix.Rmdir(path); err != nil {
		return &fs.PathError{Op: "rmdir", Path: path, Err: err}
	}
	return nil
}

// utimes sets the mtime of path and its atime to zero, as utime(path, 0,
// mtime) does: pmxcfs reads only the seconds of the mtime.
func utimes(path string, mtime unix.Timespec) error {
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{{}, mtime}, 0); err != nil {
		return &fs.PathError{Op: "utimensat", Path: path, Err: err}
	}
	return nil
}
