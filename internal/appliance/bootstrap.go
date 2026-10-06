package appliance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The modes of a bootstrap.
const (
	// ModeInstall makes a new install; refused when the store holds one.
	ModeInstall = "install"
	// ModeRepair keeps the install and the writer, replaces the Proxmox
	// token and records the VMID, the MACs, the endpoints and the node
	// addresses anew.
	ModeRepair = "repair"
	// ModeRecover adopts the install the Cloudflare token sees, for a volume
	// that holds none.
	ModeRecover = "recover"
)

const (
	// BootstrapSchema is the version of the bootstrap this binary reads.
	BootstrapSchema = 1
	// CAFile is the cluster CA the installer pushes, below the local root.
	CAFile = "pve-ca.pem"

	maxBootstrap = 64 << 10
)

var (
	nodeName  = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
	pveTokens = regexp.MustCompile(`^[^\s@!]+@[^\s@!]+![A-Za-z][A-Za-z0-9._-]+$`)
	installID = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

// Bootstrap is the file the installer pushes into the container and pco
// appliance init turns into the store: /var/lib/pco/bootstrap.json, mode
// 0600, at most 64 KiB, schema version 1. Its secrets print as [redacted].
type Bootstrap struct {
	Mode string
	// InstallID chooses the install to adopt in ModeRecover, and names the
	// one to repair in ModeRepair; empty otherwise.
	InstallID string
	VMID      int
	Node      string
	MACs      []string // in normal form
	Endpoints []store.Endpoint
	// NodeAddrs are the global IPv4 addresses of the node as the installer
	// read them on it; every mode adds them to the saved node addresses.
	NodeAddrs       []netip.Addr
	PVEToken        store.PVEToken
	CloudflareToken store.Secret // needed in ModeRecover; empty: none
	GateTag         string
	// Manifest is the installer's manifest, stored as manifest.json.
	Manifest json.RawMessage

	path string // the file it was read from
}

// bootstrapFile is a bootstrap as the file holds it, secrets in the clear.
type bootstrapFile struct {
	SchemaVersion   int              `json:"schemaVersion"`
	Mode            string           `json:"mode"`
	InstallID       string           `json:"installId"`
	VMID            int              `json:"vmid"`
	Node            string           `json:"node"`
	MACs            []string         `json:"macs"`
	Endpoints       []store.Endpoint `json:"endpoints"`
	NodeAddrs       []netip.Addr     `json:"nodeAddrs"`
	PVEToken        bootstrapToken   `json:"pveToken"`
	CloudflareToken string           `json:"cloudflareToken"`
	GateTag         string           `json:"gateTag"`
	Manifest        json.RawMessage  `json:"manifest"`
}

type bootstrapToken struct {
	TokenID string `json:"tokenId"`
	Secret  string `json:"secret"`
}

// ReadBootstrap reads and validates the file; it refuses a file that is not
// mode 0600 and owned by root, and one above 64 KiB. A link is not followed.
func ReadBootstrap(path string) (Bootstrap, error) { return readBootstrap(path, 0) }

func readBootstrap(path string, owner int) (Bootstrap, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return Bootstrap{}, fmt.Errorf("reading the bootstrap: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return Bootstrap{}, fmt.Errorf("reading the bootstrap: %w", err)
	}
	if err := checkBootstrapFile(path, info, owner); err != nil {
		return Bootstrap{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBootstrap+1))
	if err != nil {
		return Bootstrap{}, fmt.Errorf("reading the bootstrap %s: %w", path, err)
	}
	if len(data) > maxBootstrap {
		return Bootstrap{}, fmt.Errorf("the bootstrap %s is larger than 64 KiB", path)
	}
	b, err := decodeBootstrap(data)
	if err != nil {
		return Bootstrap{}, fmt.Errorf("the bootstrap %s: %w", path, err)
	}
	b.path = path
	return b, nil
}

// checkBootstrapFile refuses a file that others could have written or read.
func checkBootstrapFile(path string, info fs.FileInfo, owner int) error {
	switch {
	case !info.Mode().IsRegular():
		return fmt.Errorf("the bootstrap %s is not a regular file", path)
	case info.Mode().Perm() != 0o600:
		return fmt.Errorf("the bootstrap %s has mode %04o: want 0600", path, info.Mode().Perm())
	case info.Size() > maxBootstrap:
		return fmt.Errorf("the bootstrap %s is larger than 64 KiB", path)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("the owner of the bootstrap %s cannot be read", path)
	}
	if int(st.Uid) != owner {
		return fmt.Errorf("the bootstrap %s is owned by uid %d: want uid %d", path, st.Uid, owner)
	}
	return nil
}

// decodeBootstrap reads one JSON object with the fields of the schema and no
// others. A decode error names fields, never their values, so no secret is
// in it.
func decodeBootstrap(data []byte) (Bootstrap, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f bootstrapFile
	if err := dec.Decode(&f); err != nil {
		return Bootstrap{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Bootstrap{}, errors.New("more than one JSON object")
	}
	return f.bootstrap()
}

func (f bootstrapFile) bootstrap() (Bootstrap, error) {
	if f.SchemaVersion != BootstrapSchema {
		return Bootstrap{}, fmt.Errorf("schemaVersion %d: want %d", f.SchemaVersion, BootstrapSchema)
	}
	b := Bootstrap{
		Mode: f.Mode, InstallID: f.InstallID, VMID: f.VMID, Node: f.Node, Endpoints: f.Endpoints,
		PVEToken:        store.PVEToken{TokenID: f.PVEToken.TokenID, Secret: store.NewSecret(f.PVEToken.Secret)},
		CloudflareToken: store.NewSecret(f.CloudflareToken),
		GateTag:         f.GateTag,
		Manifest:        f.Manifest,
	}
	for _, check := range []func(*Bootstrap, bootstrapFile) error{checkMode, checkGuest, checkMACs, checkEndpoints, checkNodeAddrs, checkSecrets, checkRest} {
		if err := check(&b, f); err != nil {
			return Bootstrap{}, err
		}
	}
	return b, nil
}

func checkMode(b *Bootstrap, _ bootstrapFile) error {
	switch b.Mode {
	case ModeInstall, ModeRepair, ModeRecover:
	default:
		return fmt.Errorf("mode %q: want install, repair or recover", b.Mode)
	}
	switch {
	case b.InstallID == "":
	case !installID.MatchString(b.InstallID):
		return fmt.Errorf("installId %q: want 12 lower-case hex characters", b.InstallID)
	case b.Mode == ModeInstall:
		return errors.New("installId goes with mode repair or recover; mode install makes a new one")
	}
	return nil
}

func checkGuest(b *Bootstrap, _ bootstrapFile) error {
	if b.VMID < 100 || b.VMID > 999999999 {
		return fmt.Errorf("vmid %d: want 100 to 999999999", b.VMID)
	}
	if !nodeName.MatchString(b.Node) {
		return fmt.Errorf("node %q: want the name of a Proxmox VE node", b.Node)
	}
	return nil
}

func checkMACs(b *Bootstrap, f bootstrapFile) error {
	if len(f.MACs) == 0 {
		return errors.New("no MAC of the container")
	}
	for _, m := range f.MACs {
		n, err := model.NormalizeMAC(m)
		if err != nil {
			return fmt.Errorf("MAC %q: want one as bc:24:11:00:aa:b5", m)
		}
		b.MACs = append(b.MACs, n)
	}
	return nil
}

// checkEndpoints refuses an endpoint the appliance must not dial: one that
// is not host:port, has no server name, or is a loopback address, which in the
// container is the container.
func checkEndpoints(b *Bootstrap, _ bootstrapFile) error {
	if len(b.Endpoints) == 0 {
		return errors.New("no endpoint of the Proxmox API")
	}
	for _, e := range b.Endpoints {
		host, port, err := net.SplitHostPort(e.Address)
		if n, perr := strconv.ParseUint(port, 10, 16); err != nil || host == "" || perr != nil || n == 0 {
			return fmt.Errorf("endpoint address %q: want host:port", e.Address)
		}
		if a, err := netip.ParseAddr(host); (err == nil && a.Unmap().IsLoopback()) || strings.EqualFold(host, "localhost") {
			return fmt.Errorf("endpoint %s is a loopback address: want the node's address", e.Address)
		}
		if e.ServerName == "" {
			return fmt.Errorf("endpoint %s has no server name", e.Address)
		}
	}
	return nil
}

func checkNodeAddrs(b *Bootstrap, f bootstrapFile) error {
	if len(f.NodeAddrs) == 0 {
		return errors.New("no address of the node in nodeAddrs")
	}
	for _, a := range f.NodeAddrs {
		if !a.IsValid() || a.IsUnspecified() || a.Unmap().IsLoopback() {
			return fmt.Errorf("node address %q: want an address of the node", a)
		}
		b.NodeAddrs = append(b.NodeAddrs, a.Unmap())
	}
	return nil
}

func checkSecrets(b *Bootstrap, f bootstrapFile) error {
	switch {
	case !pveTokens.MatchString(f.PVEToken.TokenID):
		return fmt.Errorf("pveToken.tokenId %q: want user@realm!name", f.PVEToken.TokenID)
	case f.PVEToken.Secret == "":
		return errors.New("pveToken.secret is empty")
	case strings.ContainsFunc(f.PVEToken.Secret, isSpace):
		return errors.New("pveToken.secret holds white space")
	case strings.ContainsFunc(f.CloudflareToken, isSpace):
		return errors.New("cloudflareToken holds white space")
	case b.Mode == ModeRecover && f.CloudflareToken == "":
		return errors.New("mode recover needs cloudflareToken, to find the install with")
	}
	return nil
}

func isSpace(r rune) bool { return strings.ContainsRune(" \t\r\n\v\f", r) }

func checkRest(b *Bootstrap, _ bootstrapFile) error {
	if b.GateTag == "" {
		return errors.New("gateTag is empty")
	}
	var object map[string]json.RawMessage
	if len(b.Manifest) == 0 || json.Unmarshal(b.Manifest, &object) != nil || object == nil {
		return errors.New("manifest: want a JSON object")
	}
	return nil
}

// Encode returns the file of the bootstrap, secrets in the clear, as the
// installer writes it. A bootstrap ReadBootstrap would refuse is refused.
func (b Bootstrap) Encode() ([]byte, error) {
	f := bootstrapFile{
		SchemaVersion: BootstrapSchema, Mode: b.Mode, InstallID: b.InstallID, VMID: b.VMID, Node: b.Node,
		MACs: b.MACs, Endpoints: b.Endpoints, NodeAddrs: b.NodeAddrs,
		PVEToken:        bootstrapToken{TokenID: b.PVEToken.TokenID, Secret: b.PVEToken.Secret.Reveal()},
		CloudflareToken: b.CloudflareToken.Reveal(),
		GateTag:         b.GateTag,
		Manifest:        b.Manifest,
	}
	data, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("encoding the bootstrap: %w", err)
	}
	if _, err := decodeBootstrap(data); err != nil {
		return nil, fmt.Errorf("the bootstrap: %w", err)
	}
	return data, nil
}

// Remove removes the file the bootstrap was read from; one that is gone
// already is no error.
func (b Bootstrap) Remove() error {
	if b.path == "" {
		return nil
	}
	if err := os.Remove(b.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the bootstrap: %w", err)
	}
	return nil
}
