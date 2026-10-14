package engine

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// certUnder is a client certificate of the host node pve2 under ca.
func certUnder(t *testing.T, ca pki.CA) string {
	t.Helper()
	key, err := pki.NewKey(rand.Reader)
	require.NoError(t, err)
	id := pki.Identity{Kind: pki.KindHost, InstallID: pkiInstall, Node: "pve2"}
	csr, err := pki.NewCSR(key, id, rand.Reader)
	require.NoError(t, err)
	cert, err := ca.IssueClient(csr, id, t0, rand.Reader)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
}

// fingerprintOf is the SHA-256 of a certificate in PEM.
func fingerprintOf(t *testing.T, certPEM string) string {
	t.Helper()
	b, _ := pem.Decode([]byte(certPEM))
	require.NotNil(t, b)
	cert, err := x509.ParseCertificate(b.Bytes)
	require.NoError(t, err)
	return pki.Fingerprint(cert)
}

// seedCA stores ca as the install's CA.
func seedCA(t *testing.T, e *env, ca pki.CA) {
	t.Helper()
	certPEM, keyPEM, err := ca.PEM()
	require.NoError(t, err)
	require.NoError(t, e.store.SaveCA(store.CAFiles{CertPEM: string(certPEM), KeyPEM: store.NewSecret(string(keyPEM))}, nil, time.Time{}))
}

func rotationEvents(e *env) []string {
	var out []string
	for _, ev := range e.eng.Events(time.Time{}) {
		if ev.Kind == kindRotation {
			out = append(out, ev.Message)
		}
	}
	return out
}

// A node forgotten as compromised has every secret of the install rotated,
// step by step, in the order that keeps each new secret from the node: the
// admin's new credentials and Proxmox secret first, then every tunnel's
// secret, then, once every connector runs with the new ones, the CA, whose
// predecessor is taken for ten minutes more. The progress is in the store:
// a controller that takes over in the middle finishes it. A node that did
// not issue its certificate again is named afterwards.
func TestTheRotationOfACompromisedNodeRunsStepByStepAndOutlivesTheController(t *testing.T) {
	w := threeNodes(t)
	old := testCA(t)
	seedCA(t, w.env, old)
	require.NoError(t, w.store.SaveEnrolment("pve2", store.Enrolment{CertPEM: certUnder(t, old)}))
	require.NoError(t, w.store.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret("old-secret")}))
	reissued := 0
	w.reissue = func(context.Context) error { reissued++; return nil }
	w.settle()
	*w.listed = []members.Member{{Name: "pve1", ID: 1, Online: true}, {Name: "pve2", ID: 2, Online: true}}
	w.acts["pve3"].silent = true
	id := w.eng.State().Tunnels[0].ID

	_, err := w.eng.ForgetNode(t.Context(), "pve3", true)
	require.NoError(t, err)

	// The admin's steps come first; nothing else moves until they are done.
	st := w.tick(time.Second)
	require.True(t, hasProblem(st, fmt.Sprintf(problemRotationCredentials, "pve3")), st.Problems)
	require.Equal(t, stepCredentials, st.Cluster.CARotation.Next)
	require.Empty(t, st.Cluster.CARotation.Done)
	require.NotContains(t, w.cf.Calls(), "RotateTunnelSecret acc1 "+id)

	require.NoError(t, w.store.DeleteCredential(testCred))
	require.NoError(t, w.store.SaveCredential(store.Credential{ID: "new1", Label: "new", Kind: "scoped", Token: store.NewSecret("a-new-token-of-twenty-chars"), AddedAt: w.clock.now().Add(time.Second)}))
	st = w.tick(time.Second)
	require.True(t, hasProblem(st, fmt.Sprintf(problemRotationProxmox, "pve3")), st.Problems)
	require.Equal(t, []string{stepCredentials}, st.Cluster.CARotation.Done)

	// The tunnels are rotated once the Proxmox secret is new, and only by a
	// cycle that got through to Cloudflare whole: a held one may not know
	// every tunnel.
	require.NoError(t, w.store.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret("new-secret")}))
	whole := w.inv.snapshot()
	held := whole
	held.Complete = false
	w.inv.set(held)
	st = w.tick(time.Second)
	require.NotContains(t, w.cf.Calls(), "RotateTunnelSecret acc1 "+id, "a held cycle rotates nothing")
	require.Equal(t, []string{stepCredentials}, st.Cluster.CARotation.Done, "and moves no step on")
	w.inv.set(whole)

	// The CA waits for every node that runs a connector to run it ready
	// with the new secret: a replica that reports none, or one not ready,
	// holds it.
	w.acts["pve2"].edit = func(s *agentproto.Status) { s.Connectors = nil }
	st = w.tick(time.Second)
	require.Contains(t, w.cf.Calls(), "RotateTunnelSecret acc1 "+id)
	require.Equal(t, []string{stepCredentials, stepProxmox, stepTunnels}, st.Cluster.CARotation.Done)
	require.Equal(t, stepRollout, st.Cluster.CARotation.Next)
	require.True(t, hasProblem(st, "waiting for the connectors of pco-abc123 to run with the new secret"), st.Problems)
	w.acts["pve2"].edit = func(s *agentproto.Status) {
		for i := range s.Connectors {
			s.Connectors[i].Ready = false
		}
	}
	st = w.tick(time.Second)
	require.Equal(t, stepRollout, st.Cluster.CARotation.Next)
	require.True(t, hasProblem(st, "waiting for the connectors of pco-abc123 to run with the new secret"), st.Problems)
	_, prev, _, err := w.store.CA()
	require.NoError(t, err)
	require.Nil(t, prev, "the CA is not rotated yet")

	// A CA step that could not name the new CA in leader.json keeps the CA
	// it made: the next cycle names it, and makes no third one.
	w.acts["pve2"].edit = nil
	leaderFile := filepath.Join(w.paths.Cluster, "meta", "leader.json")
	leader, _, found, err := w.store.Leader()
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, os.Remove(leaderFile))
	st = w.tick(time.Second)
	require.Equal(t, stepCA, st.Cluster.CARotation.Next, st.Problems)
	require.True(t, hasProblem(st, "rotating the CA failed"), st.Problems)
	made, prev, _, err := w.store.CA()
	require.NoError(t, err)
	require.NotNil(t, prev, "the new CA is kept")
	require.NoError(t, w.store.SaveLeader(leader))
	st = w.tick(time.Second)
	require.Equal(t, []string{stepCredentials, stepProxmox, stepTunnels, stepRollout, stepCA}, st.Cluster.CARotation.Done, st.Problems)
	again, prev, _, err := w.store.CA()
	require.NoError(t, err)
	require.Equal(t, made.CertPEM, again.CertPEM, "no third CA")
	require.Equal(t, old.SHA256(), fingerprintOf(t, prev.CertPEM), "the old CA is the previous one")
	// Done by this controller: the record says so for the next one.
	rec, _, err := w.store.CARotation()
	require.NoError(t, err)
	require.Contains(t, rec.Done, stepCA)

	require.Equal(t, []string{
		"node pve3 was forgotten as compromised: every secret of the install is rotated, step by step",
		"node pve3 was forgotten as compromised: the Cloudflare credentials were replaced",
		"node pve3 was forgotten as compromised: the Proxmox token has a new secret",
		"the secret of tunnel pco-abc123 in account acc1 is rotated: every run token of it stops working",
		"node pve3 was forgotten as compromised: every tunnel of the install has a new secret",
		"node pve3 was forgotten as compromised: every connector runs with the new secrets",
		"node pve3 was forgotten as compromised: the CA of the install is rotated: the previous one is taken for 10m0s more",
	}, rotationEvents(w.env))

	// Another controller takes over in the middle and goes on from the record.
	w.eng = w.newEngine()
	w.watchHere(t)
	st = w.settle()
	require.Equal(t, []string{stepCredentials, stepProxmox, stepTunnels, stepRollout, stepCA}, st.Cluster.CARotation.Done, st.Problems)
	cur, prev, rotatedAt, err := w.store.CA()
	require.NoError(t, err)
	require.NotNil(t, prev, "the old CA is kept for the window")
	require.False(t, rotatedAt.IsZero())
	next, err := pki.ParseCA([]byte(cur.CertPEM), []byte(cur.KeyPEM.Reveal()))
	require.NoError(t, err)
	require.NotEqual(t, old.SHA256(), next.SHA256())
	leader, _, found, err = w.store.Leader()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, next.SHA256(), leader.CASHA256, "leader.json names the new CA")
	require.Equal(t, 1, reissued, "this node's certificates are issued again at once")
	require.Empty(t, st.Cluster.CARotation.FinishedAt)

	// Ten minutes later the old CA goes, and the node whose certificate is
	// still under it is named.
	st = w.tick(caRotationWindow)
	require.Equal(t, rotationSteps, st.Cluster.CARotation.Done)
	require.Empty(t, st.Cluster.CARotation.Next)
	require.False(t, st.Cluster.CARotation.FinishedAt.IsZero())
	_, prev, _, err = w.store.CA()
	require.NoError(t, err)
	require.Nil(t, prev)
	require.Equal(t, []string{"pve2"}, st.Cluster.CARotation.Refused)
	require.True(t, hasProblem(st, fmt.Sprintf(problemRotationRefused, "pve2")), st.Problems)
	require.Equal(t, []string{
		"node pve3 was forgotten as compromised: the rotation of every secret is finished: only certificates under the new CA are taken",
	}, rotationEvents(w.env))

	// Once the node issued its certificate again, nothing is refused.
	require.NoError(t, w.store.SaveEnrolment("pve2", store.Enrolment{CertPEM: certUnder(t, next)}))
	st = w.tick(time.Second)
	require.Empty(t, st.Cluster.CARotation.Refused)
}

// The tunnels step is done only by a cycle that read every account through
// to the end: one that held, or did not get to Cloudflare as the writer, or
// left an account unread, may have missed a tunnel.
func TestTheTunnelsStepNeedsACycleThatGotThroughWhole(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	for _, tt := range []struct {
		name string
		c    *cycleRun
		why  string
	}{
		{"held", &cycleRun{e: w.eng, cfHold: true, checked: true}, "not every tunnel is known"},
		{"not checked", &cycleRun{e: w.eng, checked: false}, "not every tunnel is known"},
		{"an account unread", &cycleRun{e: w.eng, checked: true, unreported: []TunnelView{{}}}, "1 tunnels are in accounts the tunnel run could not read"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := store.CARotation{Node: "pve3", StartedAt: t0}
			done, why := tt.c.rotateTunnels(&rec)
			require.False(t, done)
			require.Contains(t, why, tt.why)
			require.Empty(t, rec.Rotated)
		})
	}
}

// caFilesOf is a CA as the store keeps it.
func caFilesOf(t *testing.T, ca pki.CA) store.CAFiles {
	t.Helper()
	certPEM, keyPEM, err := ca.PEM()
	require.NoError(t, err)
	return store.CAFiles{CertPEM: string(certPEM), KeyPEM: store.NewSecret(string(keyPEM))}
}

// compromisedWorld is a cluster whose node pve3 was just forgotten as
// compromised, with a CA, an enrolment of pve2 under it and a Proxmox token.
func compromisedWorld(t *testing.T) (*world, pki.CA) {
	t.Helper()
	w := threeNodes(t)
	old := testCA(t)
	seedCA(t, w.env, old)
	require.NoError(t, w.store.SaveEnrolment("pve2", store.Enrolment{CertPEM: certUnder(t, old)}))
	require.NoError(t, w.store.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret("old-secret")}))
	w.settle()
	*w.listed = []members.Member{{Name: "pve1", ID: 1, Online: true}, {Name: "pve2", ID: 2, Online: true}}
	w.acts["pve3"].silent = true
	_, err := w.eng.ForgetNode(t.Context(), "pve3", true)
	require.NoError(t, err)
	return w, old
}

// restart is the daemon crashing and starting again: a new engine over the
// same store, with nothing remembered.
func (w *world) restart(t *testing.T) {
	t.Helper()
	w.eng = w.newEngine()
	w.watchHere(t)
}

func (w *world) rotationRecord(t *testing.T) store.CARotation {
	t.Helper()
	rec, found, err := w.store.CARotation()
	require.NoError(t, err)
	require.True(t, found)
	return rec
}

// A crash between any two steps, or inside one after its files were
// written and before the record was, changes nothing of the outcome: the
// next controller derives where the rotation is from the store, does again
// only what the files do not show, and never makes a second new CA.
func TestTheRotationGoesOnFromAnyStepACrashCutShort(t *testing.T) {
	w, old := compromisedWorld(t)
	id := w.eng.State().Tunnels[0].ID
	rotations := func() int {
		n := 0
		for _, call := range w.cf.Calls() {
			if call == "RotateTunnelSecret acc1 "+id {
				n++
			}
		}
		return n
	}

	// Before the first step.
	w.restart(t)
	require.NoError(t, w.store.DeleteCredential(testCred))
	require.NoError(t, w.store.SaveCredential(store.Credential{ID: "new1", Label: "new", Kind: "scoped", Token: store.NewSecret("a-new-token-of-twenty-chars"), AddedAt: w.clock.now().Add(time.Second)}))
	st := w.tick(time.Second)
	require.Equal(t, []string{stepCredentials}, st.Cluster.CARotation.Done)

	// Between credentials and proxmox, and a crash after the tunnel's
	// secret was rotated and before the record said so.
	w.restart(t)
	require.NoError(t, w.store.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret("new-secret")}))
	w.acts["pve2"].edit = func(s *agentproto.Status) { s.Connectors = nil }
	st = w.tick(time.Second)
	require.Equal(t, []string{stepCredentials, stepProxmox, stepTunnels}, st.Cluster.CARotation.Done)
	require.Equal(t, 1, rotations())
	rec := w.rotationRecord(t)
	rec.Rotated = nil
	require.NoError(t, w.store.SaveCARotation(rec))
	w.restart(t)
	st = w.tick(time.Second)
	require.Equal(t, 2, rotations(), "a tunnel the record does not show rotated is rotated again")
	require.Equal(t, []string{id}, w.rotationRecord(t).Rotated)
	require.Equal(t, stepRollout, st.Cluster.CARotation.Next)

	// Between rollout and the CA, then a crash after the CA was made and
	// named and before the record said so.
	w.restart(t)
	w.acts["pve2"].edit = nil
	st = w.settle()
	require.Equal(t, []string{stepCredentials, stepProxmox, stepTunnels, stepRollout, stepCA}, st.Cluster.CARotation.Done)
	made, _, rotatedAt, err := w.store.CA()
	require.NoError(t, err)
	rec = w.rotationRecord(t)
	rec.Done = rec.Done[:4]
	require.NoError(t, w.store.SaveCARotation(rec))
	w.restart(t)
	st = w.tick(time.Second)
	require.Equal(t, []string{stepCredentials, stepProxmox, stepTunnels, stepRollout, stepCA}, st.Cluster.CARotation.Done)
	again, prev, rotatedAgain, err := w.store.CA()
	require.NoError(t, err)
	require.Equal(t, made.CertPEM, again.CertPEM, "no second new CA")
	require.Equal(t, rotatedAt, rotatedAgain)
	require.Equal(t, old.SHA256(), fingerprintOf(t, prev.CertPEM))

	// Between the CA and the end of the window, then a crash after the
	// previous CA went and before the record was finished.
	w.restart(t)
	st = w.tick(caRotationWindow)
	require.False(t, st.Cluster.CARotation.FinishedAt.IsZero())
	rec = w.rotationRecord(t)
	rec.Done, rec.FinishedAt = rec.Done[:5], time.Time{}
	require.NoError(t, w.store.SaveCARotation(rec))
	w.restart(t)
	st = w.tick(time.Second)
	require.Equal(t, rotationSteps, st.Cluster.CARotation.Done)
	require.False(t, st.Cluster.CARotation.FinishedAt.IsZero())
	_, prev, _, err = w.store.CA()
	require.NoError(t, err)
	require.Nil(t, prev)

	// Finished, with a previous CA a crash left behind: it goes.
	require.NoError(t, w.store.SaveCA(again, &store.CAFiles{CertPEM: caFilesOf(t, old).CertPEM, KeyPEM: caFilesOf(t, old).KeyPEM}, rotatedAt))
	w.restart(t)
	st = w.tick(time.Second)
	_, prev, _, err = w.store.CA()
	require.NoError(t, err)
	require.Nil(t, prev)
	require.False(t, hasProblem(st, "disagree"), st.Problems)
}

// A record that says more than the store shows, or less, stops the
// rotation where it is, with an event, until the two agree again.
func TestARotationWhoseRecordAndCADisagreeStops(t *testing.T) {
	w, old := compromisedWorld(t)
	require.NoError(t, w.store.DeleteCredential(testCred))
	require.NoError(t, w.store.SaveCredential(store.Credential{ID: "new1", Label: "new", Kind: "scoped", Token: store.NewSecret("a-new-token-of-twenty-chars"), AddedAt: w.clock.now().Add(time.Second)}))
	require.NoError(t, w.store.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret("new-secret")}))
	st := w.settle()
	require.Equal(t, []string{stepCredentials, stepProxmox, stepTunnels, stepRollout, stepCA}, st.Cluster.CARotation.Done)
	made, _, rotatedAt, err := w.store.CA()
	require.NoError(t, err)
	oldFiles := caFilesOf(t, old)
	drift := func() []string {
		var out []string
		for _, ev := range w.eng.Events(time.Time{}) {
			if ev.Kind == kindRotation && ev.Level == levelError {
				out = append(out, ev.Message)
			}
		}
		return out
	}

	// The store goes back to the old CA, as a restore from a copy of before
	// the rotation does: the record says the CA was rotated, the store not.
	require.NoError(t, w.store.SaveCA(oldFiles, nil, time.Time{}))
	st = w.tick(caRotationWindow)
	require.True(t, hasProblem(st, "the record says the CA was rotated, but the store's CA was not since the rotation began"), st.Problems)
	require.Equal(t, stepPrevious, st.Cluster.CARotation.Next, "no step ran")
	require.True(t, st.Cluster.CARotation.FinishedAt.IsZero())
	st = w.tick(time.Second)
	require.Len(t, drift(), 1, "said once")
	require.True(t, drift()[0] != "" && st.Problems != nil)

	// leader.json names a CA the store does not hold.
	require.NoError(t, w.store.SaveCA(made, &oldFiles, rotatedAt))
	leader, _, _, err := w.store.Leader()
	require.NoError(t, err)
	named := leader.CASHA256
	leader.CASHA256 = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	require.NoError(t, w.store.SaveLeader(leader))
	st = w.tick(time.Second)
	require.True(t, hasProblem(st, "leader.json names CA 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"), st.Problems)
	require.Len(t, drift(), 2)

	// Once they agree again the rotation goes on from where the files show.
	leader.CASHA256 = named
	require.NoError(t, w.store.SaveLeader(leader))
	st = w.tick(time.Second)
	require.False(t, hasProblem(st, "disagree"), st.Problems)
	require.False(t, st.Cluster.CARotation.FinishedAt.IsZero())
	require.Len(t, drift(), 2)
}

// A node forgotten while a rotation runs on a store the previous build
// wrote nothing of: a single node has no rotation, and its state shows none.
func TestASingleNodeShowsNoRotation(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	st := e.cycle()
	require.Nil(t, st.Cluster)
}
