package setup

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// ManagedOptions are the answers of pco network managed enable on a host,
// given in advance.
type ManagedOptions struct {
	Yes             bool   // apply the SDN configuration without asking
	ConfirmSDNApply bool   // apply the changes of others that are pending as well
	Node            string // default: the host name
}

// EnableManaged makes the managed network of a host that is set up: what the
// step managed of pco setup --managed makes, alone.
func (s *Setup) EnableManaged(ctx context.Context, o ManagedOptions) error {
	if err := s.refuseAppliance(); err != nil {
		return err
	}
	if s.host.euid() != 0 {
		return errors.New("pco network managed enable must run as root")
	}
	r := &run{Setup: s, o: Options{Yes: o.Yes, Managed: true, ConfirmSDNApply: o.ConfirmSDNApply, Node: o.Node}}
	if r.o.Node == "" {
		name, err := s.host.hostname()
		if err != nil {
			return fmt.Errorf("reading the host name: %w", err)
		}
		r.o.Node, _, _ = strings.Cut(name, ".")
	}
	switch _, found, err := s.st.Install(); {
	case errors.Is(err, store.ErrNoRoot), err == nil && !found:
		return errors.New("pco is not set up on this node; run pco setup --managed")
	case err != nil:
		return fmt.Errorf("reading the install: %w", err)
	}
	if err := r.refuseOtherNode(); err != nil {
		return err
	}
	m, _, err := readManifest(s.manifestPath())
	if err != nil {
		return fmt.Errorf("reading the manifest: %w", err)
	}
	r.manifest = m
	return r.ensureManaged(ctx)
}

// ensureManaged is the step managed: the SDN zone and vnet of the managed
// network with isolated ports, the network in the store, and the unit of the
// gateway namespace when it is the first network of the node, as the daemon
// cannot pin the namespace itself.
func (r *run) ensureManaged(ctx context.Context) error {
	have, err := r.st.Networks()
	if err != nil {
		return fmt.Errorf("reading the networks: %w", err)
	}
	_, stored := have[ManagedVnet]
	mark := freeMark(have)
	if !stored && mark == 0 {
		return fmt.Errorf("network %s: pco reaches at most %d networks, and the node has %d: detach one first",
			ManagedVnet, gateway.MaxMark, len(have))
	}
	sdn := SDN{
		Run: r.run, Ask: r.ask, Ours: r.manifest, Record: r.record,
		IsolatePorts: true, Confirm: r.o.ConfirmSDNApply, Yes: r.o.Yes,
	}
	if err := sdn.Ensure(ctx); err != nil {
		return err
	}
	if stored {
		r.ask.Info("network %s: in the store, nothing needed", ManagedVnet)
		return nil
	}
	if err := r.saveManagedNetwork(mark); err != nil {
		return err
	}
	if len(have) > 0 {
		return nil
	}
	return r.enableNetns(ctx)
}

// freeMark is the lowest mark no network holds, 0 when none is free.
func freeMark(have map[string]store.Network) int {
	taken := map[int]bool{}
	for _, n := range have {
		taken[n.Mark] = true
	}
	for mark := 1; mark <= gateway.MaxMark; mark++ {
		if !taken[mark] {
			return mark
		}
	}
	return 0
}

// saveManagedNetwork stores the managed network of the settings' subnet, with
// the leg of this node at its first address.
func (r *run) saveManagedNetwork(mark int) error {
	settings, err := r.st.Settings()
	if err != nil {
		return fmt.Errorf("reading the settings: %w", err)
	}
	subnet := settings.Managed.Subnet
	n := store.Network{
		ID: ManagedVnet, Kind: store.NetworkManaged, Bridge: ManagedVnet, Prefix: subnet,
		Legs:   map[string]netip.Addr{r.o.Node: subnet.Addr().Next()},
		LegMAC: store.LegMACFor(store.NetworkManaged, mark), Mark: mark, Scope: store.ScopeNode, CreatedAt: r.now(),
	}
	if err := r.st.SaveNetwork(n, 0); err != nil {
		return fmt.Errorf("storing the network %s: %w", ManagedVnet, err)
	}
	r.ask.Info("network %s: stored, %s with the leg %s on node %s", ManagedVnet, subnet, subnet.Addr().Next(), r.o.Node)
	return nil
}

// enableNetns enables and starts the unit of the gateway namespace.
func (r *run) enableNetns(ctx context.Context) error {
	if !r.unitInstalled(netnsUnit) {
		r.ask.Warn("%s is not installed, as when pco runs from a build tree: run pco netns up as root before the daemon", netnsUnit)
		return nil
	}
	if _, err := r.run.Run(ctx, "systemctl", "enable", "--now", netnsUnit); err != nil {
		return fmt.Errorf("enabling %s: %w", netnsUnit, err)
	}
	r.ask.Info("%s: enabled and running; the daemon puts its leg on %s in its next cycle", netnsUnit, ManagedVnet)
	return nil
}
