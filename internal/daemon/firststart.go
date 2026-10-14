package daemon

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/registry"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// earlierRelease names the release an upgrade came from when the node's
// entry does not say.
const earlierRelease = "an earlier release"

// firstStart is what the first start of this release does to a store an
// earlier release made, once this node may write: it records that the
// upgrade is not finished, so that no setting that release would refuse is
// saved, records the identity of the cluster, makes the install's CA, makes
// the cluster copies of what a controller on another node must inherit, and
// numbers and activates the one node the registry names without a state.
// Each part is done once, and only what is missing.
type firstStart struct {
	st      *store.Store
	node    string
	version string
	now     func() time.Time
	rand    io.Reader
	cluster func() (store.ClusterIdentity, error)
	log     zerolog.Logger

	// upgradeFrom is the release the store was made by, when it was made
	// before this one: it holds an install without the identity of its
	// cluster. It is read as the daemon starts, before any cycle records
	// the identity.
	upgradeFrom string
	done        bool
}

// newFirstStart reads, before anything is written, whether the store was
// made by an earlier release.
func newFirstStart(st *store.Store, node, version string, now func() time.Time, cluster func() (store.ClusterIdentity, error),
	log zerolog.Logger) *firstStart {
	f := &firstStart{st: st, node: node, version: version, now: now, rand: rand.Reader, cluster: cluster, log: log}
	inst, found, err := st.Install()
	if err != nil || !found || inst.Cluster != nil {
		return f
	}
	f.upgradeFrom = earlierRelease
	if nodes, err := st.Nodes(); err == nil {
		for _, n := range nodes {
			if n.Name == node && n.Version != "" && n.Version != version {
				f.upgradeFrom = n.Version
			}
		}
	}
	return f
}

// try does what is left, when may says that this node may write now.
func (f *firstStart) try(may func() error) {
	if f.done {
		return
	}
	if err := may(); err != nil {
		return
	}
	if err := f.run(); err != nil {
		f.log.Warn().Err(err).Msg("the first start of this release is not done yet; it is tried again")
		return
	}
	f.done = true
}

func (f *firstStart) run() error {
	inst, found, err := f.st.Install()
	switch {
	case err != nil:
		return fmt.Errorf("reading the install: %w", err)
	case !found:
		return errors.New("pco is not set up on this node")
	}
	// Each part is done apart from the others: one that fails does not
	// keep the rest from being done.
	return errors.Join(f.upgrade(), f.identity(inst), f.ca(inst), f.copies(), f.number())
}

// upgrade records an upgrade from an earlier release, once.
func (f *firstStart) upgrade() error {
	if f.upgradeFrom == "" {
		return nil
	}
	if _, found, err := f.st.UpgradeState(); err != nil || found {
		return err
	}
	if err := f.st.SaveUpgradeState(store.UpgradeState{From: f.upgradeFrom, Since: f.now()}); err != nil {
		return fmt.Errorf("recording the upgrade from %s: %w", f.upgradeFrom, err)
	}
	f.log.Info().Str("from", f.upgradeFrom).Msg("recorded the upgrade from an earlier release; pco cluster finish-upgrade ends it")
	return nil
}

// identity records the identity of the cluster with an install that has
// none.
func (f *firstStart) identity(inst store.Install) error {
	if inst.Cluster != nil || f.cluster == nil {
		return nil
	}
	id, err := f.cluster()
	if err != nil {
		return fmt.Errorf("reading the identity of the cluster: %w", err)
	}
	inst.Cluster = &id
	if err := f.st.SaveInstall(inst); err != nil {
		return fmt.Errorf("recording the identity of the cluster: %w", err)
	}
	return nil
}

// ca makes the install's CA when it has none, and names it in leader.json.
func (f *firstStart) ca(inst store.Install) error {
	cur, _, _, err := f.st.CA()
	switch {
	case err == nil:
		return f.nameCA(cur)
	case !errors.Is(err, store.ErrNoCA):
		return fmt.Errorf("reading the install's CA: %w", err)
	}
	ca, err := pki.NewCA(inst.ID, f.now(), f.rand)
	if err != nil {
		return fmt.Errorf("making the install's CA: %w", err)
	}
	certPEM, keyPEM, err := ca.PEM()
	if err != nil {
		return err
	}
	if err := f.st.SaveCAIfAbsent(store.CAFiles{CertPEM: string(certPEM), KeyPEM: store.NewSecret(string(keyPEM))}); err != nil &&
		!errors.Is(err, store.ErrRevision) {
		return fmt.Errorf("storing the install's CA: %w", err)
	}
	if cur, _, _, err = f.st.CA(); err != nil {
		return fmt.Errorf("reading the install's CA: %w", err)
	}
	f.log.Info().Msg("made the install's CA")
	return f.nameCA(cur)
}

// nameCA writes the fingerprint of the CA into leader.json, where the
// agents read which CA to trust.
func (f *firstStart) nameCA(cur store.CAFiles) error {
	ca, err := pki.ParseCA([]byte(cur.CertPEM), []byte(cur.KeyPEM.Reveal()))
	if err != nil {
		return fmt.Errorf("reading the install's CA: %w", err)
	}
	for range 3 {
		rec, rev, found, err := f.st.Leader()
		if err != nil || !found {
			return fmt.Errorf("reading leader.json: %w", errors.Join(err, errNoLeader(found)))
		}
		if rec.CASHA256 == ca.SHA256() {
			return nil
		}
		rec.CASHA256 = ca.SHA256()
		if _, err := f.st.SaveLeaderIf(rec, rev); err == nil {
			return nil
		} else if !errors.Is(err, store.ErrRevision) {
			return fmt.Errorf("naming the CA in leader.json: %w", err)
		}
	}
	return errors.New("leader.json kept changing while the CA was named in it")
}

func errNoLeader(found bool) error {
	if found {
		return nil
	}
	return errors.New("leader.json is missing")
}

// copies reads what a controller on another node inherits, which makes the
// copy of each on the cluster root that is missing.
func (f *firstStart) copies() error {
	if _, _, err := f.st.LoadEngineMemory(); err != nil {
		return fmt.Errorf("copying the engine memory: %w", err)
	}
	if _, _, err := f.st.LoadNodeAddrs(); err != nil {
		return fmt.Errorf("copying the node addresses: %w", err)
	}
	if _, _, err := f.st.LoadSoftDeny(); err != nil {
		return fmt.Errorf("copying the soft deny list: %w", err)
	}
	return nil
}

// number gives the node an earlier release registered, the only entry
// without a state, its index and makes it active, in one write: index 0,
// unless another node holds it already.
func (f *firstStart) number() error {
	nodes, err := f.st.NodesRev()
	if err != nil {
		return fmt.Errorf("reading the node registry: %w", err)
	}
	entries := make([]store.NodeEntry, 0, len(nodes))
	var self *store.NodeRev
	for i, n := range nodes {
		entries = append(entries, n.Node)
		if n.Node.Name == f.node {
			self = &nodes[i]
		}
	}
	if self == nil || self.Node.State != "" {
		return nil
	}
	i, err := registry.AssignIndex(entries, f.node, map[string]int{f.node: 0})
	if err != nil {
		return err
	}
	n := self.Node
	n.Index, n.State, n.StateSince = &i, store.NodeActive, f.now()
	if _, err := f.st.SaveNodeIf(n, self.Rev); err != nil {
		return fmt.Errorf("numbering node %s: %w", f.node, err)
	}
	return nil
}
