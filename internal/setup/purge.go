package setup

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// cfObjects is what an install has at Cloudflare, each with the client of a
// credential that reaches it.
type cfObjects struct {
	records []cfRecord
	tunnels []cfTunnel
}

type cfRecord struct {
	api    cfapi.API
	zone   cfapi.Zone
	record cfapi.Record
}

type cfTunnel struct {
	api     cfapi.API
	account string
	tunnel  cfapi.Tunnel
}

func (o cfObjects) empty() bool { return len(o.records) == 0 && len(o.tunnels) == 0 }

// listCloudflare finds what the install has at Cloudflare through every
// stored credential: the DNS records that carry its marker, its tunnel and
// the probe tunnels a credential check left. What Cloudflare refuses to show
// a credential, the credential cannot manage either, and it is skipped with a
// warning; any other failure to list is an error, and the rest is still
// listed.
func (u *uninstall) listCloudflare(ctx context.Context) (cfObjects, error) {
	var found cfObjects
	if u.installID == "" {
		return found, nil
	}
	var errs []error
	seen := make(map[string]bool)
	for _, c := range u.creds {
		api, err := u.newClient(c.Token.Reveal())
		if err != nil {
			errs = append(errs, fmt.Errorf("credential %s: %w", c.ID, err))
			continue
		}
		accounts, err := u.listRecords(ctx, c.ID, api, &found, seen)
		if err != nil {
			errs = append(errs, fmt.Errorf("credential %s: %w", c.ID, err))
		}
		if err := u.listTunnels(ctx, c.ID, api, accounts, &found, seen); err != nil {
			errs = append(errs, fmt.Errorf("credential %s: %w", c.ID, err))
		}
	}
	return found, errors.Join(errs...)
}

// unlisted is the outcome of a listing that failed: nothing for a refusal or
// for what is gone, which say there is nothing the credential manages there,
// and the error otherwise.
func (u *uninstall) unlisted(credID, what string, err error) error {
	switch {
	case cfapi.IsAuth(err):
		u.ask.Warn("credential %s may not list %s; what is there is not deleted", credID, what)
		return nil
	case cfapi.IsNotFound(err):
		return nil
	}
	return fmt.Errorf("listing %s: %w", what, err)
}

// listRecords adds the records of the install in the zones of api, and
// returns the accounts of those zones.
func (u *uninstall) listRecords(ctx context.Context, credID string, api cfapi.API, found *cfObjects, seen map[string]bool) (map[string]bool, error) {
	accounts := make(map[string]bool)
	zones, err := api.Zones(ctx)
	if err != nil {
		return accounts, u.unlisted(credID, "the zones", err)
	}
	var errs []error
	for _, z := range zones {
		accounts[z.AccountID] = true
		records, err := api.Records(ctx, z.ID, cfapi.RecordFilter{CommentPrefix: planner.DNSMarker(u.installID)})
		switch {
		case err != nil && cfapi.IsAuth(err) && u.served != nil && !u.served[z.ID]:
			// A zone the install never served holds none of its records: a
			// token scoped to another zone of the account may not read it.
			continue
		case err != nil:
			errs = append(errs, u.unlisted(credID, "the records of zone "+z.Name, err))
			continue
		}
		for _, rec := range records {
			key := "record " + z.ID + " " + rec.ID
			if reconcile.Owned(u.installID, rec) && !seen[key] {
				seen[key] = true
				found.records = append(found.records, cfRecord{api: api, zone: z, record: rec})
			}
		}
	}
	return accounts, errors.Join(errs...)
}

// listTunnels adds the tunnels of the install in the accounts api sees and
// the accounts of its zones.
func (u *uninstall) listTunnels(ctx context.Context, credID string, api cfapi.API, accounts map[string]bool, found *cfObjects, seen map[string]bool) error {
	var errs []error
	listed, err := api.Accounts(ctx)
	if err != nil {
		errs = append(errs, u.unlisted(credID, "the accounts", err))
	}
	for _, a := range listed {
		accounts[a.ID] = true
	}
	name := planner.TunnelName(u.installID)
	for _, account := range slices.Sorted(maps.Keys(accounts)) {
		tunnels, err := api.Tunnels(ctx, account, name)
		if err != nil {
			errs = append(errs, u.unlisted(credID, "the tunnels of account "+account, err))
			continue
		}
		for _, t := range tunnels {
			key := "tunnel " + t.ID
			if (t.Name == name || planner.IsProbeTunnel(u.installID, t.Name)) && !seen[key] {
				seen[key] = true
				found.tunnels = append(found.tunnels, cfTunnel{api: api, account: account, tunnel: t})
			}
		}
	}
	return errors.Join(errs...)
}

// showCloudflare lists what the install has at Cloudflare, as the survey
// found it.
func (u *uninstall) showCloudflare() {
	f := u.found
	switch {
	case u.installID == "":
		u.ask.Info("  at Cloudflare: nothing, as the store holds no install")
		return
	case f.cloudflareErr != nil:
		u.ask.Info("  at Cloudflare: what install %s has cannot be listed: %v", u.installID, f.cloudflareErr)
		return
	case f.cloudflare.empty():
		u.ask.Info("  at Cloudflare: nothing of install %s", u.installID)
		return
	}
	u.ask.Info("  at Cloudflare, what install %s has:", u.installID)
	for _, r := range f.cloudflare.records {
		u.ask.Info("    DNS record %s %s in zone %s", r.record.Type, r.record.Name, r.zone.Name)
	}
	for _, t := range f.cloudflare.tunnels {
		u.ask.Info("    tunnel %s (%s) in account %s", t.tunnel.Name, t.tunnel.ID, t.account)
	}
}

func (u *uninstall) deleteRecords(ctx context.Context) {
	for _, r := range u.found.cloudflare.records {
		err := r.api.DeleteRecord(ctx, r.zone.ID, r.record.ID)
		if err != nil && !cfapi.IsNotFound(err) {
			u.fail("DNS record %s in zone %s was not deleted: %v", r.record.Name, r.zone.Name, err)
			continue
		}
		u.ask.Info("Cloudflare: deleted DNS record %s in zone %s", r.record.Name, r.zone.Name)
	}
}

// deleteTunnels deletes the tunnels, once their connectors are stopped.
// Cloudflare refuses to delete a tunnel while it still counts a connection.
func (u *uninstall) deleteTunnels(ctx context.Context) {
	for _, t := range u.found.cloudflare.tunnels {
		err := t.api.DeleteTunnel(ctx, t.account, t.tunnel.ID)
		if err != nil && !cfapi.IsNotFound(err) {
			u.fail("tunnel %s (%s) in account %s was not deleted: %v", t.tunnel.Name, t.tunnel.ID, t.account, err)
			continue
		}
		u.ask.Info("Cloudflare: deleted tunnel %s (%s)", t.tunnel.Name, t.tunnel.ID)
	}
}
