package daemon

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/clusterapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// joinProxmox is what the controller of an appliance install reads of
// Proxmox to attest an agent container that joins. *pve.Client is one.
type joinProxmox interface {
	Resources(ctx context.Context) ([]pve.Resource, error)
	HAResources(ctx context.Context) ([]pve.HAResource, error)
	GuestConfig(ctx context.Context, node string, ref model.GuestRef) (pve.GuestConfig, error)
	LXCInterfaces(ctx context.Context, node string, vmid int) ([]pve.GuestIface, error)
}

// attestTimeout bounds the reads of the Proxmox API a join makes, well
// within the 30 seconds an agent waits for its answer.
const attestTimeout = 10 * time.Second

// errContested is a join whose container does not hold together with what it
// says of itself: the node is held contested.
var errContested = errors.New("the join does not match the container")

// applianceJoins is the controller's side of the enrolment of an appliance
// agent. The endpoint checked the token and recorded its first use; here the
// container is attested through the Proxmox API, which is a check of
// consistency and no authentication: that rests on the single-use token root
// pushed into the container, and on the admin's admission by fingerprint. A
// container that does not match what it said, and a second use of a token,
// hold the node contested.
type applianceJoins struct {
	st      *store.Store
	px      joinProxmox
	install string
	// node is the controller's, whose file of security events holds those
	// of the joins.
	node string
	now  func() time.Time
	rand io.Reader
	log  zerolog.Logger
	// note tells the engine of an enrolment and its security events.
	note func(node, msg string)

	mu sync.Mutex // one enrolment or renewal at a time
}

// Join attests the container, issues its client certificate and records
// the node as joining.
func (j *applianceJoins) Join(ctx context.Context, tok pki.JoinToken, csr []byte, src netip.Addr, facts clusterapi.JoinFacts) ([]byte, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	rev, entry, found, err := j.entry(facts.Node)
	if err != nil {
		return nil, err
	}
	if found && entry.State != store.NodeRemoved {
		j.security(facts.Node, fmt.Sprintf("a join token for node %s, which is %s in the install already, was used from %s as container %d; "+
			"the node is left as it is", facts.Node, cmpState(entry.State), src, facts.VMID))
		return nil, fmt.Errorf("node %s is %s in the install already: one agent serves a node", facts.Node, cmpState(entry.State))
	}
	actx, cancel := context.WithTimeout(ctx, attestTimeout)
	defer cancel()
	if err := j.attest(actx, src, facts); err != nil {
		if errors.Is(err, errContested) {
			j.contest(tok, facts.Node, rev, found, err.Error())
		}
		return nil, err
	}
	ca, err := j.ca()
	if err != nil {
		return nil, err
	}
	id := pki.Identity{Kind: pki.KindAppliance, InstallID: j.install, Node: facts.Node, VMID: facts.VMID}
	now := j.now()
	cert, err := ca.IssueClient(csr, id, now, j.rand)
	if err != nil {
		return nil, fmt.Errorf("issuing the certificate of node %s: %w", facts.Node, err)
	}
	next := store.NodeEntry{
		Name: facts.Node, Version: facts.Build, Since: now,
		Kind: string(pki.KindAppliance), VMID: facts.VMID,
		Protocol:   store.ProtocolRange{Min: facts.Protocol.Min, Max: facts.Protocol.Max},
		CertSerial: cert.SerialNumber.Text(16), CertSHA256: pki.Fingerprint(cert),
		State: store.NodeJoining, StateSince: now,
	}
	if found {
		next.Index = entry.Index
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	// The certificate is kept, so that a join whose answer was lost gets it
	// again.
	if err := j.st.SaveEnrolment(facts.Node, store.Enrolment{CertPEM: string(certPEM)}); err != nil {
		return nil, fmt.Errorf("keeping the certificate of node %s: %w", facts.Node, err)
	}
	if err := j.save(next, rev, found); err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("the agent container %d of node %s joined from %s; its certificate's SHA-256 is %s: "+
		"pco node admit %s --fingerprint <at least 8 of it, as pco agent status shows it in the container>",
		facts.VMID, facts.Node, src, next.CertSHA256, facts.Node)
	j.log.Info().Str("node", facts.Node).Int("vmid", facts.VMID).Msg(msg)
	j.tell(facts.Node, msg)
	return certPEM, nil
}

// repeat answers a use of a token past the first that repeats the join its
// first use made, as an agent whose answer was lost sends it again: the same
// container from the same source, for the key of the certificate issued
// then, while the node is still joining. It returns that certificate.
func (j *applianceJoins) repeat(_ context.Context, rec store.JoinRecord, csrDER []byte, src netip.Addr, facts clusterapi.JoinFacts) ([]byte, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(rec.Uses) < 2 {
		return nil, false
	}
	first, last := rec.Uses[0], rec.Uses[len(rec.Uses)-1]
	if first.VMID != facts.VMID || last.VMID != facts.VMID || first.Source != src.Unmap() || last.Source != first.Source {
		return nil, false
	}
	_, entry, found, err := j.entry(rec.Node)
	if err != nil || !found || entry.State != store.NodeJoining || entry.VMID != facts.VMID || entry.Name != facts.Node {
		return nil, false
	}
	en, found, err := j.st.Enrolment(rec.Node)
	if err != nil || !found || en.SHA256 != entry.CertSHA256 {
		return nil, false
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, false
	}
	block, _ := pem.Decode([]byte(en.CertPEM))
	if block == nil {
		return nil, false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, false
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(csr.PublicKey) {
		return nil, false
	}
	j.log.Info().Str("node", rec.Node).Int("vmid", facts.VMID).Msg("the join of an appliance agent came again; it gets the certificate issued for it")
	return []byte(en.CertPEM), true
}

// attest checks the container through the Proxmox API: an LXC on the node
// it names, in pool pco, not an HA resource, configured with every MAC it
// reported, and holding the address the join came from.
func (j *applianceJoins) attest(ctx context.Context, src netip.Addr, f clusterapi.JoinFacts) error {
	ref := model.GuestRef{Kind: model.KindLXC, VMID: f.VMID}
	res, err := j.px.Resources(ctx)
	if err != nil {
		return fmt.Errorf("reading the cluster's resources: %w", err)
	}
	i := slices.IndexFunc(res, func(r pve.Resource) bool { return r.Kind == model.KindLXC && r.VMID == f.VMID })
	switch {
	case i < 0:
		return fmt.Errorf("%w: container %d is not in the cluster", errContested, f.VMID)
	case res[i].Node != f.Node:
		return fmt.Errorf("%w: container %d runs on %s, not on %s", errContested, f.VMID, res[i].Node, f.Node)
	case res[i].Pool != appliance.Pool:
		return fmt.Errorf("container %d is not in pool %s", f.VMID, appliance.Pool)
	}
	ha, err := j.px.HAResources(ctx)
	if err != nil {
		return fmt.Errorf("reading the HA resources: %w", err)
	}
	if slices.ContainsFunc(ha, func(r pve.HAResource) bool { return r.Ref == ref }) {
		return fmt.Errorf(appliance.LineAgentHA, f.VMID)
	}
	cfg, err := j.px.GuestConfig(ctx, f.Node, ref)
	if err != nil {
		return fmt.Errorf("reading the configuration of container %d: %w", f.VMID, err)
	}
	configured := configuredMACs(cfg)
	if len(f.MACs) == 0 {
		return fmt.Errorf("%w: container %d reported no MAC", errContested, f.VMID)
	}
	for _, m := range f.MACs {
		mac, err := model.NormalizeMAC(m)
		if err != nil || !slices.Contains(configured, mac) {
			return fmt.Errorf("%w: container %d is not configured with the MAC %s it reported", errContested, f.VMID, m)
		}
	}
	addrs, err := j.addrsOf(ctx, f.Node, f.VMID, cfg)
	if err != nil {
		return err
	}
	if !slices.Contains(addrs, src.Unmap()) {
		return fmt.Errorf("%w: the join came from %s, which is no address of container %d", errContested, src, f.VMID)
	}
	return nil
}

// addrsOf are the addresses of a container: those it reports and the static
// ones of its configuration.
func (j *applianceJoins) addrsOf(ctx context.Context, node string, vmid int, cfg pve.GuestConfig) ([]netip.Addr, error) {
	var out []netip.Addr
	ifaces, err := j.px.LXCInterfaces(ctx, node, vmid)
	if err != nil {
		return nil, fmt.Errorf("reading the addresses of container %d: %w", vmid, err)
	}
	for _, i := range ifaces {
		out = append(out, i.Addrs...)
	}
	for key, value := range cfg.Values {
		if _, ok := pve.NICIndex(key); !ok {
			continue
		}
		if p, err := netip.ParsePrefix(netOption(value, "ip")); err == nil {
			out = append(out, p.Addr())
		}
	}
	return out, nil
}

// configuredMACs are the MACs of every card of a configuration, in normal
// form.
func configuredMACs(cfg pve.GuestConfig) []string {
	var out []string
	for key, value := range cfg.Values {
		if _, ok := pve.NICIndex(key); !ok {
			continue
		}
		if mac, err := model.NormalizeMAC(netOption(value, "hwaddr")); err == nil {
			out = append(out, mac)
		}
	}
	return out
}

// netOption is the value of key in a card's property string.
func netOption(prop, key string) string {
	for part := range strings.SplitSeq(prop, ",") {
		if k, v, ok := strings.Cut(part, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// contest holds the node contested with every use of its token: an admin
// decides with pco node forget. A node enrolled already is left alone: no
// token is made for it, so the token is not its.
func (j *applianceJoins) contest(tok pki.JoinToken, node string, rev int64, found bool, why string) {
	uses := j.usesOf(tok)
	now := j.now()
	entry := store.NodeEntry{Name: node, Since: now, Kind: string(pki.KindAppliance), State: store.NodeContested, StateSince: now, ContestedUses: uses}
	if err := j.save(entry, rev, found); err != nil {
		j.log.Error().Err(err).Str("node", node).Msg("holding the node contested failed")
		return
	}
	msg := fmt.Sprintf("node %s is contested: %s; pco node forget %s removes the entry, and a new token enrols the node again", node, why, node)
	j.log.Warn().Str("node", node).Msg(msg)
	j.tell(node, msg)
	j.security(node, msg)
}

// misused holds the node of a token used more than once contested, also one
// that joined with its first use: either may be the one that stole it.
func (j *applianceJoins) misused(_ context.Context, rec store.JoinRecord) {
	j.mu.Lock()
	defer j.mu.Unlock()
	rev, entry, found, err := j.entry(rec.Node)
	if err != nil {
		j.log.Error().Err(err).Str("node", rec.Node).Msg("reading the registry for a token used twice failed")
		return
	}
	if found && entry.State != store.NodeJoining && entry.State != store.NodeRemoved && entry.State != store.NodeContested {
		last := rec.Uses[len(rec.Uses)-1]
		j.security(rec.Node, fmt.Sprintf("the used join token of node %s came again from %s as container %d; the node is %s and is left as it is",
			rec.Node, last.Source, last.VMID, entry.State))
		return
	}
	now := j.now()
	next := store.NodeEntry{Name: rec.Node, Since: now, Kind: string(pki.KindAppliance), State: store.NodeContested, StateSince: now, ContestedUses: rec.Uses}
	if found {
		next.VMID, next.Index = entry.VMID, entry.Index
	}
	if err := j.save(next, rev, found); err != nil {
		j.log.Error().Err(err).Str("node", rec.Node).Msg("holding the node contested failed")
		return
	}
	msg := fmt.Sprintf("node %s is contested: its join token was used %d times; pco node forget %s removes the entry, "+
		"and a new token enrols the node again", rec.Node, len(rec.Uses), rec.Node)
	j.log.Warn().Str("node", rec.Node).Msg(msg)
	j.tell(rec.Node, msg)
	j.security(rec.Node, msg)
}

// usesOf are the uses of the record tok is of.
func (j *applianceJoins) usesOf(tok pki.JoinToken) []store.JoinUse {
	records, err := j.st.JoinTokens()
	if err != nil {
		return nil
	}
	for _, rec := range records {
		if rec.Node == tok.Node && tok.Matches(rec.SecretSHA256) {
			return rec.Uses
		}
	}
	return nil
}

// Renew issues the next certificate of an enrolled appliance agent, for the
// new key of csr, and has the registry name it: the one it had is refused
// from then on.
func (j *applianceJoins) Renew(_ context.Context, node string, csr []byte) ([]byte, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	rev, entry, found, err := j.entry(node)
	switch {
	case err != nil:
		return nil, err
	case !found || entry.Kind != string(pki.KindAppliance):
		return nil, fmt.Errorf("node %s is no appliance agent of the install", node)
	case entry.State == store.NodeRemoved || entry.State == store.NodeContested:
		return nil, fmt.Errorf("node %s is %s", node, entry.State)
	}
	ca, err := j.ca()
	if err != nil {
		return nil, err
	}
	id := pki.Identity{Kind: pki.KindAppliance, InstallID: j.install, Node: node, VMID: entry.VMID}
	cert, err := ca.IssueClient(csr, id, j.now(), j.rand)
	if err != nil {
		return nil, fmt.Errorf("issuing the next certificate of node %s: %w", node, err)
	}
	entry.CertSerial, entry.CertSHA256 = cert.SerialNumber.Text(16), pki.Fingerprint(cert)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := j.st.SaveEnrolment(node, store.Enrolment{CertPEM: string(certPEM)}); err != nil {
		return nil, fmt.Errorf("keeping the certificate of node %s: %w", node, err)
	}
	if err := j.save(entry, rev, true); err != nil {
		return nil, err
	}
	j.log.Info().Str("node", node).Str("serial", entry.CertSerial).Msg("renewed the certificate of an appliance agent")
	return certPEM, nil
}

// entry reads node's entry of the registry with its revision.
func (j *applianceJoins) entry(node string) (int64, store.NodeEntry, bool, error) {
	nodes, err := j.st.NodesRev()
	if err != nil {
		return 0, store.NodeEntry{}, false, fmt.Errorf("reading the registry: %w", err)
	}
	for _, n := range nodes {
		if n.Node.Name == node {
			return n.Rev, n.Node, true, nil
		}
	}
	return 0, store.NodeEntry{}, false, nil
}

// save writes the entry at the revision it was read at; a new one only
// while there is none.
func (j *applianceJoins) save(n store.NodeEntry, rev int64, found bool) error {
	if !found {
		rev = 0
	}
	if _, err := j.st.SaveNodeIf(n, rev); err != nil {
		return fmt.Errorf("recording node %s in the registry: %w", n.Name, err)
	}
	return nil
}

func (j *applianceJoins) ca() (pki.CA, error) {
	cur, _, _, err := j.st.CA()
	if err != nil {
		return pki.CA{}, fmt.Errorf("reading the install's CA: %w", err)
	}
	ca, err := pki.ParseCA([]byte(cur.CertPEM), []byte(cur.KeyPEM.Reveal()))
	if err != nil {
		return pki.CA{}, fmt.Errorf("reading the install's CA: %w", err)
	}
	return ca, nil
}

// security records a contested node in the audit trail of the controller.
func (j *applianceJoins) security(node, msg string) {
	e := store.SecurityEvent{At: j.now(), Level: "warn", Kind: securityKindEnrol, Subject: node, Message: msg, Node: j.node}
	if err := j.st.AppendSecurityEvent(j.node, e); err != nil {
		j.log.Warn().Err(err).Msg("recording a security event failed")
	}
}

// securityKindEnrol is the kind of the security events of an enrolment, as
// the agent endpoint records its own.
const securityKindEnrol = "enrolment"

func (j *applianceJoins) tell(node, msg string) {
	if j.note != nil {
		j.note(node, msg)
	}
}

func cmpState(s string) string {
	if s == "" {
		return store.NodeActive
	}
	return s
}
