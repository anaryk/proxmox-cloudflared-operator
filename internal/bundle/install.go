package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var (
	// ErrNoInstall is the error of a store that holds no install to collect.
	ErrNoInstall = errors.New("the store holds no install")
	// ErrInstalled is the error of a restore into a store that holds an
	// install already. It comes wrapped, naming the install.
	ErrInstalled = errors.New("the store holds an install already")
	// ErrOtherBundle is the error of a restore into a store that holds an
	// object the bundle does not carry: what a restore of another bundle
	// left when it stopped half way, which a restore never merges with. It
	// comes wrapped, naming the kind and the object.
	ErrOtherBundle = errors.New("the store holds objects the bundle does not carry")
)

// redactedToken is what a credential's token encodes to when something that
// does not know better encodes it: a bundle that holds it holds no token.
const redactedToken = "[redacted]"

// credentialDocument is a credential as the bundle holds it, the one place
// besides the store's file where its token is a plain string.
type credentialDocument struct {
	ID      string    `json:"id"`
	Label   string    `json:"label"`
	Kind    string    `json:"kind"`
	Token   string    `json:"token"`
	AddedAt time.Time `json:"addedAt"`
}

// Collect reads the install out of the store: its identity and the cluster
// it was made in, the settings, manual routes, approvals and acknowledged
// segments, the registry of nodes, the CA with its key, and the previous CA
// while a rotation runs, leader.json, and every other object the controller
// writes: the networks and service addresses, the leases, consents and
// attachments of the managed network, the claims, tombstones, placements and
// moves, and the progress of a rotation. The Cloudflare credentials, with
// their tokens, are read only when credentials says so. Run tokens, bindings
// and the caches are left out: the daemon builds them again.
func Collect(ctx context.Context, st *store.Store, credentials bool, now time.Time, build string) (Contents, error) {
	inst, found, err := st.Install()
	switch {
	case err != nil:
		return Contents{}, fmt.Errorf("reading the install: %w", err)
	case !found:
		return Contents{}, ErrNoInstall
	}
	c := Contents{InstallID: inst.ID, InstalledAt: inst.CreatedAt, MadeAt: now.UTC(), Build: build}
	if inst.Cluster != nil {
		if c.Cluster, err = json.Marshal(inst.Cluster); err != nil {
			return Contents{}, err
		}
	}
	for _, part := range []func(context.Context, *store.Store, *Contents) error{
		collectDecisions, collectPKI, collectNetworks, collectManaged, collectEngine,
	} {
		if err := part(ctx, st, &c); err != nil {
			return Contents{}, err
		}
	}
	if credentials {
		if c.Credentials, err = collectCredentials(st); err != nil {
			return Contents{}, err
		}
	}
	return c, nil
}

func collectDecisions(_ context.Context, st *store.Store, c *Contents) (err error) {
	settings, err := st.Settings()
	if err != nil {
		return fmt.Errorf("reading the settings: %w", err)
	}
	if c.Settings, err = json.Marshal(settings); err != nil {
		return err
	}
	if c.ManualRoutes, err = listOf("manual routes", st.ManualRoutes); err != nil {
		return err
	}
	if c.Approvals, err = mapOf("approvals", st.Approvals); err != nil {
		return err
	}
	if c.Segments, err = mapOf("acknowledged segments", st.Segments); err != nil {
		return err
	}
	if c.NodeSegments, err = mapOf("segments acknowledged on nodes", st.NodeSegments); err != nil {
		return err
	}
	c.Nodes, err = listOf("registry of nodes", st.Nodes)
	return err
}

func collectPKI(_ context.Context, st *store.Store, c *Contents) error {
	switch current, previous, rotatedAt, err := st.CA(); {
	case errors.Is(err, store.ErrNoCA):
	case err != nil:
		return fmt.Errorf("reading the CA: %w", err)
	default:
		c.CA, c.PreviousCA, c.CARotatedAt = &current, previous, rotatedAt
	}
	leader, _, found, err := st.Leader()
	if err != nil {
		return fmt.Errorf("reading leader.json: %w", err)
	}
	if found {
		if c.Leader, err = json.Marshal(leader); err != nil {
			return err
		}
	}
	rotation, found, err := st.CARotation()
	if err != nil {
		return fmt.Errorf("reading the progress of the rotation: %w", err)
	}
	if found {
		if c.CARotation, err = json.Marshal(rotation); err != nil {
			return err
		}
	}
	observe, found, err := st.ObserveSince()
	if err != nil {
		return fmt.Errorf("reading the start of observe-only mode: %w", err)
	}
	if found {
		if c.Observe, err = json.Marshal(observe); err != nil {
			return err
		}
	}
	return nil
}

func collectNetworks(_ context.Context, st *store.Store, c *Contents) (err error) {
	if c.Networks, err = mapOf("networks", func() (map[string]store.Network, error) {
		nets, err := st.Networks()
		for id, n := range nets {
			// The revision is the file's, not the network's.
			n.Rev = 0
			nets[id] = n
		}
		return nets, err
	}); err != nil {
		return err
	}
	if c.ServiceAddrs, err = listOf("service addresses", st.ServiceAddrs); err != nil {
		return err
	}
	if c.NodeServiceAddrs, err = listOf("service addresses of nodes", st.NodeServiceAddrs); err != nil {
		return err
	}
	if c.ServicePoolNext, _, _, err = st.ServicePool(); err != nil {
		return fmt.Errorf("reading the pool of service addresses: %w", err)
	}
	return nil
}

func collectManaged(_ context.Context, st *store.Store, c *Contents) (err error) {
	if c.Leases, err = mapOf("leases", func() (map[string]store.Lease, error) {
		leases, err := st.Leases()
		for mac, l := range leases {
			l.Rev = 0
			leases[mac] = l
		}
		return leases, err
	}); err != nil {
		return err
	}
	if c.Consents, err = mapOf("consents", st.Consents); err != nil {
		return err
	}
	c.Attachments, err = mapOf("attachments", st.Attachments)
	return err
}

func collectEngine(ctx context.Context, st *store.Store, c *Contents) (err error) {
	if c.Claims, err = mapOf("claims", st.Claims); err != nil {
		return err
	}
	tombstones, err := st.Tombstones().Load(ctx)
	if err != nil {
		return fmt.Errorf("reading the tombstones: %w", err)
	}
	if len(tombstones) > 0 {
		if c.Tombstones, err = json.Marshal(tombstones); err != nil {
			return err
		}
	}
	if c.Placements, err = mapOf("placements", st.Placements); err != nil {
		return err
	}
	c.Moves, err = mapOf("moves", st.Moves)
	return err
}

func collectCredentials(st *store.Store) ([]json.RawMessage, error) {
	creds, err := st.Credentials()
	if err != nil {
		return nil, fmt.Errorf("reading the credentials: %w", err)
	}
	docs := make([]credentialDocument, len(creds))
	for i, cred := range creds {
		docs[i] = credentialDocument{ID: cred.ID, Label: cred.Label, Kind: cred.Kind, Token: cred.Token.Reveal(), AddedAt: cred.AddedAt}
	}
	return encodeAll(docs)
}

// listOf reads the objects of one kind and encodes each.
func listOf[T any](what string, read func() ([]T, error)) ([]json.RawMessage, error) {
	objects, err := read()
	if err != nil {
		return nil, fmt.Errorf("reading the %s: %w", what, err)
	}
	return encodeAll(objects)
}

// mapOf is listOf for objects by key, in the order of the keys, so that two
// bundles of one store hold their objects in one order.
func mapOf[T any](what string, read func() (map[string]T, error)) ([]json.RawMessage, error) {
	return listOf(what, func() ([]T, error) {
		m, err := read()
		if err != nil {
			return nil, err
		}
		return inOrder(m), nil
	})
}

// encodeAll encodes each of the objects, left nil when there are none.
func encodeAll[T any](objects []T) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for _, o := range objects {
		raw, err := json.Marshal(o)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

func inOrder[T any](m map[string]T) []T {
	out := make([]T, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, m[k])
	}
	return out
}

// restoration is what a bundle holds, read as the store's types.
type restoration struct {
	created          time.Time
	cluster          *store.ClusterIdentity
	settings         store.Settings
	routes           []model.Route
	approvals        []store.Approval
	segments         []store.Segment
	nodeSegments     []store.NodeSegment
	nodes            []store.NodeEntry
	ca, previousCA   *store.CAFiles
	caRotatedAt      time.Time
	caRotation       *store.CARotation
	credentials      []store.Credential
	leader           *store.LeaderRecord
	networks         []store.Network
	serviceAddrs     []store.ServiceAddr
	nodeServiceAddrs []store.NodeServiceAddr
	poolNext         netip.Addr
	leases           []store.Lease
	consents         []store.Consent
	attachments      []store.Attachment
	claims           map[string]planner.Claim
	tombstones       map[string]reconcile.Tombstone
	placements       map[string]store.Placement
	moves            []store.Move
}

// decodeStrict reads one object of the bundle, refusing what the type has no
// field for: it would be left out of the restore.
func decodeStrict(raw json.RawMessage, v any) error { return unmarshalStrict(raw, v) }

func decodeEach[T any](what string, raws []json.RawMessage) ([]T, error) {
	var out []T
	for i, raw := range raws {
		var v T
		if err := decodeStrict(raw, &v); err != nil {
			return nil, fmt.Errorf("%s %d of the bundle: %w", what, i+1, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// decodeKeyed is decodeEach for objects the store keeps by a key each
// names itself, which none may leave out.
func decodeKeyed[T any](what, field string, raws []json.RawMessage, key func(T) string) (map[string]T, error) {
	items, err := decodeEach[T](what, raws)
	if err != nil {
		return nil, err
	}
	out := make(map[string]T, len(items))
	for i, item := range items {
		k := key(item)
		if k == "" {
			return nil, fmt.Errorf("%s %d of the bundle has no %s", what, i+1, field)
		}
		out[k] = item
	}
	return out, nil
}

// read turns the contents into the store's objects, which writes nothing, so
// that a bundle that cannot be read is found out before the store is touched.
func read(c Contents) (r restoration, err error) {
	r.created = c.InstalledAt
	if r.created.IsZero() {
		r.created = c.MadeAt
	}
	if len(c.Cluster) > 0 {
		var cluster store.ClusterIdentity
		if err := decodeStrict(c.Cluster, &cluster); err != nil {
			return r, fmt.Errorf("the cluster of the bundle: %w", err)
		}
		if cluster.CASHA256 == "" {
			return r, errors.New("the cluster of the bundle has no fingerprint")
		}
		r.cluster = &cluster
	}
	for _, part := range []func(Contents, *restoration) error{readDecisions, readPKI, readCredentials, readNetworks, readEngine} {
		if err := part(c, &r); err != nil {
			return r, err
		}
	}
	return r, nil
}

func readDecisions(c Contents, r *restoration) (err error) {
	r.settings = store.DefaultSettings()
	if len(c.Settings) > 0 {
		if err := decodeStrict(c.Settings, &r.settings); err != nil {
			return fmt.Errorf("the settings of the bundle: %w", err)
		}
	}
	if r.routes, err = decodeEach[model.Route]("manual route", c.ManualRoutes); err != nil {
		return err
	}
	if r.approvals, err = decodeEach[store.Approval]("approval", c.Approvals); err != nil {
		return err
	}
	if r.segments, err = decodeEach[store.Segment]("segment", c.Segments); err != nil {
		return err
	}
	if r.nodeSegments, err = decodeEach[store.NodeSegment]("node segment", c.NodeSegments); err != nil {
		return err
	}
	r.nodes, err = decodeEach[store.NodeEntry]("node", c.Nodes)
	return err
}

func readPKI(c Contents, r *restoration) error {
	if c.CA != nil {
		if c.CA.CertPEM == "" || c.CA.KeyPEM.Reveal() == "" {
			return errors.New("the CA of the bundle has no certificate or no key")
		}
		r.ca = c.CA
	}
	if c.PreviousCA != nil {
		switch {
		case r.ca == nil:
			return errors.New("the bundle holds the previous CA of a rotation and no current one")
		case c.PreviousCA.CertPEM == "" || c.PreviousCA.KeyPEM.Reveal() == "":
			return errors.New("the previous CA of the bundle has no certificate or no key")
		case c.CARotatedAt.IsZero():
			return errors.New("the bundle holds the previous CA of a rotation without the time it was rotated at")
		}
		r.previousCA = c.PreviousCA
	}
	r.caRotatedAt = c.CARotatedAt
	if len(c.CARotation) > 0 {
		var rotation store.CARotation
		if err := decodeStrict(c.CARotation, &rotation); err != nil {
			return fmt.Errorf("the progress of the rotation of the bundle: %w", err)
		}
		r.caRotation = &rotation
	}
	if len(c.Leader) > 0 {
		var leader store.LeaderRecord
		if err := decodeStrict(c.Leader, &leader); err != nil {
			return fmt.Errorf("leader.json of the bundle: %w", err)
		}
		if err := leader.Validate(); err != nil {
			return fmt.Errorf("leader.json of the bundle: %w", err)
		}
		if leader.InstallID != c.InstallID {
			return fmt.Errorf("leader.json of the bundle names install %s, and the bundle is of install %s", leader.InstallID, c.InstallID)
		}
		r.leader = &leader
	}
	if len(c.Observe) > 0 {
		// Read to refuse a damaged bundle, and not restored: the restored
		// install observes from the restore on, and so begins the window
		// of the certificates of serials it does not know.
		var observe store.ObserveSince
		if err := decodeStrict(c.Observe, &observe); err != nil {
			return fmt.Errorf("the start of observe-only mode of the bundle: %w", err)
		}
	}
	return nil
}

func readCredentials(c Contents, r *restoration) error {
	docs, err := decodeEach[credentialDocument]("credential", c.Credentials)
	if err != nil {
		// What failed could be a token: say which credential, not what is in it.
		return errors.New("a credential of the bundle cannot be read")
	}
	for i, d := range docs {
		switch d.Token {
		case "", redactedToken:
			return fmt.Errorf("credential %d of the bundle, %q, holds no token: it was exported without it", i+1, d.ID)
		}
		r.credentials = append(r.credentials, store.Credential{ID: d.ID, Label: d.Label, Kind: d.Kind, Token: store.NewSecret(d.Token), AddedAt: d.AddedAt})
	}
	return nil
}

func readNetworks(c Contents, r *restoration) (err error) {
	if r.networks, err = decodeEach[store.Network]("network", c.Networks); err != nil {
		return err
	}
	if r.serviceAddrs, err = decodeEach[store.ServiceAddr]("service address", c.ServiceAddrs); err != nil {
		return err
	}
	if r.nodeServiceAddrs, err = decodeEach[store.NodeServiceAddr]("service address of a node", c.NodeServiceAddrs); err != nil {
		return err
	}
	r.poolNext = c.ServicePoolNext
	if r.leases, err = decodeEach[store.Lease]("lease", c.Leases); err != nil {
		return err
	}
	if r.consents, err = decodeEach[store.Consent]("consent", c.Consents); err != nil {
		return err
	}
	r.attachments, err = decodeEach[store.Attachment]("attachment", c.Attachments)
	return err
}

func readEngine(c Contents, r *restoration) (err error) {
	if r.claims, err = decodeKeyed("claim", "hostname", c.Claims, func(cl planner.Claim) string { return cl.Hostname }); err != nil {
		return err
	}
	if len(c.Tombstones) > 0 {
		if err := decodeStrict(c.Tombstones, &r.tombstones); err != nil {
			return fmt.Errorf("the tombstones of the bundle: %w", err)
		}
	}
	if r.placements, err = decodeKeyed("placement", "hostname", c.Placements, func(p store.Placement) string { return p.Hostname }); err != nil {
		return err
	}
	r.moves, err = decodeEach[store.Move]("move", c.Moves)
	return err
}

// NoInstall returns ErrInstalled, naming the install, when the store holds
// one: a restore goes into a store that has none.
func NoInstall(st *store.Store) error {
	switch inst, found, err := st.Install(); {
	case err != nil:
		return fmt.Errorf("reading the install: %w", err)
	case found:
		return fmt.Errorf("%w: it holds install %s", ErrInstalled, inst.ID)
	}
	return nil
}

// SameCA reports whether the CA of the store, the previous one and the time
// of the rotation are still what the bundle holds: an export that ran while
// a rotation moved them is run again.
func SameCA(st *store.Store, c Contents) (bool, error) {
	current, previous, rotatedAt, err := st.CA()
	switch {
	case errors.Is(err, store.ErrNoCA):
		return c.CA == nil, nil
	case err != nil:
		return false, fmt.Errorf("reading the CA: %w", err)
	case c.CA == nil || !sameFiles(*c.CA, current) || !c.CARotatedAt.Equal(rotatedAt):
		return false, nil
	case (c.PreviousCA == nil) != (previous == nil):
		return false, nil
	}
	return previous == nil || sameFiles(*c.PreviousCA, *previous), nil
}

func sameFiles(a, b store.CAFiles) bool { return a.CertPEM == b.CertPEM && a.KeyPEM.Equal(b.KeyPEM) }

// Restore writes the install of a bundle into a store that holds none, whose
// roots are made. The settings are stored in observe-only mode, whatever they
// were: the restored install publishes nothing until an admin says so. The
// install is written last, with the cluster it was made in, so that a restore
// that stops half way leaves a store that holds none, and is run again with
// the same bundle, over what the first run wrote. A store that holds an
// object of another bundle is refused: two bundles are never merged.
func Restore(ctx context.Context, st *store.Store, c Contents) error {
	r, err := read(c)
	if err != nil {
		return err
	}
	if err := NoInstall(st); err != nil {
		return err
	}
	if err := noLeftovers(st, r); err != nil {
		return err
	}
	for _, step := range []func(context.Context, *store.Store, restoration) error{
		restorePKI, restoreDecisions, restoreNetworks, restoreEngine,
	} {
		if err := step(ctx, st, r); err != nil {
			return err
		}
	}
	if r.leader != nil {
		if err := st.SaveLeader(*r.leader); err != nil {
			return fmt.Errorf("restoring leader.json: %w", err)
		}
	}
	if err := st.SaveInstall(store.Install{ID: c.InstallID, CreatedAt: r.created, Profile: store.ProfileHost, Cluster: r.cluster}); err != nil {
		return fmt.Errorf("restoring the install: %w", err)
	}
	return nil
}

func restorePKI(_ context.Context, st *store.Store, r restoration) error {
	if r.ca != nil {
		if err := st.SaveCA(*r.ca, r.previousCA, r.caRotatedAt); err != nil {
			return fmt.Errorf("restoring the CA: %w", err)
		}
	}
	if r.caRotation != nil {
		if err := st.SaveCARotation(*r.caRotation); err != nil {
			return fmt.Errorf("restoring the progress of the rotation: %w", err)
		}
	}
	return saveEach("credentials", r.credentials, st.SaveCredential)
}

func restoreDecisions(_ context.Context, st *store.Store, r restoration) error {
	r.settings.ObserveOnly = true
	if err := st.SaveSettings(r.settings); err != nil {
		return fmt.Errorf("restoring the settings: %w", err)
	}
	if err := saveEach("registry of nodes", r.nodes, st.SaveNode); err != nil {
		return err
	}
	if err := saveEach("acknowledged segments", r.segments, st.SaveSegment); err != nil {
		return err
	}
	if err := saveEach("segments acknowledged on nodes", r.nodeSegments, st.SaveNodeSegment); err != nil {
		return err
	}
	if err := saveEach("approvals", r.approvals, st.SaveApproval); err != nil {
		return err
	}
	return saveEach("manual routes", r.routes, st.SaveManualRoute)
}

// noLeftovers refuses a store that holds an object of a kind the restore
// writes which the bundle does not carry. What the bundle carries is written
// over again, so that a restore that stopped half way is run again.
func noLeftovers(st *store.Store, r restoration) error {
	credentials, err := st.Credentials()
	if err != nil {
		return fmt.Errorf("reading the credentials: %w", err)
	}
	nodes, err := st.Nodes()
	if err != nil {
		return fmt.Errorf("reading the registry of nodes: %w", err)
	}
	routes, err := st.ManualRoutes()
	if err != nil {
		return fmt.Errorf("reading the manual routes: %w", err)
	}
	serviceAddrs, err := st.ServiceAddrs()
	if err != nil {
		return fmt.Errorf("reading the service addresses: %w", err)
	}
	nodeServiceAddrs, err := st.NodeServiceAddrs()
	if err != nil {
		return fmt.Errorf("reading the service addresses of nodes: %w", err)
	}
	for _, check := range []func() error{
		func() error {
			return leftover("credential", ids(credentials, func(c store.Credential) string { return c.ID }), ids(r.credentials, func(c store.Credential) string { return c.ID }))
		},
		func() error {
			return leftover("node", ids(nodes, func(n store.NodeEntry) string { return n.Name }), ids(r.nodes, func(n store.NodeEntry) string { return n.Name }))
		},
		func() error {
			return leftover("manual route", ids(routes, func(r model.Route) string { return r.ManualID }), ids(r.routes, func(r model.Route) string { return r.ManualID }))
		},
		func() error { return leftoverKeyed("segment", st.Segments, ids(r.segments, store.Segment.ID)) },
		func() error {
			return leftoverKeyed("node segment", st.NodeSegments, ids(r.nodeSegments, store.NodeSegment.ID))
		},
		func() error {
			return leftoverKeyed("approval", st.Approvals, ids(r.approvals, func(a store.Approval) string { return a.Owner }))
		},
		func() error {
			return leftoverKeyed("network", st.Networks, ids(r.networks, func(n store.Network) string { return n.ID }))
		},
		func() error {
			return leftover("service address", ids(serviceAddrs, serviceAddrKey), ids(r.serviceAddrs, serviceAddrKey))
		},
		func() error {
			return leftover("service address of a node", ids(nodeServiceAddrs, nodeServiceAddrKey), ids(r.nodeServiceAddrs, nodeServiceAddrKey))
		},
		func() error {
			return leftoverKeyed("lease", st.Leases, ids(r.leases, func(l store.Lease) string { return l.MAC }))
		},
		func() error {
			return leftoverKeyed("consent", st.Consents, ids(r.consents, func(c store.Consent) string { return c.Owner }))
		},
		func() error {
			return leftoverKeyed("attachment", st.Attachments, ids(r.attachments, func(a store.Attachment) string { return a.Owner }))
		},
		func() error { return leftoverKeyed("claim", st.Claims, slices.Collect(maps.Keys(r.claims))) },
		func() error {
			return leftoverKeyed("placement", st.Placements, slices.Collect(maps.Keys(r.placements)))
		},
		func() error {
			return leftoverKeyed("move", st.Moves, ids(r.moves, func(m store.Move) string { return m.Owner }))
		},
	} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func serviceAddrKey(m store.ServiceAddr) string { return m.Network + " " + m.Addr.String() }

func nodeServiceAddrKey(m store.NodeServiceAddr) string {
	return m.Network + " " + m.Node + " " + m.Addr.String()
}

func ids[T any](objects []T, id func(T) string) []string {
	out := make([]string, 0, len(objects))
	for _, o := range objects {
		out = append(out, id(o))
	}
	return out
}

// leftoverKeyed is leftover for a kind the store reads by key.
func leftoverKeyed[T any](kind string, read func() (map[string]T, error), carried []string) error {
	have, err := read()
	if err != nil {
		return fmt.Errorf("reading the %ss: %w", kind, err)
	}
	return leftover(kind, slices.Collect(maps.Keys(have)), carried)
}

// leftover refuses an object the store holds that the bundle does not carry.
func leftover(kind string, have, carried []string) error {
	known := make(map[string]bool, len(carried))
	for _, id := range carried {
		known[id] = true
	}
	for _, id := range slices.Sorted(slices.Values(have)) {
		if !known[id] {
			return fmt.Errorf("%w: %s %s; clear the store, or import the bundle it was restored from", ErrOtherBundle, kind, id)
		}
	}
	return nil
}

// restoreNetworks writes the networks, the service addresses and then the
// pool, which is checked against them, and what the managed network holds,
// whose leases are checked against the settings. A network or a lease a run
// that stopped half way wrote is written over at the revision it has.
func restoreNetworks(_ context.Context, st *store.Store, r restoration) error {
	indexes := make(map[string]int)
	for _, n := range r.nodes {
		if n.Index != nil {
			indexes[n.Name] = *n.Index
		}
	}
	networks, err := st.Networks()
	if err != nil {
		return fmt.Errorf("reading the networks: %w", err)
	}
	if err := saveEach("networks", r.networks, func(n store.Network) error {
		return st.SaveNetworkIndexed(n, networks[n.ID].Rev, indexesFor(n, indexes))
	}); err != nil {
		return err
	}
	if err := saveEach("service addresses", r.serviceAddrs, st.SaveServiceAddr); err != nil {
		return err
	}
	if err := saveEach("service addresses of nodes", r.nodeServiceAddrs, st.SaveNodeServiceAddr); err != nil {
		return err
	}
	if r.poolNext.IsValid() {
		if err := st.SaveServicePool(r.poolNext); err != nil {
			return fmt.Errorf("restoring the pool of service addresses: %w", err)
		}
	}
	leases, err := st.Leases()
	if err != nil {
		return fmt.Errorf("reading the leases: %w", err)
	}
	if err := saveEach("leases", r.leases, func(l store.Lease) error { return st.SaveLease(l, leases[l.MAC].Rev) }); err != nil {
		return err
	}
	if err := saveEach("consents", r.consents, st.SaveConsent); err != nil {
		return err
	}
	return saveEach("attachments", r.attachments, st.SaveAttachment)
}

// indexesFor returns the indexes a network is saved with: those of the
// registry, unless a leg belongs to a node that has no index and no leg MAC
// of its own, as in a store from before the nodes had indexes; such a
// network is then saved as it was stored, without them.
func indexesFor(n store.Network, indexes map[string]int) map[string]int {
	for node := range n.Legs {
		_, indexed := indexes[node]
		_, given := n.LegMACs[node]
		if !indexed && !given {
			return nil
		}
	}
	return indexes
}

func restoreEngine(ctx context.Context, st *store.Store, r restoration) error {
	if len(r.claims) > 0 {
		if err := st.SaveClaims(r.claims); err != nil {
			return fmt.Errorf("restoring the claims: %w", err)
		}
	}
	if len(r.tombstones) > 0 {
		if err := st.Tombstones().Save(ctx, r.tombstones); err != nil {
			return fmt.Errorf("restoring the tombstones: %w", err)
		}
	}
	if len(r.placements) > 0 {
		if err := st.SavePlacements(r.placements); err != nil {
			return fmt.Errorf("restoring the placements: %w", err)
		}
	}
	return saveEach("moves", r.moves, st.SaveMove)
}

// saveEach writes the objects of one kind, naming the kind in the error.
func saveEach[T any](what string, objects []T, save func(T) error) error {
	for _, o := range objects {
		if err := save(o); err != nil {
			return fmt.Errorf("restoring the %s: %w", what, err)
		}
	}
	return nil
}
