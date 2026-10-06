package pki

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	joinVersion   = 1
	joinSecretLen = 16
	// maxJoinToken bounds what the join endpoint decodes before it knows
	// anything of the caller; a token with a few endpoints is far shorter.
	maxJoinToken = 4 << 10
	redacted     = "[redacted]"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// JoinToken lets one appliance agent container of a node enrol, once, within
// JoinTokenTTL. It tells the agent where the controller is and how to know
// it, and carries the secret the controller keeps only the hash of. It prints
// and encodes as JSON with the secret redacted; Encode is the one form that
// carries it.
type JoinToken struct {
	Version    int       `json:"v"`
	InstallID  string    `json:"install"`
	Node       string    `json:"node"`
	Endpoints  []string  `json:"endpoints"`
	ServerName string    `json:"serverName"`
	CASHA256   string    `json:"ca"`
	Secret     []byte    `json:"secret"` // 16 bytes
	Expires    time.Time `json:"exp"`
}

// joinWire is a JoinToken as Encode writes it, secret and all.
type joinWire JoinToken

// NewJoinToken makes the join token of node, whose agent reaches the
// controller at one of endpoints (host:port) and pins ca.
func NewJoinToken(installID, node string, endpoints []string, ca CA, now time.Time, rand io.Reader) (JoinToken, error) {
	if ca.Cert == nil {
		return JoinToken{}, errors.New("no CA to make a join token for")
	}
	secret := make([]byte, joinSecretLen)
	if _, err := io.ReadFull(rand, secret); err != nil {
		return JoinToken{}, fmt.Errorf("reading random bytes: %w", err)
	}
	t := JoinToken{
		Version:    joinVersion,
		InstallID:  installID,
		Node:       node,
		Endpoints:  slices.Clone(endpoints),
		ServerName: ControllerServerName(installID),
		CASHA256:   ca.SHA256(),
		Secret:     secret,
		Expires:    now.Add(JoinTokenTTL).UTC().Truncate(time.Second),
	}
	if err := t.check(); err != nil {
		return JoinToken{}, err
	}
	return t, nil
}

// Encode is the token as an admin hands it to the node:
// pco1.<base64url of its JSON>. A token that NewJoinToken or ParseJoinToken
// made always encodes; any other may encode as "".
func (t JoinToken) Encode() string {
	b, err := json.Marshal(joinWire(t))
	if err != nil {
		return ""
	}
	return JoinPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// ParseJoinToken reads a token Encode wrote, with white space around it as a
// file has it. An expired token parses; whether it is still good is the
// caller's to decide, with the record of its uses.
func ParseJoinToken(s string) (JoinToken, error) {
	s = strings.TrimSpace(s)
	if len(s) > maxJoinToken {
		return JoinToken{}, fmt.Errorf("the join token is longer than %d bytes", maxJoinToken)
	}
	payload, ok := strings.CutPrefix(s, JoinPrefix)
	if !ok {
		return JoinToken{}, fmt.Errorf("not a pco join token: it does not start with %s", JoinPrefix)
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return JoinToken{}, fmt.Errorf("reading the join token: %w", err)
	}
	var t JoinToken
	if err := json.Unmarshal(b, (*joinWire)(&t)); err != nil {
		return JoinToken{}, fmt.Errorf("reading the join token: %w", err)
	}
	if err := t.check(); err != nil {
		return JoinToken{}, err
	}
	return t, nil
}

// SecretHash is the SHA-256 of the secret in lower-case hex, which the
// controller stores in place of the secret.
func (t JoinToken) SecretHash() string {
	sum := sha256.Sum256(t.Secret)
	return hex.EncodeToString(sum[:])
}

func (t JoinToken) String() string {
	return fmt.Sprintf("join token of install %s for node %s, expires %s, secret %s",
		t.InstallID, t.Node, t.Expires.UTC().Format(time.RFC3339), redacted)
}

// Format prints String for every verb, so that no verb prints the secret.
func (t JoinToken) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, t.String()) }

// MarshalJSON encodes the token with its secret as "[redacted]", for a log
// line or a reply that holds one.
func (t JoinToken) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		joinWire
		Secret string `json:"secret"`
	}{joinWire(t), redacted})
}

func (t JoinToken) check() error {
	if t.Version != joinVersion {
		return fmt.Errorf("join token version %d, where this build reads version %d", t.Version, joinVersion)
	}
	if err := checkInstallID(t.InstallID); err != nil {
		return err
	}
	if err := checkNode(t.Node); err != nil {
		return err
	}
	if len(t.Endpoints) == 0 {
		return errors.New("the join token names no controller endpoint")
	}
	for _, e := range t.Endpoints {
		if err := checkEndpoint(e); err != nil {
			return err
		}
	}
	if want := ControllerServerName(t.InstallID); t.ServerName != want {
		return fmt.Errorf("the join token names the server name %q, not %s", t.ServerName, want)
	}
	if !sha256Hex.MatchString(t.CASHA256) {
		return fmt.Errorf("the join token's CA fingerprint %q is not a SHA-256 in lower-case hex", t.CASHA256)
	}
	if len(t.Secret) != joinSecretLen {
		return fmt.Errorf("the join token's secret has %d bytes, not %d", len(t.Secret), joinSecretLen)
	}
	if t.Expires.IsZero() {
		return errors.New("the join token has no expiry")
	}
	return nil
}

func checkEndpoint(e string) error {
	host, port, err := net.SplitHostPort(e)
	if err != nil || host == "" {
		return fmt.Errorf("controller endpoint %q is not host:port", e)
	}
	if p, err := strconv.ParseUint(port, 10, 16); err != nil || p == 0 {
		return fmt.Errorf("controller endpoint %q has no port from 1 to 65535", e)
	}
	return nil
}
