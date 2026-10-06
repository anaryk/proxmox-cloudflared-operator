package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// Visible says whether a session may see a guest. The one type for every
// caller: the gateway's rules, its filters and the stream.
type Visible func(model.GuestRef) bool

// All is the Visible of an admin.
func All(model.GuestRef) bool { return true }

const visibleAge = time.Minute

// Visible is the session's set and a hash of it (empty for admins), for
// caches and ETags. VMIDs are unique in a cluster, so the set is by VMID.
// A reader's set is what cluster/resources lists with the reader's own
// credential, the guests it holds VM.Audit on, kept for a minute.
func (a *Auth) Visible(c *gin.Context) (Visible, string, error) {
	s := SessionOf(c)
	if s == nil {
		return nil, "", errors.New("the request has no session")
	}
	return a.visibleFor(c.Request.Context(), c.Request, s)
}

// VisibleOf is Visible for a stream, which asks on its own: the session id
// as the store holds it now, not as a request copied it, so that the set
// another request of the session read within the minute is the stream's too.
// r carries the ticket. A session that is gone or over is an error.
func (a *Auth) VisibleOf(ctx context.Context, r *http.Request, id string) (Visible, string, error) {
	s, ok := a.sessions.get(id)
	if !ok || !s.live(a.cfg.Now()) {
		return nil, "", errNoSession
	}
	return a.visibleFor(ctx, r, &s)
}

// visibleFor reads the set of s from the session, or from Proxmox VE when it
// is a minute old, and keeps it in s and in the store.
func (a *Auth) visibleFor(ctx context.Context, r *http.Request, s *Session) (Visible, string, error) {
	if s.Principal.Role >= RoleAdmin {
		return All, "", nil
	}
	now := a.cfg.Now()
	if s.visible != nil && now.Sub(s.visibleAt) < visibleAge {
		return byVMID(s.visible), s.visibleOf, nil
	}
	cred := Credential{Token: s.token}
	if s.Principal.Method == MethodTicket {
		cred = Credential{Ticket: cookieValue(r, ticketCookieName)}
	}
	vmids, err := a.pve.VisibleVMIDs(ctx, cred)
	if err != nil {
		return nil, "", fmt.Errorf("listing the guests %s may see: %w", s.Principal.User, err)
	}
	set := make(map[int]bool, len(vmids))
	for _, id := range vmids {
		set[id] = true
	}
	hash := hashOf(set)
	a.sessions.update(s.ID, func(x *Session) { x.visible, x.visibleOf, x.visibleAt = set, hash, now })
	s.visible, s.visibleOf, s.visibleAt = set, hash, now
	return byVMID(set), hash, nil
}

// byVMID reads set, which is never written once made.
func byVMID(set map[int]bool) Visible {
	return func(g model.GuestRef) bool { return set[g.VMID] }
}

func hashOf(set map[int]bool) string {
	h := sha256.New()
	for _, id := range slices.Sorted(maps.Keys(set)) {
		h.Write(strconv.AppendInt(nil, int64(id), 10))
		h.Write([]byte{','})
	}
	return hex.EncodeToString(h.Sum(nil))
}
