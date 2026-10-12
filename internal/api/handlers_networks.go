package api

import (
	"errors"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

var (
	errNetworkRev = &engine.FieldError{Field: "rev", Err: errors.New("rev: give the revision the network was read at, " +
		"as /v1/networks/<id>?rev=<n>")}
	errNoPrefix   = &engine.FieldError{Field: "prefix", Err: errors.New("prefix: give the prefix of the network, as 10.77.0.0/24")}
	errNoAddress  = &engine.FieldError{Field: "address", Err: errors.New("address: give the address of the leg, as 10.77.0.250")}
	errBadPrefix  = &engine.FieldError{Field: "prefix", Err: errors.New("prefix: want an IPv4 prefix such as 10.77.0.0/24")}
	errBadAddress = &engine.FieldError{Field: "address", Err: errors.New("address: want an IPv4 address such as 10.77.0.250")}
)

// networkRoutes adds the routes of the networks pco reaches through a leg of
// its own: what is attached, what may be, the probe of an address and the
// attach and detach of a network.
func (s *Server) networkRoutes(v1 *gin.RouterGroup) {
	v1.GET("/networks", s.getNetworks)
	v1.GET("/networks/candidates", s.getCandidateNetworks)
	v1.POST("/networks/probe", s.postProbeNetwork)
	v1.POST("/networks", s.postNetwork)
	v1.GET("/networks/:id", s.getNetwork)
	v1.DELETE("/networks/:id", s.deleteNetwork)
}

func (s *Server) getNetworks(c *gin.Context) {
	nets, err := s.engine.Networks()
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, nonNil(nets))
}

func (s *Server) getNetwork(c *gin.Context) {
	n, err := s.engine.Network(c.Param("id"))
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, n)
}

// getCandidateNetworks answers with the bridges and VLANs of the node, which
// the daemon reads from Proxmox and the node at the time it is asked.
func (s *Server) getCandidateNetworks(c *gin.Context) {
	candidates, err := s.engine.CandidateNetworks(c.Request.Context())
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, nonNil(candidates))
}

// probeRequest is the body of a probe: the prefix and the address are text,
// so that one that does not parse is refused with its field, and either may
// be left out.
type probeRequest struct {
	Bridge  string `json:"bridge"`
	VLAN    int    `json:"vlan"`
	Prefix  string `json:"prefix"`
	Address string `json:"address"`
}

func (s *Server) postProbeNetwork(c *gin.Context) {
	var req probeRequest
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	prefix, addr, err := prefixAndAddress(req.Prefix, req.Address)
	if err != nil {
		s.fail(c, err)
		return
	}
	res, err := s.engine.ProbeNetwork(c.Request.Context(), engine.ProbeRequest{Bridge: req.Bridge, VLAN: req.VLAN, Prefix: prefix, Address: addr})
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// attachRequest is the body of an attach, which needs the prefix and the
// address.
type attachRequest struct {
	Bridge        string `json:"bridge"`
	VLAN          int    `json:"vlan"`
	Prefix        string `json:"prefix"`
	Address       string `json:"address"`
	ConfirmUplink bool   `json:"confirmUplink"`
	OverlapOK     bool   `json:"overlapOK"`
}

// postNetwork attaches a network and answers with it.
func (s *Server) postNetwork(c *gin.Context) {
	var req attachRequest
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	switch {
	case req.Prefix == "":
		s.fail(c, errNoPrefix)
		return
	case req.Address == "":
		s.fail(c, errNoAddress)
		return
	}
	prefix, addr, err := prefixAndAddress(req.Prefix, req.Address)
	if err != nil {
		s.fail(c, err)
		return
	}
	v, err := s.engine.Attach(c.Request.Context(), engine.AttachRequest{
		Bridge: req.Bridge, VLAN: req.VLAN, Prefix: prefix, Address: addr, ConfirmUplink: req.ConfirmUplink, OverlapOK: req.OverlapOK,
	})
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, v)
}

// prefixAndAddress reads the prefix and the address of a request; an empty
// one is the zero value.
func prefixAndAddress(prefixText, addrText string) (netip.Prefix, netip.Addr, error) {
	var (
		prefix netip.Prefix
		addr   netip.Addr
		err    error
	)
	if prefixText != "" {
		if prefix, err = netip.ParsePrefix(prefixText); err != nil || !prefix.Addr().Is4() {
			return prefix, addr, errBadPrefix
		}
	}
	if addrText != "" {
		if addr, err = netip.ParseAddr(addrText); err != nil || !addr.Is4() {
			return prefix, addr, errBadAddress
		}
	}
	return prefix, addr, nil
}

// deleteNetwork detaches a network read at the revision the query names. A
// network that serves routes is detached only when the query names them
// (served=<hostname>, repeated), which says that the admin saw them.
func (s *Server) deleteNetwork(c *gin.Context) {
	revs := c.QueryArray("rev")
	if len(revs) != 1 {
		s.fail(c, errNetworkRev)
		return
	}
	rev, err := strconv.ParseInt(revs[0], 10, 64)
	if err != nil || rev < 1 {
		s.fail(c, errNetworkRev)
		return
	}
	if err := s.engine.Detach(c.Request.Context(), c.Param("id"), rev, c.QueryArray("served")); err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, struct{}{})
}
