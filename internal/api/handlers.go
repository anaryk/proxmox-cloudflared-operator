package api

import (
	"net/http"
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
	errBadToken = &httpError{http.StatusBadRequest, codeInvalid, "the token is not a Cloudflare API token: it has 20 to 256 characters, all of A-Z a-z 0-9 _ -", false}
	errBadSince = &httpError{http.StatusBadRequest, codeInvalid, "since must be a time in RFC 3339 format", false}
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
	v1.POST("/sync", s.postSync)
	v1.POST("/apply", s.postApply)
	v1.POST("/adopt", s.postAdopt)
	v1.GET("/credentials", s.getCredentials)
	v1.POST("/credentials", s.postCredential)
	v1.POST("/credentials/:id/check", s.postCredentialCheck)
	v1.DELETE("/credentials/:id", s.deleteCredential)
	return s.logRequests(s.guard(r))
}

func (s *Server) getVersion(c *gin.Context) {
	c.JSON(http.StatusOK, struct {
		Version string `json:"version"`
	}{s.version})
}

func (s *Server) getState(c *gin.Context) {
	c.JSON(http.StatusOK, s.engine.State())
}

func (s *Server) getEvents(c *gin.Context) {
	var since time.Time
	if raw, ok := c.GetQuery("since"); ok {
		var err error
		if since, err = time.Parse(time.RFC3339, raw); err != nil {
			s.fail(c, errBadSince)
			return
		}
	}
	events := s.engine.Events(since)
	if events == nil {
		events = []engine.Event{}
	}
	c.JSON(http.StatusOK, events)
}

func (s *Server) postSync(c *gin.Context) {
	s.engine.Trigger()
	c.JSON(http.StatusAccepted, struct{}{})
}

func (s *Server) postApply(c *gin.Context) {
	var req struct {
		ConfirmDeletes bool `json:"confirmDeletes"`
	}
	if err := decode(c, &req, true); err != nil {
		s.fail(c, err)
		return
	}
	if _, err := s.engine.Apply(c.Request.Context(), req.ConfirmDeletes, ""); err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, struct{}{})
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

func (s *Server) getCredentials(c *gin.Context) {
	creds := s.engine.State().Credentials
	if creds == nil {
		creds = []engine.CredentialView{}
	}
	c.JSON(http.StatusOK, creds)
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
