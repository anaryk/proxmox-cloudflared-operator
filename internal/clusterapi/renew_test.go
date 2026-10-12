package clusterapi

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
)

func TestARenewalNeedsANewKeyOfTheSameIdentity(t *testing.T) {
	h := newHarness(t)
	h.ctrl.setRenew(func(node string, csr []byte) ([]byte, error) {
		cert, err := h.ca.IssueClient(csr, hostID(node), h.clock.now(), rand.Reader)
		if err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), nil
	})
	key, cert := h.issue(h.ca, hostID(testNode))
	c := h.clientWith(pki.TLSCertificate(key, cert))

	csrOf := func(t *testing.T, id pki.Identity, fresh bool) []byte {
		t.Helper()
		k := key
		if fresh {
			var err error
			k, err = pki.NewKey(rand.Reader)
			require.NoError(t, err)
		}
		csr, err := pki.NewCSR(k, id, rand.Reader)
		require.NoError(t, err)
		body, err := json.Marshal(RenewRequest{CSR: csr})
		require.NoError(t, err)
		return body
	}

	resp := h.post(c, "/v1/agent/renew", csrOf(t, hostID(testNode), true))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var answer CertificateResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&answer))
	block, _ := pem.Decode([]byte(answer.CertPEM))
	require.NotNil(t, block)
	renewed, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	require.NotEqual(t, cert.SerialNumber, renewed.SerialNumber)

	t.Run("the same key", func(t *testing.T) {
		resp := h.post(c, "/v1/agent/renew", csrOf(t, hostID(testNode), false))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
	t.Run("another identity", func(t *testing.T) {
		resp := h.post(c, "/v1/agent/renew", csrOf(t, hostID("pve3"), true))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
	t.Run("not a certificate request", func(t *testing.T) {
		resp := h.post(c, "/v1/agent/renew", []byte(`{"csr":"AAAA"}`))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
	t.Run("too large", func(t *testing.T) {
		resp := h.post(c, "/v1/agent/renew", make([]byte, maxRenewBody+1))
		require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	})
	t.Run("the controller refuses", func(t *testing.T) {
		h.ctrl.setRenew(func(string, []byte) ([]byte, error) { return nil, errNope })
		resp := h.post(c, "/v1/agent/renew", csrOf(t, hostID(testNode), true))
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	})
}
