package clusterapi

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// RotationWindow is how long after a rotation of the CA client certificates
// under the previous one are still taken: long enough for every host agent,
// which reads leader.json every two seconds, to issue its own again.
const RotationWindow = 10 * time.Minute

// ObserveWindow bounds the observe-only window of the check, whatever the
// controller says: a store left in observe-only mode for days must not keep
// enrolling certificates nobody asked for.
const ObserveWindow = 24 * time.Hour

// The kind of the security events of the check, as the engine names the
// events of the enrolment of a node.
const eventKindEnrolment = "enrol"

// Registry is what the check reads of the store, and writes when the registry
// follows a node's enrolment. *store.Store is one.
type Registry interface {
	NodesRev() ([]store.NodeRev, error)
	SaveNodeIf(n store.NodeEntry, rev int64) (int64, error)
	Enrolment(node string) (store.Enrolment, bool, error)
	SaveEnrolment(node string, e store.Enrolment) error
	CA() (current store.CAFiles, previous *store.CAFiles, rotatedAt time.Time, err error)
}

// Checker is the Auth of a controller: the allowlist before TLS, and per
// request the registry. A host node's certificate must be the one its
// enrolment file holds, which the node writes itself, so a certificate it
// issued on its own is taken and one minted elsewhere with the CA's key is
// not. An appliance agent's must be the one the registry holds, as the
// controller issued it.
type Checker struct {
	InstallID string
	Store     Registry
	Allow     *Allowlist
	Now       func() time.Time
	// ObserveOnly reports whether the controller is in its observe-only
	// window, after a promotion, an import or a recovery and until the
	// admin's apply, and since when. Within ObserveWindow of since, the
	// certificate of a registered node the restored store holds none for is
	// taken and recorded; one that differs from the certificate it holds is
	// refused, window or not.
	ObserveOnly func() (since time.Time, on bool)
	// Record receives the security events of the check. It may be nil.
	Record func(store.SecurityEvent)
	Log    zerolog.Logger
}

// Allowed reports whether the allowlist takes a connection from src.
func (c *Checker) Allowed(src netip.Addr, forJoin bool) bool {
	return c.Allow != nil && c.Allow.Allowed(src, forJoin)
}

// Check returns the node a client certificate speaks for, or why it is
// refused. A refusal without a code is a check that could not be made, as
// when the registry could not be read: no agent stands down for that.
func (c *Checker) Check(cert *x509.Certificate, src netip.Addr) (string, *agentproto.Refusal) {
	id, err := pki.IdentityOf(cert)
	if err != nil {
		return "", refusal(agentproto.RefusalNotEnrolled, err.Error())
	}
	if id.InstallID != c.InstallID {
		return "", refusal(agentproto.RefusalOtherInstall, fmt.Sprintf("the certificate is of install %s, this controller serves %s", id.InstallID, c.InstallID))
	}
	nodes, err := c.Store.NodesRev()
	if err != nil {
		return "", unchecked("the registry could not be read: " + err.Error())
	}
	i := slices.IndexFunc(nodes, func(n store.NodeRev) bool { return n.Node.Name == id.Node })
	if i < 0 {
		return "", refusal(agentproto.RefusalNotEnrolled, fmt.Sprintf("node %s is not in the registry of install %s", id.Node, c.InstallID))
	}
	entry, rev := nodes[i].Node, nodes[i].Rev
	switch entry.State {
	case store.NodeRemoved:
		return "", refusal(agentproto.RefusalRemoved, fmt.Sprintf("node %s was removed from install %s", id.Node, c.InstallID))
	case store.NodeContested:
		return "", refusal(agentproto.RefusalContested, fmt.Sprintf("node %s is contested; an admin decides with pco node admit or pco node forget", id.Node))
	}
	if kind := entryKind(entry); kind != id.Kind {
		return "", refusal(agentproto.RefusalNotEnrolled, fmt.Sprintf("node %s is enrolled as a %s node, the certificate is of kind %s", id.Node, kind, id.Kind))
	}
	if !c.belongs(id.Node, src) {
		return "", refusal(agentproto.RefusalOtherSource, fmt.Sprintf("the certificate of node %s came from %s, which is not an address of that node", id.Node, src))
	}
	if id.Kind == pki.KindAppliance {
		return c.checkAppliance(id, cert, entry, rev)
	}
	return c.checkHost(id, cert, entry, rev)
}

// entryKind is the kind of a node's agent; an entry made before kinds is a
// host's.
func entryKind(e store.NodeEntry) pki.Kind {
	if e.Kind == "" {
		return pki.KindHost
	}
	return pki.Kind(e.Kind)
}

func (c *Checker) belongs(node string, src netip.Addr) bool {
	return c.Allow != nil && c.Allow.Belongs(node, src)
}

func (c *Checker) checkHost(id pki.Identity, cert *x509.Certificate, entry store.NodeEntry, rev int64) (string, *agentproto.Refusal) {
	serial, sum := cert.SerialNumber.Text(16), pki.Fingerprint(cert)
	en, found, err := c.Store.Enrolment(id.Node)
	if err != nil {
		return "", unchecked(fmt.Sprintf("the enrolment of node %s could not be read: %v", id.Node, err))
	}
	switch {
	case found && en.Serial == serial && en.SHA256 == sum:
		if entry.CertSerial != serial || entry.CertSHA256 != sum {
			c.follow(entry, rev, serial, sum, fmt.Sprintf(
				"the registry's certificate of node %s now follows its enrolment: serial %s, SHA-256 %s", id.Node, serial, sum))
		}
		return id.Node, nil
	case !found && c.observing():
		if err := c.Store.SaveEnrolment(id.Node, store.Enrolment{CertPEM: encodeCert(cert)}); err != nil {
			return "", unchecked(fmt.Sprintf("the enrolment of node %s could not be written: %v", id.Node, err))
		}
		c.follow(entry, rev, serial, sum, observedLine(id.Node, serial))
		return id.Node, nil
	case !found:
		return "", refusal(agentproto.RefusalNotEnrolled, fmt.Sprintf("node %s has no enrolment; pco setup on the node enrols it", id.Node))
	}
	return "", refusal(agentproto.RefusalNotEnrolled, fmt.Sprintf(
		"the certificate of node %s (serial %s) is not the one of its enrolment (serial %s)", id.Node, serial, en.Serial))
}

func (c *Checker) checkAppliance(id pki.Identity, cert *x509.Certificate, entry store.NodeEntry, rev int64) (string, *agentproto.Refusal) {
	if entry.VMID != id.VMID {
		return "", refusal(agentproto.RefusalNotEnrolled, fmt.Sprintf(
			"the certificate is of container %d, node %s is enrolled with container %d", id.VMID, id.Node, entry.VMID))
	}
	serial, sum := cert.SerialNumber.Text(16), pki.Fingerprint(cert)
	switch {
	case entry.CertSerial == serial && entry.CertSHA256 == sum:
		return id.Node, nil
	case entry.CertSerial == "" && c.observing():
		c.follow(entry, rev, serial, sum, observedLine(id.Node, serial))
		return id.Node, nil
	}
	return "", refusal(agentproto.RefusalNotEnrolled, fmt.Sprintf(
		"the certificate of node %s (serial %s) is not the one the registry holds", id.Node, serial))
}

func observedLine(node, serial string) string {
	return fmt.Sprintf("observe-only: took a certificate of node %s, for which the restored registry held none (serial %s), and recorded it", node, serial)
}

func (c *Checker) observing() bool {
	if c.ObserveOnly == nil {
		return false
	}
	since, on := c.ObserveOnly()
	now := c.now()
	return on && !now.Before(since) && now.Before(since.Add(ObserveWindow))
}

// follow writes the certificate's serial and fingerprint into the node's
// entry, at the revision it was read at, and records why. An entry another
// writer changed meanwhile is left to the next request.
func (c *Checker) follow(entry store.NodeEntry, rev int64, serial, sum, msg string) {
	entry.CertSerial, entry.CertSHA256 = serial, sum
	if _, err := c.Store.SaveNodeIf(entry, rev); err != nil {
		if !errors.Is(err, store.ErrRevision) {
			c.Log.Warn().Err(err).Str("node", entry.Name).Msg("the registry could not follow the node's certificate")
		}
		return
	}
	c.Log.Info().Str("node", entry.Name).Msg(msg)
	if c.Record != nil {
		c.Record(store.SecurityEvent{At: c.now(), Kind: eventKindEnrolment, Subject: entry.Name, Message: msg})
	}
}

func (c *Checker) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// Pool returns the CAs client certificates are verified under: the current
// one, and the previous one for RotationWindow after a rotation. A store
// that cannot be read gives an empty pool, under which no certificate
// verifies.
func (c *Checker) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	current, previous, rotatedAt, err := c.Store.CA()
	if err != nil {
		c.Log.Error().Err(err).Msg("the install's CA could not be read; no agent can connect")
		return pool
	}
	add := func(what, text string) {
		if !pool.AppendCertsFromPEM([]byte(text)) {
			c.Log.Error().Msgf("the %s CA of the install could not be read", what)
		}
	}
	add("current", current.CertPEM)
	if previous != nil && c.now().Before(rotatedAt.Add(RotationWindow)) {
		add("previous", previous.CertPEM)
	}
	return pool
}

func encodeCert(cert *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
}

func refusal(code, msg string) *agentproto.Refusal {
	return &agentproto.Refusal{Code: code, Message: msg}
}

// unchecked is a check that could not be made.
func unchecked(msg string) *agentproto.Refusal { return &agentproto.Refusal{Message: msg} }
