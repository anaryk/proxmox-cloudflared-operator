package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const (
	webUID   = 998
	otherUID = 1000
)

// webServer answers root, the web UI's user and another one.
func webServer(f *fakeEngine) *Server {
	s := New(f, "1.2.3", []uint32{0, webUID, otherUID}, zerolog.Nop())
	s.SetWebUID(webUID)
	return s
}

func adoptFrom(uid uint32, actors ...string) *http.Request {
	req := requestFrom(uid, http.MethodPost, "/v1/adopt", `{"name":"www.example.com"}`)
	for _, a := range actors {
		req.Header.Add("Pco-Actor", a)
	}
	return req
}

func TestTheWebUINamesTheActor(t *testing.T) {
	for _, tt := range []struct {
		name  string
		uid   uint32
		actor []string
		want  string
	}{
		{"the web UI's user", webUID, []string{"alice@pve (ticket)"}, "alice@pve (ticket)"},
		{"every character it may have", webUID, []string{"a.b_c@pam!tok:x (token) -1"}, "a.b_c@pam!tok:x (token) -1"},
		{"the web UI without one", webUID, nil, ""},
		{"root, whatever it says", 0, []string{"alice@pve (ticket)"}, "root (cli)"},
		{"root without one", 0, nil, "root (cli)"},
		{"another user", otherUID, []string{"alice@pve (ticket)"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{}

			rec := send(webServer(f), adoptFrom(tt.uid, tt.actor...))

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, tt.want, engine.ActorOf(f.lastCtx()))
		})
	}
}

func TestAnActorThatIsNotOneIsRefused(t *testing.T) {
	for _, tt := range []struct {
		name  string
		actor []string
	}{
		{"empty", []string{""}},
		{"too long", []string{strings.Repeat("a", 129)}},
		{"a new line", []string{"alice@pve\nbob"}},
		{"a quote", []string{`alice"@pve`}},
		{"not ASCII", []string{"alicé@pve"}},
		{"two of them", []string{"alice@pve", "bob@pve"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{}

			rec := send(webServer(f), adoptFrom(webUID, tt.actor...))

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, "invalid", errorCode(t, rec))
			require.Contains(t, errorMessage(t, rec), "Pco-Actor")
			require.Empty(t, f.called(), "nothing is asked of the engine")
			require.Equal(t, testBoot, rec.Header().Get("Pco-Boot"))
		})
	}

	t.Run("from another user it is not read at all", func(t *testing.T) {
		f := &fakeEngine{}

		rec := send(webServer(f), adoptFrom(otherUID, "alice\n"))

		require.Equal(t, http.StatusOK, rec.Code)
		require.Empty(t, engine.ActorOf(f.lastCtx()))
	})
}

func TestWithoutAWebUserNobodyNamesTheActor(t *testing.T) {
	f := &fakeEngine{}
	s := New(f, "1.2.3", []uint32{0, webUID}, zerolog.Nop())

	rec := send(s, adoptFrom(webUID, "alice@pve (ticket)"))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, engine.ActorOf(f.lastCtx()))

	s.SetWebUID(0)
	rec = send(s, adoptFrom(0, "alice@pve (ticket)"))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "root (cli)", engine.ActorOf(f.lastCtx()), "root is never the web UI")
}
