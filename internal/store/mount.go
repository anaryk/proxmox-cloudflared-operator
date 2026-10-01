package store

import (
	"errors"
	"fmt"
	"os"
)

// ErrNotMounted is the error of an operation on the cluster or private root
// while the cluster filesystem is not mounted: while pve-cluster restarts,
// /etc/pve is an empty directory of the node's own disk, and what is read from
// it says nothing, while what is written to it is in the way when the
// filesystem is mounted again.
var ErrNotMounted = errors.New("the cluster filesystem is not mounted")

// mountGuard returns the check that runs before every operation on a shared
// root, or nil when marker is empty. marker is a file that exists only while
// the cluster filesystem is mounted.
func mountGuard(marker string) func() error {
	if marker == "" {
		return nil
	}
	return func() error {
		if _, err := os.Stat(marker); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrNotMounted, marker, err)
		}
		return nil
	}
}

// Init creates the cluster and private roots, which Open never does. Only the
// directories themselves are made, so their parents must exist, and a root that
// exists already is left as it is. It is what installation calls, once the
// cluster filesystem is mounted. The mount is checked again after each root is
// made: a filesystem that went away in between has taken the directory with it,
// or has not and the directory is then in its way, so the one just made is
// removed again and the error is ErrNotMounted.
func (s *Store) Init() error {
	if err := s.cluster.check(); err != nil {
		return err
	}
	for _, r := range []struct{ name, path string }{
		{"cluster", s.paths.Cluster},
		{"private", s.paths.Private},
	} {
		created, err := makeLeaf(r.path)
		if err != nil {
			return fmt.Errorf("store: creating the %s root: %w", r.name, err)
		}
		if !created {
			continue
		}
		if err := s.cluster.check(); err != nil {
			_ = os.Remove(r.path)
			return err
		}
	}
	return nil
}
