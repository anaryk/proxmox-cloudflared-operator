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
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
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
	problemRotationRefused = "the certificate of node %s is not under the install's new CA: its agent is refused until the node issues its certificate again, " +
		"which it does when it reads leader.json at its next start"
)

// beginRotation records the rotation of every secret a compromised node
// held, which the cycles then carry out step by step. The caller holds the
// cycle lock.
func (e *Engine) beginRotation(ctx context.Context, name string, res ForgetResult) (ForgetResult, error) {
	rec := store.CARotation{Node: name, StartedAt: e.d.Now(), Actor: ActorOf(ctx)}
	if tok, found, err := e.d.Store.PVEToken(); err != nil {
		return res, fmt.Errorf("reading the Proxmox token: %w", err)
	} else if found {
		rec.PVESecretSHA256 = secretHash(tok.Secret.Reveal())
	}
	if err := e.d.Store.SaveCARotation(rec); err != nil {
		return res, fmt.Errorf("recording the rotation: %w", err)
	}
	e.events.add(Event{At: rec.StartedAt, Level: levelWarn, Kind: kindRotation, Subject: name, Node: name, Security: true, Actor: ActorOf(ctx),
		Message: fmt.Sprintf("node %s was forgotten as compromised: every secret of the install is rotated, step by step", name)})
	res.Done = append(res.Done, "the rotation of every secret of the install began; pco cluster status follows it, and a controller that takes over finishes it")
	res.Pending = slices.Clone(rotationSteps)
	res.NotRotated = slices.Clone(notRotated)
	return res, nil
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
	case !found || !rec.FinishedAt.IsZero():
		return
	}
	for _, step := range rotationSteps {
		if slices.Contains(rec.Done, step) {
			continue
		}
		done, wait := c.rotationStepDone(&rec, step)
		if wait != "" {
			c.problem("%s", wait)
		}
		if !done {
			return
		}
		rec.Done = append(rec.Done, step)
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

// rotationStepDone does what one step of a rotation does now, and says
// whether the step is done, and what it waits for when it is not.
func (c *cycleRun) rotationStepDone(rec *store.CARotation, step string) (done bool, wait string) {
	switch step {
	case stepCredentials:
		if c.credentialsReplaced(rec.StartedAt) {
			return true, ""
		}
		return false, fmt.Sprintf(problemRotationCredentials, rec.Node)
	case stepProxmox:
		if c.pveSecretReplaced(rec.PVESecretSHA256) {
			return true, ""
		}
		return false, fmt.Sprintf(problemRotationProxmox, rec.Node)
	case stepTunnels:
		return c.rotateTunnels(rec)
	case stepRollout:
		if waiting := c.notRolledOut(rec.Rotated); len(waiting) > 0 {
			return false, fmt.Sprintf(problemRotationRollout, rec.Node, andList(waiting))
		}
		return true, ""
	case stepCA:
		if err := c.rotateCA(); err != nil {
			return false, fmt.Sprintf(problemRotationCA, rec.Node, err)
		}
		return true, ""
	case stepPrevious:
		return c.dropPreviousCA()
	}
	return true, ""
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
// run found, shared and pinned, each once.
func (c *cycleRun) rotateTunnels(rec *store.CARotation) (bool, string) {
	if !c.connected && len(c.tunnels) == 0 {
		return false, fmt.Sprintf(problemRotationTunnels, rec.Node, "the tunnels", errors.New("the tunnels were not read in this cycle"))
	}
	for _, t := range c.tunnels {
		if !t.Exists || t.ID == "" || t.CredentialID == "" || slices.Contains(rec.Rotated, t.ID) {
			continue
		}
		if err := c.e.rotateSecret(c.ctx, t); err != nil {
			return false, fmt.Sprintf(problemRotationTunnels, rec.Node, fmt.Sprintf("tunnel %s in account %s", t.Name, t.AccountID), err)
		}
		rec.Rotated = append(rec.Rotated, t.ID)
		if err := c.e.d.Store.SaveCARotation(*rec); err != nil {
			return false, fmt.Sprintf(problemRotationTunnels, rec.Node, "the record", err)
		}
		c.events = append(c.events, Event{At: c.now, Level: levelWarn, Kind: kindRotation, Subject: t.Name, Tunnel: t.Name, Account: t.AccountID, Security: true,
			Message: fmt.Sprintf("the secret of tunnel %s in account %s is rotated: every run token of it stops working", t.Name, t.AccountID)})
	}
	return true, ""
}

// notRolledOut names the tunnels a connector of which does not run ready
// with the new secret yet, on this node or on one the agents report.
func (c *cycleRun) notRolledOut(rotated []string) []string {
	waiting := map[string]bool{}
	nameOf := map[string]string{}
	for _, t := range c.tunnels {
		nameOf[t.ID] = t.Name
	}
	for _, s := range c.before {
		if slices.Contains(rotated, s.TunnelID) && s.Active && !s.Ready {
			waiting[s.TunnelID] = true
		}
	}
	for name, rep := range c.reportsNow() {
		if name == c.e.d.Node || !rep.Fresh {
			continue
		}
		for _, s := range rep.Status.Connectors {
			if slices.Contains(rotated, s.TunnelID) && s.Active && !s.Ready {
				waiting[s.TunnelID] = true
			}
		}
	}
	var out []string
	for id := range maps.Keys(waiting) {
		name := nameOf[id]
		if name == "" {
			name = id
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// rotateCA makes a new CA of the install, keeps the old one as the previous
// for the window, names the new one in leader.json, which every host agent
// reads, and has this node issue its own certificates again at once.
func (c *cycleRun) rotateCA() error {
	cur, _, _, err := c.e.d.Store.CA()
	if err != nil {
		return fmt.Errorf("reading the CA: %w", err)
	}
	old, err := pki.ParseCA([]byte(cur.CertPEM), []byte(cur.KeyPEM.Reveal()))
	if err != nil {
		return fmt.Errorf("reading the CA: %w", err)
	}
	// The new CA is of the install the old one names: the one the agents'
	// identities carry.
	installID, err := old.InstallID()
	if err != nil {
		return err
	}
	ca, err := pki.NewCA(installID, c.now, rand.Reader)
	if err != nil {
		return err
	}
	certPEM, keyPEM, err := ca.PEM()
	if err != nil {
		return err
	}
	next := store.CAFiles{CertPEM: string(certPEM), KeyPEM: store.NewSecret(string(keyPEM))}
	if err := c.e.d.Store.SaveCA(next, &cur, c.now); err != nil {
		return fmt.Errorf("saving the new CA: %w", err)
	}
	rec, rev, found, err := c.e.d.Store.Leader()
	switch {
	case err != nil:
		return fmt.Errorf("reading leader.json: %w", err)
	case !found:
		return errors.New("leader.json is missing")
	}
	rec.CASHA256 = ca.SHA256()
	if _, err := c.e.d.Store.SaveLeaderIf(rec, rev); err != nil {
		return fmt.Errorf("naming the new CA in leader.json: %w", err)
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
		stepCredentials: "the Cloudflare credentials were replaced",
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
	v := &CARotationView{Node: rec.Node, StartedAt: rec.StartedAt, Done: slices.Clone(rec.Done), FinishedAt: rec.FinishedAt}
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
