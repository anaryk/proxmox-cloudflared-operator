package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// The codes of an error answer. A client acts on the code; the status says the
// same to anything else that speaks HTTP.
const (
	codeInvalid              = "invalid"
	codeNotFound             = "not_found"
	codeRefused              = "refused"
	codeUnavailable          = "unavailable"
	codeForbidden            = "forbidden"
	codeNoRoute              = "no_route"
	codeMethodNotAllowed     = "method_not_allowed"
	codeUnsupportedMediaType = "unsupported_media_type"
	codeTooLarge             = "too_large"
	codeInternal             = "internal"
)

const redacted = "[redacted]"

// errorBody is every error answer. The credential is there when the engine
// refused a token and has a report to show for it.
type errorBody struct {
	Error      string          `json:"error"`
	Code       string          `json:"code"`
	Credential json.RawMessage `json:"credential,omitempty"`
}

// httpError is a problem found before the engine was asked anything.
type httpError struct {
	status int
	code   string
	msg    string
	// stop is set where the peer has not been read to the end and is not
	// going to be: the answer closes the connection at once.
	stop bool
}

func (e *httpError) Error() string { return e.msg }

var (
	errNotAllowed = &httpError{http.StatusForbidden, codeForbidden, "not allowed", true}
	errNoRoute    = &httpError{http.StatusNotFound, codeNoRoute, "no such route", false}
	errNoMethod   = &httpError{http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed", false}
	errInternal   = &httpError{http.StatusInternalServerError, codeInternal, "internal error", false}
)

// answered is what an error is answered with.
type answered struct {
	status int
	code   string
	msg    string
	stop   bool
}

// answer maps an error to its answer. The engine's own verdicts come first: an
// engine that refused because Cloudflare did not answer in time has said what
// it did, and the timeout is only the reason.
func answer(err error) answered {
	var he *httpError
	switch {
	case errors.As(err, &he):
		return answered{he.status, he.code, he.msg, he.stop}
	case errors.Is(err, engine.ErrInvalid):
		return answered{http.StatusBadRequest, codeInvalid, err.Error(), false}
	case errors.Is(err, engine.ErrNotFound):
		return answered{http.StatusNotFound, codeNotFound, err.Error(), false}
	case errors.Is(err, engine.ErrRefused):
		return answered{http.StatusConflict, codeRefused, err.Error(), false}
	case errors.Is(err, context.DeadlineExceeded):
		return answered{http.StatusServiceUnavailable, codeUnavailable, "the operation timed out", false}
	case errors.Is(err, context.Canceled):
		return answered{http.StatusServiceUnavailable, codeUnavailable, "the operation was cancelled", false}
	}
	return answered{http.StatusInternalServerError, codeInternal, err.Error(), false}
}

// failure is what an error answer carries besides the error.
type failure struct {
	secrets    []string
	credential *engine.CredentialView
}

type failOption func(*failure)

// withCredential adds the view of the credential that was refused, report
// included, to the answer.
func withCredential(v engine.CredentialView) failOption {
	return func(f *failure) { f.credential = &v }
}

const secretsKey = "pco.secrets"

// noteSecrets tells the request which values must not leave it, in an answer or
// in a log: whatever fails after this is scrubbed of them.
func noteSecrets(c *gin.Context, secrets ...string) {
	c.Set(secretsKey, append(secretsOf(c), secrets...))
}

func secretsOf(c *gin.Context) []string {
	v, _ := c.Get(secretsKey)
	list, _ := v.([]string)
	return list
}

// fail answers with the error and stops the request.
func (s *Server) fail(c *gin.Context, err error, opts ...failOption) {
	f := failure{secrets: secretsOf(c)}
	for _, opt := range opts {
		opt(&f)
	}
	s.writeError(c.Writer, c.Request, err, f)
	c.Abort()
}

// writeError is where every error answer is made. Nothing else writes one, so
// this is the one place that sees to it that nothing secret leaves with an
// error: the secrets of the request are cut out of the message, out of the
// credential's report and out of the log line.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error, f failure) {
	a := answer(err)
	s.logFailure(r, err, a, f.secrets)
	if a.stop {
		w.Header().Set("Connection", "close")
		// What the peer is still sending is not read, and a peer that stalls
		// cannot hold the connection open with it.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now())
	}
	body := errorBody{Error: redact(a.msg, f.secrets), Code: a.code}
	if f.credential != nil {
		body.Credential = scrubbed(*f.credential, f.secrets)
	}
	// Strings and a JSON text that was checked: this cannot fail.
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(a.status)
	_, _ = w.Write(data)
}

// logFailure logs what went wrong on the server's side. A request that the
// caller gave up on is not an error.
func (s *Server) logFailure(r *http.Request, err error, a answered, secrets []string) {
	if a.status < http.StatusInternalServerError || errors.Is(err, errInternal) {
		return // the panic that made an internal error has been logged
	}
	ev := s.log.Error()
	if errors.Is(err, context.Canceled) {
		ev = s.log.Debug()
	}
	ev.Str("method", r.Method).
		Str("path", r.URL.Path).
		Int("status", a.status).
		Str("error", redact(err.Error(), secrets)).
		Msg("request failed")
}

// scrubbed returns the view as JSON with the secrets cut out. A view that
// cannot be scrubbed into valid JSON is left out.
func scrubbed(v engine.CredentialView, secrets []string) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	data = []byte(redact(string(data), secrets))
	if !json.Valid(data) {
		return nil
	}
	return data
}

// redact cuts every occurrence of the secrets out of msg: as given, without
// surrounding white space, and as the inside of a quoted Go string, which is
// how an error that formats with %q has them. The longest go first, so that no
// piece of one is left behind another.
func redact(msg string, secrets []string) string {
	var forms []string
	for _, secret := range secrets {
		trimmed := strings.TrimSpace(secret)
		if trimmed == "" {
			continue
		}
		for _, form := range []string{secret, trimmed} {
			quoted := strconv.Quote(form)
			forms = append(forms, form, quoted[1:len(quoted)-1])
		}
	}
	slices.SortFunc(forms, func(a, b string) int { return len(b) - len(a) })
	for _, form := range forms {
		msg = strings.ReplaceAll(msg, form, redacted)
	}
	return msg
}
