package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// A join token names the controller's endpoints and the CA, carries a secret
// the store keeps only the hash of, and is good once; it is in the answer
// and nowhere else.
func TestAJoinTokenIsMadeOnceAndItsSecretIsNotKept(t *testing.T) {
	w := threeNodes(t)
	require.NoError(t, w.store.SaveInstall(store.Install{ID: pkiInstall, CreatedAt: t0}))
	ca := testCA(t)
	seedCA(t, w.env, ca)
	w.endpoints = func() []string { return []string{"10.0.0.2:8644", "10.0.0.3:8644"} }
	w.settle()

	v, err := w.eng.NewJoinToken(t.Context(), "app1")

	require.NoError(t, err)
	require.Equal(t, "app1", v.Node)
	require.Equal(t, w.clock.now().Add(pki.JoinTokenTTL), v.Expires)
	tok, err := pki.ParseJoinToken(v.Token)
	require.NoError(t, err)
	require.Equal(t, []string{"10.0.0.2:8644", "10.0.0.3:8644"}, tok.Endpoints)
	require.Equal(t, ca.SHA256(), tok.CASHA256)
	tokens, err := w.store.JoinTokens()
	require.NoError(t, err)
	require.Len(t, tokens, 1)
	rec := tokens[v.ID]
	require.Equal(t, "app1", rec.Node)
	require.True(t, tok.Matches(rec.SecretSHA256))
	for _, ev := range w.eng.Events(time.Time{}) {
		require.NotContains(t, ev.Message, v.Token, "the token is in no event")
		require.NotContains(t, ev.Message, strings.TrimPrefix(v.Token, pki.JoinPrefix)[:16])
	}
	require.Contains(t, adminEvents(w.env, time.Time{}), "app1: a join token ("+v.ID+") was made for node app1; it expires at 2026-10-01T12:15:03Z and is good once")

	again, err := w.eng.NewJoinToken(t.Context(), "app1")
	require.NoError(t, err)
	require.NotEqual(t, v.Token, again.Token, "each token is its own")
}

// A node that is in the install already gets no join token: one would enrol
// a second agent under its name.
func TestAJoinTokenIsRefusedForANodeOfTheInstall(t *testing.T) {
	w := threeNodes(t)
	require.NoError(t, w.store.SaveInstall(store.Install{ID: pkiInstall, CreatedAt: t0}))
	seedCA(t, w.env, testCA(t))
	w.endpoints = func() []string { return []string{"10.0.0.2:8644"} }
	w.settle()
	for _, state := range []string{store.NodeActive, store.NodeJoining, store.NodeDraining} {
		t.Run(state, func(t *testing.T) {
			require.NoError(t, w.store.SaveNode(store.NodeEntry{Name: "app1", Since: t0, State: state, StateSince: t0, Kind: "appliance"}))
			_, err := w.eng.NewJoinToken(t.Context(), "app1")
			require.ErrorIs(t, err, ErrRefused)
			require.ErrorContains(t, err, "node app1 is "+state)
		})
	}
	require.NoError(t, w.store.SaveNode(store.NodeEntry{Name: "app1", Since: t0, State: store.NodeRemoved, StateSince: t0, Kind: "appliance"}))
	_, err := w.eng.NewJoinToken(t.Context(), "app1")
	require.NoError(t, err, "a removed node is enrolled again with a new token")
}

// The records of tokens that expired go: when a token is made, and in the
// cycles.
func TestExpiredJoinTokensAreDeleted(t *testing.T) {
	w := threeNodes(t)
	require.NoError(t, w.store.SaveInstall(store.Install{ID: pkiInstall, CreatedAt: t0}))
	seedCA(t, w.env, testCA(t))
	w.endpoints = func() []string { return []string{"10.0.0.2:8644"} }
	w.settle()
	old, err := w.eng.NewJoinToken(t.Context(), "app1")
	require.NoError(t, err)

	w.clock.advance(pki.JoinTokenTTL + time.Second)
	fresh, err := w.eng.NewJoinToken(t.Context(), "app2")
	require.NoError(t, err)
	tokens, err := w.store.JoinTokens()
	require.NoError(t, err)
	require.NotContains(t, tokens, old.ID)
	require.Contains(t, tokens, fresh.ID)

	w.tick(pki.JoinTokenTTL + time.Second)
	tokens, err = w.store.JoinTokens()
	require.NoError(t, err)
	require.Empty(t, tokens)
}

// No token for a contested node, for an install without a CA, or for a node
// that cannot be named.
func TestAJoinTokenIsRefusedWhereItWouldNotDo(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	_, err := w.eng.NewJoinToken(t.Context(), "app1")
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "no CA yet")

	require.NoError(t, w.store.SaveInstall(store.Install{ID: pkiInstall, CreatedAt: t0}))
	seedCA(t, w.env, testCA(t))
	require.NoError(t, w.store.SaveNode(store.NodeEntry{Name: "app2", Since: t0, State: store.NodeContested, StateSince: t0, Kind: "appliance"}))
	_, err = w.eng.NewJoinToken(t.Context(), "app2")
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "contested")

	_, err = w.eng.NewJoinToken(t.Context(), "not a node")
	require.ErrorIs(t, err, ErrInvalid)

	w.endpoints = func() []string { return nil }
	_, err = w.eng.NewJoinToken(t.Context(), "app1")
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "no agent endpoint")
}

// A combined appliance takes no agents: it has no endpoint, and says how to
// make it a controller.
func TestACombinedApplianceMakesNoJoinToken(t *testing.T) {
	w := threeNodes(t)
	require.NoError(t, w.store.SaveInstall(store.Install{ID: pkiInstall, CreatedAt: t0, Profile: store.ProfileAppliance, Appliance: &store.ApplianceInstall{
		VMID: 9250, Node: testNode, MACs: []string{"bc:24:11:00:92:50"}, Endpoints: []store.Endpoint{{Address: "10.92.0.1:8006", ServerName: testNode}},
	}}))
	seedCA(t, w.env, testCA(t))
	w.endpoints = func() []string { return nil }
	w.settle()

	_, err := w.eng.NewJoinToken(t.Context(), "app1")

	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "pco appliance convert --vmid 9250")
}
