//go:build linux

package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
)

// rawGet asks the socket for a path without the client in between.
func rawGet(t *testing.T, socket, path string) (int, string) {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://pco"+path, nil)
	require.NoError(t, err)
	res, err := hc.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	var body map[string]string
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	out, err := json.Marshal(body)
	require.NoError(t, err)
	return res.StatusCode, string(out)
}

func TestThePeerOfTheSocketIsChecked(t *testing.T) {
	for _, tt := range []struct {
		name    string
		allowed []uint32
		ok      bool
	}{
		{"allowed uid", []uint32{testUID}, true},
		{"one of several", []uint32{testUID + 1, testUID, testUID + 2}, true},
		{"another uid", []uint32{testUID + 1}, false},
		{"nobody", nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{}
			socket := socketPath(t)
			s := New(f, "1.2.3", tt.allowed, zerolog.Nop())
			require.True(t, s.checkPeers, "peers are checked on Linux unless a test says otherwise")
			serve(t, s, socket)

			status, body := rawGet(t, socket, "/v1/version")
			v, err := apiclient.New(socket).Version(t.Context())

			if tt.ok {
				require.Equal(t, http.StatusOK, status)
				require.NoError(t, err)
				require.Equal(t, "1.2.3", v)
				return
			}
			require.Equal(t, http.StatusForbidden, status)
			require.JSONEq(t, `{"error":"not allowed","code":"forbidden"}`, body)
			require.EqualError(t, err, "permission denied on "+socket+": run as root")
			status, _ = rawGet(t, socket, "/v1/state")
			require.Equal(t, http.StatusForbidden, status)
			require.Empty(t, f.called())
		})
	}
}
