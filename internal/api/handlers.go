package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const (
	maxBody  = 1 << 20
	redacted = "[redacted]"
)

type errorBody struct {
	Error string `json:"error"`
}

// httpError is a problem with the request itself, found before the engine was
// asked anything.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

var (
	errNotAllowed = &httpError{http.StatusForbidden, "not allowed"}
	errInternal   = &httpError{http.StatusInternalServerError, "internal error"}
)

// routes builds the gin engine. gin keeps its mode in a global; release mode
// stops it from printing the routes.
func (s *Server) routes() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false
	r.Use(s.logRequests, s.recoverPanics, s.checkPeer)
	r.NoRoute(func(c *gin.Context) { s.fail(c, &httpError{http.StatusNotFound, "no such route"}) })
	r.NoMethod(func(c *gin.Context) { s.fail(c, &httpError{http.StatusMethodNotAllowed, "method not allowed"}) })

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
	return r
}

// logRequests logs method, path, status and duration. It never looks at the
// query string or the body.
func (s *Server) logRequests(c *gin.Context) {
	start := time.Now()
	c.Next()
	s.log.Debug().
		Str("method", c.Request.Method).
		Str("path", c.Request.URL.Path).
		Int("status", c.Writer.Status()).
		Dur("duration", time.Since(start)).
		Msg("request")
}

// recoverPanics answers a panicking handler with a 500 and logs the panic.
func (s *Server) recoverPanics(c *gin.Context) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
			panic(v)
		}
		s.log.Error().
			Str("method", c.Request.Method).
			Str("path", c.Request.URL.Path).
			Str("panic", fmt.Sprint(v)).
			Bytes("stack", debug.Stack()).
			Msg("request handler panicked")
		if c.Writer.Written() {
			c.Abort()
			return
		}
		s.fail(c, errInternal)
	}()
	c.Next()
}

// checkPeer refuses a request unless its connection comes from an allowed user.
// It answers instead of closing the connection, so that the client can say why.
func (s *Server) checkPeer(c *gin.Context) {
	if !enforcePeers {
		return
	}
	if uid, ok := peerUID(c.Request.Context()); ok && slices.Contains(s.allowedUIDs, uid) {
		return
	}
	s.fail(c, errNotAllowed)
}

// acceptJSON limits the body of a request and requires a POST to say that it
// carries JSON.
func (s *Server) acceptJSON(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBody)
	if c.Request.Method != http.MethodPost {
		return
	}
	if mt, _, err := mime.ParseMediaType(c.GetHeader("Content-Type")); err != nil || mt != "application/json" {
		s.fail(c, &httpError{http.StatusUnsupportedMediaType, "the content type must be application/json"})
	}
}

// fail answers with the error and stops the request. Every error that leaves
// the API goes through here, so that this is the one place that sees to it that
// nothing secret leaves with it: the submitted secrets are cut out of the
// message, in what the client sees and in what is logged.
func (s *Server) fail(c *gin.Context, err error, secrets ...string) {
	status, msg := answer(err)
	if status >= http.StatusInternalServerError {
		s.log.Error().
			Str("method", c.Request.Method).
			Str("path", c.Request.URL.Path).
			Int("status", status).
			Str("error", redact(err.Error(), secrets)).
			Msg("request failed")
	}
	c.AbortWithStatusJSON(status, errorBody{Error: redact(msg, secrets)})
}

// answer maps an error to a status and a message. The engine's own verdicts
// come first: an engine that refused because Cloudflare did not answer in time
// has said what it did, and the timeout is only the reason.
func answer(err error) (int, string) {
	var he *httpError
	switch {
	case errors.As(err, &he):
		return he.status, he.msg
	case errors.Is(err, engine.ErrInvalid):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, engine.ErrNotFound):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, engine.ErrRefused):
		return http.StatusConflict, err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, "the operation timed out"
	case errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable, "the operation was cancelled"
	}
	return http.StatusInternalServerError, err.Error()
}

// redact cuts every occurrence of the secrets out of msg, longest first.
func redact(msg string, secrets []string) string {
	secrets = slices.DeleteFunc(slices.Clone(secrets), func(s string) bool { return s == "" })
	slices.SortStableFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	for _, secret := range secrets {
		msg = strings.ReplaceAll(msg, secret, redacted)
	}
	return msg
}

// decode reads the JSON object of a request into dst. An empty body is an
// error unless optional, which leaves dst as it is.
func decode(c *gin.Context, dst any, optional bool) error {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	switch {
	case errors.Is(err, io.EOF):
		if optional {
			return nil
		}
		return &httpError{http.StatusBadRequest, "the request body is empty"}
	case err != nil:
		return bodyError(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if isTooLarge(err) {
			return bodyError(err)
		}
		return &httpError{http.StatusBadRequest, "unexpected data after the JSON object"}
	}
	return nil
}

func isTooLarge(err error) bool {
	var tooLarge *http.MaxBytesError
	return errors.As(err, &tooLarge)
}

// bodyError describes a body that could not be read. It says what is wrong
// without repeating what was sent: a syntax error of encoding/json quotes the
// offending character, and a secret may be what it quotes.
func bodyError(err error) *httpError {
	var (
		syntax *json.SyntaxError
		typ    *json.UnmarshalTypeError
	)
	switch {
	case isTooLarge(err):
		return &httpError{http.StatusRequestEntityTooLarge, "the request body is too large"}
	case errors.Is(err, io.ErrUnexpectedEOF), errors.As(err, &syntax):
		return &httpError{http.StatusBadRequest, "the request body is not valid JSON"}
	case errors.As(err, &typ) && typ.Field != "":
		return &httpError{http.StatusBadRequest, fmt.Sprintf("the field %q has the wrong type", typ.Field)}
	case errors.As(err, &typ):
		return &httpError{http.StatusBadRequest, "the request body must be a JSON object"}
	}
	if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return &httpError{http.StatusBadRequest, "unknown field " + field}
	}
	return &httpError{http.StatusBadRequest, "the request body is not valid"}
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
			s.fail(c, &httpError{http.StatusBadRequest, "since must be a time in RFC 3339 format"})
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
	if err := s.engine.Apply(c.Request.Context(), req.ConfirmDeletes); err != nil {
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
// of the stored credential, which has no token.
func (s *Server) postCredential(c *gin.Context) {
	var req struct {
		Label string `json:"label"`
		Token string `json:"token"`
	}
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	// The engine trims what it is given, so its errors may quote either form.
	secrets := []string{req.Token, strings.TrimSpace(req.Token)}
	view, err := s.engine.AddCredential(c.Request.Context(), req.Label, req.Token)
	if err != nil {
		s.fail(c, err, secrets...)
		return
	}
	c.JSON(http.StatusCreated, view)
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
