package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// Role is what the daemon says about its node: whether it is the controller
// of the install, the one node that writes, and where the controller is when
// it is another node. Without a role set, the node is the controller, as a
// single node always is.
type Role interface {
	Controller() bool
	// ControllerAddr names the node that holds the lease and the address its
	// agent is reached at, and when it took the lease; ok is false while no
	// controller answers, when node and since still name the lease as it was
	// last read, if it was.
	ControllerAddr() (node, addr string, since time.Time, ok bool)
}

// SetRole sets what the daemon says about its node. It may be called at any
// time: the lease moves while the server runs.
func (s *Server) SetRole(r Role) {
	s.roleMu.Lock()
	defer s.roleMu.Unlock()
	s.role = r
}

func (s *Server) currentRole() Role {
	s.roleMu.RLock()
	defer s.roleMu.RUnlock()
	return s.role
}

// localPrefixes are the routes a node answers for itself whatever its role:
// its own agent and the egress filter of the node.
var localPrefixes = []string{"/v1/local", "/v1/egress"}

// localRoutes are the reads a node answers for itself whatever its role.
// They describe this process and this node: everything else reads or changes
// what the controller holds, and is answered there.
var localRoutes = map[string]bool{
	"GET /v1/version":       true,
	"GET /v1/state":         true,
	"GET /v1/events":        true,
	"GET /v1/stream":        true,
	"GET /v1/traffic":       true,
	"GET /v1/traffic/route": true,
	"GET /v1/doctor":        true,
}

// answeredLocally reports whether a node that is not the controller answers
// the route, given as the pattern it was registered with.
func answeredLocally(method, pattern string) bool {
	if localRoutes[method+" "+pattern] {
		return true
	}
	for _, p := range localPrefixes {
		if pattern == p || strings.HasPrefix(pattern, p+"/") {
			return true
		}
	}
	return false
}

// atController runs before the body of a request is read, and refuses what
// a node that is not the controller cannot answer, pointing at the node that
// can. The peer check came first, so the refusal says nothing to anyone the
// socket does not answer.
func (s *Server) atController(c *gin.Context) {
	role := s.currentRole()
	if role == nil || role.Controller() || answeredLocally(c.Request.Method, c.FullPath()) {
		return
	}
	s.fail(c, notController(role))
}

// notController is the refusal of a non-controller.
func notController(role Role) *httpError {
	node, addr, since, ok := role.ControllerAddr()
	switch {
	case ok:
		return &httpError{http.StatusConflict, codeNotController,
			fmt.Sprintf("this node is not the controller; run it on %s (%s)", node, addr), false}
	case node != "":
		return &httpError{http.StatusServiceUnavailable, codeNoController,
			fmt.Sprintf("no controller is reachable (lease held by %s since %s)", node, since.UTC().Format(time.RFC3339)), false}
	}
	return &httpError{http.StatusServiceUnavailable, codeNoController, "no controller is reachable (no node holds the lease)", false}
}

// LeaseRole is the role as the state of the engine shows it: the node is the
// controller while the state has no lease, as in the appliance, or the lease
// names it as the holder. The holder answers while the lease is reported
// valid and its agent's address is known.
func LeaseRole(state func() engine.State) Role { return leaseRole{state} }

type leaseRole struct{ state func() engine.State }

func (r leaseRole) Controller() bool {
	st := r.state()
	return st.Lease == nil || st.Lease.Holder == st.Node
}

func (r leaseRole) ControllerAddr() (node, addr string, since time.Time, ok bool) {
	lease := r.state().Lease
	if lease == nil {
		return "", "", time.Time{}, false
	}
	if lease.Address.IsValid() {
		addr = lease.Address.String()
	}
	return lease.Holder, addr, lease.AcquiredAt, lease.Holder != "" && addr != "" && lease.Valid
}
