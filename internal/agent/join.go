package agent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/clusterapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	// maxTokenFile bounds the file of a join token: a token is far shorter.
	maxTokenFile = 8 << 10
	joinTimeout  = 30 * time.Second
	// controllerFile keeps where the controller answers once the token that
	// said so is gone.
	controllerFile = "controller.json"
)

// ErrNoJoinToken is a join without a token on the volume.
var ErrNoJoinToken = errors.New("no join token on the volume")

// JoinRefusedError is a join that cannot succeed with the token it was made
// with: the controller refused it, or the token is not this container's or
// has expired. The token is gone; a new one is needed.
type JoinRefusedError struct{ Why string }

func (e *JoinRefusedError) Error() string {
	return "the join is refused for good: " + e.Why + "; pco cluster join-token on the controller makes a new token"
}

// JoinFile is where the installer pushes an agent's join token, on its state
// volume.
func JoinFile(p store.Paths) string { return filepath.Join(p.Local, "join.token") }

// ControllerRecord is where an appliance agent finds its controller: what
// its join token named, kept once the token is gone.
type ControllerRecord struct {
	InstallID  string   `json:"installId"`
	Endpoints  []string `json:"endpoints"`
	ServerName string   `json:"serverName"`
	CASHA256   string   `json:"caSHA256"`
}

// ReadController reads the record of the controller in dir; found is false
// before the agent joined.
func ReadController(dir string) (ControllerRecord, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, controllerFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ControllerRecord{}, false, nil
	case err != nil:
		return ControllerRecord{}, false, fmt.Errorf("reading the record of the controller: %w", err)
	}
	var r ControllerRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return ControllerRecord{}, false, fmt.Errorf("reading the record of the controller: %w", err)
	}
	return r, true, nil
}

func saveController(dir string, r ControllerRecord) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(dir, controllerFile), append(data, '\n'), atomicfile.Options{Mode: 0o600})
}

// Joiner enrols an appliance agent with the controller of its install, once,
// with the join token root pushed onto its volume: it makes a key on the
// volume, sends a certificate request with the token's secret and what it
// says of itself, and keeps the client certificate it gets. The controller is
// known by the CA fingerprint and the server name the token names; no other
// is sent the token. The token is removed as soon as the controller answers
// with a certificate or refuses it for good, and also when it cannot be this
// container's; it never shows in a log, an error or an event.
type Joiner struct {
	// TokenFile is the token's file, mode 0600 and owned by Owner (root).
	TokenFile string
	Owner     int
	// Files keep the certificate, and Dir the record of the controller.
	Files TLSFiles
	Dir   string

	InstallID string
	Node      string
	VMID      int
	MACs      []string
	Build     string

	Now  func() time.Time
	Rand io.Reader
	Log  zerolog.Logger
	// Dial reaches the controller; nil dials over TCP.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Join enrols the agent and returns its certificate. An error that is not a
// *JoinRefusedError keeps the token, for the next try.
func (j Joiner) Join(ctx context.Context) (Creds, error) {
	tok, err := j.readToken()
	if err != nil {
		return Creds{}, err
	}
	now := j.now()
	switch {
	case tok.InstallID != j.InstallID:
		return Creds{}, j.drop(fmt.Sprintf("the token is of install %s, this container joins install %s", tok.InstallID, j.InstallID))
	case tok.Node != j.Node:
		return Creds{}, j.drop(fmt.Sprintf("the token is for node %s, this container runs on node %s", tok.Node, j.Node))
	case !now.Before(tok.Expires):
		return Creds{}, j.drop("the token expired at " + tok.Expires.UTC().Format(time.RFC3339))
	}
	id := pki.Identity{Kind: pki.KindAppliance, InstallID: j.InstallID, Node: j.Node, VMID: j.VMID}
	key, err := pki.NewKey(j.rand())
	if err != nil {
		return Creds{}, err
	}
	csr, err := pki.NewCSR(key, id, j.rand())
	if err != nil {
		return Creds{}, err
	}
	body, err := json.Marshal(clusterapi.JoinRequest{
		Token: tok.Encode(), CSR: csr, Node: j.Node, VMID: j.VMID, MACs: j.MACs, Build: j.Build,
		Protocol: agentproto.Range{Min: agentproto.ProtocolMin, Max: agentproto.ProtocolMax},
	})
	if err != nil {
		return Creds{}, err
	}
	answer, status, err := j.post(ctx, tok, body)
	switch {
	case err != nil:
		return Creds{}, err
	case status == http.StatusForbidden:
		return Creds{}, j.drop("the controller refused the token; its log says why")
	case status != http.StatusOK:
		return Creds{}, fmt.Errorf("the controller answered the join with %d; it is tried again", status)
	}
	creds, err := joined(key, answer, id, tok.CASHA256)
	if err != nil {
		// The controller took the token, which is used now: a certificate
		// that does not hold is as good as a refusal.
		return Creds{}, j.drop(err.Error())
	}
	if err := j.Files.SaveClient(creds); err != nil {
		return Creds{}, err
	}
	if err := saveController(j.Dir, ControllerRecord{
		InstallID: j.InstallID, Endpoints: tok.Endpoints, ServerName: tok.ServerName, CASHA256: tok.CASHA256,
	}); err != nil {
		return Creds{}, err
	}
	if err := j.removeToken(); err != nil {
		return Creds{}, err
	}
	j.Log.Info().Str("serial", creds.Cert.SerialNumber.Text(16)).Str("sha256", pki.Fingerprint(creds.Cert)).
		Msg("joined the controller; an admin admits this node with the fingerprint")
	return creds, nil
}

// readToken reads the token file, refusing one that others could have
// written or read, and a link. A token that cannot be read is removed: it is
// of no use, and it may hold a secret.
func (j Joiner) readToken() (pki.JoinToken, error) {
	f, err := os.OpenFile(j.TokenFile, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return pki.JoinToken{}, ErrNoJoinToken
	}
	if err != nil {
		return pki.JoinToken{}, fmt.Errorf("reading the join token %s: %w", j.TokenFile, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return pki.JoinToken{}, fmt.Errorf("reading the join token %s: %w", j.TokenFile, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case !info.Mode().IsRegular():
		return pki.JoinToken{}, fmt.Errorf("the join token %s is not a regular file", j.TokenFile)
	case info.Mode().Perm() != 0o600:
		return pki.JoinToken{}, j.drop(fmt.Sprintf("the file %s has mode %04o, where a token is 0600", j.TokenFile, info.Mode().Perm()))
	case !ok || int(st.Uid) != j.Owner:
		return pki.JoinToken{}, j.drop(fmt.Sprintf("the file %s is not root's", j.TokenFile))
	}
	data, err := io.ReadAll(io.LimitReader(f, maxTokenFile+1))
	if err != nil {
		return pki.JoinToken{}, fmt.Errorf("reading the join token %s: %w", j.TokenFile, err)
	}
	if len(data) > maxTokenFile {
		return pki.JoinToken{}, j.drop("the file holds more than a token")
	}
	tok, err := pki.ParseJoinToken(string(data))
	if err != nil {
		// The parse error names fields, never the secret.
		return pki.JoinToken{}, j.drop("the file holds no join token: " + err.Error())
	}
	return tok, nil
}

// post sends the join to each endpoint of the token in turn, verifying the
// controller by the token's CA and server name before anything is sent.
func (j Joiner) post(ctx context.Context, tok pki.JoinToken, body []byte) (clusterapi.CertificateResponse, int, error) {
	dial := j.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: dialTimeout}).DialContext
	}
	var errs []error
	for _, addr := range tok.Endpoints {
		tr := &http.Transport{
			TLSClientConfig:   pki.ClientTLS(tls.Certificate{}, tok.CASHA256, tok.ServerName),
			ForceAttemptHTTP2: true,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dial(ctx, network, addr)
			},
			TLSHandshakeTimeout:    dialTimeout,
			MaxResponseHeaderBytes: 64 << 10,
		}
		answer, status, err := j.postTo(ctx, httpClient(tr), tok.ServerName, body)
		tr.CloseIdleConnections()
		if err == nil {
			return answer, status, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", addr, err))
	}
	return clusterapi.CertificateResponse{}, 0, fmt.Errorf("no controller took the join: %w", errors.Join(errs...))
}

func (j Joiner) postTo(ctx context.Context, hc *http.Client, server string, body []byte) (clusterapi.CertificateResponse, int, error) {
	ctx, cancel := context.WithTimeout(ctx, joinTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+server+"/v1/agent/join", bytes.NewReader(body))
	if err != nil {
		return clusterapi.CertificateResponse{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return clusterapi.CertificateResponse{}, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return clusterapi.CertificateResponse{}, resp.StatusCode, nil
	}
	var answer clusterapi.CertificateResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswer)).Decode(&answer); err != nil {
		return clusterapi.CertificateResponse{}, 0, fmt.Errorf("reading the certificate of the join: %w", err)
	}
	return answer, resp.StatusCode, nil
}

// joined checks the certificate of a join: of the key, of the identity asked
// for, under a CA whose fingerprint the token named.
func joined(key *ecdsa.PrivateKey, answer clusterapi.CertificateResponse, id pki.Identity, caSHA256 string) (Creds, error) {
	cert, err := parsePEMCert(answer.CertPEM)
	if err != nil {
		return Creds{}, fmt.Errorf("reading the certificate of the join: %w", err)
	}
	ca, err := parsePEMCert(answer.CAPEM)
	if err != nil {
		return Creds{}, fmt.Errorf("reading the CA of the join: %w", err)
	}
	got, err := pki.IdentityOf(cert)
	pub, isEC := cert.PublicKey.(*ecdsa.PublicKey)
	switch {
	case err != nil:
		return Creds{}, err
	case got != id:
		return Creds{}, fmt.Errorf("the controller issued a certificate of %s, not %s", got, id)
	case !isEC || !pub.Equal(&key.PublicKey):
		return Creds{}, errors.New("the controller issued a certificate for another key")
	case pki.Fingerprint(ca) != caSHA256:
		return Creds{}, errors.New("the controller answered with another CA than the token names")
	case cert.CheckSignatureFrom(ca) != nil:
		return Creds{}, errors.New("the controller issued a certificate that is not under its CA")
	}
	return Creds{Key: key, Cert: cert, CA: ca}, nil
}

// drop removes the token and returns why the join is refused for good.
func (j Joiner) drop(why string) error {
	if err := j.removeToken(); err != nil {
		return errors.Join(&JoinRefusedError{Why: why}, err)
	}
	j.Log.Warn().Msg("the join token was removed: " + why)
	return &JoinRefusedError{Why: why}
}

func (j Joiner) removeToken() error {
	if err := os.Remove(j.TokenFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the join token: %w", err)
	}
	return nil
}

func (j Joiner) now() time.Time {
	if j.Now == nil {
		return time.Now()
	}
	return j.Now()
}

func (j Joiner) rand() io.Reader {
	if j.Rand == nil {
		return rand.Reader
	}
	return j.Rand
}
