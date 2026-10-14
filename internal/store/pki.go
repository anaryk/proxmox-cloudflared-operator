package store

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"
)

const (
	idCurrent  = "current"
	idPrevious = "previous"

	// useTries is how often UseJoinToken reads the record again when another
	// use wrote it in between.
	useTries = 64
)

// ErrNoCA is the error of CA when the install has no CA yet.
var ErrNoCA = errors.New("the install has no CA")

// ErrNoJoinToken is the error of UseJoinToken for a token that is not
// stored: never made, or deleted.
var ErrNoJoinToken = errors.New("no such join token")

// CAFiles are the certificate and the key of a CA of the install, in PEM.
// The key is a Secret: it neither prints nor encodes.
type CAFiles struct {
	CertPEM string
	KeyPEM  Secret
}

// String describes the CA without its key.
func (c CAFiles) String() string { return fmt.Sprintf("ca{key: %s}", c.KeyPEM) }

// GoString is String for the %#v verb.
func (c CAFiles) GoString() string { return c.String() }

// caFile is what is written for a CAFiles: the only place where its key is a
// plain string.
type caFile struct {
	CertPEM   string    `json:"certPEM"`
	KeyPEM    string    `json:"keyPEM"`
	RotatedAt time.Time `json:"rotatedAt,omitzero"`
}

func (c CAFiles) check(what string) error {
	if _, err := parseCertPEM(c.CertPEM); err != nil {
		return fmt.Errorf("%s CA: %w", what, err)
	}
	if b, _ := pem.Decode([]byte(c.KeyPEM.Reveal())); b == nil || !strings.HasSuffix(b.Type, "PRIVATE KEY") {
		return fmt.Errorf("%s CA: the key is no private key in PEM", what)
	}
	return nil
}

// parseCertPEM reads the one certificate of a PEM text.
func parseCertPEM(text string) (*x509.Certificate, error) {
	b, rest := pem.Decode([]byte(text))
	if b == nil || b.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) > 0 {
		return nil, errors.New("want one certificate in PEM")
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the certificate cannot be read: %w", err)
	}
	return cert, nil
}

// CA returns the current CA of the install and, for a while after a
// rotation, the previous one with the time it was rotated at. They are kept
// on the private root. Without a CA the error is ErrNoCA.
func (s *Store) CA() (current CAFiles, previous *CAFiles, rotatedAt time.Time, err error) {
	cur, found, err := getOne[caFile](s.private, kindCA, idCurrent)
	switch {
	case err != nil:
		return CAFiles{}, nil, time.Time{}, err
	case !found:
		return CAFiles{}, nil, time.Time{}, ErrNoCA
	}
	prev, found, err := getOne[caFile](s.private, kindCA, idPrevious)
	if err != nil {
		return CAFiles{}, nil, time.Time{}, err
	}
	current = CAFiles{CertPEM: cur.CertPEM, KeyPEM: NewSecret(cur.KeyPEM)}
	if found {
		previous = &CAFiles{CertPEM: prev.CertPEM, KeyPEM: NewSecret(prev.KeyPEM)}
	}
	return current, previous, cur.RotatedAt, nil
}

// SaveCA stores the CA of the install. A rotation names the CA it replaces as
// previous, with the time of the rotation; previous nil removes the one kept,
// once the window in which both are accepted is over. The previous one is
// written first: a rotation cut short leaves both CAs the old one, never the
// old one lost.
func (s *Store) SaveCA(current CAFiles, previous *CAFiles, rotatedAt time.Time) error {
	if err := current.check("the current"); err != nil {
		return err
	}
	if previous != nil {
		if err := previous.check("the previous"); err != nil {
			return err
		}
		if rotatedAt.IsZero() {
			return errors.New("a CA that replaces another needs the time it was rotated at")
		}
		prev := caFile{CertPEM: previous.CertPEM, KeyPEM: previous.KeyPEM.Reveal()}
		if err := s.private.put(kindCA, idPrevious, prev, true); err != nil {
			return err
		}
	}
	cur := caFile{CertPEM: current.CertPEM, KeyPEM: current.KeyPEM.Reveal(), RotatedAt: rotatedAt}
	if err := s.private.put(kindCA, idCurrent, cur, true); err != nil {
		return err
	}
	if previous == nil {
		return s.private.Delete(kindCA, idPrevious)
	}
	return nil
}

// SaveCAIfAbsent stores the first CA of an install, only while no CA is
// stored: two nodes set up at once must not each make one and sign their
// certificates under different keys. When one is stored the error is
// ErrRevision, nothing is written, and the stored CA is the install's.
func (s *Store) SaveCAIfAbsent(current CAFiles) error {
	if err := current.check("the current"); err != nil {
		return err
	}
	_, err := s.private.putIf(kindCA, idCurrent, caFile{CertPEM: current.CertPEM, KeyPEM: current.KeyPEM.Reveal()}, 0)
	return err
}

// Enrolment is the client certificate a node's agent was given, on the
// private root: the certificate a request must present is matched with it,
// so a certificate minted with a stolen key of the CA is still refused.
type Enrolment struct {
	Node    string `json:"node"`
	CertPEM string `json:"certPEM"`
	// Serial is the certificate's serial number in lower-case hex, and
	// SHA256 the SHA-256 of its DER, in lower-case hex.
	Serial string `json:"serial"`
	SHA256 string `json:"sha256"`
}

// Enrolment returns the enrolment of a node.
func (s *Store) Enrolment(node string) (Enrolment, bool, error) {
	return getOne[Enrolment](s.private, kindEnrol, node)
}

// SaveEnrolment stores the certificate of a node's agent. The serial and
// the fingerprint are those of the certificate: either left empty is filled
// in, either given otherwise is refused.
func (s *Store) SaveEnrolment(node string, e Enrolment) error {
	if err := checkNodeName("enrolment", node); err != nil {
		return err
	}
	if e.Node != "" && e.Node != node {
		return fmt.Errorf("the enrolment of node %s cannot be stored as that of node %s", e.Node, node)
	}
	cert, err := parseCertPEM(e.CertPEM)
	if err != nil {
		return fmt.Errorf("enrolment of %s: %w", node, err)
	}
	sum := sha256.Sum256(cert.Raw)
	serial, fingerprint := cert.SerialNumber.Text(16), hex.EncodeToString(sum[:])
	switch {
	case e.Serial != "" && e.Serial != serial:
		return fmt.Errorf("enrolment of %s: the serial %s is not the certificate's, %s", node, e.Serial, serial)
	case e.SHA256 != "" && e.SHA256 != fingerprint:
		return fmt.Errorf("enrolment of %s: the SHA-256 %s is not the certificate's, %s", node, e.SHA256, fingerprint)
	}
	e.Node, e.Serial, e.SHA256 = node, serial, fingerprint
	return s.private.put(kindEnrol, node, e, true)
}

// DeleteEnrolment removes the enrolment of a node. A missing one is not an
// error.
func (s *Store) DeleteEnrolment(node string) error { return s.private.Delete(kindEnrol, node) }

// JoinUse is one use of a join token: when, from where, and what the agent
// that used it said it is.
type JoinUse struct {
	At     time.Time  `json:"at"`
	Source netip.Addr `json:"source,omitzero"`
	VMID   int        `json:"vmid,omitempty"`
	MACs   []string   `json:"macs,omitempty"`
}

// JoinRecord is a join token as the install keeps it: never the secret, only
// its SHA-256, the node it is for, when it expires, and every use.
type JoinRecord struct {
	ID           string    `json:"id"`
	Node         string    `json:"node"`
	SecretSHA256 string    `json:"secretSHA256"`
	CreatedAt    time.Time `json:"createdAt"`
	Expires      time.Time `json:"expires"`
	Uses         []JoinUse `json:"uses,omitempty"`
}

func (r JoinRecord) check() error {
	switch {
	case r.ID == "":
		return errors.New("join token has no id")
	case r.Node == "":
		return fmt.Errorf("join token %s is for no node", r.ID)
	case !isSHA256(r.SecretSHA256):
		return fmt.Errorf("join token %s: want the SHA-256 of its secret, 64 lower-case hex digits", r.ID)
	case r.Expires.IsZero():
		return fmt.Errorf("join token %s has no expiry", r.ID)
	}
	return nil
}

// isSHA256 reports whether s is a SHA-256 in lower-case hex.
func isSHA256(s string) bool {
	if len(s) != 2*sha256.Size {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return (r < '0' || r > '9') && (r < 'a' || r > 'f') })
}

// JoinTokens returns the join tokens by id. They are kept on the private
// root.
func (s *Store) JoinTokens() (map[string]JoinRecord, error) {
	return loadMap(s.private, kindJoinTokens, "id", func(r *JoinRecord) *string { return &r.ID })
}

// JoinToken returns one join token with the revision of its file, which
// SaveJoinTokenIf takes.
func (s *Store) JoinToken(id string) (JoinRecord, int64, bool, error) {
	var r JoinRecord
	rev, found, err := s.private.getRev(kindJoinTokens, id, &r, false)
	if err != nil || !found {
		return JoinRecord{}, 0, false, err
	}
	return r, rev, true, nil
}

// SaveJoinToken stores a new join token. A token is made once: one of the
// id that is stored already is ErrRevision, and keeps its uses, as a retry of
// the same save would otherwise give a used token back unused. A change of a
// stored token is SaveJoinTokenIf's.
func (s *Store) SaveJoinToken(r JoinRecord) error {
	_, err := s.SaveJoinTokenIf(r, 0)
	return err
}

// SaveJoinTokenIf stores a join token read at revision rev, 0 for a new one,
// while its file still has that revision; otherwise the error is
// ErrRevision, with the revision it has. The uses it records are never taken
// back: a record that drops or changes one of the stored uses is refused. It
// returns the revision the token has after.
func (s *Store) SaveJoinTokenIf(r JoinRecord, rev int64) (int64, error) {
	if err := r.check(); err != nil {
		return 0, err
	}
	stored, have, found, err := s.JoinToken(r.ID)
	switch {
	case err != nil:
		return 0, err
	case have != rev:
		return have, fmt.Errorf("%w: join token %s is at revision %d, not %d", ErrRevision, r.ID, have, rev)
	case found && !keepsUses(stored.Uses, r.Uses):
		return have, fmt.Errorf("join token %s: the %d uses recorded must stay as they are, and a use is never taken back", r.ID, len(stored.Uses))
	}
	return s.private.putIf(kindJoinTokens, r.ID, r, rev)
}

// keepsUses reports whether next begins with every use of stored, as it is.
func keepsUses(stored, next []JoinUse) bool {
	if len(next) < len(stored) {
		return false
	}
	for i, u := range stored {
		n := next[i]
		if !u.At.Equal(n.At) || u.Source != n.Source || u.VMID != n.VMID || !slices.Equal(u.MACs, n.MACs) {
			return false
		}
	}
	return true
}

// DeleteJoinToken removes a join token. A missing one is not an error.
func (s *Store) DeleteJoinToken(id string) error { return s.private.Delete(kindJoinTokens, id) }

// UseJoinToken records a use of a join token, before the caller answers it,
// and returns the record with every use: one use is the first, more make the
// node contested. The record is written only at the revision it was read
// at, and read again when another use wrote it in between, so no use is
// lost. Whether the token expired is the caller's to judge; a token that is
// not stored is ErrNoJoinToken.
func (s *Store) UseJoinToken(id string, use JoinUse) (JoinRecord, error) {
	for range useTries {
		var r JoinRecord
		rev, found, err := s.private.getRev(kindJoinTokens, id, &r, false)
		switch {
		case err != nil:
			return JoinRecord{}, err
		case !found:
			return JoinRecord{}, fmt.Errorf("%w: %s", ErrNoJoinToken, id)
		}
		r.Uses = append(slices.Clone(r.Uses), use)
		_, err = s.private.putIf(kindJoinTokens, id, r, rev)
		switch {
		case errors.Is(err, ErrRevision):
			continue
		case err != nil:
			return JoinRecord{}, err
		}
		return r, nil
	}
	return JoinRecord{}, fmt.Errorf("join token %s kept changing while its use was recorded", id)
}

// CARotation is the progress of a rotation of every secret a lost node held,
// on the cluster root, so that a controller that takes over in the middle
// finishes it.
type CARotation struct {
	// Node is the node forgotten as compromised.
	Node      string    `json:"node"`
	StartedAt time.Time `json:"startedAt"`
	Actor     string    `json:"actor,omitempty"`
	// Done are the steps finished, in their order.
	Done       []string  `json:"done,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	// PVESecretSHA256 is the hash of the Proxmox token's secret as the
	// rotation began: the token has a new one once it differs.
	PVESecretSHA256 string `json:"pveSecretSHA256,omitempty"`
	TunnelsRotated
	// Cleaned are the tunnels rotated whose connections were all ended
	// once every connector that remains ran with the new secret.
	Cleaned []string `json:"cleaned,omitempty"`
}

// TunnelsRotated is how far the secrets of tunnels were rotated for nodes
// that are gone: the secret is given once, and what follows it, the
// connectors taking the new token up and those of the nodes dropped at
// Cloudflare, is done until it succeeded.
type TunnelsRotated struct {
	// Rotated are the ids of the tunnels whose secret was rotated so far,
	// and TakenUp those of them whose rotation was followed through.
	Rotated []string `json:"rotated,omitempty"`
	TakenUp []string `json:"takenUp,omitempty"`
	// At is when a secret was last given: a connector runs with it only by
	// a report sent after.
	At time.Time `json:"at,omitzero"`
	// Connectors are, by tunnel id, the ids of the connectors the nodes
	// ran, which are dropped at Cloudflare.
	Connectors map[string][]string `json:"connectors,omitempty"`
}

// CARotation returns the rotation under way or last made.
func (s *Store) CARotation() (CARotation, bool, error) {
	return getOne[CARotation](s.cluster, kindMeta, idCARotation)
}

// SaveCARotation stores the progress of a rotation.
func (s *Store) SaveCARotation(r CARotation) error {
	switch {
	case r.Node == "":
		return errors.New("CA rotation names no node")
	case r.StartedAt.IsZero():
		return fmt.Errorf("CA rotation of %s has no time it started at", r.Node)
	}
	return s.cluster.put(kindMeta, idCARotation, r, true)
}
