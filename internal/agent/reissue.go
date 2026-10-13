package agent

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// ErrNoCredentials is TLSFiles' answer when the node has no certificate yet.
var ErrNoCredentials = errors.New("this node has no certificate of the install yet")

const (
	clientFile = "client.pem"
	serverFile = "server.pem"
)

// Creds are a key, its certificate and the CA the certificate is under.
type Creds struct {
	Key  *ecdsa.PrivateKey
	Cert *x509.Certificate
	CA   *x509.Certificate
}

// TLS is the certificate as crypto/tls takes it.
func (c Creds) TLS() tls.Certificate { return pki.TLSCertificate(c.Key, c.Cert) }

// ServerTLS is the certificate followed by its CA, as a server must send it.
func (c Creds) ServerTLS() tls.Certificate { return pki.TLSCertificate(c.Key, c.Cert, c.CA) }

// CASHA256 is the fingerprint of the CA.
func (c Creds) CASHA256() string { return pki.Fingerprint(c.CA) }

// TLSFiles are the agent's key and certificates on its node, each in one file
// written whole (mode 0600), so that a key never stands beside a certificate
// of another: the client certificate the agent dials the controller with,
// and on a host the server certificate this node serves the agents with
// while it holds the lease.
type TLSFiles struct{ Dir string }

// TLSDir is where the agent keeps them, in the node's local root.
func TLSDir(p store.Paths) string { return filepath.Join(p.Local, "agent", "tls") }

// Client reads the client certificate, or ErrNoCredentials.
func (f TLSFiles) Client() (Creds, error) { return f.read(clientFile) }

// SaveClient writes the client certificate.
func (f TLSFiles) SaveClient(c Creds) error { return f.write(clientFile, c) }

// Server reads the server certificate, or ErrNoCredentials.
func (f TLSFiles) Server() (Creds, error) { return f.read(serverFile) }

// SaveServer writes the server certificate.
func (f TLSFiles) SaveServer(c Creds) error { return f.write(serverFile, c) }

func (f TLSFiles) read(name string) (Creds, error) {
	path := filepath.Join(f.Dir, name)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Creds{}, ErrNoCredentials
	}
	if err != nil {
		return Creds{}, fmt.Errorf("reading %s: %w", path, err)
	}
	c, err := parseCreds(data)
	if err != nil {
		return Creds{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// parseCreds reads a key, the certificate and its CA, in that order.
func parseCreds(data []byte) (Creds, error) {
	var c Creds
	var certs []*x509.Certificate
	for {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			break
		}
		switch b.Type {
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
			if err != nil {
				return Creds{}, fmt.Errorf("reading the key: %w", err)
			}
			ec, ok := k.(*ecdsa.PrivateKey)
			if !ok {
				return Creds{}, errors.New("the key is no ECDSA key")
			}
			c.Key = ec
		case "CERTIFICATE":
			cert, err := x509.ParseCertificate(b.Bytes)
			if err != nil {
				return Creds{}, fmt.Errorf("reading a certificate: %w", err)
			}
			certs = append(certs, cert)
		}
	}
	if c.Key == nil || len(certs) != 2 {
		return Creds{}, errors.New("want a key, its certificate and the CA")
	}
	c.Cert, c.CA = certs[0], certs[1]
	if pub, ok := c.Cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&c.Key.PublicKey) {
		return Creds{}, errors.New("the certificate is not of the key")
	}
	if err := c.Cert.CheckSignatureFrom(c.CA); err != nil {
		return Creds{}, fmt.Errorf("the certificate is not under the CA: %w", err)
	}
	return c, nil
}

func (f TLSFiles) write(name string, c Creds) error {
	key, err := x509.MarshalPKCS8PrivateKey(c.Key)
	if err != nil {
		return fmt.Errorf("encoding the key: %w", err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
	data = append(data, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Cert.Raw})...)
	data = append(data, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.CA.Raw})...)
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", f.Dir, err)
	}
	// A directory made before, or by hand, is brought to the mode as well.
	if err := os.Chmod(f.Dir, 0o700); err != nil {
		return fmt.Errorf("restricting %s: %w", f.Dir, err)
	}
	path := filepath.Join(f.Dir, name)
	if err := atomicfile.Write(path, data, atomicfile.Options{Mode: 0o600}); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// CASource is what a host node reads and writes to issue its own
// certificates: the install's CA in the cluster filesystem, and the node's
// enrolment, which the controller matches every request with. *store.Store
// is one.
type CASource interface {
	CA() (current store.CAFiles, previous *store.CAFiles, rotatedAt time.Time, err error)
	Enrolment(node string) (store.Enrolment, bool, error)
	SaveEnrolment(node string, e store.Enrolment) error
}

// Reissuer issues a host node's certificates itself, from the CA in the
// cluster filesystem, without a controller: when it has none, when one is
// due for renewal (60 days after it was issued, and always within 30 days of
// its end), and when the install's CA changed. The client certificate goes
// into the node's enrolment, which only this node writes, so the controller
// takes it.
type Reissuer struct {
	Node      string
	InstallID string
	Store     CASource
	Files     TLSFiles
	Now       func() time.Time
	Rand      io.Reader
	Log       zerolog.Logger
}

// Ensure returns the client certificate to dial with, issued again first
// where it must be, and reports whether it was. wantCA is the fingerprint of
// the install's current CA as leader.json names it; empty takes the store's
// current one. The enrolment is brought to the certificate on every call,
// so that one whose write failed is written the next time.
func (r Reissuer) Ensure(wantCA string) (Creds, bool, error) {
	now := r.now()
	id := pki.Identity{Kind: pki.KindHost, InstallID: r.InstallID, Node: r.Node}
	client, err := r.Files.Client()
	if err != nil && !errors.Is(err, ErrNoCredentials) {
		r.Log.Warn().Err(err).Msg("the node's client certificate cannot be read; issuing another")
	}
	issued := false
	if err != nil || r.due(client, id, wantCA, now) {
		ca, err := r.ca(wantCA)
		if err != nil {
			return Creds{}, false, err
		}
		if client, err = r.issueClient(ca, id, now); err != nil {
			return Creds{}, false, err
		}
		issued = true
	}
	if err := r.enrol(client); err != nil {
		return Creds{}, issued, err
	}
	if err := r.ensureServer(wantCA, now); err != nil {
		return Creds{}, issued, err
	}
	return client, issued, nil
}

// due reports whether creds must be issued again.
func (r Reissuer) due(c Creds, id pki.Identity, wantCA string, now time.Time) bool {
	got, err := pki.IdentityOf(c.Cert)
	return err != nil || got != id ||
		pki.DueForRenewal(c.Cert, now) || pki.DueForSelfRenewal(c.Cert, now) ||
		(wantCA != "" && c.CASHA256() != wantCA)
}

// ca reads the install's current CA, which must be the one wantCA names.
func (r Reissuer) ca(wantCA string) (pki.CA, error) {
	cur, _, _, err := r.Store.CA()
	if err != nil {
		return pki.CA{}, fmt.Errorf("reading the install's CA: %w", err)
	}
	ca, err := pki.ParseCA([]byte(cur.CertPEM), []byte(cur.KeyPEM.Reveal()))
	if err != nil {
		return pki.CA{}, fmt.Errorf("reading the install's CA: %w", err)
	}
	if wantCA != "" && ca.SHA256() != wantCA {
		return pki.CA{}, fmt.Errorf("leader.json names the CA %s, the install's current CA is %s; waiting for them to agree", wantCA, ca.SHA256())
	}
	return ca, nil
}

func (r Reissuer) issueClient(ca pki.CA, id pki.Identity, now time.Time) (Creds, error) {
	key, err := pki.NewKey(r.rand())
	if err != nil {
		return Creds{}, err
	}
	csr, err := pki.NewCSR(key, id, r.rand())
	if err != nil {
		return Creds{}, err
	}
	cert, err := ca.IssueClient(csr, id, now, r.rand())
	if err != nil {
		return Creds{}, fmt.Errorf("issuing this node's client certificate: %w", err)
	}
	c := Creds{Key: key, Cert: cert, CA: ca.Cert}
	if err := r.Files.SaveClient(c); err != nil {
		return Creds{}, err
	}
	r.Log.Info().Str("serial", cert.SerialNumber.Text(16)).Time("notAfter", cert.NotAfter).Msg("issued this node's client certificate")
	return c, nil
}

// enrol writes the certificate into the node's enrolment, unless it is there.
func (r Reissuer) enrol(c Creds) error {
	en, found, err := r.Store.Enrolment(r.Node)
	if err != nil {
		return fmt.Errorf("reading this node's enrolment: %w", err)
	}
	if found && en.SHA256 == pki.Fingerprint(c.Cert) {
		return nil
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Cert.Raw}))
	if err := r.Store.SaveEnrolment(r.Node, store.Enrolment{CertPEM: certPEM}); err != nil {
		return fmt.Errorf("writing this node's enrolment: %w", err)
	}
	return nil
}

// ensureServer issues the node's server certificate where it must be.
func (r Reissuer) ensureServer(wantCA string, now time.Time) error {
	name := pki.NodeServerName(r.InstallID, r.Node)
	s, err := r.Files.Server()
	if err == nil && len(s.Cert.DNSNames) == 1 && s.Cert.DNSNames[0] == name &&
		!pki.DueForRenewal(s.Cert, now) && !pki.DueForSelfRenewal(s.Cert, now) && (wantCA == "" || s.CASHA256() == wantCA) {
		return nil
	}
	ca, err := r.ca(wantCA)
	if err != nil {
		return err
	}
	key, err := pki.NewKey(r.rand())
	if err != nil {
		return err
	}
	cert, err := ca.IssueServer(&key.PublicKey, []string{name}, now, r.rand())
	if err != nil {
		return fmt.Errorf("issuing this node's server certificate: %w", err)
	}
	return r.Files.SaveServer(Creds{Key: key, Cert: cert, CA: ca.Cert})
}

func (r Reissuer) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

func (r Reissuer) rand() io.Reader {
	if r.Rand == nil {
		return rand.Reader
	}
	return r.Rand
}
