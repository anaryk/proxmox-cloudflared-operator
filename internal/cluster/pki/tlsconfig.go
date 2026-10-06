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

// ServerTLS is the configuration of the controller's listener: TLS 1.3 only,
// and a client certificate for client authentication under one of pool's
// CAs, two of them during a CA rotation. Whether the registry knows the
// certificate is the caller's check, per request. cert is the server's
// certificate followed by its CA, which a client needs to verify it by the
// CA's fingerprint (TLSCertificate makes one).
func ServerTLS(cert tls.Certificate, pool *x509.CertPool) *tls.Config {
	if pool == nil {
		// A nil pool would be the system's roots.
		pool = x509.NewCertPool()
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		// A resumed session is not checked against the pool again, which
		// would let a certificate of the previous CA outlive a rotation.
		SessionTicketsDisabled: true,
	}
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
