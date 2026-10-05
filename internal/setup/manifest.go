package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
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
	GrantedACL           bool      `json:"grantedACL"`     // the grant of role PCO on / to pco@pve
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
	return atomicfile.Write(path, append(b, '\n'), atomicfile.Options{Mode: 0o600})
}

// removeLeftovers removes the temporary files that a write of path, cut
// short, left next to it.
func removeLeftovers(path string) {
	dir, prefix := filepath.Dir(path), "."+filepath.Base(path)+"."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if name := e.Name(); e.Type().IsRegular() && strings.HasPrefix(name, prefix) && strings.HasSuffix(name, atomicfile.TempExt) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}
