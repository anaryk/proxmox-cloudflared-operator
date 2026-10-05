package api

import (
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const (
	minTokenLength = 20
	maxTokenLength = 256
)

var (
	errBadToken   = &httpError{http.StatusBadRequest, codeInvalid, "the token is not a Cloudflare API token: it has 20 to 256 characters, all of A-Z a-z 0-9 _ -", false}
	errBadSince   = &httpError{http.StatusBadRequest, codeInvalid, "since must be a time in RFC 3339 format", false}
	errBadAfter   = &httpError{http.StatusBadRequest, codeInvalid, "after must be the seq of an event, a whole number", false}
	errBadLimit   = &httpError{http.StatusBadRequest, codeInvalid, fmt.Sprintf("limit must be a whole number from 1 to %d", engine.MaxEventLimit), false}
	errBadHistory = &httpError{http.StatusBadRequest, codeInvalid, "history must be 1 or 0", false}
	errNoHostname = &httpError{http.StatusBadRequest, codeInvalid, "name one hostname: /v1/diagnose?hostname=<name>", false}
	errNoRouteOf  = &httpError{http.StatusBadRequest, codeInvalid, "name one hostname: /v1/traffic/route?hostname=<name>", false}
	errRootOnly   = &httpError{http.StatusForbidden, codeForbidden, "only root may rotate the secret of a tunnel", false}
)

// routes builds the handler of the API: the router, behind the peer check, in
// front of the request log. gin keeps its mode in a global; release mode stops
// it from printing the routes.
func (s *Server) routes() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false
	r.Use(s.recoverPanics)
	r.NoRoute(func(c *gin.Context) { s.fail(c, errNoRoute) })
	r.NoMethod(func(c *gin.Context) { s.fail(c, errNoMethod) })

	v1 := r.Group("/v1", s.acceptJSON)
	v1.GET("/version", s.getVersion)
	v1.GET("/state", s.getState)
	v1.GET("/events", s.getEvents)
	v1.GET("/stream", s.getStream)
	v1.GET("/traffic", s.getTraffic)
	v1.GET("/traffic/route", s.getRouteTraffic)
	v1.POST("/sync", s.postSync)
	v1.POST("/apply", s.postApply)
	v1.POST("/adopt", s.postAdopt)
	v1.POST("/tunnels/rotate", s.postRotateTunnel)
	v1.GET("/credentials", s.getCredentials)
	v1.POST("/credentials", s.postCredential)
	v1.POST("/credentials/:id/check", s.postCredentialCheck)
	v1.DELETE("/credentials/:id", s.deleteCredential)
	v1.GET("/claims", s.getClaims)
	v1.POST("/claims/resolve", s.postResolveClaim)
	v1.GET("/approvals", s.getApprovals)
	v1.POST("/guests/approve", s.postApproveGuest)
	v1.POST("/guests/revoke", s.postRevokeGuest)
	v1.GET("/segments", s.getSegments)
	v1.POST("/segments/acknowledge", s.postAcknowledgeSegment)
	v1.POST("/segments/revoke", s.postRevokeSegment)
	v1.GET("/diagnose", s.getDiagnose)
	v1.GET("/doctor", s.getDoctor)
	return s.logRequests(s.guard(s.stamp(r)))
}

// Version is the answer of GET /v1/version: the version of the daemon, its
// boot, the profile of the install and the node it is registered as, as the
// last cycle found them, and the poll interval of the last settings read, as
// a Go duration like the settings write it.
type Version struct {
	Version      string `json:"version"`
	Boot         string `json:"boot"`
	Profile      string `json:"profile"`
	Node         string `json:"node"`
	PollInterval string `json:"pollInterval"`
}

func (s *Server) getVersion(c *gin.Context) {
	profile, node := s.engine.RunsAs()
	c.JSON(http.StatusOK, Version{
		Version: s.version, Boot: s.engine.Boot(), Profile: profile, Node: node,
		PollInterval: s.engine.PollInterval().String(),
	})
}

// getState answers with the state, named by its digest: a client that holds
// it already gets a 304 without it.
func (s *Server) getState(c *gin.Context) {
	st := s.engine.State()
	if st.Digest != "" {
		tag := `"` + st.Digest + `"`
		c.Header("ETag", tag)
		if holds(c.GetHeader("If-None-Match"), tag) {
			c.Status(http.StatusNotModified)
			return
		}
	}
	c.JSON(http.StatusOK, st)
}

// holds reports whether an If-None-Match names the entity tag.
func holds(ifNoneMatch, tag string) bool {
	for candidate := range strings.SplitSeq(ifNoneMatch, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == tag {
			return true
		}
	}
	return false
}

// getTraffic answers with the samples of the connectors' traffic. They are
// no part of the state, whose digest changes only with what a cycle finds.
func (s *Server) getTraffic(c *gin.Context) {
	c.JSON(http.StatusOK, s.engine.Traffic())
}

// getRouteTraffic answers with the rates of new connections to the target of
// one route, for its detail.
func (s *Server) getRouteTraffic(c *gin.Context) {
	hosts := c.QueryArray("hostname")
	if len(hosts) != 1 || hosts[0] == "" {
		s.fail(c, errNoRouteOf)
		return
	}
	series, err := s.engine.RouteSeries(hosts[0])
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, series)
}

func (s *Server) getEvents(c *gin.Context) {
	q, err := eventQuery(c)
	if err != nil {
		s.fail(c, err)
		return
	}
	events, err := s.engine.QueryEvents(q)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, nonNil(events))
}

// eventQuery reads the query of GET /v1/events. Every list may be given more
// than once; a value is matched exactly.
func eventQuery(c *gin.Context) (engine.EventQuery, error) {
	list := func(key string) []string {
		values, ok := c.GetQueryArray(key)
		if !ok {
			return nil
		}
		return values
	}
	q := engine.EventQuery{
		Boot:  c.Query("boot"),
		Route: list("route"), Guest: list("guest"), Tunnel: list("tunnel"),
		Account: list("account"), Kind: list("kind"), Level: list("level"),
	}
	var err error
	if raw, ok := c.GetQuery("since"); ok {
		if q.Since, err = time.Parse(time.RFC3339, raw); err != nil {
			return q, errBadSince
		}
	}
	if raw, ok := c.GetQuery("after"); ok {
		if q.After, err = strconv.ParseUint(raw, 10, 64); err != nil {
			return q, errBadAfter
		}
	}
	if raw, ok := c.GetQuery("limit"); ok {
		if q.Limit, err = strconv.Atoi(raw); err != nil || q.Limit < 1 || q.Limit > engine.MaxEventLimit {
			return q, errBadLimit
		}
	}
	switch raw, _ := c.GetQuery("history"); raw {
	case "", "0":
	case "1":
		q.History = true
	default:
		return q, errBadHistory
	}
	return q, nil
}

func (s *Server) postSync(c *gin.Context) {
	s.engine.Trigger()
	c.JSON(http.StatusAccepted, struct{}{})
}

// postApply passes on the offer of the state the admin was shown: a
// confirmation accepts what that state showed waiting, or nothing. The answer
// says what was accepted.
func (s *Server) postApply(c *gin.Context) {
	var req struct {
		ConfirmDeletes bool   `json:"confirmDeletes"`
		Offer          string `json:"offer"`
	}
	if err := decode(c, &req, true); err != nil {
		s.fail(c, err)
		return
	}
	res, err := s.engine.Apply(c.Request.Context(), req.ConfirmDeletes, req.Offer)
	if err != nil {
		s.fail(c, err)
		return
	}
	if res.Accepted == nil {
		res.Accepted = []engine.Waiting{}
	}
	c.JSON(http.StatusOK, res)
}

func (s *Server) postAdopt(c *gin.Context) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	if err := s.engine.Adopt(c.Request.Context(), req.Name); err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, struct{}{})
}

// postRotateTunnel rotates the secret of a tunnel, which restarts every
// connector of it. Of the users that may use the socket only root may ask,
// where the peer can be told.
func (s *Server) postRotateTunnel(c *gin.Context) {
	if uid, ok := peerUID(c.Request.Context()); s.checkPeers && (!ok || uid != 0) {
		s.fail(c, errRootOnly)
		return
	}
	var req struct {
		Account string `json:"account"`
	}
	if err := decode(c, &req, true); err != nil {
		s.fail(c, err)
		return
	}
	res, err := s.engine.RotateTunnel(c.Request.Context(), req.Account)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// getCredentials answers from the store and the last check of each token, not
// from the state of the last cycle.
func (s *Server) getCredentials(c *gin.Context) {
	creds, err := s.engine.Credentials()
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, nonNil(creds))
}

// postCredential takes the token from the body only and answers with the view
// of the stored credential, which has no token. When the engine refuses the
// token it hands back a view that carries what the token can and cannot do,
// and the answer carries it too.
func (s *Server) postCredential(c *gin.Context) {
	var req struct {
		Label string `json:"label"`
		Token string `json:"token"`
	}
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	token := strings.TrimSpace(req.Token)
	if !validToken(token) {
		s.fail(c, errBadToken)
		return
	}
	noteSecrets(c, req.Token, token)
	view, err := s.engine.AddCredential(c.Request.Context(), req.Label, token)
	if err != nil {
		var opts []failOption
		if view.Checked {
			opts = append(opts, withCredential(view))
		}
		s.fail(c, err, opts...)
		return
	}
	c.JSON(http.StatusCreated, view)
}

// validToken tells whether token has the shape of a Cloudflare API token. It
// keeps junk away from the engine and from Cloudflare, and keeps the scrubbing
// of messages from ever working on a one-letter secret.
func validToken(token string) bool {
	if len(token) < minTokenLength || len(token) > maxTokenLength {
		return false
	}
	for i := range len(token) {
		switch b := token[i]; {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '_', b == '-':
		default:
			return false
		}
	}
	return true
}

func (s *Server) postCredentialCheck(c *gin.Context) {
	var req struct {
		Deep bool `json:"deep"`
	}
	if err := decode(c, &req, true); err != nil {
		s.fail(c, err)
		return
	}
	view, err := s.engine.CheckCredential(c.Request.Context(), c.Param("id"), req.Deep)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

func (s *Server) deleteCredential(c *gin.Context) {
	if err := s.engine.RemoveCredential(c.Request.Context(), c.Param("id")); err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, struct{}{})
}

func (s *Server) getClaims(c *gin.Context) {
	claims, err := s.engine.Claims()
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, nonNil(claims))
}

// postResolveClaim hands the claim on a hostname to one of the owners that
// claim it.
func (s *Server) postResolveClaim(c *gin.Context) {
	var req struct {
		Hostname string `json:"hostname"`
		Owner    string `json:"owner"`
	}
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	if err := s.engine.ResolveClaim(c.Request.Context(), req.Hostname, req.Owner); err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, struct{}{})
}

func (s *Server) getApprovals(c *gin.Context) {
	approvals, err := s.engine.Approvals()
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, nonNil(approvals))
}

// postApproveGuest approves a guest and answers with the approval. The
// identity and the MACs, when the request has them, are those the admin was
// shown: the engine refuses a guest that has another identity, or waits for
// other MACs, now. The addresses are those the admin allows.
func (s *Server) postApproveGuest(c *gin.Context) {
	var req struct {
		Owner     string       `json:"owner"`
		Identity  string       `json:"identity"`
		MACs      []string     `json:"macs"`
		Addresses []netip.Addr `json:"addresses"`
	}
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	approved, err := s.engine.ApproveGuest(c.Request.Context(), req.Owner, req.Identity, req.MACs, req.Addresses)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, approved)
}

func (s *Server) postRevokeGuest(c *gin.Context) {
	var req struct {
		Owner string `json:"owner"`
	}
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	if err := s.engine.RevokeGuest(c.Request.Context(), req.Owner); err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, struct{}{})
}

func (s *Server) getSegments(c *gin.Context) {
	segments, err := s.engine.Segments()
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, nonNil(segments))
}

// segmentRequest names a segment: a bridge, and its VLAN or 0 for untagged.
type segmentRequest struct {
	Bridge string `json:"bridge"`
	VLAN   int    `json:"vlan"`
}

func (s *Server) postAcknowledgeSegment(c *gin.Context) {
	var req segmentRequest
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	if err := s.engine.AcknowledgeSegment(c.Request.Context(), req.Bridge, req.VLAN); err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, struct{}{})
}

func (s *Server) postRevokeSegment(c *gin.Context) {
	var req segmentRequest
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	if err := s.engine.RevokeSegment(c.Request.Context(), req.Bridge, req.VLAN); err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, struct{}{})
}

// getDiagnose walks the chain of the route of one hostname. The daemon asks
// the target the state shows for it, and nothing the request names.
func (s *Server) getDiagnose(c *gin.Context) {
	hosts := c.QueryArray("hostname")
	if len(hosts) != 1 || hosts[0] == "" {
		s.fail(c, errNoHostname)
		return
	}
	steps, err := s.engine.Diagnose(c.Request.Context(), hosts[0])
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, nonNil(steps))
}

func (s *Server) getDoctor(c *gin.Context) {
	c.JSON(http.StatusOK, nonNil(s.engine.Doctor(c.Request.Context())))
}

// nonNil makes a list that is missing an empty one, as every list of an
// answer is.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
