package applianceinstall

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestACombinedApplianceBecomesTheControllerInPlace(t *testing.T) {
	e := newEnv(t)
	e.install(e.options())
	before := e.node.count("pct create")

	require.NoError(t, e.in.Convert(t.Context(), ConvertOptions{VMID: 100, Yes: true}), e.ask.text())

	ct := e.node.cts[100]
	require.Equal(t, "pco-appliance;pco-controller", ct.cfg["tags"])
	require.Equal(t, []int{100}, e.node.converted)
	require.True(t, ct.running, "it serves throughout")
	require.Equal(t, before, e.node.count("pct create"), "nothing is made anew")
	fw := e.node.guestFirewalls[100]
	require.Equal(t, []string{agentsIPSet}, fw.ipsets)
	require.Len(t, fw.rules, 1)
	require.True(t, e.node.has("/storage/local-zfs", "token", "pco@pve!vm100", roleDatastoreAudit))
	require.Contains(t, e.ask.text(), "pco cluster join-token --node <node>")

	require.NoError(t, e.in.Convert(t.Context(), ConvertOptions{VMID: 100, Yes: true}), "a convert runs again")
	require.Len(t, e.node.guestFirewalls[100].rules, 1, "and adds no second rule")
}

func TestOnlyACombinedApplianceOnThisNodeIsConverted(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(e *testEnv)
		want  string
	}{
		{"an agent", func(e *testEnv) { e.node.cts[100].cfg["tags"] = ctTags(KindAgent) }, "lxc/100 is no combined appliance"},
		{"not an appliance", func(e *testEnv) { e.node.cts[100].cfg["description"] = "web server\n" }, "is not an appliance pco appliance install made"},
		{"stopped", func(e *testEnv) { e.node.cts[100].running = false }, "lxc/100 is stopped"},
		{"beside a controller", func(e *testEnv) { e.withController() }, "the controller lxc/200 serves this cluster already"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.install(e.options())
			tt.setup(e)

			err := e.in.Convert(t.Context(), ConvertOptions{VMID: 100, Yes: true})

			require.ErrorContains(t, err, tt.want)
			require.Empty(t, e.node.converted)
		})
	}
}

func TestAConvertedControllerOnHAIsAnHAResource(t *testing.T) {
	e := newEnv(t)
	e.install(e.options())

	require.NoError(t, e.in.Convert(t.Context(), ConvertOptions{VMID: 100, Yes: true, HA: true}))

	require.Equal(t, []string{"add ct:100 --state started"}, e.node.ha)
}
