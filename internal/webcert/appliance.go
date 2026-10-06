package webcert

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
)

// The appliance keeps the certificate of its web interface in Dir as a node
// does, with a key of its own and a self-signed certificate the daemon makes
// at its first start, and two files more.
const (
	// ModeSelfSigned is the appliance's own certificate, which the daemon
	// renews; ModeOwn the admin's, which it leaves alone.
	ModeSelfSigned = "self-signed"
	// ModeName records the mode in the appliance, where no setup manifest
	// does.
	ModeName = "mode"
	// APIName is the node's API as the appliance's daemon reaches it, which
	// pco-web signs users in at; the daemon writes it from the install, and
	// the unit loads it as a credential.
	APIName = "pve-api.json"
	// Net0File holds the address of the appliance's net0, its management
	// card, as pco appliance install wrote it. pco-web listens there and
	// nowhere else: the other cards are legs into the networks of guests.
	Net0File = "/etc/pco/net0"
)

// API is the content of APIName: where the node's API answers, the name its
// certificate is verified under, the CA besides the system roots, and the node.
type API struct {
	URL        string `json:"url"` // https://<address>:8006/api2/json
	ServerName string `json:"serverName"`
	CA         string `json:"ca"` // PEM
	Node       string `json:"node"`
}

// ReadAPI reads and checks an APIName file.
func ReadAPI(path string) (API, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return API{}, err
	}
	var a API
	if err := json.Unmarshal(b, &a); err != nil {
		return API{}, fmt.Errorf("reading %s: %w", path, err)
	}
	if !strings.HasPrefix(a.URL, "https://") || a.ServerName == "" || !strings.Contains(a.CA, "-----BEGIN CERTIFICATE-----") {
		return API{}, fmt.Errorf("%s does not name the API, its server name and its CA", path)
	}
	return a, nil
}

// Encode is the content of an APIName file.
func (a API) Encode() []byte {
	b, _ := json.MarshalIndent(a, "", "  ")
	return append(b, '\n')
}

// ReadNet0 reads the address of net0 from path.
func ReadNet0(path string) (netip.Addr, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return netip.Addr{}, err
	}
	a, err := netip.ParseAddr(strings.TrimSpace(string(b)))
	if err != nil || a.IsUnspecified() {
		return netip.Addr{}, fmt.Errorf("%s holds no address of net0", path)
	}
	return a.Unmap(), nil
}

// ErrNotNet0 is a listen address of the appliance other than net0's.
var ErrNotNet0 = errors.New("pco-web must listen on net0's address only")

// CheckListen says whether listen is net0's address: never an unspecified
// one, such as 0.0.0.0 or ::, and never another card's, through which guests
// on a leg would reach the sign-in page.
func CheckListen(listen string, net0 netip.Addr) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	switch {
	case err != nil:
		return fmt.Errorf("%w: %q is no address", ErrNotNet0, listen)
	case a.Unmap() != net0:
		return fmt.Errorf("%w (%s), not %s", ErrNotNet0, net0, listen)
	}
	return nil
}

// ApplianceNames are the names of the appliance's certificate: its host name,
// its FQDN, the address it listens on and those of PCO_WEB_HOSTS.
func ApplianceNames(hostname, fqdn, listen string, hosts []string) (Names, error) {
	short, _, _ := strings.Cut(hostname, ".")
	return NodeNames(short, fqdn, nil, listen, hosts)
}

// ApplianceListen is the address the appliance's pco-web listens on:
// PCO_WEB_LISTEN, else net0's address on port 8643, as pco web takes it;
// empty when neither is known.
func ApplianceListen(env Env, net0File string) string {
	if env.Listen != "" {
		return env.Listen
	}
	if net0, err := ReadNet0(net0File); err == nil {
		return net.JoinHostPort(net0.String(), Port)
	}
	return ""
}

// ApplianceNamesFrom are the names of the appliance's certificate as its
// environment file and net0's file say them now.
func ApplianceNamesFrom(hostname, fqdn, envFile, net0File string) (Names, error) {
	env, _, err := ReadEnv(envFile)
	if err != nil {
		return Names{}, fmt.Errorf("reading %s: %w", envFile, err)
	}
	return ApplianceNames(hostname, fqdn, ApplianceListen(env, net0File), env.Hosts)
}

// ReadMode reads the mode recorded in dir; ModeSelfSigned when none is, as
// the daemon made what is there.
func ReadMode(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, ModeName))
	if errors.Is(err, fs.ErrNotExist) {
		return ModeSelfSigned, nil
	}
	if err != nil {
		return "", err
	}
	switch mode := strings.TrimSpace(string(b)); mode {
	case ModeSelfSigned, ModeOwn:
		return mode, nil
	default:
		return "", fmt.Errorf("%s says %q: want %s or %s", filepath.Join(dir, ModeName), mode, ModeSelfSigned, ModeOwn)
	}
}

// WriteMode records mode in dir.
func WriteMode(dir, mode string) error {
	return atomicfile.Write(filepath.Join(dir, ModeName), []byte(mode+"\n"), atomicfile.Options{Mode: 0o644})
}

// NoCertificate is why a certificate is made where there was none.
const NoCertificate = "there is none"

// SelfSignedDue says why the appliance's own certificate in dir must be made
// anew: there is none, it cannot be read, its key is not in tls.key, or it
// expires within 30 days. Empty when it need not.
func SelfSignedDue(dir string, now time.Time) string {
	if info, err := os.Lstat(filepath.Join(dir, CertName)); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return CertName + " is a link, not a certificate of its own"
	}
	certPEM, keyPEM, err := ReadPair(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return NoCertificate
	case err != nil:
		return "it could not be read"
	}
	leaf, err := ParseCert(certPEM)
	if err != nil {
		return "tls.crt holds no certificate"
	}
	if key, err := ParseKey(keyPEM); err != nil || !Matches(leaf, key) {
		return "tls.key is not its key"
	}
	if leaf.NotAfter.Sub(now) < RenewBefore {
		return "it expires at " + leaf.NotAfter.UTC().Format(time.RFC3339) + ", in less than 30 days"
	}
	return ""
}

// MakeSelfSigned writes a new key and a self-signed certificate for names into
// dir, valid 397 days, and records the mode.
func MakeSelfSigned(dir string, names Names, now time.Time, rand io.Reader) (*x509.Certificate, error) {
	certPEM, keyPEM, err := SelfSigned(names, now, SelfSignedLifetime, rand)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := WriteAtomic(dir, certPEM, keyPEM); err != nil {
		return nil, err
	}
	if err := WriteMode(dir, ModeSelfSigned); err != nil {
		return nil, err
	}
	return ParseCert(certPEM)
}

// ImportOwn checks the admin's certificate and key against names and puts them
// in dir. The mode is recorded first: should the swap not finish, the daemon
// leaves alone what is there rather than replace the admin's pair.
func ImportOwn(dir string, certPEM, keyPEM []byte, names Names, now time.Time) (*x509.Certificate, error) {
	if err := Check(certPEM, keyPEM, names, now); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := WriteMode(dir, ModeOwn); err != nil {
		return nil, err
	}
	if err := WriteAtomic(dir, certPEM, keyPEM); err != nil {
		return nil, err
	}
	return ParseCert(certPEM)
}
