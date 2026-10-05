package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
)

// checkedServer returns a server that checks peers on every platform. On the
// socket nobody is allowed; a request that is called directly carries the uid
// of its peer in its context.
func checkedServer(e *fakeEngine, allowed ...uint32) *Server {
	s := New(e, "1.2.3", allowed, zerolog.Nop())
	s.checkPeers = true
	return s
}

func TestADisallowedPeerGetsNothingElse(t *testing.T) {
	f := &fakeEngine{}
	s := checkedServer(f, testUID)
	stranger := testUID + 1

	for _, method := range []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodTrace,
	} {
		t.Run(method, func(t *testing.T) {
			existing := send(s, requestFrom(stranger, method, "/v1/state", ""))
			missing := send(s, requestFrom(stranger, method, "/v1/definitely/not/here", ""))

			require.Equal(t, http.StatusForbidden, existing.Code)
			require.JSONEq(t, `{"error":"not allowed","code":"forbidden"}`, existing.Body.String())
			require.Equal(t, "close", existing.Header().Get("Connection"))
			require.Empty(t, existing.Header().Values("Allow"), "the router must not have run")
			require.Equal(t, existing.Code, missing.Code)
			require.Equal(t, existing.Body.String(), missing.Body.String())
			require.Equal(t, existing.Header(), missing.Header())
		})
	}

	t.Run("whatever the body and the content type", func(t *testing.T) {
		for name, req := range map[string]*http.Request{
			"no content type": func() *http.Request {
				r := requestFrom(stranger, http.MethodPost, "/v1/apply", `{}`)
				r.Header.Del("Content-Type")
				return r
			}(),
			"nonsense":  requestFrom(stranger, http.MethodPost, "/v1/apply", `nonsense`),
			"too large": requestFrom(stranger, http.MethodPost, "/v1/apply", strings.Repeat(" ", 2*maxBody)),
			"delete":    requestFrom(stranger, http.MethodDelete, "/v1/credentials/abc12345", ""),
		} {
			rec := send(s, req)

			require.Equal(t, http.StatusForbidden, rec.Code, name)
			require.JSONEq(t, `{"error":"not allowed","code":"forbidden"}`, rec.Body.String(), name)
		}
	})
	require.Empty(t, f.called())
}

func TestARequestWithoutPeerCredentialsIsRefused(t *testing.T) {
	f := &fakeEngine{}
	s := checkedServer(f, 0, testUID)

	rec := send(s, httptest.NewRequest(http.MethodGet, "/v1/version", nil)) // no uid in its context

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "forbidden", errorCode(t, rec))
	require.Empty(t, f.called())
}

func TestAnAllowedPeerIsAnswered(t *testing.T) {
	for _, tt := range []struct {
		name    string
		allowed []uint32
		check   bool
		status  int
	}{
		{"the only one allowed", []uint32{7}, true, http.StatusOK},
		{"one of several", []uint32{3, 7, 11}, true, http.StatusOK},
		{"not among them", []uint32{3, 11}, true, http.StatusForbidden},
		{"nobody is allowed", nil, true, http.StatusForbidden},
		{"nobody is allowed but peers are not checked", nil, false, http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := New(&fakeEngine{}, "1.2.3", tt.allowed, zerolog.Nop())
			s.checkPeers = tt.check

			rec := send(s, requestFrom(7, http.MethodGet, "/v1/version", ""))

			require.Equal(t, tt.status, rec.Code)
		})
	}
}

func TestARefusedRequestIsLogged(t *testing.T) {
	var logged syncBuffer
	s := New(&fakeEngine{}, "v", nil, zerolog.New(&logged).Level(zerolog.DebugLevel))
	s.checkPeers = true

	send(s, requestFrom(5, http.MethodGet, "/v1/state?token=querysecret", ""))

	var line map[string]any
	require.NoError(t, json.Unmarshal([]byte(logged.String()), &line), logged.String())
	require.Equal(t, "/v1/state", line["path"])
	require.EqualValues(t, http.StatusForbidden, line["status"])
	require.NotContains(t, logged.String(), "querysecret")
}

// rawConn opens a connection to the socket. A read that waits for more than the
// deadline fails the test instead of hanging it.
func rawConn(t *testing.T, socket string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
	return conn, bufio.NewReader(conn)
}

// answerAndClose reads one response and the end of the connection, which must
// follow it: the connection is not kept for another request. It returns the
// status and the body of the response.
func answerAndClose(t *testing.T, br *bufio.Reader) (int, string) {
	t.Helper()
	res, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	require.True(t, res.Close, "the answer must say that the connection is closed")
	_, err = br.ReadByte()
	require.ErrorIs(t, err, io.EOF, "the connection must be closed after the answer")
	return res.StatusCode, string(body)
}

func TestARefusedConnectionIsClosedAtOnce(t *testing.T) {
	socket := socketPath(t)
	serve(t, checkedServer(&fakeEngine{}), socket)
	conn, br := rawConn(t, socket)

	_, err := io.WriteString(conn, "GET /v1/version HTTP/1.1\r\nHost: pco\r\n\r\n")
	require.NoError(t, err)

	status, body := answerAndClose(t, br)
	require.Equal(t, http.StatusForbidden, status)
	require.JSONEq(t, `{"error":"not allowed","code":"forbidden"}`, body)
}

func TestARefusedPeerCannotStallAnAnswer(t *testing.T) {
	f := &fakeEngine{}
	socket := socketPath(t)
	serve(t, checkedServer(f), socket)
	conn, br := rawConn(t, socket)

	// A chunked body that announces 16 bytes, sends 8 and then goes quiet.
	_, err := io.WriteString(conn, "POST /v1/apply HTTP/1.1\r\nHost: pco\r\nContent-Type: application/json\r\n"+
		"Transfer-Encoding: chunked\r\n\r\n10\r\n{\"confirm")
	require.NoError(t, err)

	status, body := answerAndClose(t, br)
	require.Equal(t, http.StatusForbidden, status)
	require.JSONEq(t, `{"error":"not allowed","code":"forbidden"}`, body)
	require.Empty(t, f.called())
}

func TestAnAllowedPeersStalledBodyIsCutOff(t *testing.T) {
	f := &fakeEngine{}
	s := newServer(f)
	s.bodyTimeout = 50 * time.Millisecond
	socket := socketPath(t)
	serve(t, s, socket)
	conn, br := rawConn(t, socket)

	_, err := io.WriteString(conn, "POST /v1/apply HTTP/1.1\r\nHost: pco\r\nContent-Type: application/json\r\n"+
		"Transfer-Encoding: chunked\r\n\r\n10\r\n{\"confirm")
	require.NoError(t, err)

	status, body := answerAndClose(t, br)
	require.Equal(t, http.StatusBadRequest, status)
	var answer struct{ Error, Code string }
	require.NoError(t, json.Unmarshal([]byte(body), &answer))
	require.Equal(t, "invalid", answer.Code)
	require.Contains(t, answer.Error, "in time")
	require.Empty(t, f.called())
}

func TestAnAllowedPeerWithAWrongContentTypeCannotStallEither(t *testing.T) {
	s := newServer(&fakeEngine{})
	s.bodyTimeout = 50 * time.Millisecond
	socket := socketPath(t)
	serve(t, s, socket)
	conn, br := rawConn(t, socket)

	_, err := io.WriteString(conn, "POST /v1/apply HTTP/1.1\r\nHost: pco\r\nContent-Type: text/plain\r\n"+
		"Transfer-Encoding: chunked\r\n\r\n10\r\nconfirm")
	require.NoError(t, err)

	status, _ := answerAndClose(t, br)
	require.Equal(t, http.StatusUnsupportedMediaType, status)
}

func TestAnAllowedPeerCannotStallWhereNoBodyIsExpected(t *testing.T) {
	// The body timeout stays at its default of half a minute: the answer must
	// come at once, not when the timeout ends.
	f := &fakeEngine{}
	socket := socketPath(t)
	serve(t, newServer(f), socket)
	const (
		chunked = "Transfer-Encoding: chunked\r\n\r\n10\r\n{\"confirm"
		counted = "Content-Length: 16\r\n\r\n{\"confirm"
	)
	for _, tt := range []struct {
		name, method, target, body string
		status                     int
		code                       string
	}{
		{"a get with a chunked body", http.MethodGet, "/v1/state", chunked, http.StatusBadRequest, "invalid"},
		{"a get with a counted body", http.MethodGet, "/v1/state", counted, http.StatusBadRequest, "invalid"},
		{"a delete with a chunked body", http.MethodDelete, "/v1/credentials/abc12345", chunked, http.StatusBadRequest, "invalid"},
		{"a delete with a counted body", http.MethodDelete, "/v1/credentials/abc12345", counted, http.StatusBadRequest, "invalid"},
		{"a get to an unknown route", http.MethodGet, "/v1/nope", chunked, http.StatusNotFound, "no_route"},
		{"a post to an unknown route", http.MethodPost, "/v1/nope", chunked, http.StatusNotFound, "no_route"},
		{"the wrong method", http.MethodPut, "/v1/state", chunked, http.StatusMethodNotAllowed, "method_not_allowed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn, br := rawConn(t, socket)

			_, err := io.WriteString(conn, tt.method+" "+tt.target+" HTTP/1.1\r\nHost: pco\r\nContent-Type: application/json\r\n"+tt.body)
			require.NoError(t, err)

			status, body := answerAndClose(t, br)
			require.Equal(t, tt.status, status, body)
			var answer struct{ Error, Code string }
			require.NoError(t, json.Unmarshal([]byte(body), &answer))
			require.Equal(t, tt.code, answer.Code)
		})
	}
	require.Empty(t, f.called())
}

func TestTheBodyDeadlineDoesNotCancelALongRequest(t *testing.T) {
	const timeout = 20 * time.Millisecond
	// The engine runs for ten times as long as the body may take to arrive. If
	// the deadline stayed on the connection, the request context would end.
	hook := func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return fmt.Errorf("the request context ended: %w", ctx.Err())
		case <-time.After(10 * timeout):
			return nil
		}
	}

	t.Run("with a body", func(t *testing.T) {
		s := newServer(&fakeEngine{hook: hook})
		s.bodyTimeout = timeout
		socket := socketPath(t)
		serve(t, s, socket)

		_, err := apiclient.New(socket).Apply(t.Context(), false, "")
		require.NoError(t, err)
	})

	// Without a body net/http starts to watch the connection before the handler
	// runs, and a deadline set from then on is what would end the context.
	t.Run("without a body", func(t *testing.T) {
		s := newServer(&fakeEngine{hook: hook})
		s.bodyTimeout = timeout
		socket := socketPath(t)
		serve(t, s, socket)
		conn, br := rawConn(t, socket)

		_, err := io.WriteString(conn, "POST /v1/apply HTTP/1.1\r\nHost: pco\r\nContent-Type: application/json\r\nContent-Length: 0\r\n\r\n")
		require.NoError(t, err)

		res, err := http.ReadResponse(br, nil)
		require.NoError(t, err)
		defer func() { _ = res.Body.Close() }()
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.StatusCode, string(body))
	})
}

// logLines parses what a logger wrote, one JSON object per line.
func logLines(t *testing.T, logged *syncBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), line)
		out = append(out, m)
	}
	return out
}

func TestRequestLogCarriesNoBodiesOrQueries(t *testing.T) {
	var logged syncBuffer
	s := New(&fakeEngine{}, "v", []uint32{testUID}, zerolog.New(&logged).Level(zerolog.DebugLevel))

	rec := send(s, request(http.MethodPost, "/v1/credentials?token=querysecret&x=1", `{"label":"main","token":"bodysecret0123456789abc"}`))

	require.Equal(t, http.StatusCreated, rec.Code)
	lines := logLines(t, &logged)
	require.Len(t, lines, 1)
	require.Equal(t, "debug", lines[0]["level"])
	require.Equal(t, "POST", lines[0]["method"])
	require.Equal(t, "/v1/credentials", lines[0]["path"])
	require.EqualValues(t, http.StatusCreated, lines[0]["status"])
	require.Contains(t, lines[0], "duration")
	for _, secret := range []string{"querysecret", "bodysecret", "token=", "x=1"} {
		require.NotContains(t, logged.String(), secret)
	}
}

func TestRequestLogIsDebugOnly(t *testing.T) {
	var logged syncBuffer
	s := New(&fakeEngine{}, "v", []uint32{testUID}, zerolog.New(&logged).Level(zerolog.InfoLevel))

	do(s, http.MethodGet, "/v1/version", "")

	require.Empty(t, logged.String())
}

func TestAPanicIsLoggedAndAnswered(t *testing.T) {
	var logged syncBuffer
	s := New(&fakeEngine{panics: true}, "v", []uint32{testUID}, zerolog.New(&logged).Level(zerolog.DebugLevel))

	rec := send(s, request(http.MethodGet, "/v1/state?q=querysecret", ""))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "internal error", errorMessage(t, rec))
	require.Equal(t, "internal", errorCode(t, rec))
	require.Contains(t, logged.String(), "state exploded")
	require.Contains(t, logged.String(), `"level":"error"`)
	require.NotContains(t, logged.String(), "querysecret")

	t.Run("and the server goes on", func(t *testing.T) {
		rec := send(s, request(http.MethodGet, "/v1/events", ""))
		require.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestAPanicDoesNotLogTheToken(t *testing.T) {
	var logged syncBuffer
	f := &fakeEngine{addPanic: "cannot use " + testToken + " here"}
	s := New(f, "v", []uint32{testUID}, zerolog.New(&logged).Level(zerolog.DebugLevel))

	rec := send(s, request(http.MethodPost, "/v1/credentials", `{"label":"main","token":"`+testToken+`"}`))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotContains(t, rec.Body.String(), testToken)
	require.Contains(t, logged.String(), "cannot use [redacted] here")
	require.NotContains(t, logged.String(), testToken)
}

func TestServerErrorsAreLogged(t *testing.T) {
	var logged syncBuffer
	s := New(&fakeEngine{err: fmt.Errorf("disk gone")}, "v", []uint32{testUID}, zerolog.New(&logged).Level(zerolog.InfoLevel))

	rec := do(s, http.MethodPost, "/v1/apply", `{}`)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, logged.String(), "disk gone")
	require.Contains(t, logged.String(), `"level":"error"`)
}

func TestACancelledRequestIsLoggedAtDebug(t *testing.T) {
	cancelled := fmt.Errorf("checking the token: %w", context.Canceled)

	t.Run("info level says nothing", func(t *testing.T) {
		var logged syncBuffer
		s := New(&fakeEngine{err: cancelled}, "v", []uint32{testUID}, zerolog.New(&logged).Level(zerolog.InfoLevel))

		rec := do(s, http.MethodPost, "/v1/apply", `{}`)

		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		require.Empty(t, logged.String())
	})

	t.Run("debug level says it at debug", func(t *testing.T) {
		var logged syncBuffer
		s := New(&fakeEngine{err: cancelled}, "v", []uint32{testUID}, zerolog.New(&logged).Level(zerolog.DebugLevel))

		do(s, http.MethodPost, "/v1/apply", `{}`)

		var sawCancel bool
		for _, line := range logLines(t, &logged) {
			require.NotEqual(t, "error", line["level"], line)
			require.NotEqual(t, "warn", line["level"], line)
			if strings.Contains(fmt.Sprint(line["error"]), "context canceled") {
				sawCancel = true
			}
		}
		require.True(t, sawCancel, "the cancellation should be in the debug log: %s", logged.String())
	})

	t.Run("a timeout is still an error", func(t *testing.T) {
		var logged syncBuffer
		timeout := fmt.Errorf("checking the token: %w", context.DeadlineExceeded)
		s := New(&fakeEngine{err: timeout}, "v", []uint32{testUID}, zerolog.New(&logged).Level(zerolog.InfoLevel))

		do(s, http.MethodPost, "/v1/apply", `{}`)

		require.Contains(t, logged.String(), `"level":"error"`)
		require.Contains(t, logged.String(), "deadline exceeded")
	})
}
