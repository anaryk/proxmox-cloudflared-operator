package doctor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

type fakeApplianceCluster struct {
	*fakeCluster
	facts ApplianceFacts
	err   error
}

func (f fakeApplianceCluster) ApplianceFacts(context.Context) (ApplianceFacts, error) {
	return f.facts, f.err
}

func TestTheControllerOfAnApplianceInstallChecksItsContainers(t *testing.T) {
	home := AgentContainer{Node: "pve2", VMID: 9301, Found: true, RunsOn: "pve2", Cmode: "tty"}
	for _, tt := range []struct {
		name  string
		facts ApplianceFacts
		check string
		level Level
		want  string
	}{
		{"no HA", ApplianceFacts{VMID: 120, Storage: &store.ApplianceStorage{ID: "local-zfs"}}, "controller ha", LevelOK, "lxc/120 is no HA resource"},
		{"HA on shared storage", ApplianceFacts{VMID: 120, HA: true, Storage: &store.ApplianceStorage{ID: "ceph", Shared: true}}, "controller ha", LevelOK,
			"on shared storage ceph"},
		{"HA on replicated storage", ApplianceFacts{VMID: 120, HA: true, Storage: &store.ApplianceStorage{ID: "local-zfs"}, Replication: []string{"120-0"}},
			"controller ha", LevelWarn, "only observes until pco appliance recover"},
		{"HA on storage of its own", ApplianceFacts{VMID: 120, HA: true, Storage: &store.ApplianceStorage{ID: "local-zfs"}}, "controller ha", LevelWarn,
			"cannot be relocated"},
		{"an agent at home", ApplianceFacts{VMID: 120, Agents: []AgentContainer{home}}, "agent container pve2", LevelOK, "agent 9301 runs on pve2"},
		{"a moved agent", ApplianceFacts{VMID: 120, Agents: []AgentContainer{{Node: "pve2", VMID: 9301, Found: true, RunsOn: "pve3"}}},
			"agent container pve2", LevelFail, "agent 9301 runs on pve3, enrolled for pve2"},
		{"an agent on HA", ApplianceFacts{VMID: 120, Agents: []AgentContainer{{Node: "pve2", VMID: 9301, Found: true, RunsOn: "pve2", HA: true}}},
			"agent container pve2", LevelFail, "is a Proxmox HA resource"},
		{"an agent with a shell console", ApplianceFacts{VMID: 120, Agents: []AgentContainer{{Node: "pve2", VMID: 9301, Found: true, RunsOn: "pve2", Cmode: "shell"}}},
			"agent container pve2", LevelFail, "a shell as root"},
		{"an agent gone", ApplianceFacts{VMID: 120, Agents: []AgentContainer{{Node: "pve2", VMID: 9301}}},
			"agent container pve2", LevelFail, "is not among the cluster's resources"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out := applianceClusterChecks(t.Context(), fakeApplianceCluster{fakeCluster: &fakeCluster{}, facts: tt.facts})

			f := findingOf(t, out, tt.check)
			require.Equal(t, tt.level, f.Level)
			require.Contains(t, f.Detail, tt.want)
		})
	}
	require.Empty(t, applianceClusterChecks(t.Context(), &fakeCluster{}), "a host's cluster has none of these")
	out := applianceClusterChecks(t.Context(), fakeApplianceCluster{fakeCluster: &fakeCluster{}, err: errors.New("no answer")})
	require.Equal(t, LevelWarn, findingOf(t, out, "controller ha").Level)
}
