package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	// maxFileSize is the largest file pmxcfs accepts. A write beyond it fails
	// halfway and leaves a truncated file behind, so nothing larger is tried.
	maxFileSize = 1 << 20

	dirMode fs.FileMode = 0o700

	tempExt = ".tmp"

	// staleTempAge is how old a temporary file must be before Open takes it
	// for the leftover of a crash. A younger one may be a write in progress of
	// another process.
	staleTempAge = 10 * time.Minute
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
// gets mode 0700; one that exists is left as it is. Only the local root is made
// this way.
func ensureDir(dir string) error {
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return nil
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	return setDirMode(dir)
}

// makeLeaf creates the directory dir, whose parent must exist, and reports
// whether it made it. A directory that is already there is fine, a file of that
// name is not.
func makeLeaf(dir string) (created bool, err error) {
	err = os.Mkdir(dir, dirMode)
	if errors.Is(err, fs.ErrExist) {
		if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
			return false, nil
		}
		return false, err
	}
	if err != nil {
		return false, err
	}
	return true, setDirMode(dir)
}

// setDirMode sets the mode of a directory this process made. The cluster
// filesystem fixes modes by path and refuses; that is no failure.
func setDirMode(dir string) error {
	if err := os.Chmod(dir, dirMode); err != nil && !unsupported(err) {
		return err
	}
	return nil
}

// writeFileAtomic replaces path with data through a temporary file of its own
// in the same directory, so that a reader sees the old or the new content and
// never a part of it, and writers of the same file do not meet in one temporary
// file. The temporary file is removed again if anything fails. It is made with
// mode 0600 and the file is never chmod-ed: the cluster filesystem decides the
// modes of its files by path.
func writeFileAtomic(path string, data []byte) (err error) {
	if len(data) > maxFileSize {
		return fmt.Errorf("%d bytes are over the %d byte limit of a file", len(data), maxFileSize)
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*"+tempExt)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
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
	return os.Rename(f.Name(), path)
}

// removeStaleTemps removes the temporary files in each of dirs that were last
// written before cutoff, the leftovers of writes that a crash interrupted. A
// directory that is not there has none.
func removeStaleTemps(cutoff time.Time, dirs ...string) error {
	var errs []error
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("listing %s: %w", dir, err))
			continue
		}
		for _, e := range entries {
			if e.Type().IsRegular() && strings.HasSuffix(e.Name(), tempExt) {
				errs = append(errs, removeIfStale(filepath.Join(dir, e.Name()), cutoff))
			}
		}
	}
	return errors.Join(errs...)
}

func removeIfStale(path string, cutoff time.Time) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking %s: %w", path, err)
	}
	if !info.ModTime().Before(cutoff) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}
