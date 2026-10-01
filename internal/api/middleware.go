package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"runtime/debug"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
)

const maxBody = 1 << 20

var (
	errNotJSON    = &httpError{http.StatusUnsupportedMediaType, codeUnsupportedMediaType, "the content type must be application/json", true}
	errTooLarge   = &httpError{http.StatusRequestEntityTooLarge, codeTooLarge, "the request body is too large", true}
	errBodySlow   = &httpError{http.StatusBadRequest, codeInvalid, "the request body was not received in time", true}
	errBodyBroken = &httpError{http.StatusBadRequest, codeInvalid, "the request body could not be read", true}
	errNoBody     = &httpError{http.StatusBadRequest, codeInvalid, "this request takes no body", true}
)

type peerKey struct{}

// withPeerUID returns ctx carrying the uid of the process on the other end of
// the connection.
func withPeerUID(ctx context.Context, uid uint32) context.Context {
	return context.WithValue(ctx, peerKey{}, uid)
}

func peerUID(ctx context.Context) (uint32, bool) {
	uid, ok := ctx.Value(peerKey{}).(uint32)
	return uid, ok
}

// logRequests logs method, path, status and duration of every request, the
// refused ones included. It never looks at the query string or the body.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		s.log.Debug().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", sw.code()).
			Dur("duration", time.Since(start)).
			Msg("request")
	})
}

// statusWriter remembers the status of an answer. It unwraps, so that the
// response controller still reaches the connection.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// guard answers a request only when its peer is allowed. It stands in front of
// the router, so that nothing of the router runs for a refused peer: whatever
// the method, the path or the body, the answer is the same and says nothing
// about which routes exist.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.checkPeers {
			uid, ok := peerUID(r.Context())
			if !ok || !slices.Contains(s.allowedUIDs, uid) {
				// It answers instead of closing the connection, so that the
				// client can say why.
				s.writeError(w, r, errNotAllowed, failure{})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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
			Str("panic", redact(fmt.Sprint(v), secretsOf(c))).
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

// acceptJSON makes sure that a POST says it carries JSON and reads its body,
// within a size and a time that bound it, and that nothing else has a body. The body is read here, before any
// handler, for the sake of the deadline: it is set for the read, and is gone
// when the engine is called. A POST without a body has net/http watching the
// connection already, and a deadline that stayed on it would end the request
// context of an apply that takes a minute.
func (s *Server) acceptJSON(c *gin.Context) {
	if c.Request.Method != http.MethodPost {
		// Nothing else takes a body, and one that is not all there already
		// (a length of -1 is a chunked body) is not waited for: a peer could
		// hold the connection by never finishing it.
		if c.Request.ContentLength != 0 {
			s.fail(c, errNoBody)
		}
		return
	}
	rc := http.NewResponseController(c.Writer)
	_ = rc.SetReadDeadline(time.Now().Add(s.bodyTimeout))
	if mt, _, err := mime.ParseMediaType(c.GetHeader("Content-Type")); err != nil || mt != "application/json" {
		s.fail(c, errNotJSON)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxBody))
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		s.fail(c, bodyReadError(err))
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
}

// bodyReadError says why the body of a request could not be read.
func bodyReadError(err error) *httpError {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return errTooLarge
	case errors.Is(err, os.ErrDeadlineExceeded):
		return errBodySlow
	}
	return errBodyBroken
}
