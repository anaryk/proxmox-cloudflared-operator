package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	// WebEnabled is true when setup enabled pco-web.service, WebEnv when it
	// wrote /etc/default/pco-web. WebCert is the mode of the certificate the
	// web interface serves: ca, own or pveproxy. WebTLS lists the files, links
	// and directories setup made for it, in the order it made them.
	WebEnabled bool     `json:"webEnabled"`
	WebEnv     bool     `json:"webEnv"`
	WebCert    string   `json:"webCert"`
	WebTLS     []string `json:"webTLS"`
	// Appliance is what pco appliance install made on the node besides the
	// objects above; nil for a host. An appliance keeps its manifest on its
	// state volume, and the installer reads it back as untrusted input.
	Appliance *ApplianceManifest `json:"appliance,omitempty"`
}

// ApplianceManifest is what the installer made for one appliance.
type ApplianceManifest struct {
	VMID int    `json:"vmid"`
	Node string `json:"node"`
	// Token is the API token of the appliance, pco@pve!vm<vmid>.
	Token string `json:"token"`
	// Pool is the pool of the container, pco; CreatedPool says the installer
	// made it.
	Pool        string `json:"pool"`
	CreatedPool bool   `json:"createdPool"`
	// Template is the volume of the template the installer downloaded,
	// <storage>:vztmpl/pco-appliance_<version>_<arch>.tar.zst; empty when it
	// was given a file or found the volume there.
	Template string `json:"template,omitempty"`
	// Grants are the networks pco appliance grant-network granted.
	Grants []NetworkGrant `json:"grants,omitempty"`
}

// The roles of a network grant: a card of the appliance on a network needs
// both, the first on the appliance and the second on the network.
const (
	RoleManaged = "PCOManaged" // VM.Config.Network
	RoleSDN     = "PCOSDN"     // SDN.Use
)

// NetworkGrant is a network the appliance may put a card on: SDN.Use on
// /sdn/zones/<zone>/<vnet>[/<vlan>] and VM.Config.Network on the appliance,
// for pco@pve and the token of the appliance. A plain Linux bridge is the vnet
// of zone localnetwork.
type NetworkGrant struct {
	Zone string `json:"zone"`
	VNet string `json:"vnet"`
	VLAN int    `json:"vlan,omitempty"`
	// CreatedRoles are the roles the grant made, of RoleManaged and RoleSDN.
	CreatedRoles []string `json:"createdRoles,omitempty"`
}

var networkName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,15}$`)

// Check refuses a grant of another shape than a network grant has: never a
// whole zone, never a role of its own.
func (g NetworkGrant) Check() error {
	switch {
	case !networkName.MatchString(g.Zone):
		return fmt.Errorf("zone %q: want the name of an SDN zone", g.Zone)
	case !networkName.MatchString(g.VNet):
		return fmt.Errorf("vnet %q: want the name of a bridge or vnet", g.VNet)
	case g.VLAN < 0 || g.VLAN > 4094:
		return fmt.Errorf("vlan %d: want 1 to 4094, or none", g.VLAN)
	}
	for _, role := range g.CreatedRoles {
		if role != RoleManaged && role != RoleSDN {
			return fmt.Errorf("role %q: a network grant makes %s and %s only", role, RoleManaged, RoleSDN)
		}
	}
	return nil
}

// Path is the ACL path of the network.
func (g NetworkGrant) Path() string {
	p := "/sdn/zones/" + g.Zone + "/" + g.VNet
	if g.VLAN != 0 {
		p += "/" + strconv.Itoa(g.VLAN)
	}
	return p
}

// Same reports whether two grants are of one network.
func (g NetworkGrant) Same(o NetworkGrant) bool {
	return g.Zone == o.Zone && g.VNet == o.VNet && g.VLAN == o.VLAN
}

// AddNetworkGrant adds a grant to the manifest of the appliance in the local
// root of its store, or the roles it made to the grant of that network that
// is there.
func AddNetworkGrant(local string, g NetworkGrant) error {
	return changeApplianceManifest(local, func(a *ApplianceManifest) error {
		if err := g.Check(); err != nil {
			return err
		}
		if i := slices.IndexFunc(a.Grants, g.Same); i >= 0 {
			for _, role := range g.CreatedRoles {
				if !slices.Contains(a.Grants[i].CreatedRoles, role) {
					a.Grants[i].CreatedRoles = append(a.Grants[i].CreatedRoles, role)
				}
			}
			return nil
		}
		a.Grants = append(a.Grants, g)
		return nil
	})
}

// RemoveNetworkGrant takes the grant of a network out of the manifest of the
// appliance; one that is not there is no error.
func RemoveNetworkGrant(local string, g NetworkGrant) error {
	return changeApplianceManifest(local, func(a *ApplianceManifest) error {
		a.Grants = slices.DeleteFunc(a.Grants, g.Same)
		return nil
	})
}

func changeApplianceManifest(local string, change func(*ApplianceManifest) error) error {
	path := filepath.Join(local, manifestName)
	m, found, err := readManifest(path)
	switch {
	case err != nil:
		return fmt.Errorf("reading the manifest: %w", err)
	case !found || m.Appliance == nil:
		return fmt.Errorf("%s is not the manifest of an appliance", path)
	}
	if err := change(m.Appliance); err != nil {
		return err
	}
	if err := writeManifest(path, m); err != nil {
		return fmt.Errorf("writing the manifest: %w", err)
	}
	return nil
}

// addWebTLS notes a file, link or directory setup makes for the certificate
// of the web interface.
func (m *Manifest) addWebTLS(path string) {
	if !slices.Contains(m.WebTLS, path) {
		m.WebTLS = append(m.WebTLS, path)
	}
}

// WebSetup is what setup did for the web interface of a node.
type WebSetup struct {
	Enabled bool   // setup enabled pco-web.service
	Mode    string // of the certificate: webcert.ModeCA, ModeOwn or ModePVEProxy
}

// ReadWebSetup reads from the manifest in the local root of the store what
// setup did for the web interface. A node without a manifest has nothing of
// it.
func ReadWebSetup(local string) (WebSetup, error) {
	m, _, err := readManifest(filepath.Join(local, manifestName))
	if err != nil {
		return WebSetup{}, fmt.Errorf("reading the manifest: %w", err)
	}
	return WebSetup{Enabled: m.WebEnabled, Mode: m.WebCert}, nil
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
