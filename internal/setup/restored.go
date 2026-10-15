package setup

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// restoredElsewhere is the cluster step for a store that pco import restored
// from a bundle made in another cluster. The first node set up takes it
// into this one; every other node waits until that node is promoted, after
// which the store is this cluster's and they join it as usual. The store
// keeps the identity of the cluster it was made in, so the daemon writes
// nothing until pco promote draws a new epoch.
func (r *run) restoredElsewhere(ctx context.Context, m members.Members, fingerprint string) error {
	inst := r.install
	by, from := inst.Restored.TakenOverBy, ""
	if by != "" && by != r.o.Node {
		// A node that claimed the store and is no longer an online member
		// of the cluster does not hold the restore up for good: its claim
		// is taken over after the same checks. One that is online may be
		// taking it over now.
		if memberOnline(m, by) {
			return errTakenBy(inst, by)
		}
		from = by
	}
	// Whether the take-over is done is read from the CA and the registry,
	// not from the install alone: a run cut short at any step, or a record
	// that says more than was done, is done again, after every check.
	done, why, err := r.st.TakenOver(inst)
	switch {
	case err != nil:
		return fmt.Errorf("reading how far this cluster took the store: %w", err)
	case done && from == "":
		r.ask.Info("cluster: the store of cluster %s was taken into this one by this node; pco promote here makes this cluster its writer",
			clusterName(*inst.Cluster))
		return nil
	case r.o.Repair:
		return fmt.Errorf("install %s was restored from a bundle of cluster %s and is not taken into this cluster (%s): %s; "+
			"run pco setup, not --repair; nothing was changed", inst.ID, clusterName(*inst.Cluster), fingerprint, why)
	}
	if err := r.oldClusterStopped(ctx); err != nil {
		return err
	}
	if err := r.claimRestored(from); err != nil {
		return err
	}
	if from != "" {
		r.ask.Warn("cluster: took the claim of node %s, which is not an online member of this cluster, on the restored store; "+
			"pco promote records it as a security event", from)
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
		"in this one: remove it with pco uninstall --keep-cloudflare, then adopt the install from Cloudflare with "+
		"pco setup --recover, which makes a store of its own under a new CA and takes nothing of the lost cluster over; "+
		"nothing was changed", inst.ID)
}

func errTakenBy(inst store.Install, by string) error {
	return fmt.Errorf("install %s was restored from a bundle of cluster %s, and node %s took it into this cluster: "+
		"run pco promote on %s first, then pco setup here; if node %s is gone for good, take it out of the cluster "+
		"(pvecm delnode %s) and pco setup here takes its claim over; nothing was changed",
		inst.ID, clusterName(*inst.Cluster), by, by, by, by)
}

// memberOnline says whether node is an online member of the cluster, as
// pmxcfs lists it.
func memberOnline(m members.Members, node string) bool {
	for _, n := range m.Nodes {
		if n.Name == node {
			return n.Online
		}
	}
	return false
}

func clusterName(id store.ClusterIdentity) string {
	return cmp.Or(id.Name, "of a node standing alone")
}

// claimRestored records this node as the one that takes the store, at the
// revision of the install it reads, on every attempt, a redo of its own
// included: of two nodes set up at once, one takes it and the other is
// refused before it changes anything. A claim of another node is taken over
// only from the node from, which is no online member; it is recorded. The
// record of a take-over that was not done is cleared with it, and the
// revision is kept, so that the take-over is saved only while nobody
// claimed the store since.
func (r *run) claimRestored(from string) error {
	const changed = "the install changed while setup ran; run pco setup again"
	inst, rev, found, err := r.st.InstallRev()
	switch {
	case err != nil:
		return fmt.Errorf("reading the install: %w", err)
	case !found || inst.ID != r.install.ID || inst.Restored == nil:
		return errors.New(changed)
	case inst.Restored.TakenOverBy != "" && inst.Restored.TakenOverBy != r.o.Node && inst.Restored.TakenOverBy != from:
		return errTakenBy(inst, inst.Restored.TakenOverBy)
	}
	restored := *inst.Restored
	if by := restored.TakenOverBy; by != "" && by != r.o.Node {
		restored.ClaimedFrom = by
	}
	restored.TakenOverBy, restored.CASHA256 = r.o.Node, ""
	inst.Restored = &restored
	rev, err = r.st.SaveInstallIf(inst, rev)
	switch {
	case errors.Is(err, store.ErrRevision):
		if now, _, err := r.st.Install(); err == nil && now.Restored != nil && now.Restored.TakenOverBy != "" {
			return errTakenBy(now, now.Restored.TakenOverBy)
		}
		return errors.New(changed)
	case err != nil:
		return fmt.Errorf("recording that this node takes the store: %w", err)
	}
	r.install, r.claimRev = inst, rev
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
	// The CA is the one thing of a take-over another node's cannot be told
	// from: it is replaced only while the claim of this node is the one
	// stored.
	if err := r.stillClaimed(); err != nil {
		return err
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
	if _, err := r.st.SaveInstallIf(inst, r.claimRev); errors.Is(err, store.ErrRevision) {
		return errors.New("the restored store was claimed again while this node took it over: run pco setup again")
	} else if err != nil {
		return fmt.Errorf("recording that this node took the store: %w", err)
	}
	r.install = inst
	return nil
}

// stillClaimed refuses unless the stored install is the one this node
// claimed, at the revision it claimed it at.
func (r *run) stillClaimed() error {
	const again = "the restored store was claimed again while this node took it over: run pco setup again"
	inst, rev, found, err := r.st.InstallRev()
	switch {
	case err != nil:
		return fmt.Errorf("reading the install: %w", err)
	case !found || inst.Restored == nil || inst.Restored.TakenOverBy != r.o.Node || rev != r.claimRev:
		return errors.New(again)
	}
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

// connectedWith is cfapi.Connected with a client of token.
func (r *run) connectedWith(ctx context.Context, token, installID string) ([]string, error) {
	api, err := r.newClient(token)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errNoClient, err)
	}
	return cfapi.Connected(ctx, api, installID)
}

// isRejected says whether err is that of a token Cloudflare does not take at
// all, or one no client can be made with: either sees nothing.
func isRejected(err error) bool {
	var apiErr *cfapi.Error
	return errors.Is(err, errNoClient) || errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized
}
