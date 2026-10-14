package setup

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// restoredElsewhere is the cluster step for a store that pco import restored
// from a bundle made in another cluster. The first node set up takes it
// into this one; every other node waits until that node is promoted, after
// which the store is this cluster's and they join it as usual. The store
// keeps the identity of the cluster it was made in, so the daemon writes
// nothing until pco promote draws a new epoch.
func (r *run) restoredElsewhere(ctx context.Context, fingerprint string) error {
	inst := r.install
	by, done := inst.Restored.TakenOverBy, inst.Restored.CASHA256 != ""
	switch {
	case by != "" && by != r.o.Node:
		return errTakenBy(inst, by)
	case by == r.o.Node && done:
		r.ask.Info("cluster: the store of cluster %s was taken into this one by this node; pco promote here makes this cluster its writer",
			clusterName(*inst.Cluster))
		return nil
	case r.o.Repair:
		return fmt.Errorf("install %s was restored from a bundle of cluster %s and is not taken into this cluster (%s) yet: "+
			"run pco setup, not --repair; nothing was changed", inst.ID, clusterName(*inst.Cluster), fingerprint)
	}
	if err := r.oldClusterStopped(ctx); err != nil {
		return err
	}
	if by == "" {
		if err := r.claimRestored(); err != nil {
			return err
		}
	}
	if err := r.takeOver(); err != nil {
		return err
	}
	r.ask.Info("cluster: took the store of cluster %s into this one (%s) under a new CA; the daemon writes nothing until "+
		"pco promote here makes this cluster its writer, and the other nodes are set up after it", clusterName(*inst.Cluster), fingerprint)
	return nil
}

// errRestoredWithoutCluster refuses a restored store that names no cluster:
// whether it was made in this one cannot be told, so it is neither joined
// nor taken over.
func errRestoredWithoutCluster(inst store.Install) error {
	return fmt.Errorf("install %s was restored from a bundle that records no cluster, so setup cannot tell whether it was made "+
		"in this one: start pco once on a node of the cluster it was made in, which records it, and export it again, or adopt "+
		"the install from Cloudflare with pco setup --recover; nothing was changed", inst.ID)
}

func errTakenBy(inst store.Install, by string) error {
	return fmt.Errorf("install %s was restored from a bundle of cluster %s, and node %s took it into this cluster: "+
		"run pco promote on %s first, then pco setup here; nothing was changed", inst.ID, clusterName(*inst.Cluster), by, by)
}

func clusterName(id store.ClusterIdentity) string {
	return cmp.Or(id.Name, "of a node standing alone")
}

// claimRestored records this node as the one that takes the store, at the
// revision of the install it reads: of two nodes set up at once, one takes
// it and the other is refused before it changes anything.
func (r *run) claimRestored() error {
	const changed = "the install changed while setup ran; run pco setup again"
	inst, rev, found, err := r.st.InstallRev()
	switch {
	case err != nil:
		return fmt.Errorf("reading the install: %w", err)
	case !found || inst.ID != r.install.ID || inst.Restored == nil:
		return errors.New(changed)
	case inst.Restored.TakenOverBy != "":
		return errTakenBy(inst, inst.Restored.TakenOverBy)
	}
	restored := *inst.Restored
	restored.TakenOverBy = r.o.Node
	inst.Restored = &restored
	_, err = r.st.SaveInstallIf(inst, rev)
	switch {
	case errors.Is(err, store.ErrRevision):
		if now, _, err := r.st.Install(); err == nil && now.Restored != nil && now.Restored.TakenOverBy != "" {
			return errTakenBy(now, now.Restored.TakenOverBy)
		}
		return errors.New(changed)
	case err != nil:
		return fmt.Errorf("recording that this node takes the store: %w", err)
	}
	r.install = inst
	return nil
}

// takeOver makes the store the one of this cluster's first node: a new CA,
// named in leader.json, so that nothing the nodes of the lost cluster hold is
// trusted; every node of that cluster removed with its enrolment, to enrol
// again, its name keeping its index; and this node's own entry, under the
// same name, made a first node's. The fingerprint of the CA is saved with the
// install last, so a run cut short does it all again.
func (r *run) takeOver() error {
	made, err := pki.NewCA(r.install.ID, r.now(), r.rand)
	if err != nil {
		return fmt.Errorf("making the CA: %w", err)
	}
	certPEM, keyPEM, err := made.PEM()
	if err != nil {
		return fmt.Errorf("encoding the CA: %w", err)
	}
	if err := r.st.SaveCA(store.CAFiles{CertPEM: string(certPEM), KeyPEM: store.NewSecret(string(keyPEM))}, nil, time.Time{}); err != nil {
		return fmt.Errorf("storing the new CA: %w", err)
	}
	if err := r.nameCA(made); err != nil {
		return err
	}
	// A rotation the lost cluster left unfinished would go on here with
	// steps meant for its nodes and its CA; the new CA and the rotation of
	// the tunnel secrets the promotion asks for take its place.
	switch rot, found, err := r.st.CARotation(); {
	case err != nil:
		return fmt.Errorf("reading the rotation of the lost cluster: %w", err)
	case found && rot.FinishedAt.IsZero():
		if err := r.st.DeleteCARotation(); err != nil {
			return fmt.Errorf("dropping the rotation of the lost cluster: %w", err)
		}
	}
	nodes, err := r.st.Nodes()
	if err != nil {
		return fmt.Errorf("reading the node registry: %w", err)
	}
	for _, n := range nodes {
		if err := r.st.DeleteEnrolment(n.Name); err != nil {
			return fmt.Errorf("removing the enrolment of %s: %w", n.Name, err)
		}
		entry := store.NodeEntry{Name: n.Name, Index: n.Index, Since: r.now(), Kind: n.Kind}
		if n.Name != r.o.Node {
			entry = n
			entry.State, entry.StateSince, entry.Rotation = string(agentproto.Removed), r.now(), nil
		}
		if err := r.st.SaveNode(entry); err != nil {
			return fmt.Errorf("retiring node %s of the lost cluster: %w", n.Name, err)
		}
	}
	if err := r.readOthers(); err != nil {
		return err
	}
	inst := r.install
	restored := *inst.Restored
	restored.CASHA256 = made.SHA256()
	inst.Restored = &restored
	if err := r.st.SaveInstall(inst); err != nil {
		return fmt.Errorf("recording that this node took the store: %w", err)
	}
	r.install = inst
	return nil
}

// errNoClient marks a token no Cloudflare client could be made with.
var errNoClient = errors.New("the token cannot be used")

// oldClusterStopped refuses to take a restored store over while a connector
// of its install is connected at Cloudflare: nothing of this cluster runs one
// yet, so it is the cluster the bundle was made in, or a copy of it, that
// still runs the install. It looks with every credential of the store and
// the token given to setup. A token Cloudflare rejects sees nothing and is
// passed over; when no token can look, or a listing fails, it refuses.
func (r *run) oldClusterStopped(ctx context.Context) error {
	id := r.install.ID
	creds, err := r.st.Credentials()
	if err != nil {
		return fmt.Errorf("reading the credentials: %w", err)
	}
	type source struct{ name, token string }
	var sources []source
	for _, c := range creds {
		sources = append(sources, source{"credential " + c.ID, c.Token.Reveal()})
	}
	if r.token != "" && !slices.ContainsFunc(sources, func(s source) bool { return s.token == r.token }) {
		sources = append(sources, source{"the token given to setup", r.token})
	}
	if len(sources) == 0 {
		return fmt.Errorf("the bundle of install %s carried no Cloudflare credential: pass a token that sees its tunnels with "+
			"--cf-token-file or --cf-token-stdin, so that setup can tell whether the cluster the bundle was made in still runs it; "+
			"nothing was changed", id)
	}
	var live, rejected []string
	looked := false
	for _, s := range sources {
		found, err := r.connectedWith(ctx, s.token, id)
		switch {
		case err == nil:
			looked = true
			live = append(live, found...)
		case isRejected(err):
			rejected = append(rejected, fmt.Sprintf("%s: %v", s.name, err))
		default:
			return fmt.Errorf("cannot tell whether the cluster the bundle was made in still runs install %s: %s: %w; "+
				"run pco setup again once Cloudflare can be read; nothing was changed", id, s.name, err)
		}
	}
	if !looked {
		return fmt.Errorf("cannot tell whether the cluster the bundle was made in still runs install %s: Cloudflare took none of "+
			"the tokens (%s); pass one that sees its tunnels with --cf-token-file or --cf-token-stdin; nothing was changed",
			id, strings.Join(rejected, "; "))
	}
	for _, why := range rejected {
		r.ask.Warn("cloudflare: %s, passed over", why)
	}
	if len(live) > 0 {
		slices.Sort(live)
		return fmt.Errorf("the cluster the bundle was made in still runs install %s, or a copy of it does: at Cloudflare %s. "+
			"Two writers of one install undo each other's changes: stop pco and its connectors there "+
			"(systemctl stop pco 'pco-cloudflared@*'), and run pco setup again once Cloudflare shows no connection; "+
			"nothing was changed", id, strings.Join(slices.Compact(live), "; "))
	}
	r.ask.Info("cloudflare: no connector of install %s is connected", id)
	return nil
}

// connectedWith is connectedTunnels with a client of token.
func (r *run) connectedWith(ctx context.Context, token, installID string) ([]string, error) {
	api, err := r.newClient(token)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errNoClient, err)
	}
	return connectedTunnels(ctx, api, installID)
}

// isRejected says whether err is that of a token Cloudflare does not take at
// all, or one no client can be made with: either sees nothing.
func isRejected(err error) bool {
	var apiErr *cfapi.Error
	return errors.Is(err, errNoClient) || errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized
}

// connectedTunnels says, for each tunnel of the install that api sees, the
// shared one and those pinned to a node, which connectors are connected and
// from where.
func connectedTunnels(ctx context.Context, api cfapi.API, installID string) ([]string, error) {
	accounts, err := accountsOf(ctx, api)
	if err != nil {
		return nil, err
	}
	shared := planner.TunnelName(installID)
	var out []string
	for _, account := range accounts {
		tunnels, err := api.Tunnels(ctx, account, shared)
		if err != nil {
			return nil, fmt.Errorf("listing the tunnels of account %s: %w", account, err)
		}
		for _, t := range tunnels {
			if t.Name != shared && !strings.HasPrefix(t.Name, shared+"-") {
				continue
			}
			conns, err := api.Connectors(ctx, account, t.ID)
			if err != nil {
				return nil, fmt.Errorf("listing the connectors of tunnel %s (%s): %w", t.Name, t.ID, err)
			}
			for _, c := range conns {
				if c.Connections > 0 {
					out = append(out, fmt.Sprintf("tunnel %s has connector %s connected from %s", t.Name, c.ID, cmp.Or(c.OriginIP, "an unknown address")))
				}
			}
		}
	}
	return out, nil
}

// accountsOf are the accounts a credential lists and those of its zones.
func accountsOf(ctx context.Context, api cfapi.API) ([]string, error) {
	listed, err := api.Accounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the accounts: %w", err)
	}
	zones, err := api.Zones(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the zones: %w", err)
	}
	accounts := map[string]bool{}
	for _, a := range listed {
		accounts[a.ID] = true
	}
	for _, z := range zones {
		accounts[z.AccountID] = true
	}
	return slices.Sorted(maps.Keys(accounts)), nil
}
