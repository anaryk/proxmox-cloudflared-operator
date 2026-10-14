package appliance_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestACombinedApplianceIsRecordedAsTheController(t *testing.T) {
	e := newInitEnv(t)
	require.NoError(t, e.init(e.bootstrap("install")))
	e.order = nil
	sysd := units{log: &e.order}

	line, err := appliance.ConvertToController(context.Background(), e.st, sysd)

	require.NoError(t, err)
	require.Equal(t, "kind: controller; pco.service restarted", line)
	require.Equal(t, store.ApplianceController, e.install().Appliance.Kind)
	require.Equal(t, []string{"restart pco.service"}, e.order)

	line, err = appliance.ConvertToController(context.Background(), e.st, sysd)
	require.NoError(t, err)
	require.Contains(t, line, "nothing to do")
	require.Len(t, e.order, 1, "no second restart")
}

func TestAnAgentIsNeverConverted(t *testing.T) {
	e := newInitEnv(t)
	require.NoError(t, e.init(e.bootstrap("install", asAgent)))

	_, err := appliance.ConvertToController(context.Background(), e.st, units{log: &e.order})

	require.ErrorContains(t, err, "this container is an agent")
	require.Equal(t, store.ApplianceAgent, e.install().Appliance.Kind)
}
