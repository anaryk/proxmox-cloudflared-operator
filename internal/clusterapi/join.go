package clusterapi

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// RefusalRefused is the one answer to a join that is not taken, whatever the
// reason: a caller with a bad token learns nothing of why.
const RefusalRefused = "refused"

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

// join answers POST /v1/agent/join, the one call made without a client
// certificate; one that is sent is not looked at.
func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	src := sourceOf(r)
	if !s.joins.allow(src, s.now()) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many joins from this address; try again in a minute", http.StatusTooManyRequests)
		return
	}
	if !s.a.Allowed(src, true) {
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
	tok, err := pki.ParseJoinToken(req.Token)
	switch {
	case err != nil:
		s.refuseJoin(w, r, "the token could not be read")
		return
	case !s.now().Before(tok.Expires):
		s.refuseJoin(w, r, "the token of node "+tok.Node+" expired")
		return
	case req.Node != tok.Node:
		s.refuseJoin(w, r, fmt.Sprintf("the token is for node %s, the agent says it runs on %s", tok.Node, req.Node))
		return
	case len(req.CSR) == 0:
		s.refuseJoin(w, r, "the join carries no certificate request")
		return
	}
	facts := JoinFacts{Node: req.Node, VMID: req.VMID, MACs: req.MACs, Build: req.Build, Protocol: req.Protocol}
	certPEM, err := s.c.Join(r.Context(), tok, req.CSR, src, facts)
	if err != nil {
		s.refuseJoin(w, r, fmt.Sprintf("the controller refused the join of node %s container %d: %v", req.Node, req.VMID, err))
		return
	}
	s.log.Info().Str("node", req.Node).Int("vmid", req.VMID).Str("source", src.String()).Msg("an appliance agent joined")
	writeJSON(w, http.StatusOK, CertificateResponse{CertPEM: string(certPEM), CAPEM: s.caPEM()})
}

// refuseJoin answers a join that is not taken, and logs why; never the token.
func (s *Server) refuseJoin(w http.ResponseWriter, r *http.Request, why string) {
	s.log.Warn().Str("source", sourceOf(r).String()).Msg("refused a join: " + why)
	writeJSON(w, http.StatusForbidden, agentproto.Refusal{Code: RefusalRefused, Message: "refused"})
}

// caPEM is the CA the server's certificate is under: the last of its chain.
func (s *Server) caPEM() string {
	chain := s.cert.Load().Certificate
	if len(chain) == 0 {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: chain[len(chain)-1]}))
}

// JoinStore is what UseToken reads and writes. *store.Store is one.
type JoinStore interface {
	JoinTokens() (map[string]store.JoinRecord, error)
	UseJoinToken(id string, use store.JoinUse) (store.JoinRecord, error)
}

// UseToken records a use of the join token whose secret and node tok
// carries, before the controller answers it, and returns the record with
// every use: one is the first, more make the node contested. A token the
// store does not hold is store.ErrNoJoinToken.
func UseToken(st JoinStore, tok pki.JoinToken, use store.JoinUse) (store.JoinRecord, error) {
	records, err := st.JoinTokens()
	if err != nil {
		return store.JoinRecord{}, err
	}
	for id, rec := range records {
		if rec.Node == tok.Node && tok.Matches(rec.SecretSHA256) {
			return st.UseJoinToken(id, use)
		}
	}
	return store.JoinRecord{}, fmt.Errorf("%w for node %s", store.ErrNoJoinToken, strconv.Quote(tok.Node))
}
