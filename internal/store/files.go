package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	// maxFileSize is the largest file pmxcfs accepts. A write beyond it fails
	// halfway and leaves a truncated file behind, so nothing larger is tried.
	maxFileSize = 1 << 20

	dirMode  fs.FileMode = 0o700
	fileMode fs.FileMode = 0o600

	tempExt = ".tmp"
)

// unsupported reports whether err says the filesystem does not do what was
// asked. pmxcfs answers chmod that way: it fixes the modes by path.
func unsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS)
}

// readOnly reports whether err says a write was refused because the
// filesystem is read-only to this process, as pmxcfs is without quorum.
func readOnly(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS)
}

// ensureDir creates dir and the directories above it. A directory made here
// gets mode 0700 where the filesystem lets it be set; one that exists is left
// as it is.
func ensureDir(dir string) error {
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return nil
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	if err := os.Chmod(dir, dirMode); err != nil && !unsupported(err) {
		return err
	}
	return nil
}

// writeFileAtomic replaces path with data through tmp, a file in the same
// directory, so that a reader sees the old or the new content and never a
// part of it. tmp is removed again if anything fails. The file is made with
// mode 0600 and never chmod-ed: the cluster filesystem decides the modes of
// its files by path.
func writeFileAtomic(path, tmp string, data []byte) (err error) {
	if len(data) > maxFileSize {
		return fmt.Errorf("%d bytes are over the %d byte limit of a file", len(data), maxFileSize)
	}
	// A temporary file left by a crash, possibly with a wrong mode, goes first.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err == nil {
		if err = f.Sync(); unsupported(err) || errors.Is(err, syscall.EINVAL) {
			err = nil
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// removeStaleTemps removes the temporary files that a write interrupted
// between creating and renaming leaves behind, in dir and in the directories
// directly below it.
func removeStaleTemps(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing %s: %w", dir, err)
	}
	var errs []error
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir():
			errs = append(errs, removeTempsIn(path))
		case e.Type().IsRegular() && strings.HasSuffix(e.Name(), tempExt):
			errs = append(errs, removeIfThere(path))
		}
	}
	return errors.Join(errs...)
}

func removeTempsIn(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing %s: %w", dir, err)
	}
	var errs []error
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), tempExt) {
			errs = append(errs, removeIfThere(filepath.Join(dir, e.Name())))
		}
	}
	return errors.Join(errs...)
}

func removeIfThere(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}
