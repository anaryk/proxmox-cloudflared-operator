package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
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
	if s.Principal.Role >= RoleAdmin {
		return All, "", nil
	}
	now := a.cfg.Now()
	if s.visible != nil && now.Sub(s.visibleAt) < visibleAge {
		return byVMID(s.visible), s.visibleOf, nil
	}
	cred := Credential{Token: s.token}
	if s.Principal.Method == MethodTicket {
		cred = Credential{Ticket: cookieValue(c.Request, ticketCookieName)}
	}
	vmids, err := a.pve.VisibleVMIDs(c.Request.Context(), cred)
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
