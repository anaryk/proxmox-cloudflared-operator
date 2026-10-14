package engine_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/api"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/api/apifake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// A node that does not hold the lease: the view a cycle makes of the keeper's
// state names the holder and its registered address, the role made of that
// view refuses an admin call on the socket, and the refusal names them.
func TestTheLeaseViewOfANonHolderMakesTheAPIPointAtTheController(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 10, 0, 0, time.UTC)
	since := now.Add(-10 * time.Minute)
	rec := store.LeaderRecord{Writer: planner.Writer{Generation: 7, Nonce: "abc"}, Node: "node2", AcquiredAt: since}
	view := engine.LeaseViewOf(lease.State{Waiting: "the lock is held", Record: rec}, now, func(node string) netip.Addr {
		require.Equal(t, "node2", node, "the address is the holder's")
		return netip.MustParseAddr("10.9.0.2")
	})
	state := func() engine.State { return engine.State{Node: "node1", Lease: view} }

	fake, err := apifake.Scenario(apifake.Scenarios()[0], func() time.Time { return now })
	require.NoError(t, err)
	t.Cleanup(func() { _ = fake.Close() })
	srv := api.New(fake, "test", []uint32{uint32(os.Getuid())}, zerolog.Nop())
	srv.SetRole(api.LeaseRole(state, func() bool { return false }))

	dir, err := os.MkdirTemp("", "pco")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "pco", "pco.sock")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listening := make(chan struct{})
	srv.OnListening(func() { close(listening) })
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, socket, os.Getgid()) }()
	<-listening

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return new(net.Dialer).DialContext(ctx, "unix", socket)
		},
	}}
	call := func(method, path, body string) (status int, header http.Header, answer string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, "http://pco"+path, strings.NewReader(body))
		require.NoError(t, err)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := client.Do(req)
		require.NoError(t, err)
		data, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())
		return res.StatusCode, res.Header, string(data)
	}

	status, header, body := call(http.MethodPost, "/v1/sync", `{}`)
	require.Equal(t, http.StatusConflict, status, body)
	var answer api.ErrorBody
	require.NoError(t, json.Unmarshal([]byte(body), &answer))
	require.Equal(t, "not_controller", answer.Code)
	require.Equal(t, "this node is not the controller; run it on node2 (10.9.0.2)", answer.Error)
	require.Equal(t, "10.9.0.2", header.Get("Pco-Controller"))

	status, _, body = call(http.MethodGet, "/v1/version", "")
	require.Equal(t, http.StatusOK, status, body)

	cancel()
	require.NoError(t, <-served)
}
