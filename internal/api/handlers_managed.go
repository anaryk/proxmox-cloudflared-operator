package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// managedRoutes adds the routes of the managed network: its view, the
// consent of a guest and its revocation, and the switch that stores the
// network once its vnet exists.
func (s *Server) managedRoutes(v1 *gin.RouterGroup) {
	v1.GET("/managed", s.getManaged)
	v1.POST("/managed/approve", s.postApproveManaged)
	v1.POST("/managed/revoke", s.postRevokeManaged)
	v1.POST("/managed/enable", s.postEnableManaged)
}

func (s *Server) getManaged(c *gin.Context) {
	v, err := s.engine.Managed()
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

// postApproveManaged consents to a card for a guest and answers with the
// consent. The identity, when given, is the one the admin was shown.
func (s *Server) postApproveManaged(c *gin.Context) {
	var req struct {
		Owner                string `json:"owner"`
		Identity             string `json:"identity"`
		AcceptFirewallImpact bool   `json:"acceptFirewallImpact"`
	}
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	consent, err := s.engine.ApproveManaged(c.Request.Context(), req.Owner, req.Identity, req.AcceptFirewallImpact)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, consent)
}

func (s *Server) postRevokeManaged(c *gin.Context) {
	var req struct {
		Owner string `json:"owner"`
	}
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	if err := s.engine.RevokeManaged(c.Request.Context(), req.Owner); err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, struct{}{})
}

// postEnableManaged stores the managed network; it takes {} or nothing.
func (s *Server) postEnableManaged(c *gin.Context) {
	if err := decode(c, &struct{}{}, true); err != nil {
		s.fail(c, err)
		return
	}
	if err := s.engine.EnableManaged(c.Request.Context()); err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, struct{}{})
}
