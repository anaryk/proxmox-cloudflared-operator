package clusterapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

// desired answers GET /v1/agent/desired?epoch=<g.n>&after=<seq>: the node's
// snapshot as soon as its seq in that epoch is above after, or the
// controller serves another epoch; 204 when the long-poll ends without one.
func (s *Server) desired(w http.ResponseWriter, r *http.Request) {
	p, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	epoch, after, err := pollQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if epoch.Generation != 0 {
		epoch.InstallID = p.id.InstallID
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.longPoll)
	defer cancel()
	if info := connInfoOf(r.Context()); info != nil {
		defer info.removePoll(info.addPoll(cancel))
	}
	snap, err := s.c.Snapshot(ctx, p.node, epoch, after)
	switch {
	case err == nil:
	case errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == nil:
		w.WriteHeader(http.StatusNoContent)
		return
	case ctx.Err() != nil:
		return // the agent went away, or the node was closed
	default:
		s.fail(w, p.node, "the desired state could not be rendered", err)
		return
	}
	if snap.Node != p.node {
		s.log.Error().Str("node", p.node).Str("rendered", snap.Node).Msg("a snapshot rendered for another node was not served")
		http.Error(w, "the desired state was rendered for another node", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := agentproto.EncodeSnapshot(&buf, snap); err != nil {
		s.fail(w, p.node, "the desired state could not be encoded", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// pollQuery reads the epoch the agent follows, zero when it follows none,
// and the seq it applied.
func pollQuery(r *http.Request) (agentproto.Epoch, uint64, error) {
	q := r.URL.Query()
	var epoch agentproto.Epoch
	if v := q.Get("epoch"); v != "" {
		e, err := agentproto.ParseEpoch(v)
		if err != nil {
			return agentproto.Epoch{}, 0, err
		}
		epoch = e
	}
	var after uint64
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return agentproto.Epoch{}, 0, fmt.Errorf("after %q is not a seq", v)
		}
		after = n
	}
	return epoch, after, nil
}
