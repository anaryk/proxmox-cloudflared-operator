package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var (
	errNoRev         = &engine.FieldError{Field: "rev", Err: errors.New("rev: give the revision the settings were read at")}
	errNoSettings    = &engine.FieldError{Field: "settings", Err: errors.New("settings: give the settings to save")}
	errRevOfNewRoute = &engine.FieldError{Field: "rev", Err: errors.New("rev: a new manual route has no revision; " +
		"to change one, PUT /v1/routes/manual/<id> with the revision it was read at")}
	errRevToDelete = &engine.FieldError{Field: "rev", Err: errors.New("rev: give the revision the route was read at, " +
		"as /v1/routes/manual/<id>?rev=<n>")}
)

// configRoutes adds the routes of the settings, the manual routes and the
// guests: what the admin configures, written at the revision it was read at.
func (s *Server) configRoutes(v1 *gin.RouterGroup) {
	v1.GET("/settings", s.getSettings)
	v1.PUT("/settings", s.putSettings)
	v1.GET("/routes/manual", s.getManualRoutes)
	v1.POST("/routes/manual", s.postManualRoute)
	v1.PUT("/routes/manual/:id", s.putManualRoute)
	v1.DELETE("/routes/manual/:id", s.deleteManualRoute)
	v1.GET("/guests", s.getGuests)
	v1.GET("/guests/:kind/:vmid/annotation", s.getAnnotation)
	v1.POST("/daemon/restart", s.postRestart)
}

// SavedSettings is the answer of PUT /v1/settings: the settings as saved,
// and those of them that take effect only once the daemon is restarted.
type SavedSettings struct {
	engine.SettingsView
	RestartNeeded []string `json:"restartNeeded"`
}

func (s *Server) getSettings(c *gin.Context) {
	v, err := s.engine.SettingsView()
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

// putSettings saves the settings with the revision they were read at. They
// are decoded as strictly as the store reads them: a key they have no field
// for is refused.
func (s *Server) putSettings(c *gin.Context) {
	var req struct {
		Rev      *int            `json:"rev"`
		Settings *store.Settings `json:"settings"`
	}
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	switch {
	case req.Rev == nil:
		s.fail(c, errNoRev)
		return
	case req.Settings == nil:
		s.fail(c, errNoSettings)
		return
	}
	v, restart, err := s.engine.SaveSettings(c.Request.Context(), *req.Rev, *req.Settings)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, SavedSettings{SettingsView: v, RestartNeeded: nonNil(restart)})
}

func (s *Server) getManualRoutes(c *gin.Context) {
	routes, err := s.engine.ManualRoutes()
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, nonNil(routes))
}

// postManualRoute makes a manual route; without an id the daemon gives it
// one.
func (s *Server) postManualRoute(c *gin.Context) {
	var req engine.ManualRouteView
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	if req.Rev != 0 {
		s.fail(c, errRevOfNewRoute)
		return
	}
	v, err := s.engine.CreateManualRoute(c.Request.Context(), req)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, v)
}

// putManualRoute replaces a manual route with the revision it was read at.
func (s *Server) putManualRoute(c *gin.Context) {
	var req engine.ManualRouteView
	if err := decode(c, &req, false); err != nil {
		s.fail(c, err)
		return
	}
	v, err := s.engine.UpdateManualRoute(c.Request.Context(), c.Param("id"), req.Rev, req)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

// deleteManualRoute removes a manual route with the revision it was read at,
// which the query names: a DELETE has no body.
func (s *Server) deleteManualRoute(c *gin.Context) {
	revs := c.QueryArray("rev")
	if len(revs) != 1 {
		s.fail(c, errRevToDelete)
		return
	}
	rev, err := strconv.Atoi(revs[0])
	if err != nil || rev < 1 {
		s.fail(c, errRevToDelete)
		return
	}
	if err := s.engine.DeleteManualRoute(c.Request.Context(), c.Param("id"), rev); err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, struct{}{})
}

func (s *Server) getGuests(c *gin.Context) {
	guests, err := s.engine.Guests()
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, nonNil(guests))
}

// getAnnotation answers with the route block of the Notes of a guest, and
// nothing else of them.
func (s *Server) getAnnotation(c *gin.Context) {
	ref, err := model.ParseGuestRef(c.Param("kind") + "/" + c.Param("vmid"))
	if err != nil {
		s.fail(c, &httpError{http.StatusBadRequest, codeInvalid, "name a guest as /v1/guests/qemu/101/annotation", false})
		return
	}
	v, err := s.engine.Annotation(ref)
	if err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

// postRestart asks the daemon to stop once the running cycle is done, for
// systemd to start it again; the answer comes before it stops.
func (s *Server) postRestart(c *gin.Context) {
	if err := s.engine.RequestRestart(c.Request.Context()); err != nil {
		s.fail(c, err)
		return
	}
	c.JSON(http.StatusAccepted, struct{}{})
}
