package applianceinstall

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

func TestTheWebInterfaceListensOnTheAddressOfIP(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.IP = "192.0.2.20/24,gw=192.0.2.1"

	e.install(o)

	ct := e.node.cts[100]
	require.Equal(t, "192.0.2.20\n", string(ct.files["/etc/pco/net0"].data))
	env := string(ct.files["/etc/default/pco-web"].data)
	require.NotContains(t, env, "\nPCO_WEB_LISTEN=", "pco-web follows net0's address as the daemon keeps it")
	require.Equal(t, map[string]string{}, webcert.ParseEnv([]byte(env)))
	require.Contains(t, e.ask.text(), "web interface: https://192.0.2.20:8643/")
}

func TestTheInstallerWaitsForTheLeaseAndTheCertificate(t *testing.T) {
	e := newEnv(t)
	noAddress := func(context.Context, []string) (string, error) { return `[{"ifname":"eth0","addr_info":[]}]`, nil }
	notYet := func(context.Context, []string) (string, error) {
		return "", exitError{1, "the web interface has no certificate yet"}
	}
	for range 3 {
		e.node.on("pct exec 100 --keep-env 0 -- ip -j addr show dev eth0", noAddress)
		e.node.on("pct exec 100 --keep-env 0 -- pco web cert", notYet)
	}

	e.install(e.options())

	require.Equal(t, 4, e.node.count("pct exec 100 --keep-env 0 -- ip -j addr show dev eth0"))
	require.Equal(t, 4, e.node.count("pct exec 100 --keep-env 0 -- pco web cert"))
	require.True(t, e.node.cts[100].web)
	require.Contains(t, e.ask.text(), fakeFingerprint)
}

func TestWithoutAnAddressOfNet0TheInstallIsTakenBack(t *testing.T) {
	for _, tt := range []struct {
		name, out, want string
	}{
		{"none", `[{"ifname":"eth0","addr_info":[{"family":"inet","local":"169.254.3.4","prefixlen":16,"scope":"link"}]}]`,
			"eth0 of lxc/100 has no IPv4 address after 30 s: give it one with --ip, or check the DHCP server on bridge vmbr0"},
		{"IPv6 only", `[{"ifname":"eth0","addr_info":[{"family":"inet6","local":"2001:db8::150","prefixlen":64,"scope":"global"}]}]`,
			"eth0 of lxc/100 has an IPv6 address only, and the web interface of the appliance serves IPv4: give net0 an IPv4 address with --ip"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			for range webTries {
				e.node.on("pct exec 100 --keep-env 0 -- ip -j addr show dev eth0", func(context.Context, []string) (string, error) { return tt.out, nil })
			}

			err := e.in.Install(e.t.Context(), e.options())

			require.ErrorContains(t, err, tt.want)
			require.Empty(t, e.node.cts, "the container is taken back")
		})
	}
}

// A run resumed after the address was written still says where the web
// interface is.
func TestTheSummaryOfAResumedRunHasTheURL(t *testing.T) {
	e := newEnv(t)
	e.node.killAt = "pct exec 100 --keep-env 0 -- pco web cert"
	require.PanicsWithValue(t, errKilled, func() { _ = e.in.Install(t.Context(), e.options()) })

	require.NoError(t, e.in.Install(t.Context(), Options{Resume: e.journal(), Yes: true}), e.ask.text())

	require.Equal(t, 1, e.node.count("pct exec 100 --keep-env 0 -- cat /etc/pco/net0"))
	require.Contains(t, e.ask.text(), "web interface: https://192.0.2.150:8643/, its certificate's SHA-256 fingerprint")
}

func TestRepairGivesAnOlderApplianceItsWebInterface(t *testing.T) {
	e := installed(t)
	ct := e.node.cts[100]
	delete(ct.files, webcert.Net0File)
	delete(ct.files, webcert.EnvFile)
	ct.web = false

	require.NoError(t, e.in.Repair(t.Context(), 100, Options{Yes: true}), e.ask.text())

	require.Equal(t, "192.0.2.150\n", string(ct.files[webcert.Net0File].data))
	require.Contains(t, string(ct.files[webcert.EnvFile].data), "pco-web listens on net0's address")
	require.True(t, ct.web)
	require.Less(t, index(e.node.ran, "pct push 100 <run>/net0 /etc/pco/net0"), index(e.node.ran, "pct exec 100 --keep-env 0 -- pco appliance init"),
		"net0's address before the daemon starts again, which makes the certificate for it")
	require.Contains(t, e.ask.text(), fakeFingerprint)
}

func TestRepairLeavesTheWebInterfaceThatIsThere(t *testing.T) {
	e := installed(t)
	ct := e.node.cts[100]
	ct.files[webcert.EnvFile] = fakeFile{data: []byte("PCO_WEB_HOSTS=pco.example.org\n"), perms: "0644"}
	delete(ct.files, webcert.Net0File)

	require.NoError(t, e.in.Repair(t.Context(), 100, Options{Yes: true}), e.ask.text())

	require.Equal(t, "PCO_WEB_HOSTS=pco.example.org\n", string(ct.files[webcert.EnvFile].data), "the admin's environment stays")
	require.Equal(t, "192.0.2.150\n", string(ct.files[webcert.Net0File].data))
	require.Zero(t, e.node.count("pct exec 100 --keep-env 0 -- systemctl enable"), "pco-web was enabled")

	e.node.ran = nil
	require.NoError(t, e.in.Repair(t.Context(), 100, Options{Yes: true}), e.ask.text())
	require.Zero(t, e.node.count("pct push 100 <run>/net0"), "nothing to write")
}

func index(lines []string, prefix string) int {
	for i, l := range lines {
		if len(l) >= len(prefix) && l[:len(prefix)] == prefix {
			return i
		}
	}
	return -1
}

func TestGlobalAddrs(t *testing.T) {
	for _, tt := range []struct {
		out, want string
		v6        bool
	}{
		{`[{"addr_info":[{"family":"inet","local":"10.92.0.150","prefixlen":24,"scope":"global"}]}]`, "10.92.0.150", false},
		{`[{"addr_info":[{"family":"inet","local":"169.254.1.1","scope":"link"},{"family":"inet","local":"10.92.0.151","scope":"global"}]}]`, "10.92.0.151", false},
		{`[{"addr_info":[{"family":"inet6","local":"fe80::1","scope":"link"},{"family":"inet6","local":"2001:db8::1","scope":"global"}]}]`, "", true},
		{`[{"addr_info":[]}]`, "", false},
		{`[]`, "", false},
		{`Device "eth0" does not exist.`, "", false},
	} {
		v4, v6 := globalAddrs(tt.out)
		require.Equal(t, tt.want != "", v4.IsValid(), tt.out)
		require.Equal(t, tt.v6, v6, tt.out)
		if v4.IsValid() {
			require.Equal(t, tt.want, v4.String())
		}
	}
	require.Equal(t, fakeFingerprint, fingerprintOf("mode         self-signed: a key\nSHA-256      "+fakeFingerprint+"\n"))
	require.Empty(t, fingerprintOf("no certificate\n"))
}
