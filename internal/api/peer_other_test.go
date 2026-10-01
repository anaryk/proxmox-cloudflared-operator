//go:build !linux

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestEveryPeerIsAcceptedOffLinux(t *testing.T) {
	// Nobody is allowed, and it makes no difference: there are no peer
	// credentials to check here.
	s := New(&fakeEngine{}, "1.2.3", nil, zerolog.Nop())
	require.False(t, s.checkPeers)

	rec := send(s, httptest.NewRequest(http.MethodGet, "/v1/version", nil))

	require.Equal(t, http.StatusOK, rec.Code)
}
