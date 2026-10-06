package applianceinstall

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	apiPort      = "8006"
	probeTimeout = 10 * time.Second
)

// apiEndpoint is how the appliance reaches the Proxmox API: the endpoint, and
// the file pushed as its CA, which with the system roots verifies the
// certificate under the server name.
type apiEndpoint struct {
	store.Endpoint
	CA  string `json:"ca"`
	Via string `json:"via"` // what verified it, for the summary
}

// probeCert reads the certificate chain the API presents at address. Nothing
// is verified here: choosing the name to verify it under is the point.
func probeCert(ctx context.Context, address string) ([]*x509.Certificate, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	d := tls.Dialer{Config: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}}
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return nil, errors.New("not a TLS connection")
	}
	return tc.ConnectionState().PeerCertificates, nil
}

// certCheck is what the server name is chosen with.
type certCheck struct {
	chain     []*x509.Certificate
	node      string
	clusterCA []byte         // /etc/pve/pve-root-ca.pem
	apiCA     []byte         // --api-ca, when given
	roots     *x509.CertPool // the system roots; nil when there are none
	now       time.Time
}

// serverName chooses the name the appliance verifies the certificate under:
// the node name when the certificate chains to the cluster CA, as pveproxy's
// own does; otherwise a DNS name of the certificate that verifies against the
// system roots and --api-ca, as an ACME or a custom certificate does. It
// returns whether the cluster CA verified, and refuses a certificate that
// verifies neither way.
func (c certCheck) serverName() (name string, viaCluster bool, err error) {
	if len(c.chain) == 0 {
		return "", false, errors.New("the API presented no certificate")
	}
	leaf, rest := c.chain[0], c.chain[1:]
	inter := x509.NewCertPool()
	for _, cert := range rest {
		inter.AddCert(cert)
	}
	verifies := func(roots *x509.CertPool, name string) bool {
		_, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: roots, Intermediates: inter, CurrentTime: c.now})
		return err == nil
	}
	cluster := x509.NewCertPool()
	if cluster.AppendCertsFromPEM(c.clusterCA) && verifies(cluster, c.node) {
		return c.node, true, nil
	}
	roots := x509.NewCertPool()
	if c.roots != nil {
		roots = c.roots.Clone()
	}
	if len(c.apiCA) > 0 && !roots.AppendCertsFromPEM(c.apiCA) {
		return "", false, errors.New("--api-ca holds no certificate")
	}
	var names []string
	for _, name := range leaf.DNSNames {
		if strings.Contains(name, "*") || strings.EqualFold(name, "localhost") || !validHostname(name) {
			continue
		}
		names = append(names, name)
		if verifies(roots, name) {
			return name, false, nil
		}
	}
	against := "the system roots"
	if len(c.apiCA) > 0 {
		against += " and --api-ca"
	}
	return "", false, fmt.Errorf("the certificate verifies neither under the node name %s against the cluster CA nor under "+
		"a DNS name it carries (%s) against %s: pass --api-ca with the CA that signed it, or --api-host with an address "+
		"whose certificate verifies", c.node, orNone(names), against)
}

func validHostname(name string) bool {
	if len(name) > 253 || strings.HasSuffix(name, ".") || strings.Contains(name, "..") {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if r != '-' && (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
				return false
			}
		}
	}
	return true
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// apiAddress is host:8006 for an address or name given as --api-host.
func apiAddress(host string) (string, error) {
	host = strings.TrimSpace(host)
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Unmap().IsLoopback() || a.IsUnspecified() {
			return "", fmt.Errorf("--api-host %s: the appliance reaches the node through its network, never a loopback address", host)
		}
		return net.JoinHostPort(a.String(), apiPort), nil
	}
	if !validHostname(host) || strings.EqualFold(host, "localhost") {
		return "", fmt.Errorf("--api-host %q: want an address or a name of the node", host)
	}
	return net.JoinHostPort(host, apiPort), nil
}

// chooseEndpoint probes the certificate at the API host and chooses the
// server name and the CA to push.
func (r *run) chooseEndpoint(ctx context.Context, host string) (apiEndpoint, error) {
	address, err := apiAddress(host)
	if err != nil {
		return apiEndpoint{}, err
	}
	chain, err := r.h.probeCert(ctx, address)
	if err != nil {
		return apiEndpoint{}, fmt.Errorf("reading the certificate of the API at %s: %w", address, err)
	}
	clusterCA, err := r.h.readFile(r.h.clusterCA)
	if err != nil {
		return apiEndpoint{}, fmt.Errorf("reading the cluster CA: %w", err)
	}
	c := certCheck{chain: chain, node: r.node, clusterCA: clusterCA, now: r.now()}
	if r.o.APICA != "" {
		if c.apiCA, err = r.h.readFile(r.o.APICA); err != nil {
			return apiEndpoint{}, fmt.Errorf("reading --api-ca: %w", err)
		}
	}
	if c.roots, err = r.h.systemRoots(); err != nil {
		r.ask.Warn("the system roots cannot be read (%v): only the cluster CA and --api-ca verify the API", err)
		c.roots = nil
	}
	name, viaCluster, err := c.serverName()
	if err != nil {
		return apiEndpoint{}, fmt.Errorf("the API at %s: %w", address, err)
	}
	ep := apiEndpoint{Endpoint: store.Endpoint{Address: address, ServerName: name}, CA: r.h.clusterCA, Via: "the cluster CA"}
	switch {
	case viaCluster:
	case r.o.APICA != "":
		ep.CA, ep.Via = r.o.APICA, "the CA of --api-ca"
	default:
		ep.Via = "the system roots"
	}
	r.ask.Info("preflight: the API at %s verifies as %s against %s", address, name, ep.Via)
	return ep, nil
}
