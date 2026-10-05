package setup

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

// errNoWeb is the error of a command on a node whose web interface setup did
// not set up.
var errNoWeb = errors.New("the web interface is not set up on this node: run pco setup")

// WebCertStatus is the certificate the web interface serves.
type WebCertStatus struct {
	Mode string // webcert.ModeCA, ModeOwn or ModePVEProxy
	Leaf *x509.Certificate
}

// WebCert returns the certificate the web interface serves, and the mode
// setup gave it.
func (s *Setup) WebCert() (WebCertStatus, error) {
	m, _, err := readManifest(s.manifestPath())
	switch {
	case err != nil:
		return WebCertStatus{}, fmt.Errorf("reading the manifest: %w", err)
	case m.WebCert == "":
		return WebCertStatus{}, errNoWeb
	}
	leaf, err := readCert(s.webPath(webcert.CertName))
	if err != nil {
		return WebCertStatus{}, fmt.Errorf("reading the certificate of the web interface: %w", err)
	}
	return WebCertStatus{Mode: m.WebCert, Leaf: leaf}, nil
}

// RenewWebCert makes a new key and leaf of the cluster CA for the web
// interface now, and restarts pco-web. Only a certificate of mode ca is pco's
// to renew.
func (s *Setup) RenewWebCert(ctx context.Context) (WebCertStatus, error) {
	r, err := s.webRun()
	if err != nil {
		return WebCertStatus{}, err
	}
	switch r.manifest.WebCert {
	case webcert.ModeOwn:
		return WebCertStatus{}, errors.New("the certificate of the web interface is the admin's own (mode own), which pco does not " +
			"renew: pco web cert import <crt> <key> puts another in place")
	case webcert.ModePVEProxy:
		return WebCertStatus{}, errors.New("the web interface serves the certificate of pveproxy (mode pveproxy), which pco follows " +
			"and does not renew; pco setup --repair --web-cert ca gives it a key and a certificate of its own")
	}
	p, err := r.planWeb(ctx)
	if err != nil {
		return WebCertStatus{}, err
	}
	ca, err := readCert(r.host.clusterCA)
	if err != nil {
		return WebCertStatus{}, fmt.Errorf("reading the cluster CA: %w", err)
	}
	if err := r.issueWebCA(ca, p.names); err != nil {
		return WebCertStatus{}, err
	}
	return r.restartWeb(ctx)
}

// ImportWebCert checks a certificate and key of the admin's, puts them in
// place of the ones the web interface serves, which makes its mode own, and
// restarts pco-web.
func (s *Setup) ImportWebCert(ctx context.Context, certFile, keyFile string) (WebCertStatus, error) {
	r, err := s.webRun()
	if err != nil {
		return WebCertStatus{}, err
	}
	r.o.WebCert, r.o.WebCertFile, r.o.WebKeyFile = webcert.ModeOwn, certFile, keyFile
	p, err := r.planWeb(ctx)
	if err != nil {
		return WebCertStatus{}, err
	}
	if _, err := r.webCertOwn(p); err != nil {
		return WebCertStatus{}, err
	}
	if err := r.record(func(m *Manifest) { m.WebCert = webcert.ModeOwn }); err != nil {
		return WebCertStatus{}, err
	}
	return r.restartWeb(ctx)
}

// webRun is the state the commands of the certificate work in: the node and
// the manifest of a node whose web interface setup set up.
func (s *Setup) webRun() (*run, error) {
	if s.host.euid() != 0 {
		return nil, errors.New("this command must run as root")
	}
	m, _, err := readManifest(s.manifestPath())
	switch {
	case err != nil:
		return nil, fmt.Errorf("reading the manifest: %w", err)
	case m.WebCert == "":
		return nil, errNoWeb
	}
	r := &run{Setup: s, manifest: m}
	r.o.Node = m.Node
	if r.o.Node == "" {
		name, err := s.host.hostname()
		if err != nil {
			return nil, fmt.Errorf("reading the host name: %w", err)
		}
		r.o.Node, _, _ = strings.Cut(name, ".")
	}
	return r, nil
}

// restartWeb restarts a pco-web that runs, so that it serves the certificate
// in place, and returns that certificate.
func (r *run) restartWeb(ctx context.Context) (WebCertStatus, error) {
	if _, err := r.run.Run(ctx, "systemctl", "try-restart", webUnit); err != nil {
		return WebCertStatus{}, fmt.Errorf("the certificate is in place, but restarting %s failed: %w", webUnit, err)
	}
	return r.WebCert()
}
