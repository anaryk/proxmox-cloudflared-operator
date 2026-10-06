package pki

import (
	"crypto/x509"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func hostID(node string) Identity {
	return Identity{Kind: KindHost, InstallID: testInstall, Node: node}
}

func applianceID(node string, vmid int) Identity {
	return Identity{Kind: KindAppliance, InstallID: testInstall, Node: node, VMID: vmid}
}

func withURIs(t *testing.T, uris ...string) *x509.Certificate {
	t.Helper()
	c := &x509.Certificate{}
	for _, s := range uris {
		u, err := url.Parse(s)
		require.NoError(t, err)
		c.URIs = append(c.URIs, u)
	}
	return c
}

func TestIdentityURI(t *testing.T) {
	cases := []struct {
		name string
		id   Identity
		want string
	}{
		{"host", hostID("node1"), "pco:host:0123456789ab:node1"},
		{"appliance", applianceID("node2", 9100), "pco:appliance:0123456789ab:node2:9100"},
		{"controller", Identity{Kind: KindController, InstallID: testInstall, Node: "node3"}, "pco:controller:0123456789ab:node3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, c.id.URI().String())
			require.Equal(t, c.want, c.id.String())

			got, err := IdentityOf(withURIs(t, c.want))
			require.NoError(t, err)
			require.Equal(t, c.id, got)
		})
	}
}

func TestIdentityOfRefuses(t *testing.T) {
	cases := []struct {
		name          string
		uris          []string
		wantSubstring string
	}{
		{"no URI", nil, "carries no pco identity"},
		{"two identities", []string{"pco:host:0123456789ab:node1", "pco:host:0123456789ab:node2"}, "carries 2 URIs"},
		{"an identity and another URI", []string{"pco:host:0123456789ab:node1", "https://example.org/"}, "carries 2 URIs"},
		{"another scheme", []string{"spiffe://example.org/node1"}, "not a pco identity"},
		{"an unknown kind", []string{"pco:agent:0123456789ab:node1"}, `unknown kind "agent"`},
		{"too few parts", []string{"pco:host:0123456789ab"}, "not a pco identity"},
		{"too many parts", []string{"pco:appliance:0123456789ab:node2:9100:1"}, "not a pco identity"},
		{"a bad install id", []string{"pco:host:0123456789AB:node1"}, "install id"},
		{"a short install id", []string{"pco:host:0123:node1"}, "install id"},
		{"a bad node name", []string{"pco:host:0123456789ab:-node1"}, "node name"},
		{"an empty node name", []string{"pco:host:0123456789ab:"}, "node name"},
		{"a host with a vmid", []string{"pco:host:0123456789ab:node1:9100"}, "only an appliance"},
		{"a controller with a vmid", []string{"pco:controller:0123456789ab:node1:9100"}, "only an appliance"},
		{"an appliance without a vmid", []string{"pco:appliance:0123456789ab:node2"}, "vmid"},
		{"an appliance with a low vmid", []string{"pco:appliance:0123456789ab:node2:99"}, "vmid"},
		{"a vmid that is not a number", []string{"pco:appliance:0123456789ab:node2:x"}, "vmid"},
		{"a vmid with a leading zero", []string{"pco:appliance:0123456789ab:node2:09100"}, "not in its canonical form"},
		{"a query", []string{"pco:host:0123456789ab:node1?x=1"}, "not in its canonical form"},
		{"a fragment", []string{"pco:host:0123456789ab:node1#x"}, "not in its canonical form"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := IdentityOf(withURIs(t, c.uris...))
			require.ErrorContains(t, err, c.wantSubstring)
		})
	}
}
