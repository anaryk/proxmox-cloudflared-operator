package bundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var (
	// ErrNoInstall is the error of a store that holds no install to collect.
	ErrNoInstall = errors.New("the store holds no install")
	// ErrInstalled is the error of a restore into a store that holds an
	// install already. It comes wrapped, naming the install.
	ErrInstalled = errors.New("the store holds an install already")
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

// Collect reads the install out of the store: its identity, settings, manual
// routes, approvals, acknowledged segments, the registry of nodes, the CA with
// its key and leader.json. The Cloudflare credentials, with their tokens, are
// read only when credentials says so. Run tokens, bindings and the caches are
// left out: the daemon builds them again.
func Collect(st *store.Store, credentials bool, now time.Time, build string) (Contents, error) {
	inst, found, err := st.Install()
	switch {
	case err != nil:
		return Contents{}, fmt.Errorf("reading the install: %w", err)
	case !found:
		return Contents{}, ErrNoInstall
	}
	c := Contents{InstallID: inst.ID, InstalledAt: inst.CreatedAt, MadeAt: now.UTC(), Build: build}

	settings, err := st.Settings()
	if err != nil {
		return Contents{}, fmt.Errorf("reading the settings: %w", err)
	}
	if c.Settings, err = json.Marshal(settings); err != nil {
		return Contents{}, err
	}
	routes, err := st.ManualRoutes()
	if err != nil {
		return Contents{}, fmt.Errorf("reading the manual routes: %w", err)
	}
	if c.ManualRoutes, err = encodeAll(routes); err != nil {
		return Contents{}, err
	}
	approvals, err := st.Approvals()
	if err != nil {
		return Contents{}, fmt.Errorf("reading the approvals: %w", err)
	}
	if c.Approvals, err = encodeAll(inOrder(approvals)); err != nil {
		return Contents{}, err
	}
	segments, err := st.Segments()
	if err != nil {
		return Contents{}, fmt.Errorf("reading the acknowledged segments: %w", err)
	}
	if c.Segments, err = encodeAll(inOrder(segments)); err != nil {
		return Contents{}, err
	}
	nodeSegments, err := st.NodeSegments()
	if err != nil {
		return Contents{}, fmt.Errorf("reading the segments acknowledged on nodes: %w", err)
	}
	if c.NodeSegments, err = encodeAll(inOrder(nodeSegments)); err != nil {
		return Contents{}, err
	}
	nodes, err := st.Nodes()
	if err != nil {
		return Contents{}, fmt.Errorf("reading the registry of nodes: %w", err)
	}
	if c.Nodes, err = encodeAll(nodes); err != nil {
		return Contents{}, err
	}
	switch current, _, _, err := st.CA(); {
	case errors.Is(err, store.ErrNoCA):
	case err != nil:
		return Contents{}, fmt.Errorf("reading the CA: %w", err)
	default:
		c.CA = &current
	}
	leader, _, found, err := st.Leader()
	if err != nil {
		return Contents{}, fmt.Errorf("reading leader.json: %w", err)
	}
	if found {
		if c.Leader, err = json.Marshal(leader); err != nil {
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

// inOrder returns the values of a map in the order of its keys, so that two
// bundles of one store hold their objects in one order.
func inOrder[T any](m map[string]T) []T {
	out := make([]T, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, m[k])
	}
	return out
}

// restoration is what a bundle holds, read as the store's types.
type restoration struct {
	created      time.Time
	settings     store.Settings
	routes       []model.Route
	approvals    []store.Approval
	segments     []store.Segment
	nodeSegments []store.NodeSegment
	nodes        []store.NodeEntry
	ca           *store.CAFiles
	credentials  []store.Credential
	leader       *store.LeaderRecord
}

// decodeStrict reads one object of the bundle, refusing what the type has no
// field for: it would be left out of the restore.
func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("text after the end of the object")
	}
	return nil
}

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

// read turns the contents into the store's objects, which writes nothing, so
// that a bundle that cannot be read is found out before the store is touched.
func read(c Contents) (r restoration, err error) {
	r.created = c.InstalledAt
	if r.created.IsZero() {
		r.created = c.MadeAt
	}
	r.settings = store.DefaultSettings()
	if len(c.Settings) > 0 {
		if err := decodeStrict(c.Settings, &r.settings); err != nil {
			return r, fmt.Errorf("the settings of the bundle: %w", err)
		}
	}
	if r.routes, err = decodeEach[model.Route]("manual route", c.ManualRoutes); err != nil {
		return r, err
	}
	if r.approvals, err = decodeEach[store.Approval]("approval", c.Approvals); err != nil {
		return r, err
	}
	if r.segments, err = decodeEach[store.Segment]("segment", c.Segments); err != nil {
		return r, err
	}
	if r.nodeSegments, err = decodeEach[store.NodeSegment]("node segment", c.NodeSegments); err != nil {
		return r, err
	}
	if r.nodes, err = decodeEach[store.NodeEntry]("node", c.Nodes); err != nil {
		return r, err
	}
	if c.CA != nil {
		if c.CA.CertPEM == "" || c.CA.KeyPEM.Reveal() == "" {
			return r, errors.New("the CA of the bundle has no certificate or no key")
		}
		r.ca = c.CA
	}
	docs, err := decodeEach[credentialDocument]("credential", c.Credentials)
	if err != nil {
		// What failed could be a token: say which credential, not what is in it.
		return r, errors.New("a credential of the bundle cannot be read")
	}
	for i, d := range docs {
		switch d.Token {
		case "", redactedToken:
			return r, fmt.Errorf("credential %d of the bundle, %q, holds no token: it was exported without it", i+1, d.ID)
		}
		r.credentials = append(r.credentials, store.Credential{ID: d.ID, Label: d.Label, Kind: d.Kind, Token: store.NewSecret(d.Token), AddedAt: d.AddedAt})
	}
	if len(c.Leader) > 0 {
		var leader store.LeaderRecord
		if err := decodeStrict(c.Leader, &leader); err != nil {
			return r, fmt.Errorf("leader.json of the bundle: %w", err)
		}
		if err := leader.Validate(); err != nil {
			return r, fmt.Errorf("leader.json of the bundle: %w", err)
		}
		if leader.InstallID != c.InstallID {
			return r, fmt.Errorf("leader.json of the bundle names install %s, and the bundle is of install %s", leader.InstallID, c.InstallID)
		}
		r.leader = &leader
	}
	return r, nil
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

// Restore writes the install of a bundle into a store that holds none, whose
// roots are made. The settings are stored in observe-only mode, whatever they
// were: the restored install publishes nothing until an admin says so. The
// cluster the install was made in is not carried over, and the install is
// written last, so that a restore that stops half way leaves a store that
// holds none, and is run again.
func Restore(st *store.Store, c Contents) error {
	r, err := read(c)
	if err != nil {
		return err
	}
	if err := NoInstall(st); err != nil {
		return err
	}
	if r.ca != nil {
		if err := st.SaveCA(*r.ca, nil, time.Time{}); err != nil {
			return fmt.Errorf("restoring the CA: %w", err)
		}
	}
	for _, cred := range r.credentials {
		if err := st.SaveCredential(cred); err != nil {
			return fmt.Errorf("restoring credential %s: %w", cred.ID, err)
		}
	}
	r.settings.ObserveOnly = true
	if err := st.SaveSettings(r.settings); err != nil {
		return fmt.Errorf("restoring the settings: %w", err)
	}
	for _, n := range r.nodes {
		if err := st.SaveNode(n); err != nil {
			return fmt.Errorf("restoring the registry of nodes: %w", err)
		}
	}
	for _, s := range r.segments {
		if err := st.SaveSegment(s); err != nil {
			return fmt.Errorf("restoring the acknowledged segments: %w", err)
		}
	}
	for _, s := range r.nodeSegments {
		if err := st.SaveNodeSegment(s); err != nil {
			return fmt.Errorf("restoring the segments acknowledged on nodes: %w", err)
		}
	}
	for _, a := range r.approvals {
		if err := st.SaveApproval(a); err != nil {
			return fmt.Errorf("restoring the approvals: %w", err)
		}
	}
	for _, route := range r.routes {
		if err := st.SaveManualRoute(route); err != nil {
			return fmt.Errorf("restoring the manual routes: %w", err)
		}
	}
	if r.leader != nil {
		if err := st.SaveLeader(*r.leader); err != nil {
			return fmt.Errorf("restoring leader.json: %w", err)
		}
	}
	if err := st.SaveInstall(store.Install{ID: c.InstallID, CreatedAt: r.created, Profile: store.ProfileHost}); err != nil {
		return fmt.Errorf("restoring the install: %w", err)
	}
	return nil
}
