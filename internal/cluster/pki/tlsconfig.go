package pki

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrNoClientCert is PeerIdentity's refusal of a connection that brought no
// client certificate.
var ErrNoClientCert = errors.New("no client certificate verified under the install's CA")

// ServerTLS is the configuration of the controller's listener: TLS 1.3 only.
// A client certificate is optional at the TLS level, because an appliance
// agent comes to the join without one; one that is sent must verify for
// client authentication under one of pool's CAs, two of them during a CA
// rotation, or the handshake fails. So every path but the join must refuse
// through PeerIdentity, which also gives the identity for the registry
// check, and the join ignores a certificate that is there. cert is the
// server's certificate followed by its CA, which a client needs to verify it
// by the CA's fingerprint (TLSCertificate makes one).
func ServerTLS(cert tls.Certificate, pool *x509.CertPool) *tls.Config {
	if pool == nil {
		// A nil pool would be the system's roots.
		pool = x509.NewCertPool()
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
		// The agents hold long-lived HTTP/2 connections, so resumption
		// saves nothing, and older toolchains resumed a session without
		// checking its client certificate against the pool of the day,
		// which would let a certificate of the previous CA outlive a
		// rotation.
		SessionTicketsDisabled: true,
	}
}

// PeerIdentity is the identity of the client of a connection that
// ServerTLS accepted, or ErrNoClientCert when it brought no certificate.
// cs is nil for a request that did not come over TLS.
func PeerIdentity(cs *tls.ConnectionState) (Identity, error) {
	if cs == nil || len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
		return Identity{}, ErrNoClientCert
	}
	return IdentityOf(cs.VerifiedChains[0][0])
}

// ClientTLS is the configuration of an agent dialling the controller: TLS
// 1.3 only, cert as the client certificate, and the server's certificate
// verified for serverName under the CA whose SHA-256 is caSHA256, which the
// server sends after its own certificate. No other root is trusted, the
// system's least of all.
func ClientTLS(cert tls.Certificate, caSHA256, serverName string) *tls.Config {
	pin := strings.ToLower(caSHA256)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: serverName,
		// cert goes whichever CAs the server asks for, so that a certificate
		// of a CA it no longer takes is refused as that, not as missing.
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &cert, nil
		},
		// The verification is VerifyConnection's: the CA is known by its
		// fingerprint only, so there is no pool to give crypto/tls.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			return verifyPinned(cs.PeerCertificates, pin, serverName)
		},
	}
}

// verifyPinned verifies the leaf of chain for serverName under the
// certificate of chain whose fingerprint is pin.
func verifyPinned(chain []*x509.Certificate, pin, serverName string) error {
	if len(chain) == 0 {
		return errors.New("the server sent no certificate")
	}
	i := slices.IndexFunc(chain[1:], func(c *x509.Certificate) bool { return Fingerprint(c) == pin })
	if i < 0 {
		return fmt.Errorf("the server's certificate is not under the CA %s", pin)
	}
	roots := x509.NewCertPool()
	roots.AddCert(chain[1+i])
	_, err := chain[0].Verify(x509.VerifyOptions{
		Roots:     roots,
		DNSName:   serverName,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return fmt.Errorf("verifying the server's certificate: %w", err)
	}
	return nil
}

// TLSCertificate is key with its certificate chain, leaf first.
func TLSCertificate(key *ecdsa.PrivateKey, chain ...*x509.Certificate) tls.Certificate {
	c := tls.Certificate{PrivateKey: key}
	for _, cert := range chain {
		c.Certificate = append(c.Certificate, cert.Raw)
	}
	if len(chain) > 0 {
		c.Leaf = chain[0]
	}
	return c
}
