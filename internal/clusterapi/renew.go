package clusterapi

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// RenewRequest asks for the next client certificate of the caller, for a
// request of a new key.
type RenewRequest struct {
	CSR []byte `json:"csr"`
}

// renew answers POST /v1/agent/renew. The request must name the caller's
// own identity and a key other than its certificate's: a renewal always
// comes with a new key.
func (s *Server) renew(w http.ResponseWriter, r *http.Request) {
	p, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRenewBody))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		http.Error(w, "the renewal is too large", http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req RenewRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		http.Error(w, "the request is no renewal: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := checkRenewal(p, req.CSR); err != nil {
		s.log.Warn().Str("node", p.node).Err(err).Msg("refused a renewal")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	certPEM, err := s.c.Renew(r.Context(), p.node, req.CSR)
	if err != nil {
		s.fail(w, p.node, "the certificate could not be renewed", err)
		return
	}
	writeJSON(w, http.StatusOK, CertificateResponse{CertPEM: string(certPEM), CAPEM: s.caPEM()})
}

func checkRenewal(p peer, der []byte) error {
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return fmt.Errorf("reading the certificate request: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return fmt.Errorf("the certificate request is not signed by its key: %w", err)
	}
	if len(csr.URIs) != 1 || csr.URIs[0].String() != p.id.String() {
		return fmt.Errorf("the certificate request is not for %s", p.id)
	}
	if old, ok := p.cert.PublicKey.(*ecdsa.PublicKey); ok && old.Equal(csr.PublicKey) {
		return errors.New("a renewal needs a new key, and the request is for the key of the certificate in use")
	}
	return nil
}
