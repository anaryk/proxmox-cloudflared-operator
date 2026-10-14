package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The steps of the rotation of every secret a compromised node held, in
// their order: the admin's two first, so that the tunnels are rotated with
// tokens the node never had, then the tunnels, the wait for their
// connectors, the CA, and the end of the window in which the old CA is
// still taken.
const (
	stepCredentials = "credentials"
	stepProxmox     = "proxmox"
	stepTunnels     = "tunnels"
	stepRollout     = "rollout"
	stepCA          = "ca"
	stepPrevious    = "previous"
)

var rotationSteps = []string{stepCredentials, stepProxmox, stepTunnels, stepRollout, stepCA, stepPrevious}

const (
	// caRotationWindow is how long after the CA was rotated certificates
	// under the previous one are still taken.
	caRotationWindow = 10 * time.Minute
	// refusedShownFor is how long after a rotation ended the nodes whose
	// certificate the new CA refuses are looked for.
	refusedShownFor = 24 * time.Hour

	kindRotation = "rotation" // the rotation of every secret a compromised node held, step by step

	problemRotationCredentials = "node %s was forgotten as compromised: add new Cloudflare API tokens with pco credential add and remove the old ones " +
		"with pco credential remove, then revoke them at Cloudflare; the tunnels and the CA are rotated after that"
	problemRotationProxmox = "node %s was forgotten as compromised: run pco setup --repair on this node to give the Proxmox token a new secret; " +
		"the tunnels and the CA are rotated after that"
	problemRotationTunnels = "node %s was forgotten as compromised: rotating the secret of %s failed: %v; it is tried again in the next cycle"
	problemRotationRollout = "node %s was forgotten as compromised: waiting for the connectors of %s to run with the new secret before the CA is rotated"
	problemRotationCA      = "node %s was forgotten as compromised: rotating the CA failed: %v; it is tried again in the next cycle"
	problemRotationCleanUp = "node %s was forgotten as compromised: ending the connections made with the old secret of tunnel %s in account %s failed: %v; " +
		"it is tried again in the next cycle"
	problemRotationHeld    = "node %s was forgotten as compromised: the rotation of %s waits for a cycle that writes: %s"
	problemRotationRefused = "the certificate of node %s is not under the install's new CA: its agent is refused until the node issues its certificate again, " +
		"which it does when it reads leader.json at its next start"
	problemRotationDrift = "node %s was forgotten as compromised, but the record of the rotation and the install's CA disagree: %s; " +
		"no step of the rotation runs until they agree"
)

// beginRotation records the rotation of every secret a compromised node
// held, which the cycles then carry out step by step, and returns the event
// that says so once the node is removed. The caller holds the cycle lock.
func (e *Engine) beginRotation(ctx context.Context, name string, res ForgetResult) (ForgetResult, Event, error) {
	under, found, err := e.d.Store.CARotation()
	if err != nil {
		return res, Event{}, fmt.Errorf("reading the rotation of the secrets: %w", err)
	}
	e.dropEveryJoinToken(&res)
	if found && under.FinishedAt.IsZero() {
		return e.joinRotation(ctx, name, under, res)
	}
	rec := store.CARotation{Node: name, StartedAt: e.d.Now(), Actor: ActorOf(ctx),
		TunnelsRotated: store.TunnelsRotated{Connectors: e.connectorIDsOf(name)}}
	if tok, found, err := e.d.Store.PVEToken(); err != nil {
		return res, Event{}, fmt.Errorf("reading the Proxmox token: %w", err)
	} else if found {
		rec.PVESecretSHA256 = secretHash(tok.Secret.Reveal())
	}
	if err := e.d.Store.SaveCARotation(rec); err != nil {
		return res, Event{}, fmt.Errorf("recording the rotation: %w", err)
	}
	ev := Event{At: rec.StartedAt, Level: levelWarn, Kind: kindRotation, Subject: name, Node: name, Security: true, Actor: ActorOf(ctx),
		Message: fmt.Sprintf("node %s was forgotten as compromised: every secret of the install is rotated, step by step", name)}
	res.Done = append(res.Done, "the rotation of every secret of the install began; pco cluster status follows it, and a controller that takes over finishes it")
	res.Pending = append(slices.Clone(rotationSteps), res.Pending...)
	res.NotRotated = slices.Clone(notRotated)
	return res, ev, nil
}

// dropEveryJoinToken deletes every join token not used yet: the node may
// have read what the store keeps of them.
func (e *Engine) dropEveryJoinToken(res *ForgetResult) {
	tokens, err := e.d.Store.JoinTokens()
	if err != nil {
		res.Pending = append(res.Pending, "the join tokens could not be read to delete them: "+err.Error())
		return
	}
	for _, id := range slices.Sorted(maps.Keys(tokens)) {
		if err := e.d.Store.DeleteJoinToken(id); err != nil {
			res.Pending = append(res.Pending, fmt.Sprintf("join token %s could not be deleted: %v", id, err))
		}
	}
	if len(tokens) > 0 {
		res.Done = append(res.Done, "every join token is deleted: a node still to join needs a new one")
	}
}

// redoneOnJoin are the steps a node that joins a rotation has done again
// when they ran while it was a member: it may hold what each of them made.
var redoneOnJoin = []string{stepCredentials, stepProxmox, stepTunnels, stepRollout, stepCA}

// joinRotation adds a node forgotten as compromised to the rotation under
// way. Every step that ran while the node was still a member is done again
// from now: the credentials and the Proxmox secret count from the values of
// now, every tunnel gets a secret again, and a CA rotated already is rotated
// a second time once the window of its previous one ended, never a third
// at once. The rotation keeps when it began.
func (e *Engine) joinRotation(ctx context.Context, name string, rec store.CARotation, res ForgetResult) (ForgetResult, Event, error) {
	now := e.d.Now()
	since, err := e.joinedAt(now, rec)
	if err != nil {
		return res, Event{}, err
	}
	if name != rec.Node && !slices.Contains(rec.Also, name) {
		rec.Also = append(rec.Also, name)
	}
	var redo []string
	// Every step done so far ran before now, whatever time it is stamped
	// with: one stamped by a clock ahead of this one included.
	for _, step := range redoneOnJoin {
		if slices.Contains(rec.Done, step) {
			redo = append(redo, step)
		}
	}
	rec.Done = slices.DeleteFunc(rec.Done, func(step string) bool { return slices.Contains(redo, step) })
	for _, step := range redo {
		delete(rec.StepsAt, step)
	}
	if !rec.At.IsZero() || slices.Contains(redo, stepTunnels) || slices.Contains(redo, stepRollout) {
		rec.Rotated, rec.TakenUp, rec.At, rec.Cleaned = nil, nil, time.Time{}, nil
	}
	// The baselines are what the node may have seen: the credentials stored
	// and the Proxmox secret of now.
	rec.Since, rec.PVESecretSHA256 = since, ""
	if tok, found, err := e.d.Store.PVEToken(); err != nil {
		return res, Event{}, fmt.Errorf("reading the Proxmox token: %w", err)
	} else if found {
		rec.PVESecretSHA256 = secretHash(tok.Secret.Reveal())
	}
	for tunnel, ids := range e.connectorIDsOf(name) {
		if rec.Connectors == nil {
			rec.Connectors = map[string][]string{}
		}
		rec.Connectors[tunnel] = slices.Compact(slices.Sorted(slices.Values(append(rec.Connectors[tunnel], ids...))))
	}
	if err := e.d.Store.SaveCARotation(rec); err != nil {
		return res, Event{}, fmt.Errorf("recording the rotation: %w", err)
	}
	msg := fmt.Sprintf("node %s was forgotten as compromised: it joins the rotation of every secret that began at %s for node %s",
		name, rec.StartedAt.UTC().Format(time.RFC3339), rec.Node)
	if len(redo) > 0 {
		msg += fmt.Sprintf(", whose steps that ran while it was a member are done again: %s", strings.Join(redo, ", "))
	}
	ev := Event{At: now, Level: levelWarn, Kind: kindRotation, Subject: name, Node: name, Security: true, Actor: ActorOf(ctx), Message: msg}
	res.Done = append(res.Done, fmt.Sprintf("the node joins the rotation of every secret under way since %s for node %s; "+
		"what it ran while the node was a member is done again; pco cluster status follows it", rec.StartedAt.UTC().Format(time.RFC3339), rec.Node))
	pending := res.Pending
	res.Pending = []string{}
	for _, step := range rotationSteps {
		if !slices.Contains(rec.Done, step) {
			res.Pending = append(res.Pending, step)
		}
	}
	res.Pending = append(res.Pending, pending...)
	res.NotRotated = slices.Clone(notRotated)
	return res, ev, nil
}

func secretHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// rotationStep carries the rotation on from where it is, once the lease let
// the cycle write, and writes each step done into the record. Only the
// controller rotates; a controller that takes over reads the record and
// goes on.
func (c *cycleRun) rotationStep() {
	if c.leaseHeld || c.storeHold {
		return
	}
	rec, found, err := c.e.d.Store.CARotation()
	switch {
	case err != nil:
		c.problem("reading the rotation of the secrets: %v", err)
		return
	case !found:
		return
	}
	if rec.FinishedAt.IsZero() && staleSteps(&rec) {
		// Steps that ran before the last node joined count for nothing,
		// and are taken out of the record before it is checked.
		if err := c.e.d.Store.SaveCARotation(rec); err != nil {
			c.problem("recording the rotation of the secrets: %v", err)
			return
		}
	}
	// What the record says was done is checked against the CA the store
	// holds and the one leader.json names, every cycle: a store restored
	// from a copy, or a write that failed between two steps, must not let
	// the rotation go on from a step the files do not show.
	facts, err := c.caFacts()
	if err != nil {
		c.problem("%s", fmt.Sprintf(problemRotationDrift, rec.Node, err))
		c.e.nodes.noteDrift(c, rec.Node, err.Error())
		return
	}
	if why := rotationDrift(rec, facts); why != "" {
		c.problem("%s", fmt.Sprintf(problemRotationDrift, rec.Node, why))
		c.e.nodes.noteDrift(c, rec.Node, why)
		return
	}
	if !rec.FinishedAt.IsZero() {
		// Finished: a previous CA a crash left behind still goes.
		if facts.previous {
			c.dropPreviousCA()
		}
		return
	}
	for _, step := range rotationSteps {
		if slices.Contains(rec.Done, step) && !c.undone(&rec, step) {
			continue
		}
		done, wait := c.rotationStepDone(&rec, step)
		if wait != "" {
			c.problem("%s", wait)
		}
		if !done {
			return
		}
		if !slices.Contains(rec.Done, step) {
			rec.Done = append(rec.Done, step)
		}
		if rec.StepsAt == nil {
			rec.StepsAt = map[string]time.Time{}
		}
		rec.StepsAt[step] = notBefore(c.now, rec.From())
		if step == stepPrevious {
			rec.FinishedAt = c.now
		}
		if err := c.e.d.Store.SaveCARotation(rec); err != nil {
			c.problem("recording the rotation of the secrets: %v", err)
			return
		}
		c.rotationEvent(rec, step)
	}
}

// undone says whether a step the record holds done has to be done again,
// as the store shows it: a tunnel of the install not rotated, which a
// tunnel made since or a record of another time leaves, is rotated before
// anything after it.
func (c *cycleRun) undone(rec *store.CARotation, step string) bool {
	if step != stepTunnels || !c.checked || c.cfHold {
		return false
	}
	for _, t := range c.tunnels {
		if t.Exists && t.ID != "" && !rotatedThrough(rec.TunnelsRotated, t.ID) {
			return true
		}
	}
	return false
}

// staleSteps takes out of a record the steps it holds done that ran before
// the last node joined the rotation, and the secrets given before it, as a
// record a join could not clear shows them: that node may hold what they
// made. It says whether it took anything out.
func staleSteps(rec *store.CARotation) bool {
	from := rec.From()
	stale := false
	rec.Done = slices.DeleteFunc(rec.Done, func(step string) bool {
		at, ok := rec.StepsAt[step]
		if ok && at.Before(from) && slices.Contains(redoneOnJoin, step) {
			delete(rec.StepsAt, step)
			stale = true
			return true
		}
		return false
	})
	if !rec.At.IsZero() && rec.At.Before(from) {
		rec.Rotated, rec.TakenUp, rec.Cleaned, rec.At = nil, nil, nil, time.Time{}
		stale = true
	}
	return stale
}

// joinedAt is when a join counts from: now, or just after the latest time
// the rotation stamped what it did, a step, a secret or the CA, when a
// clock ahead of this one stamped it. Whatever ran before the join counts
// as before it, and is done again.
func (e *Engine) joinedAt(now time.Time, rec store.CARotation) (time.Time, error) {
	_, _, rotatedAt, err := e.d.Store.CA()
	if err != nil && !errors.Is(err, store.ErrNoCA) {
		return time.Time{}, fmt.Errorf("reading the CA: %w", err)
	}
	stamps := append(slices.Collect(maps.Values(rec.StepsAt)), rec.At, rec.From(), rotatedAt)
	join := now
	for _, at := range stamps {
		if !at.Before(join) {
			join = at.Add(time.Nanosecond)
		}
	}
	return join, nil
}

// notBefore is t, or from when t is before it. What a controller does in a
// rotation is stamped no earlier than the last join: a clock behind the one
// that recorded it would make it look stale, and have it done again.
func notBefore(t, from time.Time) time.Time {
	if t.Before(from) {
		return from
	}
	return t
}

// rotatedThrough says whether the secret of a tunnel was rotated and the
// rotation followed through.
func rotatedThrough(r store.TunnelsRotated, id string) bool {
	return slices.Contains(r.Rotated, id) && slices.Contains(r.TakenUp, id)
}

// caFactsView is the CA as the store and leader.json have it now.
type caFactsView struct {
	current   string // the SHA-256 of the current CA
	previous  bool   // a previous CA is kept
	prevSHA   string
	rotatedAt time.Time
	leader    string // the CA leader.json names; empty for none
}

// caFacts reads the CA of the store and the one leader.json names.
func (c *cycleRun) caFacts() (caFactsView, error) {
	cur, prev, rotatedAt, err := c.e.d.Store.CA()
	if err != nil {
		return caFactsView{}, fmt.Errorf("the install's CA cannot be read: %w", err)
	}
	curCert, err := parseCert(cur.CertPEM)
	if err != nil {
		return caFactsView{}, fmt.Errorf("the install's current CA cannot be read: %w", err)
	}
	f := caFactsView{current: pki.Fingerprint(curCert), rotatedAt: rotatedAt}
	if prev != nil {
		prevCert, err := parseCert(prev.CertPEM)
		if err != nil {
			return caFactsView{}, fmt.Errorf("the install's previous CA cannot be read: %w", err)
		}
		f.previous, f.prevSHA = true, pki.Fingerprint(prevCert)
	}
	rec, _, found, err := c.e.d.Store.Leader()
	if err != nil {
		return caFactsView{}, fmt.Errorf("leader.json cannot be read: %w", err)
	}
	if found {
		f.leader = rec.CASHA256
	}
	return f, nil
}

// rotationDrift says how the record of a rotation and the CA of the store
// disagree, or nothing when they agree, a step cut short included: a CA
// made for the rotation and not yet named in leader.json or in the record
// is what the next cycle finishes, not drift.
func rotationDrift(rec store.CARotation, f caFactsView) string {
	rotated := !f.rotatedAt.IsZero() && !f.rotatedAt.Before(rec.From())
	caDone := slices.Contains(rec.Done, stepCA)
	switch {
	case caDone && !rotated:
		return fmt.Sprintf("the record says the CA was rotated, but the store's CA was not since the rotation began at %s: the store was restored from an earlier copy",
			rec.From().UTC().Format(time.RFC3339))
	case caDone && f.leader != f.current:
		return fmt.Sprintf("the record says the CA was rotated, but leader.json names CA %s where the store's current CA is %s", caName(f.leader), f.current)
	case !caDone && rotated && !f.previous:
		return "the store's CA was rotated for this rotation and its previous CA is gone, before the record says the CA step was done: the store and the record are of different times"
	case f.leader != "" && f.leader != f.current && (!f.previous || f.leader != f.prevSHA):
		return fmt.Sprintf("leader.json names CA %s, which is neither the store's current CA (%s) nor its previous one", f.leader, f.current)
	}
	return ""
}

func caName(sha string) string {
	if sha == "" {
		return "none"
	}
	return sha
}

// noteDrift tells once, per reason, that a rotation cannot go on: the event
// is of the audit trail.
func (m *nodeMemory) noteDrift(c *cycleRun, node, why string) {
	if m.driftSaid == why {
		return
	}
	m.driftSaid = why
	c.events = append(c.events, Event{At: c.now, Level: levelError, Kind: kindRotation, Subject: node, Node: node, Security: true,
		Message: fmt.Sprintf(problemRotationDrift, node, why)})
}

// rotationStepDone does what one step of a rotation does now, and says
// whether the step is done, and what it waits for when it is not.
func (c *cycleRun) rotationStepDone(rec *store.CARotation, step string) (done bool, wait string) {
	switch step {
	case stepCredentials:
		if c.credentialsReplaced(rec.From()) {
			return true, ""
		}
		return false, fmt.Sprintf(problemRotationCredentials, rec.Node)
	case stepProxmox:
		if c.pveSecretReplaced(rec.PVESecretSHA256) {
			return true, ""
		}
		return false, fmt.Sprintf(problemRotationProxmox, rec.Node)
	case stepTunnels:
		if why := c.rotationHeld(); why != "" {
			return false, fmt.Sprintf(problemRotationHeld, rec.Node, "the tunnels", why)
		}
		return c.rotateTunnels(rec)
	case stepRollout:
		if waiting := c.notRolledOut(rec.TunnelsRotated); len(waiting) > 0 {
			return false, fmt.Sprintf(problemRotationRollout, rec.Node, andList(waiting))
		}
		if why := c.rotationHeld(); why != "" {
			return false, fmt.Sprintf(problemRotationHeld, rec.Node, "the tunnels", why)
		}
		return c.cleanUpRotated(rec)
	case stepCA:
		if why := c.rotationHeld(); why != "" {
			return false, fmt.Sprintf(problemRotationHeld, rec.Node, "the CA", why)
		}
		if err := c.rotateCA(rec.From()); err != nil {
			return false, fmt.Sprintf(problemRotationCA, rec.Node, err)
		}
		return true, ""
	case stepPrevious:
		return c.dropPreviousCA()
	}
	return true, ""
}

// rotationHeld says why this cycle may not rotate a secret at Cloudflare or
// the CA, as it may not write what it sees: observe-only mode, a hold of the
// cycle, the egress filter or the gateway that could not be set, or a cycle
// that only adds. Empty when it may.
func (c *cycleRun) rotationHeld() string {
	switch {
	case c.settings.ObserveOnly:
		return "pco is in observe-only mode; pco apply ends it"
	case c.cfHold:
		return "the cycle changes nothing at Cloudflare"
	case c.egressHeld != "":
		return c.egressHeld
	case c.gatewayHeld != "":
		return c.gatewayHeld
	case c.addOnly != "":
		return c.addOnly
	}
	return ""
}

// credentialsReplaced says that every credential stored was added since
// the rotation began, and there is one.
func (c *cycleRun) credentialsReplaced(since time.Time) bool {
	creds, err := c.e.d.Store.Credentials()
	if err != nil || len(creds) == 0 {
		return false
	}
	for _, cred := range creds {
		if cred.AddedAt.Before(since) {
			return false
		}
	}
	return true
}

// pveSecretReplaced says that the Proxmox token has another secret than the
// rotation began with; a node without one has nothing to replace.
func (c *cycleRun) pveSecretReplaced(was string) bool {
	if was == "" {
		return true
	}
	tok, found, err := c.e.d.Store.PVEToken()
	if err != nil || !found {
		return false
	}
	return secretHash(tok.Secret.Reveal()) != was
}

// rotateTunnels rotates the secret of every tunnel of the install the tunnel
// run found, shared and pinned, each once. The step is done only in a cycle
// that read every account through to the end: a held cycle, or one with an
// account it could not read, may have missed a tunnel, and the CA must not
// be rotated while a tunnel keeps a secret the node knows.
func (c *cycleRun) rotateTunnels(rec *store.CARotation) (bool, string) {
	switch {
	case c.cfHold || !c.checked:
		return false, fmt.Sprintf(problemRotationTunnels, rec.Node, "the tunnels", errors.New("the cycle did not get through to Cloudflare, so not every tunnel is known"))
	case len(c.unreported) > 0:
		return false, fmt.Sprintf(problemRotationTunnels, rec.Node, "the tunnels", fmt.Errorf("%d tunnels are in accounts the tunnel run could not read", len(c.unreported)))
	}
	var unreached []string
	for _, t := range c.tunnels {
		if t.Exists && t.ID != "" && t.CredentialID == "" {
			unreached = append(unreached, fmt.Sprintf("tunnel %s in account %s", t.Name, t.AccountID))
		}
	}
	if len(unreached) > 0 {
		c.caHeld = "the CA waits: no credential reaches " + andList(unreached)
		return false, fmt.Sprintf(problemRotationTunnels, rec.Node, "the tunnels",
			fmt.Errorf("no credential reaches %s: add one for the account with pco credential add; the CA is not rotated until its secret is", andList(unreached)))
	}
	save := func() error {
		if !rec.At.IsZero() {
			// A secret given by a clock behind the one that recorded the
			// join must not look given before it, and be given again.
			rec.At = notBefore(rec.At, rec.From())
		}
		return c.e.d.Store.SaveCARotation(*rec)
	}
	for _, t := range c.tunnels {
		if !t.Exists || t.ID == "" || rotatedThrough(rec.TunnelsRotated, t.ID) {
			continue
		}
		fresh := !slices.Contains(rec.Rotated, t.ID)
		err := c.e.rotateFor(c.ctx, t, &rec.TunnelsRotated, save)
		if fresh && slices.Contains(rec.Rotated, t.ID) {
			c.events = append(c.events, Event{At: c.now, Level: levelWarn, Kind: kindRotation, Subject: t.Name, Tunnel: t.Name, Account: t.AccountID, Security: true,
				Message: fmt.Sprintf("the secret of tunnel %s in account %s is rotated: every run token of it stops working", t.Name, t.AccountID)})
		}
		if err != nil {
			return false, fmt.Sprintf(problemRotationTunnels, rec.Node, fmt.Sprintf("tunnel %s in account %s", t.Name, t.AccountID), err)
		}
	}
	return true, ""
}

// cleanUpRotated ends the connections of every connector of each tunnel
// whose secret was rotated, once every connector that remains runs with the
// new one: a connector that still has a session made with the old secret,
// wherever it runs, loses it then, and the replicas connect again at once.
func (c *cycleRun) cleanUpRotated(rec *store.CARotation) (bool, string) {
	for _, t := range c.tunnels {
		if !slices.Contains(rec.Rotated, t.ID) || slices.Contains(rec.Cleaned, t.ID) {
			continue
		}
		api := c.e.clients[t.CredentialID]
		if api == nil {
			return false, fmt.Sprintf(problemRotationCleanUp, rec.Node, t.Name, t.AccountID, fmt.Errorf("no client for credential %s", t.CredentialID))
		}
		if err := api.CleanUpConnections(c.ctx, t.AccountID, t.ID); err != nil {
			return false, fmt.Sprintf(problemRotationCleanUp, rec.Node, t.Name, t.AccountID, err)
		}
		rec.Cleaned = append(rec.Cleaned, t.ID)
		if err := c.e.d.Store.SaveCARotation(*rec); err != nil {
			return false, fmt.Sprintf(problemRotationTunnels, rec.Node, "the record", err)
		}
	}
	return true, ""
}

// notRolledOut names the tunnels a connector of which does not run ready
// with the new secret yet on every node that runs one: the replicas for
// the shared tunnel, the node of a pinned one. A node whose report is stale,
// or says nothing of the tunnel, has not rolled it out.
func (c *cycleRun) notRolledOut(r store.TunnelsRotated) []string {
	waiting := map[string]bool{}
	for _, t := range c.tunnels {
		if !slices.Contains(r.Rotated, t.ID) {
			continue
		}
		for _, node := range c.runners(t) {
			if !c.readySince(node, t.ID, r.At) {
				waiting[t.Name] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(waiting))
}

// readySince says whether node runs a ready connector of a tunnel by a
// status read after since: this node's in this cycle, another's by a report
// received after since. A status from before says nothing of a secret given
// at since.
func (c *cycleRun) readySince(node, tunnelID string, since time.Time) bool {
	if node == c.e.d.Node {
		return c.now.After(since) && c.readyOn(node, tunnelID)
	}
	return c.fleet.node(node).report.ReceivedAt.After(since) && c.readyOn(node, tunnelID)
}

// runners are the nodes that run a connector of a tunnel: the replicas of
// the shared one, the node of a pinned one while it is neither removed nor
// contested.
func (c *cycleRun) runners(t reconcile.TunnelState) []string {
	if t.Kind != TunnelPinned {
		return c.replicas()
	}
	if n, ok := c.nodes[t.Node]; ok && n.Node.State != store.NodeRemoved && n.Node.State != store.NodeContested {
		return []string{t.Node}
	}
	return nil
}

// rotateCA makes a new CA of the install, keeps the old one as the previous
// for the window, names the new one in leader.json, which every host agent
// reads, and has this node issue its own certificates again at once. A CA
// made for this rotation already, by a cycle that could not finish the
// step, is kept: a third one would put the agents' CA out of the window.
func (c *cycleRun) rotateCA(since time.Time) error {
	cur, prev, rotatedAt, err := c.e.d.Store.CA()
	if err != nil {
		return fmt.Errorf("reading the CA: %w", err)
	}
	ca, err := pki.ParseCA([]byte(cur.CertPEM), []byte(cur.KeyPEM.Reveal()))
	if err != nil {
		return fmt.Errorf("reading the CA: %w", err)
	}
	if prev != nil && rotatedAt.Before(since) {
		// The previous CA of an earlier rotation: replacing it would put
		// the agents still under it out before their window ended.
		if c.now.Before(rotatedAt.Add(caRotationWindow)) {
			return fmt.Errorf("the previous CA of an earlier rotation is taken until %s; the CA is rotated once it is dropped",
				rotatedAt.Add(caRotationWindow).UTC().Format(time.RFC3339))
		}
		if err := c.e.d.Store.SaveCA(cur, nil, rotatedAt); err != nil {
			return fmt.Errorf("removing the previous CA of an earlier rotation: %w", err)
		}
		prev = nil
	}
	if prev == nil {
		// The new CA is of the install the old one names: the one the
		// agents' identities carry.
		installID, err := ca.InstallID()
		if err != nil {
			return err
		}
		if ca, err = pki.NewCA(installID, c.now, rand.Reader); err != nil {
			return err
		}
		certPEM, keyPEM, err := ca.PEM()
		if err != nil {
			return err
		}
		next := store.CAFiles{CertPEM: string(certPEM), KeyPEM: store.NewSecret(string(keyPEM))}
		if err := c.e.d.Store.SaveCA(next, &cur, notBefore(c.now, since)); err != nil {
			return fmt.Errorf("saving the new CA: %w", err)
		}
	}
	rec, rev, found, err := c.e.d.Store.Leader()
	switch {
	case err != nil:
		return fmt.Errorf("reading leader.json: %w", err)
	case !found:
		return errors.New("leader.json is missing")
	}
	if rec.CASHA256 != ca.SHA256() {
		rec.CASHA256 = ca.SHA256()
		if _, err := c.e.d.Store.SaveLeaderIf(rec, rev); err != nil {
			return fmt.Errorf("naming the new CA in leader.json: %w", err)
		}
	}
	if reissue := c.e.d.Reissue; reissue != nil {
		if err := reissue(c.ctx); err != nil {
			c.problem("issuing this node's certificates from the new CA: %v; the agent of this node issues them when it reads leader.json", err)
		}
	}
	return nil
}

// dropPreviousCA removes the previous CA once the window is over: from then
// on only certificates under the new one are taken.
func (c *cycleRun) dropPreviousCA() (bool, string) {
	cur, prev, rotatedAt, err := c.e.d.Store.CA()
	switch {
	case err != nil:
		return false, "reading the CA: " + err.Error()
	case prev == nil:
		return true, ""
	case c.now.Before(rotatedAt.Add(caRotationWindow)):
		return false, ""
	}
	if err := c.e.d.Store.SaveCA(cur, nil, rotatedAt); err != nil {
		return false, "removing the previous CA: " + err.Error()
	}
	return true, ""
}

// rotationEvent tells of a step of the rotation done.
func (c *cycleRun) rotationEvent(rec store.CARotation, step string) {
	msgs := map[string]string{
		stepCredentials: "the Cloudflare credentials were replaced; revoke the old API tokens at Cloudflare, which pco cannot do",
		stepProxmox:     "the Proxmox token has a new secret",
		stepTunnels:     "every tunnel of the install has a new secret",
		stepRollout:     "every connector runs with the new secrets",
		stepCA:          "the CA of the install is rotated: the previous one is taken for " + caRotationWindow.String() + " more",
		stepPrevious:    "the rotation of every secret is finished: only certificates under the new CA are taken",
	}
	c.events = append(c.events, Event{At: c.now, Level: levelWarn, Kind: kindRotation, Subject: rec.Node, Node: rec.Node, Security: true,
		Message: fmt.Sprintf("node %s was forgotten as compromised: %s", rec.Node, msgs[step])})
}

// rotationView is the rotation as the state shows it, with the nodes whose
// certificate the new CA refuses once it is the only one taken.
func (c *cycleRun) rotationView() *CARotationView {
	rec, found, err := c.e.d.Store.CARotation()
	if err != nil || !found {
		return nil
	}
	v := &CARotationView{Node: rec.Node, Also: slices.Clone(rec.Also), StartedAt: rec.StartedAt, Done: slices.Clone(rec.Done), FinishedAt: rec.FinishedAt, Held: c.caHeld}
	for _, step := range rotationSteps {
		if !slices.Contains(rec.Done, step) {
			v.Next = step
			break
		}
	}
	if !rec.FinishedAt.IsZero() && c.now.Before(rec.FinishedAt.Add(refusedShownFor)) {
		v.Refused = c.refusedByCA()
	}
	return v
}

// refusedByCA names the nodes whose enrolled certificate is not under the
// current CA.
func (c *cycleRun) refusedByCA() []string {
	cur, _, _, err := c.e.d.Store.CA()
	if err != nil {
		return nil
	}
	ca, err := pki.ParseCA([]byte(cur.CertPEM), []byte(cur.KeyPEM.Reveal()))
	if err != nil {
		return nil
	}
	var out []string
	for _, name := range slices.Sorted(maps.Keys(c.nodes)) {
		n := c.nodes[name].Node
		if n.State == store.NodeRemoved || n.State == store.NodeContested {
			continue
		}
		en, found, err := c.e.d.Store.Enrolment(name)
		if err != nil || !found {
			continue
		}
		cert, err := parseCert(en.CertPEM)
		if err != nil || cert.CheckSignatureFrom(ca.Cert) != nil {
			out = append(out, name)
			c.problem(problemRotationRefused, name)
		}
	}
	return out
}

// parseCert reads the one certificate of a PEM text.
func parseCert(text string) (*x509.Certificate, error) {
	b, _ := pem.Decode([]byte(text))
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("no certificate in the PEM text")
	}
	return x509.ParseCertificate(b.Bytes)
}
