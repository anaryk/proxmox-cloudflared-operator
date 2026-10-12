package main

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// managedState is the healthy state with the managed network of an
// appliance: a card in each state, two guests that wait and a foreign card.
func managedState() engine.State {
	st := healthyState()
	st.Profile = "appliance"
	detachAt := t0.Add(10 * time.Minute)
	removedAt := t0.Add(-time.Minute)
	st.Managed = engine.ManagedView{
		Enabled: true, Consent: "auto", AutoAllowed: true, TagHonoured: true, Subnet: netip.MustParsePrefix("10.203.118.0/24"),
		Leg: &engine.LegView{Node: "pve1", Addr: netip.MustParseAddr("10.203.118.1"), Up: true},
		Attachments: []engine.AttachmentView{
			{
				Attachment: store.Attachment{Owner: "lxc/201", Identity: "mac:201", NIC: 1, MAC: "06:50:43:4f:76:02", Addr: netip.MustParseAddr("10.203.118.2"),
					Consent: "tag", Since: t0.Add(-2 * time.Hour)},
				Guest: lxc(201, "new-1"), State: engine.AttachmentAttached,
			},
			{
				Attachment: store.Attachment{Owner: "qemu/105", Identity: "uuid:105", NIC: 2, MAC: "06:50:43:4f:76:03", Addr: netip.MustParseAddr("10.203.118.3"),
					Consent: "auto", Since: t0.Add(-time.Hour)},
				Guest: qemu(105, "batch-5"), State: engine.AttachmentPendingReboot, Detail: "waits for the restart of the guest",
			},
			{
				Attachment: store.Attachment{Owner: "qemu/106", Identity: "uuid:106", NIC: 1, MAC: "06:50:43:4f:76:04", Addr: netip.MustParseAddr("10.203.118.4"),
					Consent: "approval", Since: t0.Add(-3 * time.Hour), DetachAt: &detachAt, DetachWhy: "cf-tunnel-managed=off"},
				Guest: qemu(106, "old-6"), State: engine.AttachmentDetaching, Detail: "goes at 2026-10-01T12:10:00Z: cf-tunnel-managed=off",
			},
			{
				Attachment: store.Attachment{Owner: "qemu/108", Identity: "uuid:108", NIC: 1, MAC: "06:50:43:4f:76:05", Addr: netip.MustParseAddr("10.203.118.5"),
					Consent: "tag", Since: t0.Add(-4 * time.Hour), DetachWhy: "the guest is gone, or no longer carries the tag cf-tunnel", RemovedAt: &removedAt},
				Guest: qemu(108, ""), State: engine.AttachmentDetachPending,
				Detail: "the guest is gone, or no longer carries the tag cf-tunnel; the card is removed once the configuration no longer lists it",
			},
			{
				Attachment: store.Attachment{Owner: "lxc/203", Identity: "mac:203", NIC: 1, MAC: "06:50:43:4f:76:06", Addr: netip.MustParseAddr("10.203.118.6"),
					Consent: "tag", Since: t0.Add(-5 * time.Hour)},
				Guest: lxc(203, "moved-3"), State: engine.AttachmentNeedsGrant,
				GrantCmd: "pveum acl modify /vms/203 --users pco@pve --tokens 'pco@pve!vm9250' --roles PCOManaged",
			},
		},
		Waiting: []engine.ManagedWaiting{
			{
				GuestView: *lxc(204, "gui-4"), Identity: "mac:204",
				Why: "needs-firewall-confirmation: turning on the firewall of lxc/204 (gui-4) also filters net0 (firewall=1); " +
					"run pco managed approve lxc/204 --accept-firewall-impact, or set managed.acceptFirewallImpact for every guest",
			},
			{GuestView: *qemu(109, "other-9"), Identity: "uuid:109", Why: "guest is on node pve2, not pve1"},
		},
		Foreign: []engine.ForeignView{{
			GuestView: *lxc(205, "new-1-clone"), NIC: 1, MAC: "06:50:43:4f:76:02", Action: engine.ForeignLinkDown,
			Why: "a card on pconet that pco did not attach, as a clone or a restore copies one; pco sets its link down",
		}},
		Free:      247,
		Vnet:      engine.VnetView{OK: true, ZoneType: "simple"},
		PoolGrant: "web",
		Filtered:  &engine.FilteredView{Holds: true, Backend: "pve-firewall", Offered: true},
	}
	return st
}

func lxc(vmid int, name string) *engine.GuestView {
	return &engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindLXC, VMID: vmid}, Name: name}
}

func TestManagedStatusGolden(t *testing.T) {
	r, _ := daemonWith(t, managedState())

	res := r.run("", "managed", "status")

	require.NoError(t, res.err)
	requireGolden(t, "managed_status.golden", res.out)

	t.Run("stopped and refused", func(t *testing.T) {
		st := managedState()
		st.Managed.AutoStopped = "registered-tags could not be read"
		st.Managed.Refused = "the datacenter firewall is off; no guest rule is in force until it is turned on"
		st.Managed.Filtered = &engine.FilteredView{Backend: "proxmox-firewall"}
		r, _ := daemonWith(t, st)

		res := r.run("", "managed", "status")

		require.NoError(t, res.err)
		requireGolden(t, "managed_status_refused.golden", res.out)
	})

	t.Run("not enabled", func(t *testing.T) {
		r, _ := daemonWith(t, healthyState())

		res := r.run("", "managed", "status")

		require.NoError(t, res.err)
		require.Equal(t, "The managed network is not enabled; pco network managed enable makes it.\n", res.out)
	})
}

func TestManagedStatusJSONIsWhatTheDaemonSent(t *testing.T) {
	const raw = `{"enabled":true,"consent":"per-guest","attachments":[],"waiting":[],"foreign":[],"free":253}`
	r := newRunner(t, serveRaw(t, map[string]rawReply{"GET /v1/managed": {200, raw}}))

	res := r.run("", "--json", "managed", "status")

	require.NoError(t, res.err)
	require.Equal(t, indented(t, raw), res.out)
}

func TestStatusShowsTheManagedLine(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(v *engine.ManagedView)
		want   string
	}{
		{"auto", func(*engine.ManagedView) {}, "Managed:     auto, 5 attached, 2 waiting, 1 foreign card\n"},
		{"stopped", func(v *engine.ManagedView) { v.AutoStopped = "registered-tags could not be read" },
			"Managed:     auto (stopped: registered-tags could not be read), 5 attached, 2 waiting, 1 foreign card\n"},
		{"per-guest, nothing waits", func(v *engine.ManagedView) { v.Consent = "per-guest"; v.Waiting = nil; v.Foreign = nil },
			"Managed:     per-guest, 5 attached\n"},
		{"refused", func(v *engine.ManagedView) { v.Refused = "pve-firewall is stopped on node pve1; no guest rule is in force until it runs" },
			"Managed:     auto, 5 attached, 2 waiting, 1 foreign card, refused: pve-firewall is stopped on node pve1; no guest rule is in force until it runs\n"},
		{"vnet changed", func(v *engine.ManagedView) { v.Vnet = engine.VnetView{Why: []string{"it has 1 subnet, and pco's leg is its only gateway"}} },
			"Managed:     auto, 5 attached, 2 waiting, 1 foreign card, vnet changed: it has 1 subnet, and pco's leg is its only gateway\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := managedState()
			tt.change(&st.Managed)
			r, _ := daemonWith(t, st)

			res := r.run("", "status")

			require.NoError(t, res.err)
			require.Contains(t, res.out, tt.want)
		})
	}

	t.Run("not enabled", func(t *testing.T) {
		r, _ := daemonWith(t, healthyState())
		res := r.run("", "status")
		require.NoError(t, res.err)
		require.NotContains(t, res.out, "Managed:")
	})
}

func TestManagedApprove(t *testing.T) {
	t.Run("a guest that waits", func(t *testing.T) {
		r, e := daemonWith(t, managedState())

		res := r.run("", "managed", "approve", "lxc/204", "--accept-firewall-impact")

		require.NoError(t, res.err)
		require.Equal(t, "lxc/204 (gui-4) waits for the managed network in identity mac:204: needs-firewall-confirmation: "+
			"turning on the firewall of lxc/204 (gui-4) also filters net0 (firewall=1); "+
			"run pco managed approve lxc/204 --accept-firewall-impact, or set managed.acceptFirewallImpact for every guest\n"+
			"Approved lxc/204 for the managed network in identity uuid:204.\n"+
			"The firewall-impact confirmation is recorded with it.\n"+
			"The card is added in the next cycle; pco managed status shows it.\n", res.out)
		require.Equal(t, []string{"approve managed lxc/204 identity=mac:204 accept-firewall-impact"}, e.called(), "the identity that was shown")
	})

	t.Run("a guest not shown", func(t *testing.T) {
		r, e := daemonWith(t, managedState())

		res := r.run("", "managed", "approve", "qemu/101")

		require.NoError(t, res.err)
		require.Equal(t, "Approved qemu/101 for the managed network in identity uuid:101.\n"+
			"The card is added in the next cycle; pco managed status shows it.\n", res.out)
		require.Equal(t, []string{"approve managed qemu/101"}, e.called(), "no identity was shown, so none is sent")
	})

	t.Run("refused", func(t *testing.T) {
		r, e := daemonWith(t, managedState())
		e.guestErr = fmt.Errorf("%w: qemu/109 is not in the last listing of Proxmox; approve what you can see", engine.ErrRefused)

		res := r.run("", "managed", "approve", "qemu/109")

		require.Error(t, res.err)
		require.Contains(t, res.err.Error(), "qemu/109 is not in the last listing of Proxmox")
	})
}

func TestManagedRevoke(t *testing.T) {
	t.Run("with --yes", func(t *testing.T) {
		r, e := daemonWith(t, managedState())

		res := r.run("", "managed", "revoke", "qemu/106", "--yes")

		require.NoError(t, res.err)
		require.Equal(t, "Revoking the consent of qemu/106: a card it has goes after the grace, unless the tag or automatic consent keeps it.\n"+
			"Revoked the consent of qemu/106.\n", res.out)
		require.Equal(t, []string{"revoke managed qemu/106"}, e.called())
	})

	t.Run("answered no", func(t *testing.T) {
		r, e := daemonWith(t, managedState())
		r.tty()

		res := r.run("n\n", "managed", "revoke", "qemu/106")

		require.ErrorIs(t, res.err, errAborted)
		require.Empty(t, e.called())
	})
}
