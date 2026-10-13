package pve

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

var managedVM = model.GuestRef{Kind: model.KindQEMU, VMID: 9320}

func TestGuestFirewallOptionsReadWhatTheManagedNetworkWrites(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/nodes/pve1/qemu/9320/firewall/options": {http.StatusOK,
			`{"data":{"enable":1,"macfilter":0,"ipfilter":"1","policy_in":"DROP","policy_out":"ACCEPT","digest":"d1"}}`},
		"/nodes/pve1/lxc/9321/firewall/options": {http.StatusOK, `{"data":{"digest":"d2"}}`},
	})
	got, err := c.GuestFirewallOptions(context.Background(), "pve1", managedVM)
	require.NoError(t, err)
	require.Equal(t, GuestFirewallOptions{Enabled: true, MACFilterOff: true, IPFilter: true, PolicyIn: "DROP", PolicyOut: "ACCEPT", Digest: "d1"}, got)

	got, err = c.GuestFirewallOptions(context.Background(), "pve1", model.GuestRef{Kind: model.KindLXC, VMID: 9321})
	require.NoError(t, err)
	require.Equal(t, GuestFirewallOptions{Digest: "d2"}, got, "an absent macfilter is on, its default")
}

func TestSetGuestFirewallOptionsPutsOnlyWhatChanges(t *testing.T) {
	c, seen := newWriteServer(t, reply{http.StatusOK, `{"data":null}`})
	on := true
	require.NoError(t, c.SetGuestFirewallOptions(t.Context(), "pve1", managedVM,
		GuestFirewallChange{Enable: &on, PolicyIn: "ACCEPT", PolicyOut: "ACCEPT"}, "d1"))
	off := false
	require.NoError(t, c.SetGuestFirewallOptions(t.Context(), "pve1", managedVM,
		GuestFirewallChange{Enable: &off, Delete: []string{"policy_in", "policy_out"}}, "d2"))

	got := seen()
	require.Len(t, got, 2)
	require.Equal(t, http.MethodPut, got[0].method)
	require.Equal(t, "/api2/json/nodes/pve1/qemu/9320/firewall/options", got[0].path)
	require.Equal(t, url.Values{"enable": {"1"}, "policy_in": {"ACCEPT"}, "policy_out": {"ACCEPT"}, "digest": {"d1"}}, got[0].form,
		"never ipfilter, never macfilter")
	require.Equal(t, url.Values{"enable": {"0"}, "delete": {"policy_in,policy_out"}, "digest": {"d2"}}, got[1].form)

	err := c.SetGuestFirewallOptions(t.Context(), "pve1", managedVM, GuestFirewallChange{PolicyIn: "ACCEPT; enable=0"}, "d")
	require.ErrorContains(t, err, "policy")
	err = c.SetGuestFirewallOptions(t.Context(), "pve1", managedVM, GuestFirewallChange{Delete: []string{"ipfilter"}}, "d")
	require.ErrorContains(t, err, "ipfilter")
	err = c.SetGuestFirewallOptions(t.Context(), "pve1", managedVM, GuestFirewallChange{}, "d")
	require.Error(t, err)
	require.Len(t, seen(), 2, "a refused change sends nothing")
}

func TestGuestFirewallRulesListsEveryRuleWithTheDigest(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/nodes/pve1/qemu/9320/firewall/rules": {http.StatusOK, `{"data":[
			{"pos":0,"type":"in","action":"ACCEPT","iface":"net1","source":"10.203.118.1","proto":"udp","sport":"67","dport":68,"comment":"pco:abc","enable":1,"digest":"d9"},
			{"pos":1,"type":"group","action":"webservers","enable":"0","digest":"d9"},
			{"pos":2,"type":"in","action":"ACCEPT","macro":"SSH","icmp-type":"","log":"nolog","digest":"d9"}
		]}`},
		"/nodes/pve1/lxc/9321/firewall/rules": {http.StatusOK, `{"data":[]}`},
	})
	rules, digest, err := c.GuestFirewallRules(t.Context(), "pve1", managedVM)
	require.NoError(t, err)
	require.Equal(t, "d9", digest)
	require.Equal(t, []FirewallRule{
		{Pos: 0, Type: "in", Action: "ACCEPT", Iface: "net1", Source: "10.203.118.1", Proto: "udp", SPort: "67", DPort: "68", Comment: "pco:abc", Enable: true},
		{Pos: 1, Type: "group", Action: "webservers"},
		{Pos: 2, Type: "in", Action: "ACCEPT", Macro: "SSH"},
	}, rules)

	rules, digest, err = c.GuestFirewallRules(t.Context(), "pve1", model.GuestRef{Kind: model.KindLXC, VMID: 9321})
	require.NoError(t, err)
	require.Empty(t, rules)
	require.Empty(t, digest)
}

func TestAddGuestFirewallRulePostsItEnabled(t *testing.T) {
	c, seen := newWriteServer(t, reply{http.StatusOK, `{"data":null}`})
	err := c.AddGuestFirewallRule(t.Context(), "pve1", managedVM, FirewallRule{
		Type: "in", Action: "ACCEPT", Iface: "net1", Source: "10.203.118.1", Proto: "tcp", DPort: "443", Comment: "pco:abc", Enable: true,
	}, "d1")
	require.NoError(t, err)
	err = c.AddGuestFirewallRule(t.Context(), "pve1", managedVM, FirewallRule{Type: "out", Action: "DROP", Iface: "net1", Comment: "pco:abc", Enable: true}, "d2")
	require.NoError(t, err)

	got := seen()
	require.Len(t, got, 2)
	require.Equal(t, http.MethodPost, got[0].method)
	require.Equal(t, "/api2/json/nodes/pve1/qemu/9320/firewall/rules", got[0].path)
	require.Equal(t, url.Values{
		"type": {"in"}, "action": {"ACCEPT"}, "iface": {"net1"}, "source": {"10.203.118.1"}, "proto": {"tcp"}, "dport": {"443"},
		"comment": {"pco:abc"}, "enable": {"1"}, "digest": {"d1"},
	}, got[0].form)
	require.Equal(t, url.Values{"type": {"out"}, "action": {"DROP"}, "iface": {"net1"}, "comment": {"pco:abc"}, "enable": {"1"}, "digest": {"d2"}}, got[1].form)

	err = c.AddGuestFirewallRule(t.Context(), "pve1", managedVM, FirewallRule{Type: "group", Action: "x"}, "d")
	require.ErrorContains(t, err, "type")
	err = c.AddGuestFirewallRule(t.Context(), "pve1", managedVM, FirewallRule{Type: "in", Action: "ACCEPT", Macro: "SSH"}, "d")
	require.ErrorContains(t, err, "macro")
	err = c.AddGuestFirewallRule(t.Context(), "pve1", managedVM, FirewallRule{Type: "in", Action: "ACCEPT", Iface: "net1", Enable: true}, "")
	require.ErrorContains(t, err, "digest")
	require.Len(t, seen(), 2)
}

func TestDeleteGuestFirewallRuleSendsTheDigest(t *testing.T) {
	c, seen := escapedServer(t)
	require.NoError(t, c.DeleteGuestFirewallRule(t.Context(), "pve1", managedVM, 3, "d3"))
	require.Equal(t, []string{"DELETE /api2/json/nodes/pve1/qemu/9320/firewall/rules/3?digest=d3"}, seen())

	require.ErrorContains(t, c.DeleteGuestFirewallRule(t.Context(), "pve1", managedVM, -1, "d"), "position")
	require.ErrorContains(t, c.DeleteGuestFirewallRule(t.Context(), "pve1", managedVM, 0, ""), "digest")
}

func TestAStaleFirewallDigestIsErrStaleDigest(t *testing.T) {
	c, _ := newWriteServer(t, reply{http.StatusInternalServerError,
		`{"data":null,"message":"detected modified configuration - file changed by other user? Try again.\n"}`})
	err := c.AddGuestFirewallRule(t.Context(), "pve1", managedVM, FirewallRule{Type: "in", Action: "DROP", Iface: "net1", Enable: true}, "old")
	require.ErrorIs(t, err, ErrStaleDigest)
	err = c.DeleteGuestFirewallRule(t.Context(), "pve1", managedVM, 0, "old")
	require.ErrorIs(t, err, ErrStaleDigest)
	on := true
	err = c.SetGuestFirewallOptions(t.Context(), "pve1", managedVM, GuestFirewallChange{Enable: &on}, "old")
	require.ErrorIs(t, err, ErrStaleDigest)
	err = c.DeleteGuestIPSetEntry(t.Context(), "pve1", managedVM, "ipfilter-net1", "10.203.118.2", "old")
	require.ErrorIs(t, err, ErrStaleDigest)
}

func TestGuestIPSetsAndTheirEntries(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/nodes/pve1/qemu/9320/firewall/ipset": {http.StatusOK,
			`{"data":[{"name":"ipfilter-net1","comment":"pco:abc","digest":"s1"},{"name":"admins","digest":"s1"}]}`},
		"/nodes/pve1/qemu/9320/firewall/ipset/ipfilter-net1": {http.StatusOK,
			`{"data":[{"cidr":"10.203.118.2","comment":"pco:abc","digest":"e1"},{"cidr":"10.203.118.0/24","nomatch":1,"digest":"e1"}]}`},
	})
	sets, err := c.GuestIPSets(t.Context(), "pve1", managedVM)
	require.NoError(t, err)
	require.Equal(t, []IPSet{{Name: "ipfilter-net1", Comment: "pco:abc"}, {Name: "admins"}}, sets)

	entries, digest, err := c.GuestIPSet(t.Context(), "pve1", managedVM, "ipfilter-net1")
	require.NoError(t, err)
	require.Equal(t, "e1", digest)
	require.Equal(t, []IPSetEntry{{CIDR: "10.203.118.2", Comment: "pco:abc"}, {CIDR: "10.203.118.0/24", NoMatch: true}}, entries)

	_, _, err = c.GuestIPSet(t.Context(), "pve1", managedVM, "../rules")
	require.ErrorContains(t, err, "ipset name")
}

func TestTheIPSetWritesOfTheManagedNetwork(t *testing.T) {
	c, seen := newWriteServer(t, reply{http.StatusOK, `{"data":null}`})
	ctx := t.Context()
	require.NoError(t, c.CreateGuestIPSet(ctx, "pve1", managedVM, "ipfilter-net1", "pco:abc"))
	require.NoError(t, c.AddGuestIPSetEntry(ctx, "pve1", managedVM, "ipfilter-net1", "10.203.118.2", "pco:abc"))

	got := seen()
	require.Len(t, got, 2)
	require.Equal(t, http.MethodPost, got[0].method)
	require.Equal(t, "/api2/json/nodes/pve1/qemu/9320/firewall/ipset", got[0].path)
	require.Equal(t, url.Values{"name": {"ipfilter-net1"}, "comment": {"pco:abc"}}, got[0].form)
	require.Equal(t, http.MethodPost, got[1].method)
	require.Equal(t, "/api2/json/nodes/pve1/qemu/9320/firewall/ipset/ipfilter-net1", got[1].path)
	require.Equal(t, url.Values{"cidr": {"10.203.118.2"}, "comment": {"pco:abc"}}, got[1].form)

	require.ErrorContains(t, c.AddGuestIPSetEntry(ctx, "pve1", managedVM, "ipfilter-net1", "not an address", ""), "cidr")
	require.ErrorContains(t, c.CreateGuestIPSet(ctx, "pve1", managedVM, "a b", ""), "ipset name")
	require.Len(t, seen(), 2)
}

func TestTheIPSetDeletesEscapeAPrefix(t *testing.T) {
	c, seen := escapedServer(t)
	ctx := t.Context()
	require.NoError(t, c.DeleteGuestIPSetEntry(ctx, "pve1", managedVM, "ipfilter-net1", "10.203.118.0/24", "e1"))
	require.NoError(t, c.DeleteGuestIPSetEntry(ctx, "pve1", managedVM, "ipfilter-net1", "10.203.118.2", "e2"))
	require.NoError(t, c.DeleteGuestIPSet(ctx, "pve1", managedVM, "ipfilter-net1"))
	require.Equal(t, []string{
		"DELETE /api2/json/nodes/pve1/qemu/9320/firewall/ipset/ipfilter-net1/10.203.118.0%2F24?digest=e1",
		"DELETE /api2/json/nodes/pve1/qemu/9320/firewall/ipset/ipfilter-net1/10.203.118.2?digest=e2",
		"DELETE /api2/json/nodes/pve1/qemu/9320/firewall/ipset/ipfilter-net1",
	}, seen())
	require.ErrorContains(t, c.DeleteGuestIPSetEntry(ctx, "pve1", managedVM, "ipfilter-net1", "10.203.118.2/x", "e"), "cidr")
}

// escapedServer answers every request with an empty success and remembers
// each as its method and the request URI as sent, escapes and all.
func escapedServer(t *testing.T) (*Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	c := startServer(t, testConfig(""), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.RequestURI)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":null}`))
	}))
	return c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}
