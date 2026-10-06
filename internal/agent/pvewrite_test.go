package agent

import (
	"encoding/json"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

var update = flag.Bool("update", false, "write the golden files of the tests")

const digest = "0123456789abcdef0123456789abcdef01234567"

// cards is a snapshot with a VM and a container of this node and a VM of
// another, each with a card of pco's at net1.
func (e *env) cards() agentproto.Snapshot {
	s := e.snapshot("www.example.com -> :8080")
	s.Targets[0].Guest.NICs = append(s.Targets[0].Guest.NICs, model.NIC{Index: 1, MAC: "06:50:43:4f:76:05", Bridge: "pconet"})
	s.Guests = []model.Guest{
		{
			Ref: model.GuestRef{Kind: model.KindLXC, VMID: 200}, Node: testNode, Running: true,
			NICs: []model.NIC{{Index: 0, MAC: "bc:24:11:00:02:00", Bridge: "vmbr0"}, {Index: 1, MAC: "06:50:43:4f:76:06", Bridge: "pconet"}},
		},
		{Ref: model.GuestRef{Kind: model.KindQEMU, VMID: 300}, Node: "pve2", Running: true},
		{
			Ref: model.GuestRef{Kind: model.KindQEMU, VMID: 102}, Node: testNode, Running: true,
			NICs: []model.NIC{{Index: 1, MAC: "bc:24:11:00:01:02", Bridge: "vmbr0"}},
		},
	}
	return s
}

func no() *bool  { b := false; return &b }
func yes() *bool { b := true; return &b }

func TestTheArgumentVectorsOfACardWrite(t *testing.T) {
	cases := []struct {
		name  string
		write agentproto.PVEWrite
	}{
		{"qm-set", agentproto.PVEWrite{
			Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, NIC: 2, Bridge: "pconet", MAC: "06:50:43:4f:76:07", Firewall: no(), Digest: digest,
		}},
		{"qm-set-tag-link-down", agentproto.PVEWrite{
			Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, NIC: 1, Bridge: "vmbr1", Tag: 20, MAC: "06:50:43:4F:76:05", Firewall: yes(), LinkDown: yes(), Digest: digest,
		}},
		{"qm-delete", agentproto.PVEWrite{Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, NIC: 1, Remove: true, Digest: digest}},
		{"pct-set", agentproto.PVEWrite{
			Guest: model.GuestRef{Kind: model.KindLXC, VMID: 200}, NIC: 1, Name: "eth1", Bridge: "pconet", MAC: "06:50:43:4f:76:06",
			IP: netip.MustParsePrefix("10.203.118.6/24"), Firewall: no(), Digest: digest,
		}},
		{"pct-delete", agentproto.PVEWrite{Guest: model.GuestRef{Kind: model.KindLXC, VMID: 200}, NIC: 1, Remove: true, Digest: digest}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			args, err := json.Marshal(tc.write)
			require.NoError(t, err)

			st := e.apply(withAction(e.cards(), "a1", agentproto.ActionPVEWrite, string(args)))

			require.Equal(t, []agentproto.ActionResult{{ID: "a1", OK: true}}, st.Actions)
			runs := e.runs.all()
			require.Len(t, runs, 1)
			got := strings.Join(runs[0], "\n") + "\n"
			path := filepath.Join("testdata", "argv", tc.name+".golden")
			if *update {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
			}
			want, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, string(want), got)
		})
	}
}

func TestACardWriteIsBoundedBy30Seconds(t *testing.T) {
	e := newEnv(t)
	start := time.Now()

	e.apply(withAction(e.cards(), "a1", agentproto.ActionPVEWrite, writeArgs))

	deadline, ok := e.runs.ctxs[0].Deadline()
	require.True(t, ok)
	require.WithinDuration(t, start.Add(30*time.Second), deadline, 5*time.Second)
}

func TestACardWriteIsRefused(t *testing.T) {
	cases := map[string]struct {
		args string
		why  string
	}{
		"a MAC outside pco's range": {
			`{"guest":{"kind":"qemu","vmid":101},"nic":2,"bridge":"pconet","mac":"bc:24:11:00:00:09","digest":"` + digest + `"}`,
			"MAC bc:24:11:00:00:09 is not one of pco's (06:50:43:4f:xx:xx)",
		},
		"a card whose MAC is not pco's": {
			`{"guest":{"kind":"qemu","vmid":101},"nic":0,"remove":true,"digest":"` + digest + `"}`,
			"net0 of qemu/101 has MAC bc:24:11:00:00:01, which is not one of pco's: pco writes only its own cards",
		},
		"a guest of another node": {
			`{"guest":{"kind":"qemu","vmid":300},"nic":1,"bridge":"pconet","mac":"06:50:43:4f:76:09","digest":"` + digest + `"}`,
			"qemu/300 runs on node pve2, not on pve1",
		},
		"a guest the desired state does not name": {
			`{"guest":{"kind":"qemu","vmid":999},"nic":1,"bridge":"pconet","mac":"06:50:43:4f:76:09","digest":"` + digest + `"}`,
			"qemu/999 is not a guest of the desired state",
		},
		"an unknown field": {
			`{"guest":{"kind":"qemu","vmid":101},"nic":2,"bridge":"pconet","mac":"06:50:43:4f:76:07","hookscript":"local:snippets/x","digest":"` + digest + `"}`,
			`json: unknown field "hookscript"`,
		},
		"no digest": {
			`{"guest":{"kind":"qemu","vmid":101},"nic":2,"bridge":"pconet","mac":"06:50:43:4f:76:07"}`,
			`digest "" is not the digest of a guest's configuration`,
		},
		"a bridge that is no name": {
			`{"guest":{"kind":"qemu","vmid":101},"nic":2,"bridge":"pconet,trunks=1-4094","mac":"06:50:43:4f:76:07","digest":"` + digest + `"}`,
			`bridge "pconet,trunks=1-4094" is not the name of a bridge`,
		},
		"a tag out of range": {
			`{"guest":{"kind":"qemu","vmid":101},"nic":2,"bridge":"pconet","tag":4095,"mac":"06:50:43:4f:76:07","digest":"` + digest + `"}`,
			"tag 4095 is not 1 to 4094",
		},
		"a card out of range": {
			`{"guest":{"kind":"qemu","vmid":101},"nic":32,"bridge":"pconet","mac":"06:50:43:4f:76:07","digest":"` + digest + `"}`,
			"card net32 is not net0 to net31",
		},
		"a new card without a MAC": {
			`{"guest":{"kind":"qemu","vmid":101},"nic":2,"bridge":"pconet","digest":"` + digest + `"}`,
			"a card is set with its MAC and its bridge",
		},
		"a container's card without a name": {
			`{"guest":{"kind":"lxc","vmid":200},"nic":1,"bridge":"pconet","mac":"06:50:43:4f:76:06","digest":"` + digest + `"}`,
			`name "" is not the name of an interface in a container (eth0 to eth31)`,
		},
		"a VM's card with a name": {
			`{"guest":{"kind":"qemu","vmid":101},"nic":2,"name":"eth1","bridge":"pconet","mac":"06:50:43:4f:76:07","digest":"` + digest + `"}`,
			"a name and an address are a container's",
		},
		"a removal with more": {
			`{"guest":{"kind":"qemu","vmid":101},"nic":1,"remove":true,"bridge":"pconet","digest":"` + digest + `"}`,
			"a removal names its card and nothing else",
		},
		"a removal of a card the desired state does not know": {
			`{"guest":{"kind":"qemu","vmid":101},"nic":5,"remove":true,"digest":"` + digest + `"}`,
			"qemu/101 has no card net5 in the desired state",
		},
		"a container's address that is not IPv4": {
			`{"guest":{"kind":"lxc","vmid":200},"nic":1,"name":"eth1","bridge":"pconet","mac":"06:50:43:4f:76:06","ip":"fd00::6/64","digest":"` + digest + `"}`,
			"address fd00::6/64 is not an IPv4 address",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)

			st := e.apply(withAction(e.cards(), "a1", agentproto.ActionPVEWrite, tc.args))

			require.Len(t, st.Actions, 1)
			require.False(t, st.Actions[0].OK)
			require.Contains(t, st.Actions[0].Error, tc.why)
			require.Empty(t, e.runs.all(), "nothing is run")
		})
	}
}

func TestOnlyAHostAgentWritesCards(t *testing.T) {
	e := newEnv(t)
	e.kind = pki.KindAppliance
	e.agent = e.newAgent()

	st := e.apply(withAction(e.cards(), "a1", agentproto.ActionPVEWrite, writeArgs))

	require.Equal(t, "an appliance agent writes no guest configuration; the controller writes it through the Proxmox API", st.Actions[0].Error)
	require.Empty(t, e.runs.all())
}
