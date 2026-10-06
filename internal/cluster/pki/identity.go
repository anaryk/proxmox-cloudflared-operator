package pki

import (
	"crypto/x509"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Kind is what a certificate's holder is: the agent of a host node, an agent
// container of the appliance, or the controller.
type Kind string

const (
	KindHost       Kind = "host"
	KindAppliance  Kind = "appliance"
	KindController Kind = "controller"
)

const uriScheme = "pco"

var (
	installIDForm = regexp.MustCompile(`^[0-9a-f]{12}$`)
	nodeNameForm  = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
)

// Identity is who a client certificate speaks for. Its URI, the one URI SAN
// of the certificate, is pco:<kind>:<install>:<node>, followed by :<vmid> for
// an appliance agent.
type Identity struct {
	Kind      Kind
	InstallID string
	Node      string
	VMID      int // appliance only
}

// URI is the identity as the certificate carries it.
func (id Identity) URI() *url.URL {
	opaque := string(id.Kind) + ":" + id.InstallID + ":" + id.Node
	if id.VMID != 0 {
		opaque += ":" + strconv.Itoa(id.VMID)
	}
	return &url.URL{Scheme: uriScheme, Opaque: opaque}
}

func (id Identity) String() string { return id.URI().String() }

func (id Identity) check() error {
	switch id.Kind {
	case KindHost, KindAppliance, KindController:
	default:
		return fmt.Errorf("unknown kind %q, want host, appliance or controller", string(id.Kind))
	}
	if err := checkInstallID(id.InstallID); err != nil {
		return err
	}
	if err := checkNode(id.Node); err != nil {
		return err
	}
	switch {
	case id.Kind == KindAppliance && (id.VMID < 100 || id.VMID > 999999999):
		return fmt.Errorf("an appliance agent's vmid must be 100 to 999999999, not %d", id.VMID)
	case id.Kind != KindAppliance && id.VMID != 0:
		return fmt.Errorf("a vmid names only an appliance agent, not a %s", id.Kind)
	}
	return nil
}

func checkInstallID(s string) error {
	if !installIDForm.MatchString(s) {
		return fmt.Errorf("install id %q is not 12 lower-case hexadecimal digits", s)
	}
	return nil
}

func checkNode(s string) error {
	if !nodeNameForm.MatchString(s) {
		return fmt.Errorf("node name %q is not a host name", s)
	}
	return nil
}

// IdentityOf reads the identity of a certificate pco issued. A certificate
// must carry exactly one URI, the identity in its canonical form, so that no
// two readers can take it for two holders.
func IdentityOf(cert *x509.Certificate) (Identity, error) {
	return identityOfURIs("the certificate", cert.URIs)
}

// identityOfURIs reads the identity from the URIs of what, a certificate or
// a certificate request.
func identityOfURIs(what string, uris []*url.URL) (Identity, error) {
	switch len(uris) {
	case 0:
		return Identity{}, fmt.Errorf("%s carries no pco identity", what)
	case 1:
	default:
		return Identity{}, fmt.Errorf("%s carries %d URIs, where pco puts one", what, len(uris))
	}
	u := uris[0].String()
	parts := strings.Split(uris[0].Opaque, ":")
	if uris[0].Scheme != uriScheme || len(parts) < 3 || len(parts) > 4 {
		return Identity{}, fmt.Errorf("%s names %s, which is not a pco identity", what, u)
	}
	id := Identity{Kind: Kind(parts[0]), InstallID: parts[1], Node: parts[2]}
	if len(parts) == 4 {
		vmid, err := strconv.Atoi(parts[3])
		if err != nil {
			return Identity{}, fmt.Errorf("%s names %s, whose vmid is not a number", what, u)
		}
		id.VMID = vmid
	}
	if err := id.check(); err != nil {
		return Identity{}, fmt.Errorf("%s names %s: %w", what, u, err)
	}
	if id.String() != u {
		return Identity{}, fmt.Errorf("%s names %s, which is not in its canonical form %s", what, u, id)
	}
	return id, nil
}
