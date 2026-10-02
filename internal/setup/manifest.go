package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// manifestName is the file of the manifest, in the local root of the store.
const manifestName = "manifest.json"

// Manifest is what setup created on this node, in this run and the ones
// before: what uninstall may remove. Something that was there before setup
// is not in it.
type Manifest struct {
	Node                 string    `json:"node"`
	InstalledAt          time.Time `json:"installedAt"`
	CreatedRole          bool      `json:"createdRole"`
	CreatedUser          bool      `json:"createdUser"`
	CreatedToken         bool      `json:"createdToken"`
	RegisteredTags       []string  `json:"registeredTags"` // tags setup added
	InstalledCloudflared bool      `json:"installedCloudflared"`
	AddedAptSource       bool      `json:"addedAptSource"`
	// AddedKeyring is true when setup downloaded the key of the apt source,
	// which may have been there before the source was.
	AddedKeyring bool `json:"addedKeyring"`
}

// addTags notes tags setup registered.
func (m *Manifest) addTags(tags []string) {
	for _, tag := range tags {
		if !slices.Contains(m.RegisteredTags, tag) {
			m.RegisteredTags = append(m.RegisteredTags, tag)
		}
	}
}

// readManifest reads the manifest at path; found is false when there is
// none.
func readManifest(path string) (m Manifest, found bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, false, nil
	}
	if err != nil {
		return Manifest{}, false, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return m, true, nil
}

func writeManifest(path string, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'), 0o600)
}

// writeFileAtomic replaces path with data through a temporary file in the
// same directory, so that a reader sees the old or the new content and never
// a part of it.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(mode); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
