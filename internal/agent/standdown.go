package agent

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
)

const enrolsAgain = "pco setup (host) or a new join token (appliance) enrols it again"

// StandDown takes the node out of service after a definitive refusal of the
// controller: the egress set is emptied, the connectors are stopped and
// disabled and their files removed, run tokens and all, and the desired
// state kept on disk goes, or, for a node held contested or not enrolled,
// only its run tokens. The gateway stays until pco uninstall, so that the
// guests on a managed network keep their addresses. The agent then serves
// nothing until a controller serves it again. A refusal that is not
// definitive, as one of a protocol version, is no reason to stand down.
// What could not be done is returned; the rest is done all the same.
func (a *Agent) StandDown(ctx context.Context, r agentproto.Refusal) error {
	if !r.Definitive() {
		return fmt.Errorf("refusal %q is not definitive: the agent serves on what it was last told", r.Code)
	}
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	why := standDownLine(r.Code, a.d.InstallID, a.d.Now())
	a.stood = why
	var errs []error
	a.egMu.Lock()
	a.set, a.pins = nil, map[netip.Addr]egress.Pin{}
	err := a.d.Egress.Set(ctx, nil)
	a.egMu.Unlock()
	if err != nil && !errors.Is(err, egress.ErrOff) {
		errs = append(errs, fmt.Errorf("emptying the egress filter: %w", err))
	}
	a.vouch.pin(nil, a.d.Now())
	if err := a.saveSet(); err != nil {
		errs = append(errs, err)
	}
	if err := a.d.Connectors.StopAll(ctx, a.d.InstallID); err != nil {
		errs = append(errs, fmt.Errorf("stopping the connectors: %w", err))
	}
	if err := a.d.Connectors.PruneInstall(ctx, a.d.InstallID, nil); err != nil {
		errs = append(errs, fmt.Errorf("removing the connectors and their run tokens: %w", err))
	}
	keep := r.Code == agentproto.RefusalContested || r.Code == agentproto.RefusalNotEnrolled
	if err := a.forgetDesired(keep); err != nil {
		errs = append(errs, fmt.Errorf("forgetting the desired state: %w", err))
	}
	a.d.Log.Error().Str("code", r.Code).Str("refusal", r.Message).Msg("the controller refused this node for good")
	a.event(levelError, kindNode, a.d.Node, why)
	a.republish()
	return errors.Join(errs...)
}

// standDownLine says why the node stood down, and what brings it back.
func standDownLine(code, install string, at time.Time) string {
	when := at.UTC().Format(time.RFC3339)
	switch code {
	case agentproto.RefusalRemoved:
		return fmt.Sprintf("this node was removed from install %s at %s; %s", install, when, enrolsAgain)
	case agentproto.RefusalOtherInstall:
		return fmt.Sprintf("the controller serves another install than this node's %s (refused at %s); %s", install, when, enrolsAgain)
	case agentproto.RefusalContested:
		return fmt.Sprintf("the controller holds this node contested since %s: two nodes claim its name or its certificate; "+
			"an admin decides with pco node admit or pco node forget", when)
	case agentproto.RefusalNotEnrolled:
		return fmt.Sprintf("the controller does not know the certificate of this node (refused at %s); %s", when, enrolsAgain)
	}
	return fmt.Sprintf("the controller refused this node for good (%s) at %s; %s", code, when, enrolsAgain)
}
