// Package pki is the install's own certificate authority: the CA, the
// certificates it issues to the agents and the controller, the join tokens
// of appliance agents and the TLS configurations of the channel between them.
//
// The CA is pco's alone, never the cluster CA of Proxmox VE: a certificate
// that one signs is taken wherever the cluster CA is trusted.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"
)

// Lifetimes, renewal and join tokens.
const (
	CAValidity      = 10 * 365 * 24 * time.Hour
	LeafValidity    = 90 * 24 * time.Hour
	RenewAfter      = 60 * 24 * time.Hour
	SelfRenewBefore = 30 * 24 * time.Hour // host nodes, at daemon start
	Backdate        = time.Hour           // for a clock a little behind the issuer's
	JoinTokenTTL    = 15 * time.Minute
	JoinPrefix      = "pco1."
)

// CA is the install's certificate authority.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
}

// NewCA makes the CA of an install: an ECDSA P-256 key and a self-signed
// certificate valid for CAValidity, which signs leaves and nothing that
// signs in turn.
func NewCA(installID string, now time.Time, rand io.Reader) (CA, error) {
	if err := checkInstallID(installID); err != nil {
		return CA{}, err
	}
	key, err := NewKey(rand)
	if err != nil {
		return CA{}, err
	}
	serial, err := randomSerial(rand)
	if err != nil {
		return CA{}, err
	}
	// The serial in the subject keeps the name of each CA its own, so that a
	// CA rotation, which has two CAs of the install, has two issuers.
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "pco CA " + installID, SerialNumber: serial.Text(16)},
		NotBefore:             now.Add(-Backdate),
		NotAfter:              now.Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return CA{}, fmt.Errorf("signing the CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return CA{}, fmt.Errorf("reading the CA certificate: %w", err)
	}
	return CA{Cert: cert, Key: key}, nil
}

// PEM encodes the certificate and the key, the key in PKCS #8.
func (ca CA) PEM() (certPEM, keyPEM []byte, err error) {
	if ca.Cert == nil || ca.Key == nil {
		return nil, nil, errors.New("no CA")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(ca.Key)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding the CA key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// ParseCA reads a CA that PEM wrote: one certificate of a CA, and its P-256
// key in PKCS #8 or SEC 1.
func ParseCA(certPEM, keyPEM []byte) (CA, error) {
	certDER, err := onePEM(certPEM, "certificate", "CERTIFICATE")
	if err != nil {
		return CA{}, err
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return CA{}, fmt.Errorf("reading the CA certificate: %w", err)
	}
	key, err := parseKey(keyPEM)
	if err != nil {
		return CA{}, err
	}
	pub, err := p256(cert.PublicKey)
	if err != nil {
		return CA{}, fmt.Errorf("the CA certificate: %w", err)
	}
	switch {
	case !cert.BasicConstraintsValid || !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0:
		return CA{}, errors.New("the certificate is not a CA's")
	case !pub.Equal(&key.PublicKey):
		return CA{}, errors.New("the key is not the key of the CA certificate")
	}
	return CA{Cert: cert, Key: key}, nil
}

// SHA256 is the fingerprint of the CA certificate, by which a join token and
// an agent pin it.
func (ca CA) SHA256() string { return Fingerprint(ca.Cert) }

// NewKey makes an ECDSA P-256 key, the only kind pco issues for.
func NewKey(rand io.Reader) (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand)
	if err != nil {
		return nil, fmt.Errorf("making a key: %w", err)
	}
	return key, nil
}

// Fingerprint is the SHA-256 of a certificate's DER, in lower-case hex.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// p256 returns pub as an ECDSA P-256 key, or why it is not one.
func p256(pub any) (*ecdsa.PublicKey, error) {
	k, ok := pub.(*ecdsa.PublicKey)
	switch {
	case !ok || k == nil:
		return nil, fmt.Errorf("the key is a %T, not an ECDSA P-256 key", pub)
	case k.Curve != elliptic.P256():
		return nil, fmt.Errorf("the key is on %s, not P-256", k.Curve.Params().Name)
	}
	return k, nil
}

// onePEM returns the bytes of the one block of type typ in b.
func onePEM(b []byte, what, typ string) ([]byte, error) {
	var found []byte
	for rest := b; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != typ {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("more than one %s in the PEM data", what)
		}
		found = block.Bytes
	}
	if found == nil {
		return nil, fmt.Errorf("no %s in the PEM data", what)
	}
	return found, nil
}

// parseKey reads the P-256 key of PEM data, in PKCS #8 or SEC 1.
func parseKey(b []byte) (*ecdsa.PrivateKey, error) {
	for rest := b; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("no private key in the PEM data")
		}
		var key any
		var err error
		switch block.Type {
		case "PRIVATE KEY":
			key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		case "EC PRIVATE KEY":
			key, err = x509.ParseECPrivateKey(block.Bytes)
		case "ENCRYPTED PRIVATE KEY":
			return nil, errors.New("the private key is encrypted")
		default:
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading the private key: %w", err)
		}
		ec, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("the private key is a %T, not an ECDSA P-256 key", key)
		}
		if _, err := p256(&ec.PublicKey); err != nil {
			return nil, err
		}
		return ec, nil
	}
}

// randomSerial draws a serial of 128 random bits, never zero.
func randomSerial(rand io.Reader) (*big.Int, error) {
	b := make([]byte, 16)
	for {
		if _, err := io.ReadFull(rand, b); err != nil {
			return nil, fmt.Errorf("reading random bytes: %w", err)
		}
		if s := new(big.Int).SetBytes(b); s.Sign() > 0 {
			return s, nil
		}
	}
}
