package appliance

import (
	"context"
	"errors"
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// ConvertToController records in the store of a combined appliance that it
// is the controller of a cluster, and restarts the daemon, which makes the
// install's CA and serves the agent endpoint from its next start. The
// connectors are units of their own and run on throughout. A controller is
// left as it is; an agent is never converted.
func ConvertToController(ctx context.Context, st *store.Store, sysd connector.Systemd) (string, error) {
	inst, found, err := st.Install()
	switch {
	case err != nil:
		return "", fmt.Errorf("reading the install: %w", err)
	case !found || inst.Appliance == nil:
		return "", errors.New("this appliance holds no install of its own to convert")
	}
	switch inst.Appliance.KindName() {
	case store.ApplianceController:
		return "kind: controller already, nothing to do", nil
	case store.ApplianceAgent:
		return "", errors.New("this container is an agent: it follows a controller, and is not converted")
	}
	inst.Appliance.Kind = store.ApplianceController
	if err := st.SaveInstall(inst); err != nil {
		return "", fmt.Errorf("recording the kind: %w", err)
	}
	if err := sysd.Restart(ctx, Unit); err != nil {
		return "", fmt.Errorf("the kind is recorded, and restarting %s failed: %w; restart it to serve the agent endpoint", Unit, err)
	}
	return "kind: controller; " + Unit + " restarted", nil
}
