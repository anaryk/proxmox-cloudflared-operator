package store

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConsentsRoundTrip(t *testing.T) {
	s, p := openStore(t)
	got, err := s.Consents()
	require.NoError(t, err)
	require.Empty(t, got)

	first := Consent{Owner: "qemu/101", Identity: "id-101", At: t0, Actor: "root@pam", AcceptFirewallImpact: true}
	second := Consent{Owner: "lxc/105", Identity: "id-105", At: t0}
	require.NoError(t, s.SaveConsent(first))
	require.NoError(t, s.SaveConsent(second))

	got, err = s.Consents()
	require.NoError(t, err)
	require.Equal(t, map[string]Consent{"qemu/101": first, "lxc/105": second}, got)
	require.Equal(t, []string{"consents/lxc_105.json", "consents/qemu_101.json"}, stored(t, p.Cluster))

	require.NoError(t, s.DeleteConsent("qemu/101"))
	require.NoError(t, s.DeleteConsent("qemu/101"), "a consent that is gone is no error")
	require.Equal(t, []string{"consents/lxc_105.json"}, stored(t, p.Cluster))
}

func TestSaveConsentRefusesAnIncompleteOne(t *testing.T) {
	for name, c := range map[string]Consent{
		"no owner":                  {Identity: "id", At: t0},
		"an owner that is no guest": {Owner: "manual/x", Identity: "id", At: t0},
		"no identity":               {Owner: "qemu/101", At: t0},
		"no time":                   {Owner: "qemu/101", Identity: "id"},
	} {
		t.Run(name, func(t *testing.T) {
			s, p := openStore(t)
			require.Error(t, s.SaveConsent(c))
			require.Empty(t, stored(t, p.Cluster))
		})
	}
}

func attachmentOf(owner, a string) Attachment {
	addr := netip.MustParseAddr(a)
	return Attachment{Owner: owner, Identity: "id-" + owner, NIC: 2, MAC: ManagedMAC(addr), Addr: addr, Consent: "tag", Since: t0}
}

func TestAttachmentsRoundTrip(t *testing.T) {
	s, p := openStore(t)
	got, err := s.Attachments()
	require.NoError(t, err)
	require.Empty(t, got)

	detach, removed := t0.Add(time.Minute), t0.Add(11*time.Minute)
	first := attachmentOf("qemu/101", "10.203.118.5")
	first.MAC = "06:50:43:4F:76:05"
	first.Consent = "auto"
	first.Firewall = &FirewallBefore{Enabled: false, Rules: 3}
	first.DetachAt, first.DetachWhy, first.RemovedAt = &detach, "cf-tunnel-managed=off", &removed
	second := attachmentOf("lxc/105", "10.203.118.6")
	second.Consent = "approval"
	require.NoError(t, s.SaveAttachment(first))
	require.NoError(t, s.SaveAttachment(second))

	got, err = s.Attachments()
	require.NoError(t, err)
	first.MAC = "06:50:43:4f:76:05"
	require.Equal(t, map[string]Attachment{"qemu/101": first, "lxc/105": second}, got)
	require.Equal(t, []string{"attachments/lxc_105.json", "attachments/qemu_101.json"}, stored(t, p.Cluster))

	require.NoError(t, s.DeleteAttachment("lxc/105"))
	require.NoError(t, s.DeleteAttachment("lxc/105"))
	require.Equal(t, []string{"attachments/qemu_101.json"}, stored(t, p.Cluster))
}

func TestSaveAttachmentRefusesWhatDoesNotFit(t *testing.T) {
	edit := func(f func(*Attachment)) Attachment {
		a := attachmentOf("qemu/101", "10.203.118.5")
		f(&a)
		return a
	}
	for name, a := range map[string]Attachment{
		"an owner that is no guest":       edit(func(a *Attachment) { a.Owner = "qemu/0" }),
		"no identity":                     edit(func(a *Attachment) { a.Identity = "" }),
		"a card above net31":              edit(func(a *Attachment) { a.NIC = 32 }),
		"no MAC":                          edit(func(a *Attachment) { a.MAC = "" }),
		"the MAC of another address":      edit(func(a *Attachment) { a.MAC = "06:50:43:4f:76:06" }),
		"no address":                      edit(func(a *Attachment) { a.Addr = netip.Addr{} }),
		"a consent that is no way of one": edit(func(a *Attachment) { a.Consent = "manual" }),
		"no consent":                      edit(func(a *Attachment) { a.Consent = "" }),
	} {
		t.Run(name, func(t *testing.T) {
			s, p := openStore(t)
			require.Error(t, s.SaveAttachment(a))
			require.Empty(t, stored(t, p.Cluster))
		})
	}
}
