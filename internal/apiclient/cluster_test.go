package apiclient

import (
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// Every call of the cluster goes to its route with the body the daemon
// reads, and the answer comes back decoded.
func TestTheClusterCallsReachTheirRoutes(t *testing.T) {
	no := false
	for _, tt := range []struct {
		name   string
		call   func(c *Client) (any, error)
		answer string
		want   seen
		got    any
	}{
		{"cluster status", func(c *Client) (any, error) { return c.ClusterStatus(t.Context()) },
			`{"cluster":{"controller":"pve1","members":["pve1"],"quorate":true,"coverage":{"covered":true}},"nodes":[]}`,
			seen{Method: "GET", Path: "/v1/cluster", Accept: "application/json"},
			engine.ClusterStatus{Cluster: engine.ClusterView{Controller: "pve1", Members: []string{"pve1"}, Quorate: true, Coverage: engine.CoverageView{Covered: true}}, Nodes: []engine.NodeView{}}},
		{"nodes", func(c *Client) (any, error) { return c.Nodes(t.Context()) },
			`[{"name":"pve2","state":"active","kind":"host","controller":false,"statusAge":0,"watchRunning":true,"covered":true,"replicaReady":true,"verified":2,"failing":0,"belowMinimum":0,"pinned":0,"clockSkew":0}]`,
			seen{Method: "GET", Path: "/v1/nodes", Accept: "application/json"},
			[]engine.NodeView{{Name: "pve2", State: "active", Kind: "host", WatchRunning: true, Covered: true, ReplicaReady: true, Verified: 2}}},
		{"admit", func(c *Client) (any, error) { return c.AdmitNode(t.Context(), "app1", "9f86d081", true) },
			`{"name":"app1","state":"joining","kind":"appliance","controller":false,"statusAge":0,"watchRunning":false,"covered":false,"replicaReady":false,"verified":0,"failing":0,"belowMinimum":0,"pinned":0,"clockSkew":0}`,
			seen{Method: "POST", Path: "/v1/nodes/app1/admit", ContentType: "application/json", Accept: "application/json", Body: `{"fingerprint":"9f86d081","renew":true}`},
			engine.NodeView{Name: "app1", State: "joining", Kind: "appliance"}},
		{"drain", func(c *Client) (any, error) { return c.DrainNode(t.Context(), "pve2", true) },
			`{"name":"pve2","state":"draining","drained":"admin","kind":"host","controller":false,"statusAge":0,"watchRunning":false,"covered":false,"replicaReady":false,"verified":0,"failing":0,"belowMinimum":0,"pinned":0,"clockSkew":0}`,
			seen{Method: "POST", Path: "/v1/nodes/pve2/drain", ContentType: "application/json", Accept: "application/json", Body: `{"force":true}`},
			engine.NodeView{Name: "pve2", State: "draining", Drained: "admin", Kind: "host"}},
		{"undrain", func(c *Client) (any, error) { return c.UndrainNode(t.Context(), "pve2") },
			`{"name":"pve2","state":"active","kind":"host","controller":false,"statusAge":0,"watchRunning":false,"covered":false,"replicaReady":false,"verified":0,"failing":0,"belowMinimum":0,"pinned":0,"clockSkew":0}`,
			seen{Method: "POST", Path: "/v1/nodes/pve2/undrain", ContentType: "application/json", Accept: "application/json"},
			engine.NodeView{Name: "pve2", State: "active", Kind: "host"}},
		{"forget", func(c *Client) (any, error) { return c.ForgetNode(t.Context(), "pve3", true) },
			`{"node":"pve3","compromised":true,"done":["removed"],"pending":["credentials"],"notRotated":["the corosync authkey"]}`,
			seen{Method: "POST", Path: "/v1/nodes/pve3/forget", ContentType: "application/json", Accept: "application/json", Body: `{"compromised":true}`},
			engine.ForgetResult{Node: "pve3", Compromised: true, Done: []string{"removed"}, Pending: []string{"credentials"}, NotRotated: []string{"the corosync authkey"}}},
		{"set", func(c *Client) (any, error) {
			return c.SetNode(t.Context(), "pve2", engine.NodePatch{ControllerEligible: &no})
		},
			`{"name":"pve2","state":"active","kind":"host","controller":false,"statusAge":0,"watchRunning":false,"covered":false,"replicaReady":false,"verified":0,"failing":0,"belowMinimum":0,"pinned":0,"clockSkew":0}`,
			seen{Method: "PUT", Path: "/v1/nodes/pve2", ContentType: "application/json", Accept: "application/json", Body: `{"controllerEligible":false}`},
			engine.NodeView{Name: "pve2", State: "active", Kind: "host"}},
		{"join token", func(c *Client) (any, error) { return c.JoinToken(t.Context(), "app1") },
			`{"id":"tok1","node":"app1","expires":"2026-10-01T12:15:00Z","token":"pco1.c2VjcmV0"}`,
			seen{Method: "POST", Path: "/v1/cluster/join-tokens", ContentType: "application/json", Accept: "application/json", Body: `{"node":"app1"}`},
			engine.JoinTokenView{ID: "tok1", Node: "app1", Expires: time.Date(2026, 10, 1, 12, 15, 0, 0, time.UTC), Token: "pco1.c2VjcmV0"}},
		{"finish upgrade", func(c *Client) (any, error) { return c.FinishUpgrade(t.Context()) },
			`{"from":"0.9.0","schema":1,"backup":"/var/lib/pco/backup/1-x"}`,
			seen{Method: "POST", Path: "/v1/cluster/finish-upgrade", ContentType: "application/json", Accept: "application/json"},
			engine.FinishUpgradeResult{From: "0.9.0", Schema: 1, Backup: "/var/lib/pco/backup/1-x"}},
		{"unblock", func(c *Client) (any, error) { return nil, c.Unblock(t.Context(), netip.MustParseAddr("10.0.0.7")) },
			`{}`,
			seen{Method: "POST", Path: "/v1/cluster/unblock", ContentType: "application/json", Accept: "application/json", Body: `{"address":"10.0.0.7"}`},
			nil},
		{"uninstall cluster", func(c *Client) (any, error) { return c.UninstallCluster(t.Context(), true) },
			`{"clean":[],"left":["pve2: pco uninstall"]}`,
			seen{Method: "POST", Path: "/v1/cluster/uninstall", ContentType: "application/json", Accept: "application/json", Body: `{"force":true}`},
			engine.UninstallResult{Clean: []string{}, Left: []string{"pve2: pco uninstall"}}},
		{"agent status", func(c *Client) (any, error) { return c.AgentStatus(t.Context()) },
			`{"node":"pve1","kind":"host","controller":"pve2","silentFor":0,"epoch":{"install":"abc","generation":3,"nonce":"n"},"seq":4,"seqAge":0,"connectors":1,"egressTargets":2,"heartbeatAge":0,"problems":[]}`,
			seen{Method: "GET", Path: "/v1/local/agent", Accept: "application/json"},
			nil},
		{"reset epoch", func(c *Client) (any, error) { return nil, c.ResetAgentEpoch(t.Context()) },
			`{}`,
			seen{Method: "POST", Path: "/v1/local/agent/reset-epoch", ContentType: "application/json", Accept: "application/json"},
			nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, socket := fakeDaemon(t, reply(http.StatusOK, tt.answer))

			got, err := tt.call(New(socket))

			require.NoError(t, err)
			require.Equal(t, tt.want, d.last(t))
			if tt.got != nil {
				require.Equal(t, tt.got, got)
			}
		})
	}
}

// A refusal of the daemon is an answer, not a failure to ask.
func TestAClusterRefusalIsAnAnswer(t *testing.T) {
	_, socket := fakeDaemon(t, reply(http.StatusConflict, `{"error":"node pve3 is contested","code":"refused"}`))
	_, err := New(socket).AdmitNode(t.Context(), "pve3", "9f86d081", false)
	require.EqualError(t, err, "node pve3 is contested")
	require.ErrorIs(t, err, engine.ErrRefused)
}
