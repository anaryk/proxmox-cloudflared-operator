package apiclient

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func TestHandoffAsksTheDaemonForTheLease(t *testing.T) {
	for _, tt := range []struct {
		name string
		to   string
		body string
	}{
		{"any node", "", `{}`},
		{"a named node", "pve2", `{"to":"pve2"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, socket := fakeDaemon(t, reply(200, `{"to":"pve2"}`))

			require.NoError(t, New(socket).Handoff(t.Context(), tt.to))

			require.Equal(t, seen{Method: "POST", Path: "/v1/cluster/handoff", ContentType: "application/json", Accept: "application/json", Body: tt.body}, d.last(t))
		})
	}
}

func TestHandoffTellsADaemonThatDoesNotKnowIt(t *testing.T) {
	_, socket := fakeDaemon(t, reply(http.StatusNotFound, `404 page not found`))

	err := New(socket).Handoff(t.Context(), "")

	require.ErrorIs(t, err, ErrUnknownRequest)
	require.ErrorIs(t, err, ErrNoAnswer)
	require.ErrorContains(t, err, "does not know this request")
}

func TestHandoffPassesTheRefusalOn(t *testing.T) {
	_, socket := fakeDaemon(t, reply(http.StatusConflict, `{"error":"no other node may take the lease","code":"refused"}`))

	err := New(socket).Handoff(t.Context(), "")

	require.EqualError(t, err, "no other node may take the lease")
	require.ErrorIs(t, err, engine.ErrRefused)
	require.False(t, errors.Is(err, ErrNoAnswer), "a refusal is an answer")
}
