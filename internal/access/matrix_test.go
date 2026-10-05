package access

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// matrix is testdata/matrix.json: the access control of a Proxmox VE node
// under one ACL per row, and what GET /access/permissions answered there for
// a user, its privilege-separated token and its token without separation.
type matrix struct {
	Path       string            `json:"path"`
	Principals map[string]string `json:"principals"`
	Groups     json.RawMessage   `json:"groups"`
	Roles      json.RawMessage   `json:"roles"`
	Resources  []struct {
		Type string `json:"type"`
		VMID int    `json:"vmid"`
	} `json:"resources"`
	Pools map[string][]int `json:"pools"`
	Root  struct {
		Answer json.RawMessage `json:"answer"`
	} `json:"root"`
	Rows []struct {
		Name    string                     `json:"name"`
		Grants  []string                   `json:"grants"`
		ACL     json.RawMessage            `json:"acl"`
		Users   json.RawMessage            `json:"users"`
		Answers map[string]json.RawMessage `json:"answers"`
	} `json:"rows"`
}

// TestMatrix computes the privileges of every principal of every row and
// compares them with what Proxmox answered. Both sides are read through
// pve.Client, as the daemon reads them.
func TestMatrix(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "matrix.json"))
	require.NoError(t, err)
	var m matrix
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Len(t, m.Rows, 17)

	kinds := map[int]model.GuestKind{}
	for _, r := range m.Resources {
		kinds[r.VMID] = model.GuestKind(r.Type)
	}
	pools := map[model.GuestRef]string{}
	for pool, vmids := range m.Pools {
		for _, vmid := range vmids {
			pools[model.GuestRef{Kind: kinds[vmid], VMID: vmid}] = pool
		}
	}

	for _, row := range m.Rows {
		t.Run(row.Name, func(t *testing.T) {
			routes := map[string]json.RawMessage{
				"/api2/json/access/acl":    row.ACL,
				"/api2/json/access/users":  row.Users,
				"/api2/json/access/groups": m.Groups,
				"/api2/json/access/roles":  m.Roles,
			}
			answers := map[string]json.RawMessage{"root@pam": m.Root.Answer}
			for p, a := range row.Answers {
				answers[p] = a
			}
			c := serve(t, func(r *http.Request) (json.RawMessage, bool) {
				if r.URL.Path == "/api2/json/access/permissions" {
					a, ok := answers[r.URL.Query().Get("userid")]
					return a, ok && r.URL.Query().Get("path") == m.Path
				}
				data, ok := routes[r.URL.Path]
				return data, ok
			})
			ctx := context.Background()
			d := Data{Pools: pools}
			var err error
			d.ACL, err = c.ACL(ctx)
			require.NoError(t, err)
			d.Users, err = c.Users(ctx)
			require.NoError(t, err)
			d.Groups, err = c.Groups(ctx)
			require.NoError(t, err)
			d.Roles, err = c.Roles(ctx)
			require.NoError(t, err)

			for _, p := range []string{m.Principals["user"], m.Principals["privsep"], m.Principals["shared"], "root@pam"} {
				want, err := c.Permissions(ctx, p, m.Path)
				require.NoError(t, err)
				require.Equal(t, want[m.Path], Privileges(d, p, m.Path), "%s on %s with %v", p, m.Path, row.Grants)
			}
		})
	}
}

// serve answers the requests of a pve.Client with the data route gives, as
// the API would, and returns that client.
func serve(t *testing.T, route func(r *http.Request) (json.RawMessage, bool)) *pve.Client {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := route(r)
		if !ok {
			http.Error(w, `{"data":null}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]json.RawMessage{"data": data})
	}))
	t.Cleanup(srv.Close)

	// The server is on a loopback address, where the client pins the
	// certificate of the node: this one.
	dir := t.TempDir()
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pve-ssl.pem"), cert, 0o600))
	c, err := pve.New(pve.Config{BaseURL: srv.URL, TokenID: "pco@pve!vm9201", Secret: "secret", NodeCertDir: dir})
	require.NoError(t, err)
	return c
}
