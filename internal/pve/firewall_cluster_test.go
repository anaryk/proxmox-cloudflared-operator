package pve

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClusterFirewallRules(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{"/cluster/firewall/rules": {http.StatusOK, `{"data":[
		{"pos":1,"type":"in","action":"ACCEPT","proto":"tcp","dport":"22","source":"+management","enable":1,"digest":"d1"},
		{"pos":0,"type":"in","action":"ACCEPT","proto":"tcp","dport":8644,"source":"+pco-nodes","comment":"pco","digest":"d1"}
	]}`}})
	got, err := c.ClusterFirewallRules(context.Background())
	require.NoError(t, err)
	require.Equal(t, []FirewallRule{
		{Pos: 0, Type: "in", Action: "ACCEPT", Proto: "tcp", DPort: "8644", Source: "+pco-nodes", Comment: "pco"},
		{Pos: 1, Type: "in", Action: "ACCEPT", Proto: "tcp", DPort: "22", Source: "+management", Enable: true},
	}, got)
}

func TestClusterIPSet(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/cluster/firewall/ipset":           {http.StatusOK, `{"data":[{"name":"management"},{"name":"pco-nodes","comment":"pco"}]}`},
		"/cluster/firewall/ipset/pco-nodes": {http.StatusOK, `{"data":[{"cidr":"10.0.0.11","comment":"pco: node pve1"},{"cidr":"10.0.0.12","nomatch":1}]}`},
	})
	ctx := context.Background()

	got, found, err := c.ClusterIPSet(ctx, "pco-nodes")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, []IPSetEntry{{CIDR: "10.0.0.11", Comment: "pco: node pve1"}, {CIDR: "10.0.0.12", NoMatch: true}}, got)

	got, found, err = c.ClusterIPSet(ctx, "other")
	require.NoError(t, err)
	require.False(t, found)
	require.Empty(t, got)

	_, _, err = c.ClusterIPSet(ctx, "../x")
	require.Error(t, err)
}
