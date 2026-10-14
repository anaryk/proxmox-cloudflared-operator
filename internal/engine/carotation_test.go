package engine

import (
	"context"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// certUnder is a client certificate of a host node under ca.
func certUnder(t *testing.T, ca pki.CA, node string) string {
	t.Helper()
	key, err := pki.NewKey(rand.Reader)
	require.NoError(t, err)
	id := pki.Identity{Kind: pki.KindHost, InstallID: pkiInstall, Node: node}
	csr, err := pki.NewCSR(key, id, rand.Reader)
	require.NoError(t, err)
	cert, err := ca.IssueClient(csr, id, t0, rand.Reader)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
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
	require.NoError(t, w.store.SaveEnrolment("pve2", store.Enrolment{CertPEM: certUnder(t, old, "pve2")}))
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

	// The tunnels are rotated once the Proxmox secret is new; the CA waits
	// for every connector to run with the new secrets.
	require.NoError(t, w.store.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret("new-secret")}))
	w.acts["pve2"].edit = func(s *agentproto.Status) {
		for i := range s.Connectors {
			s.Connectors[i].Ready = false
		}
	}
	st = w.tick(time.Second)
	require.Contains(t, w.cf.Calls(), "RotateTunnelSecret acc1 "+id)
	require.Equal(t, []string{stepCredentials, stepProxmox, stepTunnels}, st.Cluster.CARotation.Done)
	require.Equal(t, stepRollout, st.Cluster.CARotation.Next)
	require.True(t, hasProblem(st, "waiting for the connectors of pco-abc123 to run with the new secret"), st.Problems)
	_, prev, _, err := w.store.CA()
	require.NoError(t, err)
	require.Nil(t, prev, "the CA is not rotated yet")

	require.Equal(t, []string{
		"node pve3 was forgotten as compromised: every secret of the install is rotated, step by step",
		"node pve3 was forgotten as compromised: the Cloudflare credentials were replaced",
		"node pve3 was forgotten as compromised: the Proxmox token has a new secret",
		"the secret of tunnel pco-abc123 in account acc1 is rotated: every run token of it stops working",
		"node pve3 was forgotten as compromised: every tunnel of the install has a new secret",
	}, rotationEvents(w.env))

	// Another controller takes over in the middle and goes on from the record.
	w.eng = w.newEngine()
	w.watchHere(t)
	w.acts["pve2"].edit = nil
	st = w.settle()
	require.Equal(t, []string{stepCredentials, stepProxmox, stepTunnels, stepRollout, stepCA}, st.Cluster.CARotation.Done, st.Problems)
	cur, prev, rotatedAt, err := w.store.CA()
	require.NoError(t, err)
	require.NotNil(t, prev, "the old CA is kept for the window")
	require.False(t, rotatedAt.IsZero())
	next, err := pki.ParseCA([]byte(cur.CertPEM), []byte(cur.KeyPEM.Reveal()))
	require.NoError(t, err)
	require.NotEqual(t, old.SHA256(), next.SHA256())
	leader, _, found, err := w.store.Leader()
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
		"node pve3 was forgotten as compromised: every connector runs with the new secrets",
		"node pve3 was forgotten as compromised: the CA of the install is rotated: the previous one is taken for 10m0s more",
		"node pve3 was forgotten as compromised: the rotation of every secret is finished: only certificates under the new CA are taken",
	}, rotationEvents(w.env))

	// Once the node issued its certificate again, nothing is refused.
	require.NoError(t, w.store.SaveEnrolment("pve2", store.Enrolment{CertPEM: certUnder(t, next, "pve2")}))
	st = w.tick(time.Second)
	require.Empty(t, st.Cluster.CARotation.Refused)
}

// A node forgotten while a rotation runs on a store the previous build
// wrote nothing of: a single node has no rotation, and its state shows none.
func TestASingleNodeShowsNoRotation(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	st := e.cycle()
	require.Nil(t, st.Cluster)
}
