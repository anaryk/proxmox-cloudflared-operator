// Package atomicfile replaces a file so that a reader sees the old content or
// the new one, and never a part of it.
package atomicfile

import (
	"io/fs"
	"os"
	"path/filepath"
)

// TempExt ends the name of the temporary file of a write. The leftovers of a
// write that a crash cut short are found by it.
const TempExt = ".tmp"

// Options are what a write may ask for beyond the content.
type Options struct {
	// Mode is the mode of the file, set before it takes the place of the old
	// one. Zero leaves the 0600 the temporary file is made with and never
	// chmods it, as on the cluster filesystem, which decides the modes of its
	// files by path.
	Mode fs.FileMode
	// SyncIgnored says which errors of flushing the file are no failure, as
	// those of a filesystem that cannot do it. Nil ignores none.
	SyncIgnored func(error) bool
}

// Write replaces path with data through a temporary file of its own in the
// same directory, so that writers of one file do not meet in one temporary
// file. The temporary file is removed again if anything fails.
func Write(path string, data []byte, o Options) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*"+TempExt)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if o.Mode != 0 {
		if err = f.Chmod(o.Mode); err != nil {
			return err
		}
	}
	if err = f.Sync(); err != nil && o.SyncIgnored != nil && o.SyncIgnored(err) {
		err = nil
	}
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
