package managednet_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

var _ managednet.Configurer = (*managednet.HostConfigurer)(nil)

// step is one command the scripted runner expects, and what it answers.
type step struct {
	cmd string
	out string
	err error
}

// script is a runner that expects its commands in order.
type script struct {
	t     *testing.T
	steps []step
}

func (s *script) Run(_ context.Context, name string, args ...string) (string, error) {
	s.t.Helper()
	cmd := strings.Join(append([]string{name}, args...), " ")
	require.NotEmpty(s.t, s.steps, "unexpected command %q", cmd)
	next := s.steps[0]
	s.steps = s.steps[1:]
	require.Equal(s.t, next.cmd, cmd)
	return next.out, next.err
}

func (s *script) done() { require.Empty(s.t, s.steps, "commands not run") }

func run(t *testing.T, steps ...step) *script {
	t.Helper()
	s := &script{t: t, steps: steps}
	t.Cleanup(s.done)
	return s
}

// stale is the error qm set and pct set end with when --digest no longer
// matches, as the host runner reports it.
func stale(cmd string) error {
	return fmt.Errorf("%s: exit status 255: detected modified configuration - file changed by other user? Try again.", cmd)
}

var (
	container = model.Guest{Ref: model.GuestRef{Kind: model.KindLXC, VMID: 9320}, Name: "ct", Node: thisNode, Running: true}
	vm        = model.Guest{Ref: model.GuestRef{Kind: model.KindQEMU, VMID: 9330}, Name: "vm", Node: thisNode, Running: true}

	ctSpec = managednet.NICSpec{MAC: "06:50:43:4f:76:05", Bridge: "pconet", Addr: netip.MustParsePrefix("10.203.118.5/24")}
	vmSpec = managednet.NICSpec{MAC: "06:50:43:4f:76:06", Bridge: "pconet"}
)

const (
	ctConfig = "pvesh get /nodes/pve1/lxc/9320/config --output-format json"
	vmConfig = "pvesh get /nodes/pve1/qemu/9330/config --output-format json"
	ctNet0   = `"net0":"name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:01,ip=dhcp,type=veth"`
	vmNet0   = `"net0":"virtio=BC:24:11:00:00:02,bridge=vmbr0,firewall=1"`
)

// config is the JSON pvesh prints for a configuration with the digest and
// the keys given.
func config(digest string, keys ...string) string {
	return `{"arch":"amd64","cores":1,"digest":"` + digest + `","onboot":1,` + strings.Join(keys, ",") + `}`
}

func TestHostAddNICToAContainer(t *testing.T) {
	r := run(t,
		step{cmd: ctConfig, out: config("d1", ctNet0)},
		step{cmd: "pct set 9320 --net1 name=eth1,bridge=pconet,hwaddr=06:50:43:4f:76:05,ip=10.203.118.5/24,firewall=0 --digest d1"},
	)
	live, err := managednet.NewHostConfigurer(r, thisNode).AddNIC(t.Context(), container, 1, ctSpec)
	require.NoError(t, err)
	require.True(t, live, "a container takes its card live")
}

func TestHostAddNICNamesTheLowestFreeInterface(t *testing.T) {
	r := run(t,
		step{cmd: ctConfig, out: config("d1", ctNet0,
			`"net1":"name=eth1,bridge=vmbr1,hwaddr=BC:24:11:00:00:03,ip=10.0.0.5/24"`,
			`"net3":"name=lan,bridge=vmbr1,hwaddr=BC:24:11:00:00:04"`)},
		step{cmd: "pct set 9320 --net2 name=eth2,bridge=pconet,hwaddr=06:50:43:4f:76:05,ip=10.203.118.5/24,firewall=0 --digest d1"},
	)
	_, err := managednet.NewHostConfigurer(r, thisNode).AddNIC(t.Context(), container, 2, ctSpec)
	require.NoError(t, err)
}

func TestHostAddNICKeepsAGivenName(t *testing.T) {
	spec := ctSpec
	spec.Name = "pco0"
	r := run(t,
		step{cmd: ctConfig, out: config("d1", ctNet0)},
		step{cmd: "pct set 9320 --net1 name=pco0,bridge=pconet,hwaddr=06:50:43:4f:76:05,ip=10.203.118.5/24,firewall=0 --digest d1"},
	)
	_, err := managednet.NewHostConfigurer(r, thisNode).AddNIC(t.Context(), container, 1, spec)
	require.NoError(t, err)
}

func TestHostAddNICToAVM(t *testing.T) {
	for _, tt := range []struct {
		name    string
		pending string
		live    bool
	}{
		{name: "with network hotplug", pending: "cur net0: virtio=BC:24:11:00:00:02,bridge=vmbr0,firewall=1\ncur net1: virtio=06:50:43:4f:76:06,bridge=pconet,firewall=0\n", live: true},
		{name: "without network hotplug", pending: "cur net0: virtio=BC:24:11:00:00:02,bridge=vmbr0,firewall=1\nnew net1: virtio=06:50:43:4f:76:06,bridge=pconet,firewall=0\n", live: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := run(t,
				step{cmd: vmConfig, out: config("d1", vmNet0)},
				step{cmd: "qm set 9330 --net1 virtio=06:50:43:4f:76:06,bridge=pconet,firewall=0 --digest d1"},
				step{cmd: "qm pending 9330", out: tt.pending},
			)
			live, err := managednet.NewHostConfigurer(r, thisNode).AddNIC(t.Context(), vm, 1, vmSpec)
			require.NoError(t, err)
			require.Equal(t, tt.live, live)
		})
	}
}

func TestHostAddNICRetriesAStaleDigestOnce(t *testing.T) {
	set := func(digest string) string {
		return "pct set 9320 --net1 name=eth1,bridge=pconet,hwaddr=06:50:43:4f:76:05,ip=10.203.118.5/24,firewall=0 --digest " + digest
	}
	t.Run("once", func(t *testing.T) {
		r := run(t,
			step{cmd: ctConfig, out: config("d1", ctNet0)},
			step{cmd: set("d1"), err: stale(set("d1"))},
			step{cmd: ctConfig, out: config("d2", ctNet0)},
			step{cmd: set("d2")},
		)
		_, err := managednet.NewHostConfigurer(r, thisNode).AddNIC(t.Context(), container, 1, ctSpec)
		require.NoError(t, err)
	})
	// qm set says so in other words when the file changed between its read
	// of the configuration and its write.
	t.Run("checksum mismatch", func(t *testing.T) {
		r := run(t,
			step{cmd: ctConfig, out: config("d1", ctNet0)},
			step{cmd: set("d1"), err: fmt.Errorf("%s: exit status 255: checksum mismatch (file change by other user?)", set("d1"))},
			step{cmd: ctConfig, out: config("d2", ctNet0)},
			step{cmd: set("d2")},
		)
		_, err := managednet.NewHostConfigurer(r, thisNode).AddNIC(t.Context(), container, 1, ctSpec)
		require.NoError(t, err)
	})
	t.Run("twice", func(t *testing.T) {
		r := run(t,
			step{cmd: ctConfig, out: config("d1", ctNet0)},
			step{cmd: set("d1"), err: stale(set("d1"))},
			step{cmd: ctConfig, out: config("d2", ctNet0)},
			step{cmd: set("d2"), err: stale(set("d2"))},
		)
		_, err := managednet.NewHostConfigurer(r, thisNode).AddNIC(t.Context(), container, 1, ctSpec)
		require.ErrorIs(t, err, pve.ErrStaleDigest)
		require.ErrorContains(t, err, "adding net1 to lxc/9320")
	})
}

// The new configuration may already hold what the write wanted: a write whose
// answer was lost, or a change made by someone else meanwhile.
func TestHostAddNICOfACardThatIsThere(t *testing.T) {
	t.Run("container", func(t *testing.T) {
		r := run(t, step{cmd: ctConfig, out: config("d1", ctNet0,
			`"net1":"name=eth1,bridge=pconet,hwaddr=06:50:43:4F:76:05,ip=10.203.118.5/24,firewall=0"`)})
		live, err := managednet.NewHostConfigurer(r, thisNode).AddNIC(t.Context(), container, 1, ctSpec)
		require.NoError(t, err)
		require.True(t, live)
	})
	t.Run("vm", func(t *testing.T) {
		r := run(t,
			step{cmd: vmConfig, out: config("d1", vmNet0, `"net1":"virtio=06:50:43:4F:76:06,bridge=pconet,firewall=0"`)},
			step{cmd: "qm pending 9330", out: "new net1: virtio=06:50:43:4F:76:06,bridge=pconet,firewall=0\n"},
		)
		live, err := managednet.NewHostConfigurer(r, thisNode).AddNIC(t.Context(), vm, 1, vmSpec)
		require.NoError(t, err)
		require.False(t, live)
	})
}

func TestHostAddNICReplacesAForeignCardOnTheManagedNetwork(t *testing.T) {
	r := run(t,
		step{cmd: ctConfig, out: config("d1", ctNet0,
			`"net1":"name=eth1,bridge=pconet,hwaddr=BC:24:11:AA:BB:CC,ip=10.203.118.9/24"`)},
		step{cmd: "pct set 9320 --net1 name=eth1,bridge=pconet,hwaddr=06:50:43:4f:76:05,ip=10.203.118.5/24,firewall=0 --digest d1"},
	)
	_, err := managednet.NewHostConfigurer(r, thisNode).AddNIC(t.Context(), container, 1, ctSpec)
	require.NoError(t, err)
}

func TestHostAddNICRefuses(t *testing.T) {
	tests := []struct {
		name  string
		guest model.Guest
		index int
		spec  *managednet.NICSpec
		steps []step
		err   string
	}{
		{
			name: "a card on another bridge", guest: container, index: 0,
			steps: []step{{cmd: ctConfig, out: config("d1", ctNet0)}},
			err:   "adding net0 to lxc/9320: net0 is a card on vmbr0; pco replaces only a card on pconet",
		},
		{
			name: "a card with the MAC at another index", guest: container, index: 2,
			steps: []step{{cmd: ctConfig, out: config("d1", ctNet0,
				`"net1":"name=eth1,bridge=pconet,hwaddr=06:50:43:4f:76:05,ip=10.203.118.5/24"`)}},
			err: "adding net2 to lxc/9320: the card 06:50:43:4f:76:05 is net1 already",
		},
		{
			name: "a guest on another node", guest: func() model.Guest { g := container; g.Node = "pve2"; return g }(), index: 1,
			err: "adding net1 to lxc/9320: the guest is on node pve2, and pco changes the guests of pve1 only",
		},
		{
			name: "a container card without an address", guest: container, index: 1,
			spec:  &managednet.NICSpec{MAC: ctSpec.MAC, Bridge: ctSpec.Bridge},
			steps: []step{{cmd: ctConfig, out: config("d1", ctNet0)}},
			err:   "adding net1 to lxc/9320: a container's card on the managed network needs its address",
		},
		{
			name: "an index out of range", guest: container, index: 32,
			err: "adding net32 to lxc/9320: net32 is not net0 to net31",
		},
		{
			name: "a configuration that cannot be read", guest: container, index: 1,
			steps: []step{{cmd: ctConfig, err: errors.New("pvesh: exit status 2: Configuration file 'nodes/pve1/lxc/9320.conf' does not exist")}},
			err:   "adding net1 to lxc/9320: reading the configuration: pvesh: exit status 2: Configuration file 'nodes/pve1/lxc/9320.conf' does not exist",
		},
		{
			name: "a configuration that is no JSON", guest: container, index: 1,
			steps: []step{{cmd: ctConfig, out: "arch: amd64\n"}},
			err:   "adding net1 to lxc/9320: reading the configuration: invalid character 'a' looking for beginning of value",
		},
		{
			name: "a configuration without a digest", guest: container, index: 1,
			steps: []step{{cmd: ctConfig, out: `{"arch":"amd64"}`}},
			err:   "adding net1 to lxc/9320: reading the configuration: it has no digest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := ctSpec
			if tt.spec != nil {
				spec = *tt.spec
			}
			r := run(t, tt.steps...)
			_, err := managednet.NewHostConfigurer(r, thisNode).AddNIC(t.Context(), tt.guest, tt.index, spec)
			require.EqualError(t, err, tt.err)
		})
	}
}

func TestHostAddNICWithTheFirewall(t *testing.T) {
	spec := vmSpec
	spec.Firewall = true
	r := run(t,
		step{cmd: vmConfig, out: config("d1", vmNet0)},
		step{cmd: "qm set 9330 --net1 virtio=06:50:43:4f:76:06,bridge=pconet,firewall=1 --digest d1"},
		step{cmd: "qm pending 9330", out: ""},
	)
	_, err := managednet.NewHostConfigurer(r, thisNode).AddNIC(t.Context(), vm, 1, spec)
	require.NoError(t, err)
}

func TestHostRemoveNIC(t *testing.T) {
	t.Run("container", func(t *testing.T) {
		r := run(t,
			step{cmd: ctConfig, out: config("d1", ctNet0, `"net1":"name=eth1,bridge=pconet,hwaddr=06:50:43:4f:76:05,ip=10.203.118.5/24"`)},
			step{cmd: "pct set 9320 --delete net1 --digest d1"},
		)
		require.NoError(t, managednet.NewHostConfigurer(r, thisNode).RemoveNIC(t.Context(), container, 1))
	})
	t.Run("vm, stale once", func(t *testing.T) {
		r := run(t,
			step{cmd: vmConfig, out: config("d1", vmNet0, `"net1":"virtio=06:50:43:4f:76:06,bridge=pconet"`)},
			step{cmd: "qm set 9330 --delete net1 --digest d1", err: stale("qm set")},
			step{cmd: vmConfig, out: config("d2", vmNet0, `"net1":"virtio=06:50:43:4f:76:06,bridge=pconet"`)},
			step{cmd: "qm set 9330 --delete net1 --digest d2"},
		)
		require.NoError(t, managednet.NewHostConfigurer(r, thisNode).RemoveNIC(t.Context(), vm, 1))
	})
	t.Run("a card that is gone, or whose deletion waits for a restart", func(t *testing.T) {
		r := run(t, step{cmd: ctConfig, out: config("d1", ctNet0)})
		require.NoError(t, managednet.NewHostConfigurer(r, thisNode).RemoveNIC(t.Context(), container, 1))
	})
	t.Run("a card on another bridge", func(t *testing.T) {
		r := run(t, step{cmd: ctConfig, out: config("d1", ctNet0)})
		err := managednet.NewHostConfigurer(r, thisNode).RemoveNIC(t.Context(), container, 0)
		require.EqualError(t, err, "removing net0 from lxc/9320: net0 is a card on vmbr0; pco removes only a card on pconet")
	})
}

func TestHostSetLinkDown(t *testing.T) {
	t.Run("container", func(t *testing.T) {
		r := run(t,
			step{cmd: ctConfig, out: config("d1", ctNet0, `"net1":"name=eth1,bridge=pconet,hwaddr=BC:24:11:AA:BB:CC,ip=10.203.118.5/24,type=veth"`)},
			step{cmd: "pct set 9320 --net1 name=eth1,bridge=pconet,hwaddr=BC:24:11:AA:BB:CC,ip=10.203.118.5/24,type=veth,link_down=1 --digest d1"},
		)
		require.NoError(t, managednet.NewHostConfigurer(r, thisNode).SetLinkDown(t.Context(), container, 1))
	})
	t.Run("vm with link_down=0", func(t *testing.T) {
		r := run(t,
			step{cmd: vmConfig, out: config("d1", vmNet0, `"net1":"virtio=BC:24:11:AA:BB:CC,bridge=pconet,link_down=0,firewall=1"`)},
			step{cmd: "qm set 9330 --net1 virtio=BC:24:11:AA:BB:CC,bridge=pconet,firewall=1,link_down=1 --digest d1"},
			step{cmd: "qm pending 9330", out: "cur net1: virtio=BC:24:11:AA:BB:CC,bridge=pconet,firewall=1,link_down=1\n"},
		)
		require.NoError(t, managednet.NewHostConfigurer(r, thisNode).SetLinkDown(t.Context(), vm, 1))
	})
	t.Run("vm without network hotplug", func(t *testing.T) {
		r := run(t,
			step{cmd: vmConfig, out: config("d1", vmNet0, `"net1":"virtio=BC:24:11:AA:BB:CC,bridge=pconet"`)},
			step{cmd: "qm set 9330 --net1 virtio=BC:24:11:AA:BB:CC,bridge=pconet,link_down=1 --digest d1"},
			step{cmd: "qm pending 9330", out: "cur net1: virtio=BC:24:11:AA:BB:CC,bridge=pconet\nnew net1: virtio=BC:24:11:AA:BB:CC,bridge=pconet,link_down=1\n"},
		)
		err := managednet.NewHostConfigurer(r, thisNode).SetLinkDown(t.Context(), vm, 1)
		require.ErrorIs(t, err, managednet.ErrPending)
		require.EqualError(t, err, "setting the link of net1 of qemu/9330 down: the change waits for the guest's restart")
	})
	t.Run("vm whose link_down waits for a restart", func(t *testing.T) {
		r := run(t,
			step{cmd: vmConfig, out: config("d1", vmNet0, `"net1":"virtio=BC:24:11:AA:BB:CC,bridge=pconet,link_down=1"`)},
			step{cmd: "qm pending 9330", out: "cur net1: virtio=BC:24:11:AA:BB:CC,bridge=pconet\nnew net1: virtio=BC:24:11:AA:BB:CC,bridge=pconet,link_down=1\n"},
		)
		err := managednet.NewHostConfigurer(r, thisNode).SetLinkDown(t.Context(), vm, 1)
		require.ErrorIs(t, err, managednet.ErrPending, "nothing is written again, and the change still waits")
	})
	t.Run("a link that is down", func(t *testing.T) {
		r := run(t, step{cmd: ctConfig, out: config("d1", ctNet0, `"net1":"name=eth1,bridge=pconet,hwaddr=BC:24:11:AA:BB:CC,link_down=1"`)})
		require.NoError(t, managednet.NewHostConfigurer(r, thisNode).SetLinkDown(t.Context(), container, 1))
	})
	t.Run("a card on another bridge", func(t *testing.T) {
		r := run(t, step{cmd: ctConfig, out: config("d1", ctNet0)})
		err := managednet.NewHostConfigurer(r, thisNode).SetLinkDown(t.Context(), container, 0)
		require.EqualError(t, err, "setting the link of net0 of lxc/9320 down: net0 is a card on vmbr0; pco sets down only a card on pconet")
	})
	t.Run("a card that is gone", func(t *testing.T) {
		r := run(t, step{cmd: ctConfig, out: config("d1", ctNet0)})
		err := managednet.NewHostConfigurer(r, thisNode).SetLinkDown(t.Context(), container, 1)
		require.EqualError(t, err, "setting the link of net1 of lxc/9320 down: the guest has no net1")
	})
	t.Run("a container that refuses the option", func(t *testing.T) {
		set := "pct set 9320 --net1 name=eth1,bridge=pconet,hwaddr=BC:24:11:AA:BB:CC,link_down=1 --digest d1"
		r := run(t,
			step{cmd: ctConfig, out: config("d1", ctNet0, `"net1":"name=eth1,bridge=pconet,hwaddr=BC:24:11:AA:BB:CC"`)},
			step{cmd: set, err: fmt.Errorf("%s: exit status 255: format error\nlink_down: property is not defined in schema and the schema does not allow additional properties", set)},
		)
		err := managednet.NewHostConfigurer(r, thisNode).SetLinkDown(t.Context(), container, 1)
		require.ErrorIs(t, err, managednet.ErrLinkDownRefused)
	})
	t.Run("a write that fails otherwise", func(t *testing.T) {
		set := "pct set 9320 --net1 name=eth1,bridge=pconet,hwaddr=BC:24:11:AA:BB:CC,link_down=1 --digest d1"
		r := run(t,
			step{cmd: ctConfig, out: config("d1", ctNet0, `"net1":"name=eth1,bridge=pconet,hwaddr=BC:24:11:AA:BB:CC"`)},
			step{cmd: set, err: fmt.Errorf("%s: exit status 2: CT is locked (backup)", set)},
		)
		err := managednet.NewHostConfigurer(r, thisNode).SetLinkDown(t.Context(), container, 1)
		require.Error(t, err)
		require.NotErrorIs(t, err, managednet.ErrLinkDownRefused, "only a refusal of the option is the sentinel")
	})
}

func TestHostCanWrite(t *testing.T) {
	c := managednet.NewHostConfigurer(run(t), thisNode)
	ok, why, err := c.CanWrite(t.Context(), container)
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, why)

	elsewhere := container
	elsewhere.Node = "pve2"
	ok, why, err = c.CanWrite(t.Context(), elsewhere)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, "the guest is on node pve2, and pco changes the guests of pve1 only", why)
}
