// Package store keeps the state of the daemon in files: one JSON file per
// object, under three roots.
//
// The cluster root holds what every node shares, the private root the secrets
// every node shares, and the local root what belongs to this node. The first
// two live on pmxcfs, which fixes the modes of files by path, has no links,
// refuses files over 1 MiB, replicates every write to every node and is
// read-only without quorum. So the store never chmods a file, replaces a
// file through a rename within its directory, leaves an object alone when a
// save would not change it, and reports a write that fails as an error of
// that write while reads go on.
package store

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// Kinds and ids of the objects. The kind is the directory below a root.
const (
	kindMeta        = "meta"
	kindNodes       = "nodes"
	kindClaims      = "claims"
	kindRoutes      = "routes"
	kindApprovals   = "approvals"
	kindCredentials = "credentials"
	kindBindings    = "bindings"

	idInstall    = "install"
	idSettings   = "settings"
	idLeader     = "leader"
	idTombstones = "tombstones"
	idPVEToken   = "pve-token"
)

// Store groups the typed accessors over the three roots. It is safe for
// concurrent use; two processes on the same roots are not coordinated.
type Store struct {
	paths   Paths
	cluster Dir
	private Dir
	local   Dir

	mu sync.Mutex // serialises the writes of files that are not objects of a Dir
}

// Open makes the roots that are missing and removes the temporary files a
// crash left in them. The cluster and private roots may be read-only, as they
// are without quorum, and then Open still succeeds and the writes fail one by
// one. The local root must be writable.
func Open(p Paths) (*Store, error) {
	roots := []struct {
		name, path string
		local      bool
	}{
		{"cluster", p.Cluster, false},
		{"private", p.Private, false},
		{"local", p.Local, true},
	}
	for _, r := range roots {
		if r.path == "" {
			return nil, fmt.Errorf("store: the %s path is empty", r.name)
		}
	}
	for _, r := range roots {
		if err := prepareRoot(r.path, r.local); err != nil {
			return nil, fmt.Errorf("store: %s root: %w", r.name, err)
		}
	}
	return &Store{
		paths:   p,
		cluster: NewDir(p.Cluster),
		private: NewDir(p.Private),
		local:   NewDir(p.Local),
	}, nil
}

// prepareRoot creates the root and sweeps its temporary files. The local root
// must be writable; for a shared root, a refusal to write is no reason to fail.
func prepareRoot(path string, local bool) error {
	if err := ensureDir(path); err != nil {
		if !local && readOnly(err) {
			return nil
		}
		return fmt.Errorf("creating %s: %w", path, err)
	}
	err := removeStaleTemps(path)
	if !local {
		return nil
	}
	if err != nil {
		return err
	}
	return probeWritable(path)
}

// probeWritable fails when files cannot be made in dir.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".probe-*"+tempExt)
	if err != nil {
		return fmt.Errorf("%s is not writable: %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	return removeIfThere(name)
}

// Install returns the identity of this installation.
func (s *Store) Install() (Install, bool, error) {
	return getOne[Install](s.cluster, kindMeta, idInstall)
}

// SaveInstall stores the identity of this installation.
func (s *Store) SaveInstall(i Install) error {
	if i.ID == "" {
		return errors.New("install id is empty")
	}
	return s.cluster.put(kindMeta, idInstall, i, true)
}

// Settings returns the settings, or the defaults when none were saved. Settings
// that are stored but invalid are an error, never the defaults: the daemon
// would act on rules the admin did not write. A field the stored settings
// leave out keeps its default.
func (s *Store) Settings() (Settings, error) {
	stored := DefaultSettings()
	found, err := s.cluster.Get(kindMeta, idSettings, &stored)
	if err != nil {
		return Settings{}, err
	}
	if !found {
		return DefaultSettings(), nil
	}
	n, err := stored.normalized()
	if err != nil {
		return Settings{}, fmt.Errorf("stored settings are invalid: %w", err)
	}
	return n, nil
}

// SaveSettings validates the settings and stores them with their patterns and
// zone names in normal form. Invalid settings are refused, naming the field,
// and nothing is written.
func (s *Store) SaveSettings(v Settings) error {
	n, err := v.normalized()
	if err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	return s.cluster.put(kindMeta, idSettings, n, true)
}

// Writer returns the writer named in leader.json. The file is read on every
// call, as the answer decides whether a write to Cloudflare may go ahead: a
// missing file is not found, a file that cannot be read is an error.
func (s *Store) Writer() (planner.Writer, bool, error) {
	return getOne[planner.Writer](s.cluster, kindMeta, idLeader)
}

// SaveWriter stores the writer that may change Cloudflare.
func (s *Store) SaveWriter(w planner.Writer) error {
	if err := w.Validate(); err != nil {
		return err
	}
	return s.cluster.put(kindMeta, idLeader, w, true)
}

// Nodes returns the nodes that run pco, by name.
func (s *Store) Nodes() ([]NodeEntry, error) { return loadList[NodeEntry](s.cluster, kindNodes) }

// SaveNode stores a node entry under its name.
func (s *Store) SaveNode(n NodeEntry) error {
	if n.Name == "" {
		return errors.New("node name is empty")
	}
	return s.cluster.put(kindNodes, n.Name, n, true)
}

// DeleteNode removes a node entry. A missing one is not an error.
func (s *Store) DeleteNode(name string) error { return s.cluster.Delete(kindNodes, name) }

// Claims returns the claims by hostname.
func (s *Store) Claims() (map[string]planner.Claim, error) {
	return loadMap(s.cluster, kindClaims, "hostname", func(c *planner.Claim) *string { return &c.Hostname })
}

// SaveClaims makes next the stored claims: it writes the ones that changed and
// deletes the ones that are gone. A claim whose Hostname is empty is stored
// under the key; one that names another hostname is an error.
func (s *Store) SaveClaims(next map[string]planner.Claim) error {
	return saveMap(s.cluster, kindClaims, next, func(c *planner.Claim) *string { return &c.Hostname })
}

// Bindings returns the bindings by hostname. They live on the node-local root,
// as they change every cycle.
func (s *Store) Bindings() (map[string]resolve.Binding, error) {
	return loadMap(s.local, kindBindings, "hostname", func(b *resolve.Binding) *string { return &b.Hostname })
}

// SaveBindings makes next the stored bindings, as SaveClaims does for claims.
func (s *Store) SaveBindings(next map[string]resolve.Binding) error {
	return saveMap(s.local, kindBindings, next, func(b *resolve.Binding) *string { return &b.Hostname })
}

// ManualRoutes returns the routes an admin made by hand, by id.
func (s *Store) ManualRoutes() ([]model.Route, error) {
	return loadList[model.Route](s.cluster, kindRoutes)
}

// SaveManualRoute stores a manual route under its ManualID, with its hostname
// in normal form. A route without an id or with a hostname that is not valid
// is refused.
func (s *Store) SaveManualRoute(r model.Route) error {
	if r.ManualID == "" {
		return errors.New("manual route has no id")
	}
	host, err := hostname.Normalize(r.Hostname)
	if err != nil {
		return fmt.Errorf("manual route %s: %w", r.ManualID, err)
	}
	r.Hostname = host
	return s.cluster.put(kindRoutes, r.ManualID, r, true)
}

// DeleteManualRoute removes a manual route. A missing one is not an error.
func (s *Store) DeleteManualRoute(id string) error { return s.cluster.Delete(kindRoutes, id) }

// Approvals returns the identity approved for each owner.
func (s *Store) Approvals() (map[string]string, error) {
	byOwner, err := loadMap(s.cluster, kindApprovals, "owner", func(a *approval) *string { return &a.Owner })
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(byOwner))
	for owner, a := range byOwner {
		out[owner] = a.Identity
	}
	return out, nil
}

// SaveApproval records that the guest owner, in the identity given, may be
// published.
func (s *Store) SaveApproval(owner, identity string) error {
	if owner == "" || identity == "" {
		return errors.New("approval needs an owner and an identity")
	}
	return s.cluster.put(kindApprovals, owner, approval{Owner: owner, Identity: identity}, true)
}

// DeleteApproval removes the approval of an owner. A missing one is not an
// error.
func (s *Store) DeleteApproval(owner string) error { return s.cluster.Delete(kindApprovals, owner) }

// Credentials returns the Cloudflare credentials, in the order of their ids.
// They are kept on the private root only.
func (s *Store) Credentials() ([]Credential, error) {
	return loadList[Credential](s.private, kindCredentials)
}

// SaveCredential stores a credential under its id.
func (s *Store) SaveCredential(c Credential) error {
	if c.ID == "" {
		return errors.New("credential id is empty")
	}
	if c.Token == "" {
		return fmt.Errorf("credential %s: the token is empty", c.ID)
	}
	return s.private.put(kindCredentials, c.ID, c, true)
}

// DeleteCredential removes a credential. A missing one is not an error.
func (s *Store) DeleteCredential(id string) error { return s.private.Delete(kindCredentials, id) }

// PVEToken returns the Proxmox API token of the daemon.
func (s *Store) PVEToken() (PVEToken, bool, error) {
	return getOne[PVEToken](s.private, kindMeta, idPVEToken)
}

// SavePVEToken stores the Proxmox API token of the daemon.
func (s *Store) SavePVEToken(t PVEToken) error {
	if t.TokenID == "" || t.Secret == "" {
		return errors.New("pve token needs an id and a secret")
	}
	return s.private.put(kindMeta, idPVEToken, t, true)
}
