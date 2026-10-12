package clusterapi

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

// status answers POST /v1/agent/status with the controller's heartbeat. The
// body may be gzip-compressed; what it expands to is held to the bounds of
// the protocol, and so is the resend of a new epoch, by what arrives and not
// by what the agent says of it.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	p, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	body := io.Reader(http.MaxBytesReader(w, r.Body, agentproto.MaxStatusBytes+1))
	switch enc := r.Header.Get("Content-Encoding"); enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "the status is no gzip stream: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer func() { _ = zr.Close() }()
		body = zr
	default:
		http.Error(w, fmt.Sprintf("content encoding %q: want gzip or none", enc), http.StatusUnsupportedMediaType)
		return
	}
	st, err := agentproto.DecodeStatus(body)
	var tooBig *http.MaxBytesError
	switch {
	case errors.Is(err, agentproto.ErrTooLarge) || errors.As(err, &tooBig):
		s.record(p.node, fmt.Sprintf("a status of node %s was refused: %v", p.node, err))
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := matches(p, st); err != nil {
		s.log.Warn().Str("node", p.node).Err(err).Msg("refused a status")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if ref := rangeRefusal(st.Protocol); ref != nil {
		s.refuse(w, r, p.node, *ref)
		return
	}
	if s.resend.over(p.node, st.EpochSeen, len(st.Events), st.Resend > 0) {
		n := s.resend.count(p.node)
		s.record(p.node, fmt.Sprintf("node %s sent %d events again to epoch %s, past the bound of %d; the rest is refused",
			p.node, n, st.EpochSeen, agentproto.MaxResend))
		http.Error(w, fmt.Sprintf("the resend holds %d events, at most %d go", n, agentproto.MaxResend), http.StatusRequestEntityTooLarge)
		return
	}
	hb, err := s.c.Report(r.Context(), p.node, st)
	if err != nil {
		s.fail(w, p.node, "the status could not be taken", err)
		return
	}
	writeJSON(w, http.StatusOK, hb)
}

// matches refuses a status that speaks for another node, kind or container
// than its certificate: an agent reports only for itself.
func matches(p peer, st agentproto.Status) error {
	switch {
	case st.Node != p.node:
		return fmt.Errorf("the status is of node %q, the certificate of node %s", st.Node, p.node)
	case st.Kind != p.id.Kind:
		return fmt.Errorf("the status is of kind %q, the certificate of kind %s", st.Kind, p.id.Kind)
	case st.VMID != p.id.VMID:
		return fmt.Errorf("the status is of container %d, the certificate of container %d", st.VMID, p.id.VMID)
	}
	return nil
}
