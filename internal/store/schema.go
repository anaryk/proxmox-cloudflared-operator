package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxObjectFile is more than any object of the store holds: pmxcfs refuses
// files over 1 MiB.
const maxObjectFile = 1 << 20

// SchemaVersion is the version of the envelope this build reads and writes.
// A build reads no object of a higher one.
func SchemaVersion() int { return schemaVersion }

// NewestSchema returns the highest schema version an object under the roots of
// p was written with, and its file; 0 and "" when there is none. An object is
// a .json file one directory below a root, as the store writes it; hidden and
// temporary files are not, and neither is a file whose envelope cannot be
// read, which no build can read either. A directory that cannot be listed is
// an error: what it holds is not known.
func NewestSchema(p Paths) (version int, path string, err error) {
	for _, root := range []string{p.Cluster, p.Private, p.Local} {
		kinds, err := os.ReadDir(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, "", fmt.Errorf("listing %s: %w", root, err)
		}
		for _, k := range kinds {
			if !k.IsDir() || strings.HasPrefix(k.Name(), ".") {
				continue
			}
			v, at, err := newestIn(filepath.Join(root, k.Name()))
			if err != nil {
				return 0, "", err
			}
			if v > version {
				version, path = v, at
			}
		}
	}
	return version, path, nil
}

// newestIn is NewestSchema for the objects of one kind.
func newestIn(dir string) (version int, path string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, "", fmt.Errorf("listing %s: %w", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, objectExt) {
			continue
		}
		file := filepath.Join(dir, name)
		v, err := schemaOf(file)
		if err != nil {
			return 0, "", err
		}
		if v > version {
			version, path = v, file
		}
	}
	return version, path, nil
}

// schemaOf reads the schema version of the object in file; 0 when its
// envelope cannot be read.
func schemaOf(file string) (int, error) {
	f, err := os.Open(file)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", file, err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxObjectFile))
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", file, err)
	}
	var env struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if json.Unmarshal(b, &env) != nil {
		return 0, nil
	}
	return env.SchemaVersion, nil
}
