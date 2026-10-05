// Package pvefake is a Proxmox VE API with the few calls the sign-in of the web
// interface makes: the privileges and the guests of whoever presents a ticket
// or an API token, and the realms. The tests of the web process serve it over
// TLS, and hack/fakepve serves it to a browser.
package pvefake

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// Users is the content of users.json.
type Users struct {
	Users []User `json:"users"`
}

// User is a Proxmox VE user, with what it may do and the credentials it signs
// in with.
type User struct {
	ID string `json:"user"` // "alice@pve"
	// Privileges are the effective privileges by path, as access/permissions
	// answers them; a path not listed has those of the nearest one above it.
	Privileges map[string][]string `json:"privileges"`
	// Guests are what cluster/resources lists for the user, as "qemu/101".
	Guests []string `json:"guests"`
	// Tickets are the PVEAuthCookie values accepted for the user, each
	// PVE:<user>:<anything>::<signature>.
	Tickets []string `json:"tickets"`
	Tokens  []Token  `json:"tokens"`
}

// Token is an API token of a user, presented as <user>!<id>=<secret>.
type Token struct {
	ID     string `json:"id"` // "pco" of "alice@pve!pco"
	Secret string `json:"secret"`
	// Privsep is Proxmox VE's privilege separation: the token has what it is
	// given here and the user has too, not all the user has.
	Privsep    bool                `json:"privsep"`
	Privileges map[string][]string `json:"privileges"`
	Guests     []string            `json:"guests"`
}

// Fake answers as pveproxy would. It is safe for concurrent use.
type Fake struct {
	node string

	mu    sync.Mutex
	users map[string]*User
	calls map[string]int
}

// New returns a fake for users on the node named node.
func New(node string, users Users) (*Fake, error) {
	f := &Fake{node: node, users: map[string]*User{}, calls: map[string]int{}}
	for i := range users.Users {
		u := users.Users[i]
		if err := check(u); err != nil {
			return nil, err
		}
		if _, dup := f.users[u.ID]; dup {
			return nil, fmt.Errorf("user %s is listed twice", u.ID)
		}
		f.users[u.ID] = &u
	}
	return f, nil
}

// Load reads users.json.
func Load(file, node string) (*Fake, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var users Users
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&users); err != nil {
		return nil, fmt.Errorf("reading %s: %w", file, err)
	}
	return New(node, users)
}

func check(u User) error {
	if name, realm, ok := strings.Cut(u.ID, "@"); !ok || name == "" || realm == "" {
		return fmt.Errorf("user %q is not user@realm", u.ID)
	}
	for _, t := range u.Tickets {
		if user, ok := ticketUser(t); !ok || user != u.ID {
			return fmt.Errorf("ticket %q of %s is not PVE:%s:<anything>::<signature>", t, u.ID, u.ID)
		}
	}
	guests := slices.Clone(u.Guests)
	for _, t := range u.Tokens {
		if t.ID == "" || t.Secret == "" || strings.ContainsAny(t.ID+t.Secret, "!=") {
			return fmt.Errorf("token %q of %s needs an id and a secret without ! or =", t.ID, u.ID)
		}
		guests = append(guests, t.Guests...)
	}
	for _, g := range guests {
		if _, err := model.ParseGuestRef(g); err != nil {
			return fmt.Errorf("guest of %s: %w", u.ID, err)
		}
	}
	return nil
}

// ticketUser is the user a ticket names, when it has the form of one.
func ticketUser(ticket string) (string, bool) {
	parts := strings.Split(ticket, ":")
	if len(parts) != 5 || parts[0] != "PVE" || parts[1] == "" || parts[3] != "" || parts[4] == "" {
		return "", false
	}
	return parts[1], true
}

// SetPrivileges gives user exactly privs at path, as a change of its ACL would.
func (f *Fake) SetPrivileges(user, path string, privs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.users[user]; ok {
		u.Privileges = maps.Clone(u.Privileges)
		if u.Privileges == nil {
			u.Privileges = map[string][]string{}
		}
		u.Privileges[path] = privs
	}
}

// AddTicket accepts ticket for user from now on: Proxmox VE renewed it.
func (f *Fake) AddTicket(user, ticket string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.users[user]; ok {
		u.Tickets = append(slices.Clone(u.Tickets), ticket)
	}
}

// RemoveTicket refuses ticket from now on, as Proxmox VE does once it expired.
func (f *Fake) RemoveTicket(ticket string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		u.Tickets = slices.DeleteFunc(slices.Clone(u.Tickets), func(t string) bool { return t == ticket })
	}
}

// RemoveToken revokes the token user@realm!id.
func (f *Fake) RemoveToken(tokenID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user, id, _ := strings.Cut(tokenID, "!")
	if u, ok := f.users[user]; ok {
		u.Tokens = slices.DeleteFunc(slices.Clone(u.Tokens), func(t Token) bool { return t.ID == id })
	}
}

// Calls says how often endpoint, such as "access/permissions", was asked.
func (f *Fake) Calls(endpoint string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[endpoint]
}

const apiPrefix = "/api2/json/"

// ServeHTTP answers the calls of the API under /api2/json.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	endpoint, ok := strings.CutPrefix(r.URL.Path, apiPrefix)
	if !ok || r.Method != http.MethodGet {
		answer(w, http.StatusNotImplemented, nil)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[endpoint]++
	if endpoint == "access/domains" {
		answer(w, http.StatusOK, f.domains())
		return
	}
	privs, guests, ok := f.principal(r)
	switch {
	case endpoint != "access/permissions" && endpoint != "cluster/resources":
		answer(w, http.StatusNotImplemented, nil)
	case !ok:
		answer(w, http.StatusUnauthorized, nil)
	case endpoint == "access/permissions":
		answer(w, http.StatusOK, permissions(privs, r.URL.Query().Get("path")))
	default:
		answer(w, http.StatusOK, f.resources(guests, r.URL.Query().Get("type")))
	}
}

// principal is what the request's ticket or token may do and see.
func (f *Fake) principal(r *http.Request) (map[string][]string, []string, bool) {
	if auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), "PVEAPIToken="); ok {
		return f.token(auth)
	}
	c, err := r.Cookie("PVEAuthCookie")
	if err != nil {
		return nil, nil, false
	}
	// pveproxy unescapes the cookie, which Proxmox VE's page sets escaped.
	ticket, err := url.PathUnescape(c.Value)
	if err != nil {
		return nil, nil, false
	}
	user, ok := ticketUser(ticket)
	if !ok {
		return nil, nil, false
	}
	u, ok := f.users[user]
	if !ok || !slices.Contains(u.Tickets, ticket) {
		return nil, nil, false
	}
	return u.Privileges, u.Guests, true
}

func (f *Fake) token(value string) (map[string][]string, []string, bool) {
	tokenID, secret, ok := strings.Cut(value, "=")
	if !ok {
		return nil, nil, false
	}
	user, id, ok := strings.Cut(tokenID, "!")
	if !ok {
		return nil, nil, false
	}
	u, ok := f.users[user]
	if !ok {
		return nil, nil, false
	}
	i := slices.IndexFunc(u.Tokens, func(t Token) bool { return t.ID == id && t.Secret == secret })
	if i < 0 {
		return nil, nil, false
	}
	t := u.Tokens[i]
	if !t.Privsep {
		return u.Privileges, u.Guests, true
	}
	privs := map[string][]string{}
	for p, given := range t.Privileges {
		held := privilegesAt(u.Privileges, p)
		privs[p] = slices.DeleteFunc(slices.Clone(given), func(priv string) bool { return !slices.Contains(held, priv) })
	}
	guests := slices.DeleteFunc(slices.Clone(t.Guests), func(g string) bool { return !slices.Contains(u.Guests, g) })
	return privs, guests, true
}

// privilegesAt are the privileges at p: those listed for it, or for the
// nearest path above it.
func privilegesAt(privs map[string][]string, p string) []string {
	for {
		if list, ok := privs[p]; ok {
			return list
		}
		if p == "/" {
			return nil
		}
		p = path.Dir(p)
	}
}

// permissions is the answer of access/permissions: every path, or the one
// asked for. Proxmox VE gives each privilege its propagate flag.
func permissions(privs map[string][]string, at string) map[string]map[string]int {
	out := map[string]map[string]int{}
	add := func(p string, list []string) {
		m := map[string]int{}
		for _, priv := range list {
			m[priv] = 1
		}
		out[p] = m
	}
	if at != "" {
		add(at, privilegesAt(privs, at))
		return out
	}
	for p, list := range privs {
		add(p, list)
	}
	return out
}

type resource struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Node   string `json:"node"`
	Status string `json:"status"`
}

// resources lists guests as cluster/resources does, for any type or "vm".
func (f *Fake) resources(guests []string, kind string) []resource {
	out := []resource{}
	if kind != "" && kind != "vm" {
		return out
	}
	for _, g := range guests {
		ref, _ := model.ParseGuestRef(g)
		out = append(out, resource{
			ID:     g,
			Type:   string(ref.Kind),
			VMID:   ref.VMID,
			Name:   fmt.Sprintf("guest-%d", ref.VMID),
			Node:   f.node,
			Status: "running",
		})
	}
	return out
}

type domain struct {
	Realm   string `json:"realm"`
	Type    string `json:"type"`
	Comment string `json:"comment,omitempty"`
}

// domains are the realms: pam and pve, which every cluster has, and those of
// the users.
func (f *Fake) domains() []domain {
	out := []domain{
		{Realm: "pam", Type: "pam", Comment: "Linux PAM standard authentication"},
		{Realm: "pve", Type: "pve", Comment: "Proxmox VE authentication server"},
	}
	var more []string
	for id := range f.users {
		_, realm, _ := strings.Cut(id, "@")
		if realm != "pam" && realm != "pve" && !slices.Contains(more, realm) {
			more = append(more, realm)
		}
	}
	slices.Sort(more)
	for _, realm := range more {
		out = append(out, domain{Realm: realm, Type: "ldap"})
	}
	return out
}

func answer(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// SelfSigned makes an ECDSA P-256 key and a self-signed certificate for names
// (DNS names or addresses), valid a year from now. It returns the pair for a
// server and the certificate as PEM, which a client pins.
func SelfSigned(now time.Time, names ...string) (tls.Certificate, []byte, error) {
	if len(names) == 0 {
		return tls.Certificate{}, nil, errors.New("a certificate needs a name")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "pvefake"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pair := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	return pair, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}
