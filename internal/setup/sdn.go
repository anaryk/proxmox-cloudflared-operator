package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// The objects of the managed network in the SDN of the cluster: a zone of
// type simple with one vnet, without a subnet, whose bridge carries the
// managed cards of the guests and pco's leg.
const (
	ManagedZone = "pco"
	ManagedVnet = "pconet"
)

// applyQuestion is asked before the SDN configuration is applied.
const applyQuestion = "Apply the SDN configuration? It reloads the network on every node."

// SDN makes the zone and the vnet of the managed network, and removes them,
// through pvesh as root on the node. Proxmox VE 9 has a lock of the SDN
// configuration, which every change and the apply carry, so that nothing of
// somebody else's is applied with pco's between the look at what is pending
// and the apply; Proxmox VE 8 has none, and there the pending changes are
// read again right before the apply.
type SDN struct {
	Run Runner
	Ask Prompter
	// Ours is the manifest as the run found it: a zone or vnet it notes as made
	// by pco whose change is pending is pco's, not somebody else's.
	Ours Manifest
	// Record notes in the manifest what is about to be made, before the
	// command that makes it, and takes the note back.
	Record func(change func(*Manifest)) error
	// IsolatePorts keeps the ports of the vnet from each other. It is set on
	// a host, where the bridge isolates the guests; not in the appliance,
	// whose own card is a port of the vnet and where the guest firewall
	// isolates.
	IsolatePorts bool
	// Confirm applies the changes of others that are pending along with
	// pco's; without it they refuse the step.
	Confirm bool
	// Yes applies without asking.
	Yes bool
}

// sdnLock is the lock of the SDN configuration a run holds; its token is
// empty where Proxmox VE has no lock.
type sdnLock struct {
	token   string
	applied bool
}

// args are the parameters that carry the lock to a change.
func (l *sdnLock) args() []string {
	if l.token == "" {
		return nil
	}
	return []string{"--lock-token", l.token}
}

// lock takes the lock of the SDN configuration, also while changes are
// pending: what is pending is looked at under it.
func (s SDN) lock(ctx context.Context) (*sdnLock, error) {
	out, err := s.Run.Run(ctx, "pvesh", "create", "/cluster/sdn/lock", "--allow-pending", "1", "--output-format", "json")
	if err != nil {
		if noLockEndpoint(err) {
			s.Ask.Info("SDN: this Proxmox VE has no lock of the SDN configuration; the pending changes are read again right before the apply")
			return &sdnLock{}, nil
		}
		return nil, fmt.Errorf("taking the lock of the SDN configuration: %w; a lock that a run cut short left is "+
			"released with pvesh delete /cluster/sdn/lock --force", err)
	}
	var token string
	if json.Unmarshal([]byte(out), &token) != nil || strings.TrimSpace(token) == "" || strings.ContainsFunc(token, isSpace) {
		return nil, errors.New("taking the lock of the SDN configuration: pvesh printed no lock token")
	}
	return &sdnLock{token: token}, nil
}

func isSpace(r rune) bool { return r <= ' ' }

// noLockEndpoint reports whether pvesh says that the API has no lock of the
// SDN configuration, as on Proxmox VE 8.
func noLockEndpoint(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "handler defined for") || strings.Contains(msg, "not implemented")
}

// release releases the lock. The apply releases it on Proxmox VE 9, so after
// an apply a release that fails is taken for one that found no lock; before
// it, the admin is told how to lift the lock.
func (s SDN) release(ctx context.Context, l *sdnLock) {
	if l.token == "" {
		return
	}
	_, err := s.Run.Run(context.WithoutCancel(ctx), "pvesh", "delete", "/cluster/sdn/lock", "--lock-token", l.token)
	if err != nil && !l.applied {
		s.Ask.Warn("the lock of the SDN configuration could not be released (%v): pvesh delete /cluster/sdn/lock --force releases it", err)
	}
}

// read reads the SDN with what is pending, and the subnets of the vnets that
// are not about to go when subnets is set.
func (s SDN) read(ctx context.Context, subnets bool) (pve.SDN, error) {
	var cfg pve.SDN
	out, err := s.Run.Run(ctx, "pvesh", "get", "/cluster/sdn/zones", "--pending", "1", "--output-format", "json")
	if err != nil {
		return cfg, fmt.Errorf("reading the SDN zones: %w", err)
	}
	if cfg.Zones, err = pve.DecodeSDNZones([]byte(out)); err != nil {
		return cfg, err
	}
	if out, err = s.Run.Run(ctx, "pvesh", "get", "/cluster/sdn/vnets", "--pending", "1", "--output-format", "json"); err != nil {
		return cfg, fmt.Errorf("reading the SDN vnets: %w", err)
	}
	if cfg.Vnets, err = pve.DecodeSDNVnets([]byte(out)); err != nil {
		return cfg, err
	}
	if !subnets {
		return cfg, nil
	}
	for _, v := range cfg.Vnets {
		if v.State == pve.SDNDeleted {
			continue
		}
		out, err := s.Run.Run(ctx, "pvesh", "get", "/cluster/sdn/vnets/"+v.Name+"/subnets", "--pending", "1", "--output-format", "json")
		if err != nil {
			return cfg, fmt.Errorf("reading the subnets of SDN vnet %s: %w", v.Name, err)
		}
		found, err := pve.DecodeSDNSubnets([]byte(out))
		if err != nil {
			return cfg, err
		}
		cfg.Subnets = append(cfg.Subnets, found...)
	}
	return cfg, nil
}

// managedObjects is what the SDN holds of the managed network.
type managedObjects struct {
	zone *pve.SDNZone
	vnet *pve.SDNVnet
}

// find finds the zone and the vnet of the managed network as configured; one
// whose deletion is pending is gone.
func find(cfg pve.SDN) managedObjects {
	var m managedObjects
	if i := slices.IndexFunc(cfg.Zones, func(z pve.SDNZone) bool { return z.Name == ManagedZone && z.State != pve.SDNDeleted }); i >= 0 {
		m.zone = &cfg.Zones[i]
	}
	if i := slices.IndexFunc(cfg.Vnets, func(v pve.SDNVnet) bool { return v.Name == ManagedVnet && v.State != pve.SDNDeleted }); i >= 0 {
		m.vnet = &cfg.Vnets[i]
	}
	return m
}

// split divides what is pending into pco's own changes, of the zone and the
// vnet it made, and the others'.
func (s SDN) split(cfg pve.SDN, madeZone, madeVnet bool) (ours, others []string) {
	var zones []pve.SDNZone
	for _, z := range cfg.Zones {
		if z.Name == ManagedZone && z.State != "" && (s.Ours.CreatedZone || madeZone) {
			ours = append(ours, fmt.Sprintf("zone %s (%s)", z.Name, z.State))
			continue
		}
		zones = append(zones, z)
	}
	var vnets []pve.SDNVnet
	for _, v := range cfg.Vnets {
		if v.Name == ManagedVnet && v.State != "" && (s.Ours.CreatedVnet || madeVnet) {
			ours = append(ours, fmt.Sprintf("vnet %s (%s)", v.Name, v.State))
			continue
		}
		vnets = append(vnets, v)
	}
	others = pve.SDN{Zones: zones, Vnets: vnets, Subnets: cfg.Subnets}.Pending()
	return ours, others
}

// The ways out of changes of others that are pending, for a run that makes
// the managed network and for one that removes it.
const (
	othersOnEnsure = "apply them or roll them back first, or pass --confirm-sdn-apply to apply them along"
	othersOnRemove = "apply them or roll them back, then run this again"
)

// refuseOthers refuses changes of others that are pending, unless Confirm
// applies them along; how says what to do about them.
func (s SDN) refuseOthers(others []string, how string) error {
	if len(others) == 0 || s.Confirm {
		return nil
	}
	return fmt.Errorf("the SDN has changes that are not applied and are not pco's: %s; an apply would apply them as well: %s",
		strings.Join(others, ", "), how)
}

// check refuses a zone or a vnet of the managed network's names that it
// cannot be.
func (s SDN) check(cfg pve.SDN, m managedObjects) error {
	if m.zone != nil && m.zone.Type != "simple" {
		return fmt.Errorf("SDN zone %s is there with type %s: the managed network needs a zone of type simple", ManagedZone, m.zone.Type)
	}
	if m.vnet == nil {
		return nil
	}
	if m.vnet.Zone != ManagedZone {
		return fmt.Errorf("SDN vnet %s is there in zone %s, not in zone %s", ManagedVnet, m.vnet.Zone, ManagedZone)
	}
	if m.vnet.IsolatePorts != s.IsolatePorts {
		want := isolateValue(s.IsolatePorts)
		return fmt.Errorf("SDN vnet %s has isolate-ports %s, and the managed network of this node needs %s: "+
			"pvesh set /cluster/sdn/vnets/%s --isolate-ports %s, then pvesh set /cluster/sdn",
			ManagedVnet, isolateValue(m.vnet.IsolatePorts), want, ManagedVnet, want)
	}
	for _, n := range cfg.Subnets {
		if n.Vnet == ManagedVnet && n.State != pve.SDNDeleted {
			return fmt.Errorf("SDN vnet %s has the subnet %s: the managed network has none, pco's leg is its only gateway", ManagedVnet, n.CIDR)
		}
	}
	return nil
}

func isolateValue(on bool) string {
	if on {
		return "1"
	}
	return "0"
}

// Ensure makes the zone and the vnet of the managed network that are not
// there and applies the SDN configuration, under the lock: it looks at what is
// pending first, refuses what is pending of others unless Confirm, notes each
// object in the manifest before making it, shows what the apply applies and
// asks unless Yes. An operator who does not apply has what the run made taken
// back, so that nothing of pco's waits for somebody else's apply.
func (s SDN) Ensure(ctx context.Context) error {
	l, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer s.release(ctx, l)
	cfg, err := s.read(ctx, true)
	if err != nil {
		return err
	}
	m := find(cfg)
	if err := s.check(cfg, m); err != nil {
		return err
	}
	ours, others := s.split(cfg, false, false)
	if err := s.refuseOthers(others, othersOnEnsure); err != nil {
		return err
	}
	var madeZone, madeVnet bool
	if m.zone == nil {
		if err := s.create(ctx, l, "zone", func(m *Manifest, v bool) { m.CreatedZone = v },
			"/cluster/sdn/zones", "--zone", ManagedZone, "--type", "simple"); err != nil {
			return err
		}
		madeZone = true
		ours = append(ours, fmt.Sprintf("zone %s (%s)", ManagedZone, pve.SDNNew))
	}
	if m.vnet == nil {
		if err := s.create(ctx, l, "vnet", func(m *Manifest, v bool) { m.CreatedVnet = v },
			"/cluster/sdn/vnets", "--vnet", ManagedVnet, "--zone", ManagedZone, "--isolate-ports", isolateValue(s.IsolatePorts)); err != nil {
			return err
		}
		madeVnet = true
		ours = append(ours, fmt.Sprintf("vnet %s (%s)", ManagedVnet, pve.SDNNew))
	}
	if len(ours) == 0 && len(others) == 0 {
		s.Ask.Info("SDN: zone %s and vnet %s are there, nothing needed", ManagedZone, ManagedVnet)
		return nil
	}
	if len(ours) > 0 {
		s.Ask.Info("SDN: applying %s", strings.Join(ours, ", "))
	}
	if len(others) > 0 {
		s.Ask.Info("SDN: and changes that are not pco's, as --confirm-sdn-apply says: %s", strings.Join(others, ", "))
	}
	if !s.Yes {
		ok, err := s.Ask.Confirm(applyQuestion, false)
		if err == nil && !ok {
			err = ErrAborted
		}
		if err != nil {
			s.takeBack(ctx, l, madeZone, madeVnet)
			return err
		}
	}
	if l.token == "" {
		if err := s.recheck(ctx, madeZone, madeVnet, othersOnEnsure); err != nil {
			return err
		}
	}
	return s.apply(ctx, l)
}

// recheck reads what is pending again right before an apply without the
// lock, which is the only guard where Proxmox VE has none.
func (s SDN) recheck(ctx context.Context, madeZone, madeVnet bool, how string) error {
	cfg, err := s.read(ctx, true)
	if err != nil {
		return err
	}
	_, others := s.split(cfg, madeZone, madeVnet)
	return s.refuseOthers(others, how)
}

// Remove removes the vnet and the zone of the managed network that the
// manifest notes as pco's, under the lock, and applies, which takes them from
// every node; a zone that holds a vnet of somebody else's stays. What is
// pending of others refuses it. The caller makes sure that no guest has a
// card on the vnet, as the apply takes its bridge away.
func (s SDN) Remove(ctx context.Context) error {
	l, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer s.release(ctx, l)
	cfg, err := s.read(ctx, true)
	if err != nil {
		return err
	}
	ours, others := s.split(cfg, false, false)
	if err := s.refuseOthers(others, othersOnRemove); err != nil {
		return err
	}
	m := find(cfg)
	removed := false
	if s.Ours.CreatedVnet && m.vnet != nil {
		if err := s.remove(ctx, l, "vnet", ManagedVnet, "/cluster/sdn/vnets/"+ManagedVnet, nil); err != nil {
			return err
		}
		removed = true
	}
	if s.Ours.CreatedZone && m.zone != nil {
		var rest []string
		for _, v := range cfg.Vnets {
			if v.Zone == ManagedZone && v.Name != ManagedVnet && v.State != pve.SDNDeleted {
				rest = append(rest, v.Name)
			}
		}
		if len(rest) > 0 {
			s.Ask.Info("SDN: zone %s is kept: it holds the vnet %s as well", ManagedZone, strings.Join(rest, ", "))
		} else {
			if err := s.remove(ctx, l, "zone", ManagedZone, "/cluster/sdn/zones/"+ManagedZone, nil); err != nil {
				return err
			}
			removed = true
		}
	}
	if !removed && len(ours) == 0 {
		return nil
	}
	if l.token == "" {
		if err := s.recheck(ctx, s.Ours.CreatedZone, s.Ours.CreatedVnet, othersOnRemove); err != nil {
			return err
		}
	}
	return s.apply(ctx, l)
}

// apply applies the SDN configuration under the lock, which the apply
// releases.
func (s SDN) apply(ctx context.Context, l *sdnLock) error {
	if _, err := s.Run.Run(ctx, "pvesh", append([]string{"set", "/cluster/sdn"}, l.args()...)...); err != nil {
		return fmt.Errorf("applying the SDN configuration: %w", err)
	}
	l.applied = true
	s.Ask.Info("SDN: applied; the network of every node is reloaded")
	return nil
}

// create notes an object of the managed network in the manifest and makes it
// under the lock. A create that fails and left nothing takes the note back.
func (s SDN) create(ctx context.Context, l *sdnLock, kind string, note func(*Manifest, bool), path string, args ...string) error {
	if err := s.Record(func(m *Manifest) { note(m, true) }); err != nil {
		return err
	}
	name := ManagedZone
	if kind == "vnet" {
		name = ManagedVnet
	}
	if _, err := s.Run.Run(ctx, "pvesh", append(append([]string{"create", path}, args...), l.args()...)...); err != nil {
		if cfg, rerr := s.read(ctx, false); rerr == nil {
			m := find(cfg)
			if kind == "zone" && m.zone == nil || kind == "vnet" && m.vnet == nil {
				if err := s.Record(func(m *Manifest) { note(m, false) }); err != nil {
					s.Ask.Warn("%v", err)
				}
			}
		}
		return fmt.Errorf("creating SDN %s %s: %w", kind, name, err)
	}
	s.Ask.Info("SDN: created %s %s", kind, name)
	return nil
}

// takeBack removes, under the lock, what a run that is not applied made, and
// the notes of it.
func (s SDN) takeBack(ctx context.Context, l *sdnLock, zone, vnet bool) {
	ctx = context.WithoutCancel(ctx)
	if vnet {
		if err := s.remove(ctx, l, "vnet", ManagedVnet, "/cluster/sdn/vnets/"+ManagedVnet, func(m *Manifest) { m.CreatedVnet = false }); err != nil {
			s.Ask.Warn("%v", err)
			return
		}
	}
	if zone {
		if err := s.remove(ctx, l, "zone", ManagedZone, "/cluster/sdn/zones/"+ManagedZone, func(m *Manifest) { m.CreatedZone = false }); err != nil {
			s.Ask.Warn("%v", err)
		}
	}
}

// remove removes an object of the managed network under the lock, and takes
// its note back with unnote, unless that is nil: a removal that is not
// applied yet is still pco's to apply.
func (s SDN) remove(ctx context.Context, l *sdnLock, kind, name, path string, unnote func(*Manifest)) error {
	if _, err := s.Run.Run(ctx, "pvesh", append([]string{"delete", path}, l.args()...)...); err != nil {
		return fmt.Errorf("removing SDN %s %s: %w", kind, name, err)
	}
	s.Ask.Info("SDN: removed %s %s", kind, name)
	if unnote != nil {
		if err := s.Record(unnote); err != nil {
			s.Ask.Warn("%v", err)
		}
	}
	return nil
}
