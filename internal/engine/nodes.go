package engine

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// minFingerprint is how much of a certificate's SHA-256 an admin types to
// admit a node.
const minFingerprint = 8

// notRotated is what the disk of a node holds that pco cannot rotate: the
// admin changes these by hand.
var notRotated = []string{
	"the corosync authkey (/etc/corosync/authkey): a node that keeps it can join the cluster again; pvecm updatecerts after a new key",
	"the SSH keys of root (/root/.ssh, /etc/ssh/ssh_host_*): the other nodes trust them until they are replaced",
	"the key of the cluster CA (/etc/pve/priv/pve-root-ca.key): every node's pveproxy certificate is signed with it",
}

// nodeMemory is what the admin's decisions about the nodes keep between
// cycles, guarded by the cycle lock: the nodes the admin admitted by hand,
// whose routes may all move, until they are active.
type nodeMemory struct {
	admitted map[string]bool
	// driftSaid is the last reason a rotation could not go on that was
	// told as an event.
	driftSaid string
}

// NodePatch is what PUT /v1/nodes/:name changes of an entry: each field set
// is written, the rest stays.
type NodePatch struct {
	ControllerEligible *bool `json:"controllerEligible,omitempty"`
}

// entryOf reads the entry of a node with its revision; a node without one
// is not found.
func (e *Engine) entryOf(name string) (store.NodeRev, error) {
	nodes, err := e.d.Store.NodesRev()
	if err != nil {
		return store.NodeRev{}, fmt.Errorf("reading the node registry: %w", err)
	}
	for _, n := range nodes {
		if n.Node.Name == name {
			return n, nil
		}
	}
	return store.NodeRev{}, fmt.Errorf("%w: node %s is not in the registry; pco node list names the nodes", ErrNotFound, name)
}

// saveEntry writes an entry read at rev, and tells what changed of it.
func (e *Engine) saveEntry(ctx context.Context, before store.NodeEntry, rev int64, after store.NodeEntry, msg string) error {
	if _, err := e.d.Store.SaveNodeIf(after, rev); err != nil {
		if errors.Is(err, store.ErrRevision) {
			return fmt.Errorf("%w: the entry of node %s changed meanwhile; look again and repeat", ErrRefused, after.Name)
		}
		return fmt.Errorf("saving the entry of node %s: %w", after.Name, err)
	}
	now := e.d.Now()
	if before.State != after.State {
		e.events.add(Event{At: now, Level: levelInfo, Kind: kindNode, Subject: after.Name, Node: after.Name,
			Message: fmt.Sprintf("node %s is %s, was %s", after.Name, after.State, stateName(before.State)), Actor: ActorOf(ctx)})
	}
	e.events.add(Event{At: now, Level: levelInfo, Kind: kindAdmin, Subject: after.Name, Node: after.Name, Message: msg, Actor: ActorOf(ctx)})
	return nil
}

// entryView is a node's entry as an answer shows it, without what only a
// cycle knows of its agent.
func (e *Engine) entryView(n store.NodeEntry) NodeView {
	v := NodeView{
		Name: n.Name, State: n.State, StateSince: n.StateSince, Kind: entryKind(n), VMID: n.VMID,
		Index: n.Index, AgentAddress: n.AgentAddress, Controller: n.Name == e.d.Node,
	}
	if n.Index != nil {
		i := *n.Index
		v.Index = &i
	}
	if n.Drained != nil {
		v.Drained = n.Drained.Reason
	}
	v.Contested = joinUseViews(n.ContestedUses)
	return v
}

func joinUseViews(uses []store.JoinUse) []JoinUseView {
	out := make([]JoinUseView, 0, len(uses))
	for _, u := range uses {
		out = append(out, JoinUseView{At: u.At, Source: u.Source, VMID: u.VMID, MACs: slices.Clone(u.MACs)})
	}
	return out
}

// AdmitNode admits a node that joins, by the admin: an appliance agent's
// certificate is trusted once the fingerprint typed is that of the
// certificate enrolled, at least minFingerprint characters of it, and a node
// that stays joining because too many routes would move is let through, the
// routes moving first. A contested node admits nothing: only forgetting it
// ends that. renew lets an appliance agent whose certificate expired renew
// it once.
func (e *Engine) AdmitNode(ctx context.Context, name, fingerprint string, renew bool) (NodeView, error) {
	fp, err := checkFingerprint(fingerprint)
	if err != nil {
		return NodeView{}, err
	}
	if err := e.acquireWriter(ctx); err != nil {
		return NodeView{}, err
	}
	defer e.release()
	rev, err := e.entryOf(name)
	if err != nil {
		return NodeView{}, err
	}
	n := rev.Node
	switch n.State {
	case store.NodeContested:
		return e.entryView(n), fmt.Errorf("%w: node %s is contested: its join token was used %s; "+
			"pco node forget %s removes the entry, and a new token enrols the node again", ErrRefused, name, usesText(n.ContestedUses), name)
	case store.NodeRemoved:
		return e.entryView(n), fmt.Errorf("%w: node %s was removed from the install; pco setup on the node, or a new join token, enrols it again", ErrRefused, name)
	case store.NodeActive, store.NodeDraining:
		return e.entryView(n), fmt.Errorf("%w: node %s is %s already", ErrRefused, name, n.State)
	}
	if fp != "" {
		enrolled, err := e.enrolledFingerprint(n)
		if err != nil {
			return e.entryView(n), err
		}
		if !fingerprintMatches(enrolled, fp) {
			return e.entryView(n), fmt.Errorf("%w: the fingerprint %s is not that of the certificate enrolled for node %s (%s); "+
				"read it with pco agent status on the node", ErrRefused, fp, name, enrolled)
		}
	} else if entryKind(n) == string(pki.KindAppliance) {
		return e.entryView(n), fmt.Errorf("%w: an appliance agent is admitted with the fingerprint of its certificate: "+
			"pco node admit %s --fingerprint <sha256>, as pco agent status shows it in the container", ErrInvalid, name)
	}
	if renew && entryKind(n) != string(pki.KindAppliance) {
		return e.entryView(n), fmt.Errorf("%w: node %s is a host node, which issues its own certificates; --renew is for an appliance agent", ErrRefused, name)
	}
	if renew {
		return e.entryView(n), fmt.Errorf("%w: --renew is not available until the appliance install can renew the certificate of an agent; "+
			"a new join token enrols node %s again", ErrRefused, name)
	}
	if n.State != store.NodeJoining {
		return e.entryView(n), fmt.Errorf("%w: node %s is %s; it is admitted once its agent reports again", ErrRefused, name, n.State)
	}
	if e.nodes.admitted == nil {
		e.nodes.admitted = map[string]bool{}
	}
	e.nodes.admitted[name] = true
	msg := fmt.Sprintf("node %s is admitted by the admin; every shared route it fails moves to its guest's node first", name)
	if fp != "" {
		msg = fmt.Sprintf("node %s is admitted by the admin, its certificate trusted by fingerprint %s; every shared route it fails moves to its guest's node first", name, fp)
	}
	e.adminEvent(ctx, name, msg)
	e.Trigger()
	return e.entryView(n), nil
}

// checkFingerprint reads what the admin typed of a certificate's SHA-256:
// hex, with or without colons, at least minFingerprint digits of it; empty
// stays empty.
func checkFingerprint(s string) (string, error) {
	s = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), ":", ""))
	if s == "" {
		return "", nil
	}
	if len(s) < minFingerprint {
		return "", fmt.Errorf("%w: a fingerprint has at least %d hex digits, %q has %d", ErrInvalid, minFingerprint, s, len(s))
	}
	if strings.ContainsFunc(s, func(r rune) bool { return (r < '0' || r > '9') && (r < 'a' || r > 'f') }) {
		return "", fmt.Errorf("%w: a fingerprint is the SHA-256 of the certificate in hex, not %q", ErrInvalid, s)
	}
	return s, nil
}

// fingerprintMatches says whether what the admin typed is the beginning of
// the enrolled fingerprint, a whole SHA-256 in hex, compared in time that
// does not depend on where the two differ.
func fingerprintMatches(enrolled, typed string) bool {
	if len(enrolled) != 2*sha256.Size || len(typed) < minFingerprint || len(typed) > len(enrolled) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(enrolled[:len(typed)]), []byte(typed)) == 1
}

// enrolledFingerprint is the SHA-256 of the certificate enrolled for a
// node: its enrolment's on a host, the registry's for an appliance agent. A
// value that is no SHA-256 in hex is refused as none.
func (e *Engine) enrolledFingerprint(n store.NodeEntry) (string, error) {
	sum := n.CertSHA256
	if en, found, err := e.d.Store.Enrolment(n.Name); err != nil {
		return "", fmt.Errorf("reading the enrolment of node %s: %w", n.Name, err)
	} else if found {
		sum = en.SHA256
	}
	if sum == "" {
		return "", fmt.Errorf("%w: no certificate is enrolled for node %s yet", ErrRefused, n.Name)
	}
	if len(sum) != 2*sha256.Size || strings.ContainsFunc(sum, func(r rune) bool { return (r < '0' || r > '9') && (r < 'a' || r > 'f') }) {
		return "", fmt.Errorf("%w: the certificate enrolled for node %s has no fingerprint pco can check (%q)", ErrRefused, n.Name, sum)
	}
	return sum, nil
}

func usesText(uses []store.JoinUse) string {
	if len(uses) == 0 {
		return "in a way that does not add up"
	}
	parts := make([]string, 0, len(uses))
	for _, u := range uses {
		part := u.At.UTC().Format(time.RFC3339)
		if u.Source.IsValid() {
			part += " from " + u.Source.String()
		}
		if u.VMID != 0 {
			part += fmt.Sprintf(" as container %d", u.VMID)
		}
		parts = append(parts, part)
	}
	return fmt.Sprintf("%d times (%s)", len(uses), strings.Join(parts, "; "))
}

// adminAdmitted says whether the admin admitted a node by hand, so that every
// route it fails may move; the answer is used up once the node is active.
func (e *Engine) adminAdmitted(name string) bool { return e.nodes.admitted[name] }

// DrainNode takes a node out of the shared tunnel's replicas, by the admin:
// its pinned tunnel keeps serving the routes of its own guests. The last
// replica is not drained without force, which leaves the shared tunnel
// without a connector. A node that is not active yet keeps the intent, and
// is drained as it is admitted.
func (e *Engine) DrainNode(ctx context.Context, name string, force bool) (NodeView, error) {
	if err := e.acquireWriter(ctx); err != nil {
		return NodeView{}, err
	}
	defer e.release()
	rev, err := e.entryOf(name)
	if err != nil {
		return NodeView{}, err
	}
	n := rev.Node
	switch n.State {
	case store.NodeRemoved, store.NodeContested:
		return e.entryView(n), fmt.Errorf("%w: node %s is %s; it serves nothing to drain", ErrRefused, name, n.State)
	}
	if n.Drained != nil && n.Drained.Reason == store.DrainAdmin {
		return e.entryView(n), nil
	}
	if !force && n.State == store.NodeActive {
		others, err := e.otherReplicas(name)
		if err != nil {
			return e.entryView(n), err
		}
		if len(others) == 0 {
			return e.entryView(n), fmt.Errorf("%w: node %s is the only active replica of the shared tunnel; draining it stops the tunnel's only connector, "+
				"which --force does all the same", ErrRefused, name)
		}
	}
	before := n
	n.Drained = &store.NodeDrain{Reason: store.DrainAdmin, Since: e.d.Now()}
	msg := fmt.Sprintf("node %s is drained by the admin: it leaves the shared tunnel's replicas; its pinned tunnel keeps the routes of its own guests", name)
	if n.State == store.NodeActive {
		n.State, n.StateSince = store.NodeDraining, e.d.Now()
	} else {
		msg = fmt.Sprintf("node %s is %s: it is drained as it is admitted", name, n.State)
	}
	if err := e.saveEntry(ctx, before, rev.Rev, n, msg); err != nil {
		return e.entryView(before), err
	}
	e.Trigger()
	return e.entryView(n), nil
}

// otherReplicas are the active, undrained nodes other than name that may run
// the shared tunnel.
func (e *Engine) otherReplicas(name string) ([]string, error) {
	nodes, err := e.d.Store.Nodes()
	if err != nil {
		return nil, fmt.Errorf("reading the node registry: %w", err)
	}
	s, err := e.d.Store.Settings()
	if err != nil {
		return nil, fmt.Errorf("reading the settings: %w", err)
	}
	var out []string
	for _, n := range nodes {
		if n.Name == name || n.State != store.NodeActive || n.Drained != nil {
			continue
		}
		if list := s.Cluster.Replicas; len(list) > 0 && !slices.Contains(list, n.Name) {
			continue
		}
		out = append(out, n.Name)
	}
	return out, nil
}

// UndrainNode ends an admin's drain. A drain Proxmox's maintenance mode
// made ends with that mode, not here.
func (e *Engine) UndrainNode(ctx context.Context, name string) (NodeView, error) {
	if err := e.acquireWriter(ctx); err != nil {
		return NodeView{}, err
	}
	defer e.release()
	rev, err := e.entryOf(name)
	if err != nil {
		return NodeView{}, err
	}
	n := rev.Node
	switch {
	case n.Drained == nil:
		return e.entryView(n), nil
	case n.Drained.Reason == store.DrainMaintenance:
		return e.entryView(n), fmt.Errorf("%w: Proxmox has node %s in maintenance mode, which drains it; end it there "+
			"(ha-manager crm-command node-maintenance disable %s), or set cluster.followMaintenance to false", ErrRefused, name, name)
	}
	before := n
	n.Drained = nil
	if n.State == store.NodeDraining {
		n.State, n.StateSince = store.NodeActive, e.d.Now()
	}
	if err := e.saveEntry(ctx, before, rev.Rev, n, fmt.Sprintf("node %s is no longer drained: the admin ended it", name)); err != nil {
		return e.entryView(before), err
	}
	e.Trigger()
	return e.entryView(n), nil
}

// SetNode changes what the admin may of an entry.
func (e *Engine) SetNode(ctx context.Context, name string, p NodePatch) (NodeView, error) {
	if p.ControllerEligible == nil {
		return NodeView{}, fmt.Errorf("%w: nothing to set: name a field, as controllerEligible", ErrInvalid)
	}
	if err := e.acquireWriter(ctx); err != nil {
		return NodeView{}, err
	}
	defer e.release()
	rev, err := e.entryOf(name)
	if err != nil {
		return NodeView{}, err
	}
	n := rev.Node
	if n.State == store.NodeRemoved {
		return e.entryView(n), fmt.Errorf("%w: node %s was removed from the install", ErrRefused, name)
	}
	before := n
	eligible := *p.ControllerEligible
	n.ControllerEligible = &eligible
	word := "may"
	if !eligible {
		word = "may not"
	}
	if err := e.saveEntry(ctx, before, rev.Rev, n, fmt.Sprintf("node %s %s hold the lease from now on", name, word)); err != nil {
		return e.entryView(before), err
	}
	return e.entryView(n), nil
}

// ForgetNode removes a node that is gone from the install, in this order:
// its entry is removed, so that its agent is refused from its next request;
// its connections are closed; then the secret of the shared tunnel is
// rotated, so that the run token the node keeps stops working, and the
// remaining replicas take the new one up. Its pinned tunnels go in the next
// cycle. With compromised, for a disk that is out of the admin's hands, every
// secret of the install is rotated after that, with the progress kept in
// the store so that a controller that takes over finishes it; a node still
// in the Proxmox cluster, which would receive every new secret through
// pmxcfs, is refused.
func (e *Engine) ForgetNode(ctx context.Context, name string, compromised bool) (ForgetResult, error) {
	if name == e.d.Node {
		return ForgetResult{}, fmt.Errorf("%w: node %s is this node; pco uninstall takes it out of the install", ErrRefused, name)
	}
	if err := e.acquireWriter(ctx); err != nil {
		return ForgetResult{}, err
	}
	defer e.release()
	rev, err := e.entryOf(name)
	if err != nil {
		return ForgetResult{}, err
	}
	n := rev.Node
	s, err := e.d.Store.Settings()
	if err != nil {
		return ForgetResult{}, fmt.Errorf("reading the settings: %w", err)
	}
	if compromised {
		if err := e.stillInCluster(name); err != nil {
			return ForgetResult{}, err
		}
		if s.ObserveOnly {
			return ForgetResult{}, fmt.Errorf("%w: pco is in observe-only mode and changes nothing at Cloudflare, where every secret of the install "+
				"is rotated; run pco apply first, then pco node forget %s --compromised", ErrRefused, name)
		}
	}
	res := ForgetResult{Node: name, Compromised: compromised, Done: []string{}}
	before := n
	n.State, n.StateSince, n.Drained = store.NodeRemoved, e.d.Now(), nil
	rotates := !compromised && s.Cluster.RotatesOnForget()
	if rotates {
		// Kept with the entry, so that the cycles finish what this call
		// cannot, a controller that takes over included.
		n.Rotation = &store.ForgetRotation{Since: e.d.Now(), TunnelsRotated: store.TunnelsRotated{Connectors: e.connectorIDsOf(name)}}
	}
	var began Event
	if compromised {
		// The rotation is recorded before the node is removed: a crash in
		// between leaves a rotation of a node that is still there, which a
		// second forget joins, never a removed node no rotation is for.
		var err error
		if res, began, err = e.beginRotation(ctx, name, res); err != nil {
			return res, err
		}
	}
	if err := e.saveEntry(ctx, before, rev.Rev, n, fmt.Sprintf("node %s is forgotten%s", name, compromisedText(compromised))); err != nil {
		if compromised {
			began.Message = fmt.Sprintf("rotation of every secret started for node %s; its removal failed: %v; run pco node forget %s --compromised again",
				name, err, name)
			e.events.add(began)
		}
		return res, err
	}
	if compromised {
		e.events.add(began)
	}
	res.Done = append(res.Done, "the entry of the node is removed: its agent is refused from its next request")
	delete(e.nodes.admitted, name)
	if err := e.d.Store.DeleteEnrolment(name); err != nil {
		res.Done = append(res.Done, "its enrolment could not be removed: "+err.Error())
	}
	e.dropJoinTokens(name, &res)
	if e.d.Close != nil {
		e.d.Close(name)
		res.Done = append(res.Done, "its connections to this controller are closed")
	}
	e.forgetReports(name)
	res.Done = append(res.Done, "its pinned tunnels are deleted in the next cycle")
	defer e.Trigger()
	if compromised {
		return res, nil
	}
	switch {
	case !rotates:
		res.Done = append(res.Done, "the secret of the shared tunnel is kept (cluster.rotateOnForget is off): the run token the node holds still works")
	case s.ObserveOnly:
		res.Pending = append(res.Pending, "the secret of the shared tunnel is rotated once pco apply ends observe-only mode: until then the run token the node holds still works")
	default:
		done, pending := e.rotateShared(ctx, name)
		res.Done = append(res.Done, done...)
		res.Pending = pending
	}
	return res, nil
}

func compromisedText(compromised bool) string {
	if compromised {
		return " as compromised: every secret of the install is rotated"
	}
	return ""
}

// stillInCluster refuses to treat a node as compromised while it is in the
// corosync node list or the members file has it online: it would receive
// every new secret through the cluster filesystem.
func (e *Engine) stillInCluster(name string) error {
	read := e.d.Corosync
	if read == nil {
		return fmt.Errorf("%w: this daemon cannot read corosync.conf, so it cannot tell whether node %s left the cluster; "+
			"a node is forgotten as compromised on a host node of the cluster", ErrRefused, name)
	}
	c, err := read()
	if err != nil {
		return fmt.Errorf("%w: reading corosync.conf: %w; a node is forgotten as compromised only once it is known to have left the cluster", ErrRefused, err)
	}
	listed := slices.Contains(c.Nodes, name)
	if m := e.d.Members; m != nil && !listed {
		for _, n := range m().Nodes {
			if n.Name == name && n.Online {
				listed = true
			}
		}
	}
	if listed {
		return fmt.Errorf("%w: run pvecm delnode %s first: a node still in the cluster would receive every new secret", ErrRefused, name)
	}
	return nil
}

// dropJoinTokens removes the join tokens made for a node: a replacement
// needs a new one.
func (e *Engine) dropJoinTokens(name string, res *ForgetResult) {
	tokens, err := e.d.Store.JoinTokens()
	if err != nil {
		res.Done = append(res.Done, "its join tokens could not be read: "+err.Error())
		return
	}
	for _, id := range slices.Sorted(maps.Keys(tokens)) {
		if tokens[id].Node != name {
			continue
		}
		if err := e.d.Store.DeleteJoinToken(id); err != nil {
			res.Done = append(res.Done, fmt.Sprintf("its join token %s could not be removed: %v", id, err))
		}
	}
}

// forgetReports drops what the node's agent was served from the memory of
// the placement: nothing it reports counts any more.
func (e *Engine) forgetReports(name string) {
	delete(e.place.listedAt, name)
}

// rotateShared rotates the secret of the shared tunnel in every account the
// last state shows for a node that was forgotten, as far as its entry shows
// it not done, and says what it did and what is left: the cycles finish
// what is left, and the node's run token works until they did.
func (e *Engine) rotateShared(ctx context.Context, name string) (done, pending []string) {
	rev, err := e.entryOf(name)
	if err != nil || rev.Node.Rotation == nil {
		return nil, []string{"the secret of the shared tunnel is rotated by the next cycles: until then the run token the node holds still works"}
	}
	rot := rev.Node.Rotation
	save := func() error {
		n := rev.Node
		n.Rotation = rot
		r, err := e.d.Store.SaveNodeIf(n, rev.Rev)
		if err != nil {
			return err
		}
		rev = store.NodeRev{Node: n, Rev: r}
		return nil
	}
	for _, t := range e.State().Tunnels {
		if t.Kind == TunnelPinned || !t.Exists || t.ID == "" {
			continue
		}
		what := fmt.Sprintf("tunnel %s in account %s", t.Name, t.AccountID)
		if err := e.rotateFor(ctx, t.TunnelState, &rot.TunnelsRotated, save); err != nil {
			pending = append(pending, fmt.Sprintf("the secret of %s could not be rotated through: %v; it is tried again in the next cycle, "+
				"and until then the run token the node holds may still work", what, err))
			continue
		}
		done = append(done, fmt.Sprintf("the secret of %s is rotated: the run token the node holds stops working, its connector is dropped, "+
			"and the remaining replicas restart with the new one, keeping their connections until then", what))
	}
	if len(done) == 0 && len(pending) == 0 {
		pending = append(pending, "no tunnel of the install is known yet: the secret of the shared tunnel is rotated by the next cycles")
	}
	return done, pending
}

// Handoff releases the lease for another node, after the running cycle: to
// names the node that alone may take it for a while, or no node, and every
// other holds back as long. The caller holds the lease.
func (e *Engine) Handoff(ctx context.Context, to string) (HandoffResult, error) {
	if e.d.Lease == nil {
		return HandoffResult{}, fmt.Errorf("%w: the appliance has no lease to hand off", ErrRefused)
	}
	if err := e.acquireAdmin(ctx); err != nil {
		return HandoffResult{}, err
	}
	defer e.release()
	if !e.d.Lease.State().Holder {
		return HandoffResult{}, fmt.Errorf("%w: this node does not hold the lease", ErrRefused)
	}
	if to != "" {
		if err := e.mayTakeTheLease(to); err != nil {
			return HandoffResult{}, err
		}
	}
	if err := e.d.Lease.Handoff(ctx, to); err != nil {
		return HandoffResult{}, fmt.Errorf("%w: the lease was not handed off: %w", ErrRefused, err)
	}
	res := HandoffResult{From: e.d.Node, To: to, Until: e.d.Now().Add(lease.HandoffBackoff)}
	msg := "the lease is handed off to whichever node comes first"
	if to != "" {
		msg = "the lease is handed off to node " + to
	}
	e.events.add(Event{At: e.d.Now(), Level: levelWarn, Kind: kindLease, Subject: e.d.Node, Message: msg, Actor: ActorOf(ctx)})
	return res, nil
}

// mayTakeTheLease refuses a hand-off to a node that cannot take it.
func (e *Engine) mayTakeTheLease(to string) error {
	if to == e.d.Node {
		return fmt.Errorf("%w: node %s is this node; the lease cannot be handed to itself", ErrRefused, to)
	}
	rev, err := e.entryOf(to)
	if err != nil {
		return err
	}
	n := rev.Node
	switch {
	case n.State == store.NodeRemoved, n.State == store.NodeContested, n.State == store.NodeDown:
		return fmt.Errorf("%w: node %s is %s and cannot take the lease", ErrRefused, to, n.State)
	case n.ControllerEligible != nil && !*n.ControllerEligible:
		return fmt.Errorf("%w: node %s may not hold the lease (controllerEligible is false)", ErrRefused, to)
	}
	return nil
}

// Unblock ends the block of an address every node blocks since an agent saw
// its MAC where it must not be, before the ten minutes without a report
// have passed.
func (e *Engine) Unblock(ctx context.Context, addr netip.Addr) error {
	if err := e.acquireWriter(ctx); err != nil {
		return err
	}
	defer e.release()
	b, ok := e.place.blocks[addr]
	if !ok {
		return fmt.Errorf("%w: %s is not blocked", ErrNotFound, addr)
	}
	// The store first: a block ended in memory alone would be found
	// again by the controller that takes over.
	if err := e.d.Store.DeleteBlock(addr); err != nil {
		return fmt.Errorf("ending the block of %s: %w", addr, err)
	}
	delete(e.place.blocks, addr)
	e.events.add(Event{At: e.d.Now(), Level: levelWarn, Kind: kindBlock, Subject: addr.String(), Guest: b.guest.String(), Node: b.node, Security: true,
		Message: fmt.Sprintf("the block of %s is ended by the admin; its MAC %s was last seen on %s of %s", addr, b.mac, b.port, b.node), Actor: ActorOf(ctx)})
	e.Trigger()
	return nil
}

// manualResolver is the node that resolves the manual routes, which are of no
// guest: this node, unless it is drained, when the first replica whose agent
// reports fresh takes them.
func (c *cycleRun) manualResolver() string {
	self := c.nodes[c.e.d.Node].Node
	if self.Drained == nil && self.State != store.NodeDraining {
		return c.e.d.Node
	}
	for _, name := range c.replicas() {
		if name == c.e.d.Node {
			continue
		}
		if f := c.fleet.node(name); f.fresh && servedState(f.entry.State) {
			return name
		}
	}
	return c.e.d.Node
}
