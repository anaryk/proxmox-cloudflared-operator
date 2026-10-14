package api

import (
	"net/http"
	"net/netip"

	"github.com/gin-gonic/gin"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

var (
	errBadBlockAddr = &httpError{http.StatusBadRequest, codeInvalid, "address must be an IP address", false}
	errUninstallBy  = &httpError{http.StatusForbidden, codeForbidden, "only root may uninstall the cluster", false}
	errResetEpochBy = &httpError{http.StatusForbidden, codeForbidden, "only root may reset the epoch of the agent", false}
)

// clusterRoutes adds the routes of a cluster: its status and its nodes,
// what the admin does to the lease, the upgrade, a block and the whole
// install, what an admin does to one node, and the agent of this node, which
// every node answers for itself.
func (s *Server) clusterRoutes(v1 *gin.RouterGroup) {
	v1.GET("/cluster", s.getCluster)
	v1.POST("/cluster/handoff", s.postHandoff)
	v1.POST("/cluster/join-tokens", s.postJoinToken)
	v1.POST("/cluster/finish-upgrade", s.postFinishUpgrade)
	v1.POST("/cluster/unblock", s.postUnblock)
	v1.POST("/cluster/uninstall", s.postUninstallCluster)
	v1.GET("/nodes", s.getNodes)
	v1.POST("/nodes/:name/admit", s.postAdmitNode)
	v1.POST("/nodes/:name/drain", s.postDrainNode)
	v1.POST("/nodes/:name/undrain", s.postUndrainNode)
	v1.POST("/nodes/:name/forget", s.postForgetNode)
	v1.PUT("/nodes/:name", s.putNode)
	v1.GET("/local/agent", s.getLocalAgent)
	v1.POST("/local/agent/reset-epoch", s.postResetEpoch)
}

func (s *Server) getCluster(c *gin.Context) {
	c.JSON(http.StatusOK, s.engine.ClusterStatus())
}

func (s *Server) getNodes(c *gin.Context) {
	c.JSON(http.StatusOK, nonNil(s.engine.Nodes()))
}

func (s *Server) postHandoff(c *gin.Context) {
	var req struct {
		To string `json:"to"`
	}
	if err := decode(c, &req, true); err != nil {
		s.fail(c, err)
		return
	}
	res, err := s.engine.Handoff(c.Request.Context(), req.To)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// postJoinToken answers with the one copy of the token there is; the
// daemon keeps its hash alone, and the log never sees the body.
func (s *Server) postJoinToken(c *gin.Context) {
	var req struct {
		Node string `json:"node"`
	}
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	res, err := s.engine.NewJoinToken(c.Request.Context(), req.Node)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, res)
}

func (s *Server) postFinishUpgrade(c *gin.Context) {
	if err := decode(c, &struct{}{}, true); err != nil {
		s.fail(c, err)
		return
	}
	res, err := s.engine.FinishUpgrade(c.Request.Context())
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func (s *Server) postUnblock(c *gin.Context) {
	var req struct {
		Address string `json:"address"`
	}
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	addr, err := netip.ParseAddr(req.Address)
	if err != nil {
		s.fail(c, errBadBlockAddr)
		return
	}
	if err := s.engine.Unblock(c.Request.Context(), addr); err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, struct{}{})
}

// postUninstallCluster takes every other node out of the install. Only root
// may ask, where the peer can be told: it is the first step of pco uninstall
// --cluster.
func (s *Server) postUninstallCluster(c *gin.Context) {
	if uid, ok := peerUID(c.Request.Context()); s.checkPeers && (!ok || uid != 0) {
		s.fail(c, errUninstallBy)
		return
	}
	var req struct {
		Force bool `json:"force"`
	}
	if err := decode(c, &req, true); err != nil {
		s.fail(c, err)
		return
	}
	res, err := s.engine.UninstallCluster(c.Request.Context(), req.Force)
	if err != nil {
		s.fail(c, err)
		return
	}
	if res.Clean == nil {
		res.Clean = []string{}
	}
	c.JSON(http.StatusOK, res)
}

func (s *Server) postAdmitNode(c *gin.Context) {
	var req struct {
		Fingerprint string `json:"fingerprint"`
		Renew       bool   `json:"renew"`
	}
	if err := decode(c, &req, true); err != nil {
		s.fail(c, err)
		return
	}
	v, err := s.engine.AdmitNode(c.Request.Context(), c.Param("name"), req.Fingerprint, req.Renew)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

func (s *Server) postDrainNode(c *gin.Context) {
	var req struct {
		Force bool `json:"force"`
	}
	if err := decode(c, &req, true); err != nil {
		s.fail(c, err)
		return
	}
	v, err := s.engine.DrainNode(c.Request.Context(), c.Param("name"), req.Force)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

func (s *Server) postUndrainNode(c *gin.Context) {
	if err := decode(c, &struct{}{}, true); err != nil {
		s.fail(c, err)
		return
	}
	v, err := s.engine.UndrainNode(c.Request.Context(), c.Param("name"))
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

func (s *Server) postForgetNode(c *gin.Context) {
	var req struct {
		Compromised bool `json:"compromised"`
	}
	if err := decode(c, &req, true); err != nil {
		s.fail(c, err)
		return
	}
	res, err := s.engine.ForgetNode(c.Request.Context(), c.Param("name"), req.Compromised)
	if err != nil {
		s.fail(c, err)
		return
	}
	if res.Done == nil {
		res.Done = []string{}
	}
	c.JSON(http.StatusOK, res)
}

func (s *Server) putNode(c *gin.Context) {
	var req engine.NodePatch
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	v, err := s.engine.SetNode(c.Request.Context(), c.Param("name"), req)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

// getLocalAgent answers from this node's own agent, whatever the node's
// role.
func (s *Server) getLocalAgent(c *gin.Context) {
	v, err := s.engine.AgentView()
	if err != nil {
		s.fail(c, err)
		return
	}
	if v.Problems == nil {
		v.Problems = []string{}
	}
	c.JSON(http.StatusOK, v)
}

// postResetEpoch forgets the epoch the agent of this node remembers. Only
// root may ask, where the peer can be told.
func (s *Server) postResetEpoch(c *gin.Context) {
	if uid, ok := peerUID(c.Request.Context()); s.checkPeers && (!ok || uid != 0) {
		s.fail(c, errResetEpochBy)
		return
	}
	if err := decode(c, &struct{}{}, true); err != nil {
		s.fail(c, err)
		return
	}
	if err := s.engine.ResetAgentEpoch(c.Request.Context()); err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, struct{}{})
}
