package api

import (
	"net/http"
	"regexp"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const (
	// actorHeader is where the web UI names the user it asks for.
	actorHeader = "Pco-Actor"
	// actorCLI is who the events of an admin action say asked, when root did.
	actorCLI = "root (cli)"
)

// actorForm is what the web UI may name as the actor: "alice@pve (ticket)".
var actorForm = regexp.MustCompile(`^[A-Za-z0-9._@!:() -]{1,128}$`)

// errBadActor is answered before the body is read, which is then not going to
// be.
var errBadActor = &httpError{http.StatusBadRequest, codeInvalid,
	"the Pco-Actor header is not an actor: 1 to 128 of the letters A-Z and a-z, the digits and . _ @ ! : ( ) - and space", true}

// SetWebUID names the user of the web UI, whose requests name their actor in
// the Pco-Actor header; 0, as root never is the web UI, is none. It has to be
// set before Serve is called.
func (s *Server) SetWebUID(uid uint32) { s.webUID = uid }

// stamp gives every answer to an allowed peer the boot of the daemon, and
// makes who asks the actor of the request: the command line when the peer is
// root, the user the web UI names when it is the web UI.
func (s *Server) stamp(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(bootHeader, s.engine.Boot())
		actor, err := s.actorOf(r)
		if err != nil {
			s.writeError(w, r, err, failure{})
			return
		}
		if actor != "" {
			r = r.WithContext(engine.WithActor(r.Context(), actor))
		}
		next.ServeHTTP(w, r)
	})
}

// actorOf is the actor of a request. The header is read only from the web
// UI's user, as the peer credentials tell it; anyone else's is ignored.
func (s *Server) actorOf(r *http.Request) (string, error) {
	uid, ok := peerUID(r.Context())
	switch {
	case !ok:
		return "", nil
	case uid == 0:
		return actorCLI, nil
	case s.webUID == 0 || uid != s.webUID:
		return "", nil
	}
	values := r.Header.Values(actorHeader)
	switch {
	case len(values) == 0:
		return "", nil
	case len(values) > 1 || !actorForm.MatchString(values[0]):
		return "", errBadActor
	}
	return values[0], nil
}
