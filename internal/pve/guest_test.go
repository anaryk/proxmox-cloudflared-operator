package pve

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

var appliance = model.GuestRef{Kind: model.KindLXC, VMID: 9201}

func TestGuestConfigReadsTheValuesThatRun(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/nodes/pco-test-2/lxc/9201/config?current=1": okReply(t, "lxc_config_current.json"),
	})
	got, err := c.GuestConfig(context.Background(), "pco-test-2", appliance)
	require.NoError(t, err)
	require.Equal(t, "/api2/json/nodes/pco-test-2/lxc/9201/config?current=1", rec.requests()[0].uri)

	// A hot-plugged NIC whose removal is pending is still attached.
	require.Equal(t, "name=eth10,bridge=vmbr1,hwaddr=BC:24:11:92:01:0A,type=veth", got.Values["net10"])
	require.NotContains(t, got.Values, "cmode", "a pending value is not one that runs")
	require.Equal(t, "1", got.Values["cores"])
	require.Equal(t, "256cbbe2f7cd3a686dc9ab8854c9a7f61057830b", got.Digest)
}

func TestPendingConfig(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/nodes/pco-test-2/lxc/9201/pending": okReply(t, "lxc_pending.json"),
	})
	got, err := c.PendingConfig(context.Background(), "pco-test-2", appliance)
	require.NoError(t, err)
	require.Equal(t, "/api2/json/nodes/pco-test-2/lxc/9201/pending", rec.requests()[0].uri)

	require.Equal(t, "console", got["cmode"], "a pending value is the one of the next start")
	require.NotContains(t, got, "net10", "a pending delete is gone at the next start")
	require.NotContains(t, got, "digest")
	require.Equal(t, "nesting=1", got["features"])
	require.Equal(t, "768", got["memory"])
	require.Equal(t, "name=eth0,bridge=vmbr1,gw=10.92.0.1,hwaddr=BC:24:11:92:00:10,ip=10.92.0.10/24,type=veth", got["net0"])
}

func TestPendingConfigOfAVM(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{"/nodes/pco-test-2/qemu/9220/pending": okReply(t, "qemu_pending.json")})
	got, err := c.PendingConfig(context.Background(), "pco-test-2", model.GuestRef{Kind: model.KindQEMU, VMID: 9220})
	require.NoError(t, err)
	require.Equal(t, "pcot-guest", got["name"])
	require.Equal(t, "virtio=BC:24:11:92:02:20,bridge=vmbr1", got["net0"])
	require.Len(t, got, 8)
}

func TestPendingConfigForms(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{"/nodes/pve1/lxc/200/pending": {http.StatusOK, `{"data":[
		{"key":"hostname","value":"db-1"},
		{"key":"cmode","value":"tty","pending":"shell"},
		{"key":"mp1","delete":2},
		{"key":"onboot","value":1,"delete":"0"},
		{"key":"swap","value":512,"pending":0}
	]}`}})
	got, err := c.PendingConfig(context.Background(), "pve1", model.GuestRef{Kind: model.KindLXC, VMID: 200})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"hostname": "db-1", "cmode": "shell", "onboot": "1", "swap": "0"}, got)

	for _, body := range []string{`{"data":[]}`, `{"data":[{"value":"x"}]}`, `{"data":{}}`} {
		c, _ := newTestClient(t, map[string]reply{"/nodes/pve1/lxc/200/pending": {http.StatusOK, body}})
		_, err := c.PendingConfig(context.Background(), "pve1", model.GuestRef{Kind: model.KindLXC, VMID: 200})
		require.Error(t, err, body)
	}
}

func TestSnapshots(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{"/nodes/pco-test-2/lxc/9201/snapshot": okReply(t, "lxc_snapshot.json")})
	got, err := c.Snapshots(context.Background(), "pco-test-2", appliance)
	require.NoError(t, err)
	require.Equal(t, []Snapshot{{Name: "pcotest-before", Time: time.Unix(1791198707, 0).UTC()}}, got,
		"the current state is no snapshot")
	require.Equal(t, "/api2/json/nodes/pco-test-2/lxc/9201/snapshot", rec.requests()[0].uri)

	c, _ = newTestClient(t, map[string]reply{"/nodes/pve1/qemu/101/snapshot": {http.StatusOK,
		`{"data":[{"name":"current","running":1},{"name":"old"},{"description":"no name"}]}`}})
	_, err = c.Snapshots(context.Background(), "pve1", model.GuestRef{Kind: model.KindQEMU, VMID: 101})
	require.ErrorContains(t, err, "snapshot")

	c, _ = newTestClient(t, map[string]reply{"/nodes/pve1/qemu/101/snapshot": {http.StatusOK,
		`{"data":[{"name":"current","running":1},{"name":"old"}]}`}})
	got, err = c.Snapshots(context.Background(), "pve1", model.GuestRef{Kind: model.KindQEMU, VMID: 101})
	require.NoError(t, err)
	require.Equal(t, []Snapshot{{Name: "old"}}, got, "a snapshot without a time has the zero time")
}

func TestReplication(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{"/cluster/replication": okReply(t, "replication_none.json")})
	got, err := c.Replication(context.Background())
	require.NoError(t, err)
	require.Empty(t, got)
	require.Len(t, rec.requests(), 1, "no jobs, no guests to look up")

	c, _ = newTestClient(t, map[string]reply{
		"/cluster/replication": {http.StatusOK, `{"data":[
			{"id":"101-0","type":"local","guest":101,"target":"pve2","jobnum":0,"schedule":"*/15"},
			{"id":"200-1","type":"local","guest":"200","target":"pve1","jobnum":1},
			{"id":"777-0","type":"local","guest":777,"target":"pve2"}
		]}`},
		"/cluster/resources?type=vm": okReply(t, "resources.json"),
	})
	got, err = c.Replication(context.Background())
	require.NoError(t, err)
	require.Equal(t, []ReplicationJob{
		{ID: "101-0", Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Target: "pve2"},
		{ID: "200-1", Guest: model.GuestRef{Kind: model.KindLXC, VMID: 200}, Target: "pve1"},
		{ID: "777-0", Guest: model.GuestRef{VMID: 777}, Target: "pve2"},
	}, got, "the kind comes from the cluster resources; a guest not listed has none")

	c, _ = newTestClient(t, map[string]reply{"/cluster/replication": {http.StatusOK, `{"data":[{"id":"x","guest":0}]}`}})
	_, err = c.Replication(context.Background())
	require.ErrorContains(t, err, "replication")
}

func TestUptimes(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{"/cluster/resources?type=vm": okReply(t, "resources_pools.json")})
	got, err := c.Uptimes(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[model.GuestRef]time.Duration{
		{Kind: model.KindLXC, VMID: 9200}: 629 * time.Second,
		{Kind: model.KindLXC, VMID: 9201}: 602 * time.Second,
	}, got, "a stopped guest has no uptime")
	require.Equal(t, "/api2/json/cluster/resources?type=vm", rec.requests()[0].uri)

	c, _ = newTestClient(t, map[string]reply{"/cluster/resources?type=vm": {http.StatusOK, `{"data":[
		{"type":"lxc","vmid":1,"node":"pve1","status":"running","uptime":30},
		{"type":"lxc","vmid":2,"node":"pve1","status":"unknown"},
		{"type":"lxc","vmid":3,"node":"pve1","status":"running"}
	]}`}})
	got, err = c.Uptimes(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[model.GuestRef]time.Duration{{Kind: model.KindLXC, VMID: 1}: 30 * time.Second}, got,
		"a guest whose uptime is not reported is left out")
}
