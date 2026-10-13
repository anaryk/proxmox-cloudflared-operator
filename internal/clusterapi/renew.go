package clusterapi

import (
	"crypto/ecdsa"
	"crypto/elliptic"
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
	s.readDeadline(w)
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRenewBody))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		http.Error(w, "the renewal is too large", http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		s.badRequest(w, p.node, "the renewal could not be read", err)
		return
	}
	var req RenewRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		s.badRequest(w, p.node, "the request is no renewal", err)
		return
	}
	if err := checkRenewal(p, req.CSR); err != nil {
		s.badRequest(w, p.node, "refused a renewal: it must be for this certificate's identity and a new P-256 key", err)
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
	if key, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok || key.Curve != elliptic.P256() {
		return errors.New("the certificate request is not for a P-256 key")
	}
	if len(csr.URIs) != 1 || csr.URIs[0].String() != p.id.String() {
		return fmt.Errorf("the certificate request is not for %s", p.id)
	}
	if old, ok := p.cert.PublicKey.(*ecdsa.PublicKey); ok && old.Equal(csr.PublicKey) {
		return errors.New("a renewal needs a new key, and the request is for the key of the certificate in use")
	}
	return nil
}
