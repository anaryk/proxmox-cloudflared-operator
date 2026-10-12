package clusterapi

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	base := t.TempDir()
	s, err := store.Open(store.Paths{
		Cluster: filepath.Join(base, "cluster"), Private: filepath.Join(base, "private"), Local: filepath.Join(base, "local"),
	})
	require.NoError(t, err)
	require.NoError(t, s.Init())
	return s
}

func applianceID(node string, vmid int) pki.Identity {
	return pki.Identity{Kind: pki.KindAppliance, InstallID: testInstall, Node: node, VMID: vmid}
}

// joinSetup is a store with one join token of testNode, and a controller
// whose join records the use and issues on the first.
type joinSetup struct {
	st  *store.Store
	tok pki.JoinToken
	csr []byte
}

func (h *harness) joinSetup(t *testing.T) *joinSetup {
	t.Helper()
	st := openStore(t)
	tok, err := pki.NewJoinToken(testInstall, testNode, []string{h.addr.String()}, h.ca, h.clock.now(), rand.Reader)
	require.NoError(t, err)
	require.NoError(t, st.SaveJoinToken(store.JoinRecord{
		ID: "tok1", Node: testNode, SecretSHA256: tok.SecretHash(), CreatedAt: h.clock.now(), Expires: tok.Expires,
	}))
	h.ctrl.setJoin(func(tok pki.JoinToken, csr []byte, src netip.Addr, facts JoinFacts) ([]byte, error) {
		rec, err := UseToken(st, tok, store.JoinUse{At: h.clock.now(), Source: src, VMID: facts.VMID, MACs: facts.MACs})
		if err != nil {
			return nil, err
		}
		if len(rec.Uses) > 1 {
			return nil, errors.New("contested")
		}
		cert, err := h.ca.IssueClient(csr, applianceID(facts.Node, facts.VMID), h.clock.now(), rand.Reader)
		if err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), nil
	})
	key, err := pki.NewKey(rand.Reader)
	require.NoError(t, err)
	csr, err := pki.NewCSR(key, applianceID(testNode, 200), rand.Reader)
	require.NoError(t, err)
	return &joinSetup{st: st, tok: tok, csr: csr}
}

func (j *joinSetup) request(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(JoinRequest{
		Token: j.tok.Encode(), CSR: j.csr, Node: testNode, VMID: 200, MACs: []string{"bc:24:11:00:00:aa"},
		Build: "dev", Protocol: agentproto.Range{Min: 1, Max: 1},
	})
	require.NoError(t, err)
	return body
}

func TestAnUnusedJoinTokenIsAcceptedOnce(t *testing.T) {
	h := newHarness(t)
	j := h.joinSetup(t)
	c := h.bare()

	resp := h.post(c, "/v1/agent/join", j.request(t))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var answer CertificateResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&answer))
	block, _ := pem.Decode([]byte(answer.CertPEM))
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	id, err := pki.IdentityOf(cert)
	require.NoError(t, err)
	require.Equal(t, applianceID(testNode, 200), id)
	caBlock, _ := pem.Decode([]byte(answer.CAPEM))
	require.NotNil(t, caBlock)
	require.Equal(t, h.ca.Cert.Raw, caBlock.Bytes, "the CA the agent pinned comes with the certificate")

	rec, _, found, err := j.st.JoinToken("tok1")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, rec.Uses, 1)
	require.Equal(t, loopback, rec.Uses[0].Source)
	require.Equal(t, 200, rec.Uses[0].VMID)

	t.Run("a second use is refused and recorded", func(t *testing.T) {
		resp := h.post(c, "/v1/agent/join", j.request(t))
		r := refusalOf(t, resp)
		require.Equal(t, RefusalRefused, r.Code)
		require.Equal(t, "refused", r.Message, "a bad token learns nothing")
		rec, _, _, err := j.st.JoinToken("tok1")
		require.NoError(t, err)
		require.Len(t, rec.Uses, 2)
	})
	t.Run("a certificate that is sent is ignored", func(t *testing.T) {
		resp := h.post(h.client(h.ca, hostID("pve3")), "/v1/agent/join", j.request(t))
		require.Equal(t, http.StatusForbidden, resp.StatusCode, "refused for the token, not for the certificate")
	})
}

func TestAJoinForAnotherNodeOrAnExpiredTokenIsRefused(t *testing.T) {
	h := newHarness(t)
	j := h.joinSetup(t)
	c := h.bare()

	var req JoinRequest
	require.NoError(t, json.Unmarshal(j.request(t), &req))
	req.Node = "pve3"
	body, err := json.Marshal(req)
	require.NoError(t, err)
	resp := h.post(c, "/v1/agent/join", body)
	require.Equal(t, RefusalRefused, refusalOf(t, resp).Code)

	t.Run("expired", func(t *testing.T) {
		h.clock.advance(pki.JoinTokenTTL + time.Second)
		resp := h.post(c, "/v1/agent/join", j.request(t))
		require.Equal(t, RefusalRefused, refusalOf(t, resp).Code)
		h.clock.advance(-pki.JoinTokenTTL - time.Second)
	})
	t.Run("malformed token", func(t *testing.T) {
		resp := h.post(c, "/v1/agent/join", []byte(`{"token":"pco9.xx","csr":"AA=="}`))
		require.Equal(t, RefusalRefused, refusalOf(t, resp).Code)
	})
	t.Run("another install", func(t *testing.T) {
		other, err := pki.NewCA("fedcba987654", t0, rand.Reader)
		require.NoError(t, err)
		tok, err := pki.NewJoinToken("fedcba987654", testNode, []string{h.addr.String()}, other, h.clock.now(), rand.Reader)
		require.NoError(t, err)
		req.Node = testNode
		req.Token = tok.Encode()
		body, err := json.Marshal(req)
		require.NoError(t, err)
		resp := h.post(c, "/v1/agent/join", body)
		require.Equal(t, RefusalRefused, refusalOf(t, resp).Code)
	})
	t.Run("a body too large", func(t *testing.T) {
		resp := h.post(c, "/v1/agent/join", make([]byte, maxJoinBody+1))
		require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	})
	t.Run("a source not allowed to join", func(t *testing.T) {
		h.auth.allowJoin(loopback, false)
		resp := h.post(c, "/v1/agent/join", j.request(t))
		require.Equal(t, RefusalRefused, refusalOf(t, resp).Code)
	})
	rec, _, _, err := j.st.JoinToken("tok1")
	require.NoError(t, err)
	require.Empty(t, rec.Uses, "no refusal of the endpoint itself reached the token")
}

func TestTheJoinIsRateLimitedPerSource(t *testing.T) {
	h := newHarness(t)
	h.joinSetup(t)
	c := h.bare()
	bad := []byte(`{"token":"nothing"}`)
	for i := range joinPerMinute {
		resp := h.post(c, "/v1/agent/join", bad)
		require.Equal(t, http.StatusForbidden, resp.StatusCode, "attempt %d", i)
	}
	resp := h.post(c, "/v1/agent/join", bad)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.NotEmpty(t, resp.Header.Get("Retry-After"))

	h.clock.advance(time.Minute)
	resp = h.post(c, "/v1/agent/join", bad)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestUseTokenFindsTheTokenByItsSecret(t *testing.T) {
	st := openStore(t)
	ca, err := pki.NewCA(testInstall, t0, rand.Reader)
	require.NoError(t, err)
	tok1, err := pki.NewJoinToken(testInstall, testNode, []string{"10.0.0.1:8644"}, ca, t0, rand.Reader)
	require.NoError(t, err)
	tok2, err := pki.NewJoinToken(testInstall, testNode, []string{"10.0.0.1:8644"}, ca, t0, rand.Reader)
	require.NoError(t, err)
	for i, tok := range []pki.JoinToken{tok1, tok2} {
		require.NoError(t, st.SaveJoinToken(store.JoinRecord{
			ID: "tok" + string(rune('1'+i)), Node: testNode, SecretSHA256: tok.SecretHash(), CreatedAt: t0, Expires: tok.Expires,
		}))
	}
	use := store.JoinUse{At: t0, Source: loopback, VMID: 200}
	rec, err := UseToken(st, tok2, use)
	require.NoError(t, err)
	require.Equal(t, "tok2", rec.ID)
	require.Len(t, rec.Uses, 1)

	rec, err = UseToken(st, tok2, use)
	require.NoError(t, err)
	require.Len(t, rec.Uses, 2, "the second use is recorded, for the controller to judge")

	rec, err = UseToken(st, tok1, use)
	require.NoError(t, err)
	require.Equal(t, "tok1", rec.ID)
	require.Len(t, rec.Uses, 1)

	t.Run("a record of another node with the same secret is left alone", func(t *testing.T) {
		require.NoError(t, st.SaveJoinToken(store.JoinRecord{
			ID: "tok3", Node: "pve3", SecretSHA256: tok1.SecretHash(), CreatedAt: t0, Expires: tok1.Expires,
		}))
		rec, err := UseToken(st, tok1, use)
		require.NoError(t, err)
		require.Equal(t, "tok1", rec.ID)
		other, _, _, err := st.JoinToken("tok3")
		require.NoError(t, err)
		require.Empty(t, other.Uses)
	})
	t.Run("unknown secret", func(t *testing.T) {
		tok, err := pki.NewJoinToken(testInstall, testNode, []string{"10.0.0.1:8644"}, ca, t0, rand.Reader)
		require.NoError(t, err)
		_, err = UseToken(st, tok, use)
		require.ErrorIs(t, err, store.ErrNoJoinToken)
		require.NotContains(t, err.Error(), tok.SecretHash())
	})
}
