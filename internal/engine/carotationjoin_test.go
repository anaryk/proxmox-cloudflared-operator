package engine

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Every secret a second compromised node could read while it was a member
// ends rotated after it joins the rotation, with the controller crashing
// before every cycle of the way: the credentials, the Proxmox secret, the
// secret of every tunnel, the CA and the certificates under it, and the
// join tokens.
func TestEverySecretTheJoiningNodeSawEndsRotated(t *testing.T) {
	w, _ := compromisedWorld(t)
	reissued := 0
	w.reissue = func(context.Context) error { reissued++; return nil }
	require.NoError(t, w.store.DeleteCredential(testCred))
	require.NoError(t, w.store.SaveCredential(store.Credential{ID: "new1", Label: "new", Kind: "scoped", Token: store.NewSecret("a-new-token-of-twenty-chars"), AddedAt: w.clock.now().Add(time.Second)}))
	require.NoError(t, w.store.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret("new-secret")}))
	st := w.settle()
	require.Equal(t, stepPrevious, st.Cluster.CARotation.Next, st.Problems)

	// What the joining node could read as a member, at the time it leaves.
	var tunnels []string
	for _, tv := range w.eng.State().Tunnels {
		if tv.Exists && tv.ID != "" {
			tunnels = append(tunnels, tv.ID)
		}
	}
	require.NotEmpty(t, tunnels)
	rotationsAtJoin := map[string]int{}
	for _, id := range tunnels {
		rotationsAtJoin[id] = count(w.cf.Calls(), "RotateTunnelSecret acc1 "+id)
	}
	caAtJoin, _, _, err := w.store.CA()
	require.NoError(t, err)
	tokAtJoin, _, err := w.store.PVEToken()
	require.NoError(t, err)
	require.NoError(t, w.store.SaveJoinToken(store.JoinRecord{ID: "j1", Node: "app9", SecretSHA256: secretHash("j1"),
		CreatedAt: w.clock.now(), Expires: w.clock.now().Add(time.Hour)}))
	reissuedAtJoin := reissued
	*w.listed = []members.Member{{Name: "pve1", ID: 1, Online: true}}
	w.acts["pve2"].silent = true
	w.clock.advance(time.Second)
	joined := w.clock.now()
	_, err = w.eng.ForgetNode(t.Context(), "pve2", true)
	require.NoError(t, err)

	// The admin's part, and the rest, a crash before every cycle.
	require.NoError(t, w.store.DeleteCredential("new1"))
	require.NoError(t, w.store.SaveCredential(store.Credential{ID: "new2", Label: "newer", Kind: "scoped", Token: store.NewSecret("a-newer-token-of-twenty"), AddedAt: joined.Add(time.Second)}))
	require.NoError(t, w.store.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret("newer-secret")}))
	for range 40 {
		w.restart(t)
		st = w.tick(30 * time.Second)
		require.False(t, hasProblem(st, "disagree"), st.Problems)
		if v := st.Cluster.CARotation; v != nil && !v.FinishedAt.IsZero() {
			break
		}
	}
	require.False(t, st.Cluster.CARotation.FinishedAt.IsZero(), st.Problems)
	rec := w.rotationRecord(t)
	for _, step := range rotationSteps {
		require.True(t, rec.StepsAt[step].After(joined), "%s ran after the join", step)
	}

	cur, prev, _, err := w.store.CA()
	require.NoError(t, err)
	tok, _, err := w.store.PVEToken()
	require.NoError(t, err)
	creds, err := w.store.Credentials()
	require.NoError(t, err)
	joins, err := w.store.JoinTokens()
	require.NoError(t, err)
	enrolled, found, err := w.store.Enrolment("pve2")
	require.NoError(t, err)
	for _, tt := range []struct {
		secret  string
		rotated bool
	}{
		{"the Cloudflare credentials", len(creds) == 1 && creds[0].AddedAt.After(joined)},
		{"the Proxmox secret", tok.Secret.Reveal() != tokAtJoin.Secret.Reveal()},
		{"the CA", cur.CertPEM != caAtJoin.CertPEM && prev == nil},
		{"this node's certificates", reissued > reissuedAtJoin},
		{"the joining node's certificate", !found || !underCA(t, enrolled.CertPEM, cur.CertPEM)},
		{"the join tokens", len(joins) == 0},
	} {
		require.True(t, tt.rotated, tt.secret)
	}
	for _, id := range tunnels {
		require.Greater(t, count(w.cf.Calls(), "RotateTunnelSecret acc1 "+id), rotationsAtJoin[id], "the secret of tunnel %s", id)
	}
	require.True(t, rec.At.After(joined), "the tunnels' secrets were given after the join")
}

// A record that holds steps done before the last node joined, as one a
// join could not clear, has them done again: none counts.
func TestStepsThatRanBeforeTheLastJoinAreDoneAgain(t *testing.T) {
	w, _ := compromisedWorld(t)
	require.NoError(t, w.store.DeleteCredential(testCred))
	require.NoError(t, w.store.SaveCredential(store.Credential{ID: "new1", Label: "new", Kind: "scoped", Token: store.NewSecret("a-new-token-of-twenty-chars"), AddedAt: w.clock.now().Add(time.Second)}))
	require.NoError(t, w.store.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret("new-secret")}))
	st := w.settle()
	require.Equal(t, stepPrevious, st.Cluster.CARotation.Next, st.Problems)
	rec := w.rotationRecord(t)
	w.clock.advance(time.Second)
	rec.Since = w.clock.now()
	require.NoError(t, w.store.SaveCARotation(rec))
	w.restart(t)

	st = w.tick(time.Second)

	require.Equal(t, stepCredentials, st.Cluster.CARotation.Next, "the credentials added before count for nothing")
	require.Empty(t, w.rotationRecord(t).Rotated)
}

// A controller whose clock is behind the one that recorded the last join
// stamps what it does no earlier than the join: its own steps, the secrets
// it gives and the CA it makes never look as if they ran before, and none
// is done a second time. A rollout counts from the secret's time, so it
// waits for the clock to pass the join.
func TestAClockBehindTheJoinDoesNoStepTwice(t *testing.T) {
	w, _ := compromisedWorld(t)
	id := w.tunnelID(t, tunnelName)
	rec := w.rotationRecord(t)
	rec.Since = w.clock.now().Add(10 * time.Minute)
	require.NoError(t, w.store.SaveCARotation(rec))
	w.restart(t)
	require.NoError(t, w.store.DeleteCredential(testCred))
	require.NoError(t, w.store.SaveCredential(store.Credential{ID: "new1", Label: "new", Kind: "scoped", Token: store.NewSecret("a-new-token-of-twenty-chars"), AddedAt: rec.Since.Add(time.Second)}))
	require.NoError(t, w.store.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret("new-secret")}))

	var st State
	for range 3 {
		st = w.settle()
	}
	require.Equal(t, stepRollout, st.Cluster.CARotation.Next, st.Problems)
	w.clock.advance(10 * time.Minute)
	for range 3 {
		st = w.settle()
	}

	require.Equal(t, stepPrevious, st.Cluster.CARotation.Next, st.Problems)
	require.Equal(t, 1, count(w.cf.Calls(), "RotateTunnelSecret acc1 "+id), "the secret is given once")
	rec = w.rotationRecord(t)
	for _, step := range rec.Done {
		require.False(t, rec.StepsAt[step].Before(rec.From()), "%s is stamped no earlier than the join", step)
	}
	require.False(t, rec.At.Before(rec.From()))
	_, _, rotatedAt, err := w.store.CA()
	require.NoError(t, err)
	require.False(t, rotatedAt.Before(rec.From()), "the CA counts as made in this rotation")
}

// A node that joins has every step done so far done again, also those a
// clock ahead of this one stamped later than now, and the join counts from
// after them.
func TestAJoinRedoesStepsStampedByAClockAhead(t *testing.T) {
	w, _ := compromisedWorld(t)
	require.NoError(t, w.store.DeleteCredential(testCred))
	require.NoError(t, w.store.SaveCredential(store.Credential{ID: "new1", Label: "new", Kind: "scoped", Token: store.NewSecret("a-new-token-of-twenty-chars"), AddedAt: w.clock.now().Add(time.Second)}))
	require.NoError(t, w.store.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret("new-secret")}))
	st := w.settle()
	require.Equal(t, stepPrevious, st.Cluster.CARotation.Next, st.Problems)
	rec := w.rotationRecord(t)
	ahead := 10 * time.Minute
	for step, at := range rec.StepsAt {
		rec.StepsAt[step] = at.Add(ahead)
	}
	rec.At = rec.At.Add(ahead)
	require.NoError(t, w.store.SaveCARotation(rec))
	latest := rec.At

	*w.listed = []members.Member{{Name: "pve1", ID: 1, Online: true}}
	w.acts["pve2"].silent = true
	w.clock.advance(time.Second)
	_, err := w.eng.ForgetNode(t.Context(), "pve2", true)
	require.NoError(t, err)

	rec = w.rotationRecord(t)
	require.Empty(t, rec.Done, "every step is done again")
	require.Empty(t, rec.Rotated)
	require.True(t, rec.Since.After(latest), "the join counts from after what ran before it")
}

// underCA says whether a certificate is signed by a CA, both in PEM.
func underCA(t *testing.T, certPEM, caPEM string) bool {
	t.Helper()
	cert, err := parseCert(certPEM)
	require.NoError(t, err)
	b, _ := pem.Decode([]byte(caPEM))
	require.NotNil(t, b)
	ca, err := x509.ParseCertificate(b.Bytes)
	require.NoError(t, err)
	return cert.CheckSignatureFrom(ca) == nil
}

// The tunnel pinned to a node that stays is rotated with the shared one:
// the compromised node could read its secret too.
func TestARotationGivesThePinnedTunnelsOfOtherNodesNewSecrets(t *testing.T) {
	w, pinned2 := pinnedDB(t)
	pinnedID := w.tunnelID(t, pinned2)
	sharedID := w.tunnelID(t, tunnelName)
	seedCA(t, w.env, testCA(t))
	require.NoError(t, w.store.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret("old-secret")}))
	*w.listed = []members.Member{{Name: "pve1", ID: 1, Online: true}, {Name: "pve2", ID: 2, Online: true}}
	w.acts["pve3"].silent = true
	_, err := w.eng.ForgetNode(t.Context(), "pve3", true)
	require.NoError(t, err)
	require.NoError(t, w.store.DeleteCredential(testCred))
	require.NoError(t, w.store.SaveCredential(store.Credential{ID: "new1", Label: "new", Kind: "scoped", Token: store.NewSecret("a-new-token-of-twenty-chars"), AddedAt: w.clock.now().Add(time.Second)}))
	require.NoError(t, w.store.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret("new-secret")}))

	var st State
	for range 3 {
		st = w.tick(time.Minute)
	}

	require.Contains(t, w.cf.Calls(), "RotateTunnelSecret acc1 "+pinnedID, st.Problems)
	require.Contains(t, w.cf.Calls(), "RotateTunnelSecret acc1 "+sharedID)
}
