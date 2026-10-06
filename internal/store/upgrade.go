package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
)

// UpgradeState is meta/upgrade: an upgrade from an earlier release that is
// not finished. Until it is, the old objects stay beside the new ones and
// the previous release can be installed again; the settings that release
// would refuse cannot be set.
type UpgradeState struct {
	// From is the release the store was upgraded from.
	From  string    `json:"from"`
	Since time.Time `json:"since"`
}

// UpgradeState returns the unfinished upgrade, if there is one.
func (s *Store) UpgradeState() (UpgradeState, bool, error) {
	return getOne[UpgradeState](s.cluster, kindMeta, idUpgrade)
}

// SaveUpgradeState records an upgrade that is not finished.
func (s *Store) SaveUpgradeState(u UpgradeState) error {
	switch {
	case u.From == "":
		return errors.New("upgrade: the release it came from is empty")
	case u.Since.IsZero():
		return fmt.Errorf("upgrade from %s has no time it is unfinished since", u.From)
	}
	return s.cluster.put(kindMeta, idUpgrade, u, true)
}

// DeleteUpgradeState records that the upgrade is finished. A missing record
// is not an error.
func (s *Store) DeleteUpgradeState() error { return s.cluster.Delete(kindMeta, idUpgrade) }

// BackupLocal copies every object of the cluster and private roots below
// dir, into dir/cluster and dir/private, as they are on disk, each file with
// mode 0600. It is the backup taken before an upgrade is finished, on the
// node's own disk: in the cluster filesystem it would double what every node
// keeps in memory. dir must not exist yet; its parent is made when missing.
func (s *Store) BackupLocal(dir string) error {
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("backup: %s exists already", dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup: %w", err)
	}
	for _, r := range []struct {
		name string
		d    Dir
	}{{"cluster", s.cluster}, {"private", s.private}} {
		if err := backupRoot(r.d, filepath.Join(dir, r.name)); err != nil {
			return fmt.Errorf("backup of the %s root: %w", r.name, err)
		}
	}
	return nil
}

// backupRoot copies the objects of the root of d below to: the .json files
// one directory below the root that are not hidden.
func backupRoot(d Dir, to string) error {
	if err := d.check(); err != nil {
		return err
	}
	if err := d.requireRoot(); err != nil {
		return err
	}
	kinds, err := os.ReadDir(d.root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(to, dirMode); err != nil {
		return err
	}
	for _, k := range kinds {
		if !k.IsDir() || strings.HasPrefix(k.Name(), ".") {
			continue
		}
		files, err := os.ReadDir(filepath.Join(d.root, k.Name()))
		if err != nil {
			return err
		}
		for _, f := range files {
			name := f.Name()
			if !f.Type().IsRegular() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, objectExt) {
				continue
			}
			b, err := os.ReadFile(filepath.Join(d.root, k.Name(), name))
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Join(to, k.Name()), dirMode); err != nil {
				return err
			}
			if err := atomicfile.Write(filepath.Join(to, k.Name(), name), b, atomicfile.Options{Mode: 0o600}); err != nil {
				return err
			}
		}
	}
	return syncDir(to)
}
