package store

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

const (
	// ProfileFile is the marker that names the profile of the machine. Only
	// the appliance has one.
	ProfileFile = "/etc/pco/profile"

	// ApplianceLocal is the local root of the appliance, the mount point of
	// its state volume. The cluster and private roots are below it.
	ApplianceLocal = "/var/lib/pco"

	// VolumeMarker is the file below ApplianceLocal that the installer puts on
	// the state volume. Without it the volume is not mounted, or is a new one.
	VolumeMarker = ".volume"

	// maxProfileFile is more than any profile name needs.
	maxProfileFile = 64
)

// DetectProfile reads the profile marker at path: ProfileAppliance when it says
// so, ProfileHost when it is missing or says host, and an error for anything
// else. A marker that is there but cannot be read is an error too, never the
// host: on an appliance the paths of a host would be the wrong ones.
func DetectProfile(path string) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ProfileHost, nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the profile: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxProfileFile+1))
	if err != nil {
		return "", fmt.Errorf("reading the profile %s: %w", path, err)
	}
	if len(b) > maxProfileFile {
		return "", fmt.Errorf("the profile %s is longer than %d bytes: want %q or %q", path, maxProfileFile, ProfileHost, ProfileAppliance)
	}
	switch name := string(bytes.TrimSpace(b)); name {
	case ProfileHost, ProfileAppliance:
		return name, nil
	case "":
		return "", fmt.Errorf("the profile %s is empty: want %q or %q", path, ProfileHost, ProfileAppliance)
	default:
		return "", fmt.Errorf("the profile %s says %q: want %q or %q", path, name, ProfileHost, ProfileAppliance)
	}
}

// PathsFor returns the roots of a profile: DefaultPaths for the host, and for
// the appliance the three roots on its state volume, with the volume marker as
// the mount check and durable writes.
func PathsFor(profile string) (Paths, error) {
	switch profile {
	case ProfileHost:
		return DefaultPaths(), nil
	case ProfileAppliance:
		return Paths{
			Cluster:    filepath.Join(ApplianceLocal, "cluster"),
			Private:    filepath.Join(ApplianceLocal, "private"),
			Local:      ApplianceLocal,
			MountCheck: filepath.Join(ApplianceLocal, VolumeMarker),
			Durable:    true,
		}, nil
	}
	return Paths{}, fmt.Errorf("profile %q: want %q or %q", profile, ProfileHost, ProfileAppliance)
}

// Endpoint is how the appliance reaches the Proxmox API: where it dials, and
// the name the certificate is verified under.
type Endpoint struct {
	Address    string `json:"address"`    // host:port, 8006
	ServerName string `json:"serverName"` // the node name, or the DNS name of a custom certificate
}

// ApplianceInstall is what an appliance records about itself.
type ApplianceInstall struct {
	VMID      int        `json:"vmid"`
	Node      string     `json:"node"`      // the Proxmox node the container is on
	MACs      []string   `json:"macs"`      // of its NICs at init, normalised, sorted
	Endpoints []Endpoint `json:"endpoints"` // the first is used
	CAFile    string     `json:"caFile"`    // /var/lib/pco/pve-ca.pem
}

// checkAppliance checks the appliance block of an install against its profile,
// and returns the block as it is stored.
func checkAppliance(i Install) (*ApplianceInstall, error) {
	switch {
	case i.Profile == ProfileAppliance && i.Appliance == nil:
		return nil, errors.New("the install of an appliance records nothing about the appliance")
	case i.Profile != ProfileAppliance && i.Appliance != nil:
		return nil, fmt.Errorf("install profile %q: only an appliance records an appliance", i.ProfileName())
	case i.Appliance == nil:
		return nil, nil
	}
	a := *i.Appliance
	switch {
	case a.VMID <= 0:
		return nil, fmt.Errorf("appliance vmid %d: want a positive number", a.VMID)
	case a.Node == "":
		return nil, errors.New("appliance node is empty")
	case len(a.MACs) == 0:
		return nil, errors.New("the appliance has no MAC")
	case len(a.Endpoints) == 0:
		return nil, errors.New("the appliance has no endpoint of the Proxmox API")
	}
	macs, err := sortedMACs(a.MACs)
	if err != nil {
		return nil, fmt.Errorf("appliance: %w", err)
	}
	a.MACs = macs
	for _, e := range a.Endpoints {
		if err := e.check(); err != nil {
			return nil, fmt.Errorf("appliance: %w", err)
		}
	}
	return &a, nil
}

func (e Endpoint) check() error {
	host, port, err := net.SplitHostPort(e.Address)
	if err != nil || host == "" || !validPort(port) {
		return fmt.Errorf("endpoint address %q: want host:port", e.Address)
	}
	if e.ServerName == "" {
		return fmt.Errorf("endpoint %s has no server name", e.Address)
	}
	return nil
}

func validPort(s string) bool {
	n, err := strconv.ParseUint(s, 10, 16)
	return err == nil && n > 0
}

// sortedMACs returns macs sorted and each once. Every one must be in the
// normal form, lower case and colon separated.
func sortedMACs(macs []string) ([]string, error) {
	for _, m := range macs {
		if n, err := model.NormalizeMAC(m); err != nil || n != m {
			return nil, fmt.Errorf("MAC %q: want the normal form, as bc:24:11:00:aa:b5", m)
		}
	}
	out := slices.Clone(macs)
	slices.Sort(out)
	return slices.Compact(out), nil
}
