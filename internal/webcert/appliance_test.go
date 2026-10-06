package webcert

import (
	"crypto/rand"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCheckListen(t *testing.T) {
	net0 := netip.MustParseAddr("10.92.0.150")
	require.NoError(t, CheckListen("10.92.0.150:8643", net0))
	require.NoError(t, CheckListen("10.92.0.150:9443", net0), "any port")
	for _, listen := range []string{"0.0.0.0:8643", "[::]:8643", "10.92.1.1:8643", "127.0.0.1:8643", "pco:8643", ""} {
		require.ErrorIs(t, CheckListen(listen, net0), ErrNotNet0, listen)
	}
	require.EqualError(t, CheckListen("0.0.0.0:8643", net0), "pco-web must listen on net0's address only (10.92.0.150), not 0.0.0.0:8643")
}

func TestReadNet0(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "net0")
	require.NoError(t, os.WriteFile(file, []byte(" 10.92.0.150\n"), 0o644))
	a, err := ReadNet0(file)
	require.NoError(t, err)
	require.Equal(t, netip.MustParseAddr("10.92.0.150"), a)

	for _, bad := range []string{"", "0.0.0.0", "10.92.0.150/24", "pco"} {
		require.NoError(t, os.WriteFile(file, []byte(bad), 0o644))
		_, err := ReadNet0(file)
		require.Error(t, err, bad)
	}
	_, err = ReadNet0(filepath.Join(dir, "missing"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestTheListenAddressOfTheAppliance(t *testing.T) {
	dir := t.TempDir()
	net0 := filepath.Join(dir, "net0")
	require.Empty(t, ApplianceListen(Env{}, net0), "nothing known")
	require.NoError(t, os.WriteFile(net0, []byte("10.92.0.150\n"), 0o644))
	require.Equal(t, "10.92.0.150:8643", ApplianceListen(Env{}, net0))
	require.Equal(t, "10.92.0.151:9443", ApplianceListen(Env{Listen: "10.92.0.151:9443"}, net0), "PCO_WEB_LISTEN as it stands")
}

func TestTheAPIOfTheNode(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, APIName)
	ca, _, err := SelfSigned(Names{DNS: []string{"pve1"}}, t0, time.Hour, rand.Reader)
	require.NoError(t, err)
	want := API{URL: "https://192.0.2.10:8006/api2/json", ServerName: "pve1", CA: string(ca), Node: "pve1"}
	require.NoError(t, os.WriteFile(file, want.Encode(), 0o644))
	got, err := ReadAPI(file)
	require.NoError(t, err)
	require.Equal(t, want, got)

	for _, bad := range []API{
		{URL: "http://192.0.2.10:8006/api2/json", ServerName: "pve1", CA: string(ca)},
		{URL: "https://192.0.2.10:8006/api2/json", CA: string(ca)},
		{URL: "https://192.0.2.10:8006/api2/json", ServerName: "pve1"},
	} {
		require.NoError(t, os.WriteFile(file, bad.Encode(), 0o644))
		_, err := ReadAPI(file)
		require.Error(t, err)
	}
}

func TestTheModeOfTheAppliancesCertificate(t *testing.T) {
	dir := t.TempDir()
	mode, err := ReadMode(dir)
	require.NoError(t, err)
	require.Equal(t, ModeSelfSigned, mode, "what the daemon made")
	require.NoError(t, WriteMode(dir, ModeOwn))
	mode, err = ReadMode(dir)
	require.NoError(t, err)
	require.Equal(t, ModeOwn, mode)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ModeName), []byte("ca\n"), 0o644))
	_, err = ReadMode(dir)
	require.ErrorContains(t, err, `says "ca"`)
}

func TestTheAppliancesOwnCertificateAndWhenItIsDue(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "web")
	require.Equal(t, NoCertificate, SelfSignedDue(dir, t0))

	names, err := ApplianceNames("pco.example.org", "", "10.92.0.150:8643", []string{"pco.lab.example.org"})
	require.NoError(t, err)
	leaf, err := MakeSelfSigned(dir, names, t0, rand.Reader)
	require.NoError(t, err)
	require.Equal(t, []string{"pco", "pco.lab.example.org"}, leaf.DNSNames)
	require.Equal(t, t0.Add(SelfSignedLifetime), leaf.NotAfter)
	require.Empty(t, SelfSignedDue(dir, t0))
	require.Empty(t, SelfSignedDue(dir, leaf.NotAfter.Add(-RenewBefore)))
	require.Contains(t, SelfSignedDue(dir, leaf.NotAfter.Add(-RenewBefore+time.Second)), "in less than 30 days")

	_, otherKey, err := SelfSigned(names, t0, time.Hour, rand.Reader)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, KeyName), otherKey, 0o600))
	require.Equal(t, "tls.key is not its key", SelfSignedDue(dir, t0))
}

func TestImportingTheAdminsCertificateIntoTheAppliance(t *testing.T) {
	dir := t.TempDir()
	names, err := ApplianceNames("pco", "", "10.92.0.150:8643", nil)
	require.NoError(t, err)
	other, otherKey, err := SelfSigned(Names{DNS: []string{"elsewhere.example.org"}}, t0, 90*24*time.Hour, rand.Reader)
	require.NoError(t, err)

	_, err = ImportOwn(dir, other, otherKey, names, t0)
	require.ErrorContains(t, err, "names none of")
	mode, err := ReadMode(dir)
	require.NoError(t, err)
	require.Equal(t, ModeSelfSigned, mode, "nothing changed")

	own, ownKey, err := SelfSigned(Names{Addrs: []netip.Addr{netip.MustParseAddr("10.92.0.150")}}, t0, 90*24*time.Hour, rand.Reader)
	require.NoError(t, err)
	_, err = ImportOwn(dir, own, ownKey, names, t0)
	require.NoError(t, err)
	mode, err = ReadMode(dir)
	require.NoError(t, err)
	require.Equal(t, ModeOwn, mode)
	have, _, err := ReadPair(dir)
	require.NoError(t, err)
	require.Equal(t, own, have)
}
