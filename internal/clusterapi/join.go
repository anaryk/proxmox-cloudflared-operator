package clusterapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// RefusalRefused is the one answer to a join that is not taken, whatever the
// reason: a caller with a bad token learns nothing of why.
const RefusalRefused = "refused"

const (
	minVMID = 100
	maxVMID = 999999999
)

// JoinRequest is what an appliance agent sends to join: its token as the
// controller wrote it, a certificate request of its new key, and what it
// says of itself.
type JoinRequest struct {
	Token    string           `json:"token"`
	CSR      []byte           `json:"csr"`
	Node     string           `json:"node"`
	VMID     int              `json:"vmid"`
	MACs     []string         `json:"macs,omitempty"`
	Build    string           `json:"build,omitempty"`
	Protocol agentproto.Range `json:"protocol"`
}

// CertificateResponse answers a join and a renewal: the client certificate
// and the CA it is under, both in PEM.
type CertificateResponse struct {
	CertPEM string `json:"certPEM"`
	CAPEM   string `json:"caPEM"`
}

// JoinStore is what the join reads and writes of the join tokens.
// *store.Store is one.
type JoinStore interface {
	JoinTokens() (map[string]store.JoinRecord, error)
	UseJoinToken(id string, use store.JoinUse) (store.JoinRecord, error)
}

// Joins is what the endpoint needs to take joins, which only an appliance
// controller does: its install, the fingerprint of its current CA, which a
// token must name, and the join tokens. Misused, when set, is told of every
// use of a token past the first, so that the controller holds the node
// contested.
type Joins struct {
	InstallID string
	CASHA256  func() string
	Tokens    JoinStore
	Misused   func(ctx context.Context, rec store.JoinRecord)
}

// AcceptJoins switches the join on. Without it every join is refused, as a
// host controller's must be. It is called before Serve.
func (s *Server) AcceptJoins(j Joins) { s.joinCfg = &j }

// join answers POST /v1/agent/join, the one call made without a client
// certificate; one that is sent is not looked at. A token is taken once: its
// use is recorded before anything is answered, and only the first use of it
// gets a certificate, however many arrive at once.
func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	src := sourceOf(r)
	if !s.joins.allow(src, s.now()) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many joins from this address; try again in a minute", http.StatusTooManyRequests)
		return
	}
	j := s.joinCfg
	switch {
	case j == nil:
		s.refuseJoin(w, r, "this controller takes no joins")
		return
	case !s.a.Allowed(src, true):
		s.refuseJoin(w, r, "the source may not join")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJoinBody))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		http.Error(w, "the join is too large", http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		s.refuseJoin(w, r, "the request could not be read")
		return
	}
	var req JoinRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		s.refuseJoin(w, r, "the request is no join")
		return
	}
	tok, csr, why := s.checkJoin(j, req)
	if why != "" {
		s.refuseJoin(w, r, why)
		return
	}
	first, why := s.useToken(r.Context(), j, tok, store.JoinUse{At: s.now(), Source: src, VMID: req.VMID, MACs: req.MACs})
	if !first {
		s.refuseJoin(w, r, why)
		return
	}
	facts := JoinFacts{Node: req.Node, VMID: req.VMID, MACs: req.MACs, Build: req.Build, Protocol: req.Protocol}
	certPEM, err := s.c.Join(r.Context(), tok, req.CSR, src, facts)
	if err == nil {
		err = s.checkIssued(certPEM, csr, applianceIdentity(j.InstallID, req.Node, req.VMID))
	}
	if err != nil {
		s.refuseJoin(w, r, fmt.Sprintf("the controller refused the join of node %s container %d: %v", req.Node, req.VMID, err))
		return
	}
	s.log.Info().Str("node", req.Node).Int("vmid", req.VMID).Str("source", src.String()).Msg("an appliance agent joined")
	writeJSON(w, http.StatusOK, CertificateResponse{CertPEM: string(certPEM), CAPEM: s.caPEM()})
}

// checkJoin checks what a join can be checked by before its token is used:
// the token's form, install, CA, expiry and node, and that the certificate
// request is signed by a P-256 key and asks for the identity of the token's
// node and the container that says it joins. It returns why it refuses.
func (s *Server) checkJoin(j *Joins, req JoinRequest) (pki.JoinToken, *x509.CertificateRequest, string) {
	tok, err := pki.ParseJoinToken(req.Token)
	switch {
	case err != nil:
		return pki.JoinToken{}, nil, "the token could not be read"
	case tok.SecretHash() == "":
		return pki.JoinToken{}, nil, "the token carries no secret"
	case tok.InstallID != j.InstallID:
		return pki.JoinToken{}, nil, "the token is of install " + tok.InstallID
	case j.CASHA256 == nil || tok.CASHA256 != j.CASHA256():
		return pki.JoinToken{}, nil, "the token names another CA than the install's"
	case tok.ServerName != pki.ControllerServerName(j.InstallID):
		return pki.JoinToken{}, nil, "the token names another server"
	case !s.now().Before(tok.Expires):
		return pki.JoinToken{}, nil, "the token of node " + tok.Node + " expired"
	case req.Node == "" || req.Node != tok.Node:
		return pki.JoinToken{}, nil, fmt.Sprintf("the token is for node %s, the agent says it runs on %q", tok.Node, req.Node)
	case req.VMID < minVMID || req.VMID > maxVMID:
		return pki.JoinToken{}, nil, fmt.Sprintf("container %d is no container", req.VMID)
	}
	csr, err := x509.ParseCertificateRequest(req.CSR)
	if err != nil {
		return pki.JoinToken{}, nil, "the certificate request could not be read"
	}
	want := applianceIdentity(j.InstallID, req.Node, req.VMID)
	key, isEC := csr.PublicKey.(*ecdsa.PublicKey)
	switch {
	case csr.CheckSignature() != nil:
		return pki.JoinToken{}, nil, "the certificate request is not signed by its key"
	case !isEC || key.Curve != elliptic.P256():
		return pki.JoinToken{}, nil, "the certificate request is not for a P-256 key"
	case len(csr.URIs) != 1 || csr.URIs[0].String() != want.String():
		return pki.JoinToken{}, nil, "the certificate request is not for " + want.String()
	}
	return tok, csr, ""
}

func applianceIdentity(install, node string, vmid int) pki.Identity {
	return pki.Identity{Kind: pki.KindAppliance, InstallID: install, Node: node, VMID: vmid}
}

// useToken records a use of tok and reports whether it is the token's first,
// stored unexpired. Uses are recorded one at a time, so of two that come at
// once exactly one is the first.
func (s *Server) useToken(ctx context.Context, j *Joins, tok pki.JoinToken, use store.JoinUse) (bool, string) {
	s.joinMu.Lock()
	rec, err := UseToken(j.Tokens, tok, use)
	s.joinMu.Unlock()
	switch {
	case err != nil:
		return false, "the token is not one of the install's: " + err.Error()
	case !s.now().Before(rec.Expires):
		return false, "the token of node " + rec.Node + " expired"
	case len(rec.Uses) != 1:
		if j.Misused != nil {
			j.Misused(ctx, rec)
		}
		return false, fmt.Sprintf("the token of node %s was used %d times", rec.Node, len(rec.Uses))
	}
	return true, ""
}

// checkIssued refuses a certificate the controller issued that is not for
// the request's key and identity, or not under the install's CA: the agent
// gets nothing the join did not ask for.
func (s *Server) checkIssued(certPEM []byte, csr *x509.CertificateRequest, want pki.Identity) error {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("the controller issued no certificate in PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("reading the issued certificate: %w", err)
	}
	id, err := pki.IdentityOf(cert)
	switch {
	case err != nil:
		return err
	case id != want:
		return fmt.Errorf("the issued certificate is of %s, not %s", id, want)
	}
	if key, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !key.Equal(csr.PublicKey) {
		return errors.New("the issued certificate is not for the key of the request")
	}
	ca, err := s.caCert()
	if err != nil {
		return err
	}
	if err := cert.CheckSignatureFrom(ca); err != nil {
		return fmt.Errorf("the issued certificate is not under the install's CA: %w", err)
	}
	return nil
}

// refuseJoin answers a join that is not taken, and logs why; never the token.
func (s *Server) refuseJoin(w http.ResponseWriter, r *http.Request, why string) {
	s.log.Warn().Str("source", sourceOf(r).String()).Msg("refused a join: " + why)
	writeJSON(w, http.StatusForbidden, agentproto.Refusal{Code: RefusalRefused, Message: "refused"})
}

// caCert is the CA the server's certificate is under: the last of its chain.
func (s *Server) caCert() (*x509.Certificate, error) {
	chain := s.cert.Load().Certificate
	if len(chain) < 2 {
		return nil, errors.New("the server's certificate does not come with its CA")
	}
	return x509.ParseCertificate(chain[len(chain)-1])
}

func (s *Server) caPEM() string {
	ca, err := s.caCert()
	if err != nil {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}))
}

// UseToken records a use of the join token whose secret and node tok
// carries, before the controller answers it, and returns the record with
// every use: one is the first, more make the node contested. The secret is
// compared in constant time with the hash the store keeps. A token the store
// does not hold is store.ErrNoJoinToken.
func UseToken(st JoinStore, tok pki.JoinToken, use store.JoinUse) (store.JoinRecord, error) {
	if tok.Node == "" || tok.SecretHash() == "" {
		return store.JoinRecord{}, fmt.Errorf("%w: the token names no node or carries no secret", store.ErrNoJoinToken)
	}
	records, err := st.JoinTokens()
	if err != nil {
		return store.JoinRecord{}, err
	}
	for id, rec := range records {
		if rec.Node == tok.Node && rec.SecretSHA256 != "" && tok.Matches(rec.SecretSHA256) {
			return st.UseJoinToken(id, use)
		}
	}
	return store.JoinRecord{}, fmt.Errorf("%w for node %s", store.ErrNoJoinToken, tok.Node)
}
