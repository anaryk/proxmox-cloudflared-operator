// Package bundle is the install in one encrypted file: what pco export writes
// and pco import reads back, to restore an install on another node or in
// another cluster.
//
// The file is a header, then the text. The header is
//
//	magic "PCOBUNDL" | format (1) | derivation (1: Argon2id) |
//	passes (4) | memory in KiB (4) | lanes (1) | salt (16) | nonce (12)
//
// with the numbers big-endian. The text is a JSON document sealed with
// AES-256-GCM under a key that Argon2id derives from the passphrase and the
// salt, the header as the additional data, so that nothing in it can be
// changed. Only one set of parameters is accepted, and a header that asks for
// another is refused before any key is derived: whoever hands over a file
// could otherwise make the node that opens it allocate what it chooses.
//
// The bundle carries the key of the install's CA, and with it whoever knows the
// passphrase can pose as the controller to every agent.
package bundle

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// MinPassphrase is the fewest characters of a passphrase: the file can be
// attacked offline for as long as it exists.
const MinPassphrase = 16

const (
	magic         = "PCOBUNDL"
	formatVersion = 1
	kdfArgon2id   = 1

	// The parameters of the derivation: 3 passes over 64 MiB on 4 lanes.
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4

	saltLen   = 16
	nonceLen  = 12
	keyLen    = 32
	tagLen    = 16
	headerLen = len(magic) + 1 + 1 + 4 + 4 + 1 + saltLen + nonceLen

	// generatedLen is the length of a generated passphrase.
	generatedLen = 24
)

// passphraseAlphabet is what a generated passphrase is made of: lower-case
// letters and digits, without the l, o, 0 and 1 that are read as each other.
// It has 32 characters, so that every byte of randomness picks one.
const passphraseAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

var (
	// ErrNotBundle is the error of a file that is no bundle of pco.
	ErrNotBundle = errors.New("this is not a pco bundle")
	// ErrTruncated is the error of a file that ends before the bundle does.
	ErrTruncated = errors.New("the bundle is cut short")
	// ErrUnsupported is the error of a bundle of a format or with parameters
	// this build does not read.
	ErrUnsupported = errors.New("this build cannot read the bundle")
	// ErrWrongPassphrase is the error of a bundle that does not open. A wrong
	// passphrase and a file that was changed are not told apart: either way
	// the text does not authenticate.
	ErrWrongPassphrase = errors.New("the passphrase is wrong, or the bundle was changed or cut short")
	// ErrWeakPassphrase is the error of a passphrase of fewer than MinPassphrase
	// characters.
	ErrWeakPassphrase = errors.New("the passphrase is too short")
)

// CAFiles are the certificate and the key of the install's CA, in PEM, as the
// store keeps them: the key neither prints nor encodes.
type CAFiles = store.CAFiles

// Contents is what the bundle holds. The objects are kept as the JSON the
// store's types encode to, so that a later build reads the bundle of an
// earlier one by what the store reads, not by what this package knew.
type Contents struct {
	InstallID string
	// InstalledAt is when the install was made, which a restore keeps.
	InstalledAt  time.Time
	Settings     json.RawMessage
	ManualRoutes []json.RawMessage
	Approvals    []json.RawMessage
	// Segments are the acknowledged segments of the previous release, and
	// NodeSegments those of each node.
	Segments     []json.RawMessage
	NodeSegments []json.RawMessage
	Nodes        []json.RawMessage
	CA           *CAFiles
	// Credentials are the Cloudflare credentials, with their tokens, and are
	// there only when the export asked for them.
	Credentials []json.RawMessage
	// Leader is leader.json, which the promote that follows a restore draws
	// its epoch above.
	Leader json.RawMessage
	MadeAt time.Time
	Build  string
}

// summary is what a Contents prints as: what it holds, never the content.
type summary struct {
	InstallID   string    `json:"installId"`
	Nodes       int       `json:"nodes"`
	CA          bool      `json:"ca"`
	Credentials int       `json:"credentials"`
	MadeAt      time.Time `json:"madeAt"`
	Build       string    `json:"build"`
}

func (c Contents) summary() summary {
	return summary{
		InstallID: c.InstallID, Nodes: len(c.Nodes), CA: c.CA != nil, Credentials: len(c.Credentials),
		MadeAt: c.MadeAt, Build: c.Build,
	}
}

// String describes the bundle without what is in it: its credentials and the
// key of its CA do not print, whichever way the bundle is printed.
func (c Contents) String() string {
	s := c.summary()
	return fmt.Sprintf("bundle{install: %s, nodes: %d, ca: %t, credentials: %d}", s.InstallID, s.Nodes, s.CA, s.Credentials)
}

// GoString is String for the %#v verb.
func (c Contents) GoString() string { return c.String() }

// Format prints String for every verb.
func (c Contents) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// MarshalJSON encodes what String says, for a log: the bundle itself is
// written by Seal.
func (c Contents) MarshalJSON() ([]byte, error) { return json.Marshal(c.summary()) }

// document is the text of a bundle.
type document struct {
	InstallID    string            `json:"installId"`
	InstalledAt  time.Time         `json:"installedAt,omitzero"`
	Settings     json.RawMessage   `json:"settings,omitempty"`
	ManualRoutes []json.RawMessage `json:"manualRoutes,omitempty"`
	Approvals    []json.RawMessage `json:"approvals,omitempty"`
	Segments     []json.RawMessage `json:"segments,omitempty"`
	NodeSegments []json.RawMessage `json:"nodeSegments,omitempty"`
	Nodes        []json.RawMessage `json:"nodes,omitempty"`
	CA           *caDocument       `json:"ca,omitempty"`
	Credentials  []json.RawMessage `json:"credentials,omitempty"`
	Leader       json.RawMessage   `json:"leader,omitempty"`
	MadeAt       time.Time         `json:"madeAt"`
	Build        string            `json:"build,omitempty"`
}

// caDocument is the only place where the key of the CA is a plain string.
type caDocument struct {
	CertPEM string `json:"certPEM"`
	KeyPEM  string `json:"keyPEM"`
}

func (c Contents) document() document {
	d := document{
		InstallID: c.InstallID, InstalledAt: c.InstalledAt, Settings: c.Settings, ManualRoutes: c.ManualRoutes,
		Approvals: c.Approvals, Segments: c.Segments, NodeSegments: c.NodeSegments, Nodes: c.Nodes,
		Credentials: c.Credentials, Leader: c.Leader, MadeAt: c.MadeAt, Build: c.Build,
	}
	if c.CA != nil {
		d.CA = &caDocument{CertPEM: c.CA.CertPEM, KeyPEM: c.CA.KeyPEM.Reveal()}
	}
	return d
}

func (d document) contents() Contents {
	c := Contents{
		InstallID: d.InstallID, InstalledAt: d.InstalledAt, Settings: d.Settings, ManualRoutes: d.ManualRoutes,
		Approvals: d.Approvals, Segments: d.Segments, NodeSegments: d.NodeSegments, Nodes: d.Nodes,
		Credentials: d.Credentials, Leader: d.Leader, MadeAt: d.MadeAt, Build: d.Build,
	}
	if d.CA != nil {
		c.CA = &CAFiles{CertPEM: d.CA.CertPEM, KeyPEM: store.NewSecret(d.CA.KeyPEM)}
	}
	return c
}

// CheckPassphrase refuses a passphrase of fewer than MinPassphrase characters.
// The error says how many it has, never what it is.
func CheckPassphrase(passphrase []byte) error {
	if n := utf8.RuneCount(passphrase); n < MinPassphrase {
		return fmt.Errorf("%w: it has %d characters, and at least %d are needed", ErrWeakPassphrase, n, MinPassphrase)
	}
	return nil
}

// Generate returns a passphrase of 24 characters, 120 bits drawn from rand.
func Generate(rand io.Reader) (string, error) {
	raw := make([]byte, generatedLen)
	if _, err := io.ReadFull(rand, raw); err != nil {
		return "", fmt.Errorf("drawing a passphrase: %w", err)
	}
	for i, b := range raw {
		raw[i] = passphraseAlphabet[int(b)%len(passphraseAlphabet)]
	}
	return string(raw), nil
}

// Seal encrypts the bundle with AES-256-GCM under a key derived with Argon2id
// (3 passes, 64 MiB, 4 lanes, a salt of 16 bytes from rand) from the
// passphrase, which must have at least MinPassphrase characters.
func Seal(c Contents, passphrase []byte, rand io.Reader) ([]byte, error) {
	if err := CheckPassphrase(passphrase); err != nil {
		return nil, err
	}
	if c.InstallID == "" {
		return nil, errors.New("the bundle holds no install")
	}
	text, err := json.Marshal(c.document())
	if err != nil {
		return nil, fmt.Errorf("encoding the bundle: %w", err)
	}
	defer clear(text)
	return sealBytes(text, passphrase, rand)
}

// sealBytes seals text, whatever it is, as the format asks.
func sealBytes(text, passphrase []byte, rand io.Reader) ([]byte, error) {
	out := make([]byte, headerLen, headerLen+len(text)+tagLen)
	copy(out, magic)
	out[len(magic)], out[len(magic)+1] = formatVersion, kdfArgon2id
	at := len(magic) + 2
	binary.BigEndian.PutUint32(out[at:], argonTime)
	binary.BigEndian.PutUint32(out[at+4:], argonMemory)
	out[at+8] = argonThreads
	if _, err := io.ReadFull(rand, out[at+9:]); err != nil {
		return nil, fmt.Errorf("drawing the salt and the nonce: %w", err)
	}
	gcm, key, err := newGCM(passphrase, out[headerLen-nonceLen-saltLen:headerLen-nonceLen])
	if err != nil {
		return nil, err
	}
	defer clear(key)
	return gcm.Seal(out, out[headerLen-nonceLen:], text, out[:headerLen]), nil
}

// Open decrypts a bundle. A passphrase that is wrong and a file that was
// changed fail alike, with ErrWrongPassphrase; one that ends too soon is
// ErrTruncated.
func Open(b []byte, passphrase []byte) (Contents, error) {
	if len(b) < len(magic) || !bytes.Equal(b[:len(magic)], []byte(magic)) {
		if len(b) < len(magic) && bytes.HasPrefix([]byte(magic), b) {
			return Contents{}, ErrTruncated
		}
		return Contents{}, ErrNotBundle
	}
	if len(b) < headerLen+tagLen {
		return Contents{}, ErrTruncated
	}
	if err := checkHeader(b[len(magic):headerLen]); err != nil {
		return Contents{}, err
	}
	gcm, key, err := newGCM(passphrase, b[headerLen-nonceLen-saltLen:headerLen-nonceLen])
	if err != nil {
		return Contents{}, err
	}
	defer clear(key)
	text, err := gcm.Open(nil, b[headerLen-nonceLen:headerLen], b[headerLen:], b[:headerLen])
	if err != nil {
		return Contents{}, ErrWrongPassphrase
	}
	defer clear(text)
	var d document
	if err := unmarshalStrict(text, &d); err != nil {
		return Contents{}, fmt.Errorf("reading the bundle: %w", err)
	}
	if d.InstallID == "" {
		return Contents{}, errors.New("the bundle holds no install")
	}
	return d.contents(), nil
}

// checkHeader refuses a format or parameters this build does not read. h is
// the header after the magic.
func checkHeader(h []byte) error {
	switch {
	case h[0] > formatVersion:
		return fmt.Errorf("%w: it is of format %d, and this build reads format %d; a newer pco made it", ErrUnsupported, h[0], formatVersion)
	case h[0] != formatVersion:
		return fmt.Errorf("%w: it is not a bundle of a known format (format %d)", ErrUnsupported, h[0])
	case h[1] != kdfArgon2id:
		return fmt.Errorf("%w: it derives its key by method %d, and this build knows method %d, Argon2id", ErrUnsupported, h[1], kdfArgon2id)
	case binary.BigEndian.Uint32(h[2:]) != argonTime, binary.BigEndian.Uint32(h[6:]) != argonMemory, h[10] != argonThreads:
		return fmt.Errorf("%w: it asks for other parameters of the key derivation than the ones pco uses", ErrUnsupported)
	}
	return nil
}

// newGCM derives the key from the passphrase and the salt, and returns the
// cipher over it, with the key to clear.
func newGCM(passphrase, salt []byte) (cipher.AEAD, []byte, error) {
	key := argon2.IDKey(passphrase, salt, argonTime, argonMemory, argonThreads, keyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		clear(key)
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		clear(key)
		return nil, nil, err
	}
	return gcm, key, nil
}
