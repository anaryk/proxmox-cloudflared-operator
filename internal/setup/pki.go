package setup

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The files of the node's agent in the local root of the store: its key and
// client certificate, the server certificate the node presents when it holds
// the lease, and a copy of the CA.
const (
	agentDir       = "agent"
	agentKeyFile   = "key.pem"
	agentCertFile  = "cert.pem"
	agentCAFile    = "ca.pem"
	serverKeyFile  = "server-key.pem"
	serverCertFile = "server.pem"
)

// ensureCA reads the CA of the install, or makes one when the install has
// none and this is its first node. A node that joins others needs the CA
// they made: the first node of an install that was set up before clusters
// makes it when it first runs this release.
func (r *run) ensureCA() (pki.CA, error) {
	ca, found, err := r.storedCA()
	switch {
	case err != nil:
		return pki.CA{}, err
	case found:
		return ca, nil
	case len(r.others) > 0:
		who := "the node " + r.others[0] + " makes"
		if len(r.others) > 1 {
			who = "one of the nodes " + strings.Join(r.others, ", ") + " makes"
		}
		return pki.CA{}, fmt.Errorf("the install has no CA yet, which %s when it runs this release of pco: upgrade and start pco there first", who)
	}
	return r.createCA()
}

// storedCA reads the current CA of the install; found is false when there
// is none.
func (r *run) storedCA() (pki.CA, bool, error) {
	current, _, _, err := r.st.CA()
	switch {
	case errors.Is(err, store.ErrNoCA):
		return pki.CA{}, false, nil
	case err != nil:
		return pki.CA{}, false, fmt.Errorf("reading the CA: %w", err)
	}
	ca, err := pki.ParseCA([]byte(current.CertPEM), []byte(current.KeyPEM.Reveal()))
	if err != nil {
		return pki.CA{}, false, fmt.Errorf("the CA of the install cannot be read: %w", err)
	}
	return ca, true, nil
}

// createCA makes the CA of the install, stored only while none is: a setup
// on another node that stored one first wins, and this node issues under
// that one. leader.json then names the CA that is stored, whichever made it.
func (r *run) createCA() (pki.CA, error) {
	made, err := pki.NewCA(r.install.ID, r.now(), r.rand)
	if err != nil {
		return pki.CA{}, fmt.Errorf("making the CA: %w", err)
	}
	certPEM, keyPEM, err := made.PEM()
	if err != nil {
		return pki.CA{}, fmt.Errorf("encoding the CA: %w", err)
	}
	err = r.st.SaveCAIfAbsent(store.CAFiles{CertPEM: string(certPEM), KeyPEM: store.NewSecret(string(keyPEM))})
	if err != nil && !errors.Is(err, store.ErrRevision) {
		return pki.CA{}, fmt.Errorf("storing the CA: %w", err)
	}
	ca, found, rerr := r.storedCA()
	switch {
	case rerr != nil:
		return pki.CA{}, rerr
	case !found:
		return pki.CA{}, errors.New("the CA of the install was stored and is gone again")
	case err != nil:
		r.ask.Info("cluster CA: another node made it meanwhile (SHA-256 %s)", ca.SHA256())
	default:
		r.ask.Info("cluster CA: created (SHA-256 %s)", ca.SHA256())
	}
	if err := r.nameCA(ca); err != nil {
		return pki.CA{}, err
	}
	return ca, nil
}

// nameCA writes the fingerprint of the stored CA into leader.json, which
// every agent of the install reads to know which CA is current, at the
// revision it read, reading again when another writer came between.
func (r *run) nameCA(ca pki.CA) error {
	for range registerTries {
		leader, rev, found, err := r.st.Leader()
		switch {
		case err != nil:
			return fmt.Errorf("reading leader.json: %w", err)
		case !found, leader.CASHA256 == ca.SHA256():
			return nil
		}
		leader.CASHA256 = ca.SHA256()
		_, err = r.st.SaveLeaderIf(leader, rev)
		switch {
		case errors.Is(err, store.ErrRevision):
			continue
		case err != nil:
			return fmt.Errorf("naming the CA in leader.json: %w", err)
		}
		return nil
	}
	return errors.New("leader.json kept changing while the CA was named in it")
}

// ensureCertificates keeps the node's client and server certificates while
// they are of the CA, name the node and have more than 30 days left, and
// issues them anew, with new keys, otherwise. The client certificate is
// written to the node's enrolment, which the controller matches a connection
// against.
func (r *run) ensureCertificates(ca pki.CA) error {
	dir := filepath.Join(r.paths().Local, agentDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("making %s: %w", dir, err)
	}
	now := r.now()
	want := pki.Identity{Kind: pki.KindHost, InstallID: r.install.ID, Node: r.o.Node}
	client, clientOK := r.leafKept(dir, agentKeyFile, agentCertFile, ca, now)
	if clientOK {
		id, err := pki.IdentityOf(client)
		clientOK = err == nil && id == want
	}
	serverName := pki.NodeServerName(r.install.ID, r.o.Node)
	server, serverOK := r.leafKept(dir, serverKeyFile, serverCertFile, ca, now)
	serverOK = serverOK && slices.Equal(server.DNSNames, []string{serverName}) && len(server.URIs) == 0

	issued := false
	if !clientOK {
		key, err := pki.NewKey(r.rand)
		if err != nil {
			return fmt.Errorf("making the key of the agent: %w", err)
		}
		csr, err := pki.NewCSR(key, want, r.rand)
		if err != nil {
			return fmt.Errorf("requesting the certificate of the agent: %w", err)
		}
		if client, err = ca.IssueClient(csr, want, now, r.rand); err != nil {
			return fmt.Errorf("issuing the certificate of the agent: %w", err)
		}
		if err := writeLeaf(dir, agentKeyFile, agentCertFile, key, client); err != nil {
			return err
		}
		issued = true
	}
	if !serverOK {
		key, err := pki.NewKey(r.rand)
		if err != nil {
			return fmt.Errorf("making the server key of the node: %w", err)
		}
		if server, err = ca.IssueServer(&key.PublicKey, []string{serverName}, now, r.rand); err != nil {
			return fmt.Errorf("issuing the server certificate of the node: %w", err)
		}
		if err := writeLeaf(dir, serverKeyFile, serverCertFile, key, server); err != nil {
			return err
		}
		issued = true
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw})
	if have, err := os.ReadFile(filepath.Join(dir, agentCAFile)); err != nil || string(have) != string(caPEM) {
		if err := atomicfile.Write(filepath.Join(dir, agentCAFile), caPEM, atomicfile.Options{Mode: 0o600}); err != nil {
			return fmt.Errorf("writing %s: %w", filepath.Join(dir, agentCAFile), err)
		}
	}
	r.serial, r.fingerprint = client.SerialNumber.Text(16), pki.Fingerprint(client)
	enrol, found, err := r.st.Enrolment(r.o.Node)
	if err != nil {
		return fmt.Errorf("reading the enrolment of %s: %w", r.o.Node, err)
	}
	if !found || enrol.SHA256 != r.fingerprint {
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: client.Raw})
		if err := r.st.SaveEnrolment(r.o.Node, store.Enrolment{CertPEM: string(certPEM)}); err != nil {
			return fmt.Errorf("storing the enrolment of %s: %w", r.o.Node, err)
		}
	}
	if issued {
		r.ask.Info("certificates: issued the client certificate of node %s (serial %s, SHA-256 %s) and the server certificate %s, "+
			"both until %s", r.o.Node, r.serial, r.fingerprint, serverName, dateOf(client.NotAfter))
		return nil
	}
	r.ask.Info("certificates: kept, until %s", dateOf(client.NotAfter))
	return nil
}

// leafKept reads a key and its certificate from dir and reports whether they
// can stay: both read, the key is the certificate's, the CA signed it, it is
// valid now and not within 30 days of its end.
func (r *run) leafKept(dir, keyFile, certFile string, ca pki.CA, now time.Time) (*x509.Certificate, bool) {
	key, err := readKey(filepath.Join(dir, keyFile))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			r.ask.Warn("%s: %v; a new key is made", filepath.Join(dir, keyFile), err)
		}
		return nil, false
	}
	cert, err := readCert(filepath.Join(dir, certFile))
	if err != nil {
		return nil, false
	}
	switch {
	case !key.PublicKey.Equal(cert.PublicKey),
		cert.CheckSignatureFrom(ca.Cert) != nil,
		now.Before(cert.NotBefore),
		pki.DueForSelfRenewal(cert, now):
		return nil, false
	}
	return cert, true
}

// writeLeaf writes a key and its certificate, the key first and both readable
// by root alone.
func writeLeaf(dir, keyFile, certFile string, key *ecdsa.PrivateKey, cert *x509.Certificate) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("encoding the key: %w", err)
	}
	keyPath, certPath := filepath.Join(dir, keyFile), filepath.Join(dir, certFile)
	if err := atomicfile.Write(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), atomicfile.Options{Mode: 0o600}); err != nil {
		return fmt.Errorf("writing %s: %w", keyPath, err)
	}
	if err := atomicfile.Write(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), atomicfile.Options{Mode: 0o600}); err != nil {
		return fmt.Errorf("writing %s: %w", certPath, err)
	}
	return nil
}

// readKey reads the P-256 key writeLeaf wrote.
func readKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("no key in PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("the key is not an ECDSA key")
	}
	return ec, nil
}
