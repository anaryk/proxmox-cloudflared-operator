package pve

import (
	"bytes"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// DefaultNodeCertDir is where a node keeps the certificates pveproxy serves.
const DefaultNodeCertDir = "/etc/pve/local"

// nodeCertFiles are the certificates pveproxy serves on port 8006, in the
// order it picks them: one the admin installed, when there is one, and else
// the one the cluster issued, which names 127.0.0.1 and ::1.
var nodeCertFiles = []string{"pveproxy-ssl.pem", "pve-ssl.pem"}

// nodePin verifies that what answers on a loopback address is pveproxy of
// this node: it must present, byte for byte, the certificate the node
// serves. Port 8006 is no privileged port, so while pveproxy is down any
// local user could listen there and would be handed the token otherwise.
type nodePin struct {
	dir string

	mu   sync.Mutex
	file string // where der was read from
	der  []byte // the certificate of the node, as last read
}

// verify is the check of the handshake. A certificate that does not match the
// one read before has the file read again, as the admin may have replaced
// it, before it is refused.
func (p *nodePin) verify(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("the Proxmox API on the loopback address presented no certificate")
	}
	leaf := cs.PeerCertificates[0].Raw
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.der != nil && bytes.Equal(p.der, leaf) {
		return nil
	}
	file, der, err := readNodeCert(p.dir)
	if err != nil {
		return err
	}
	p.file, p.der = file, der
	if !bytes.Equal(der, leaf) {
		return fmt.Errorf("the Proxmox API on the loopback address does not present the certificate of this node in %s", file)
	}
	return nil
}

// readNodeCert returns the certificate pveproxy serves, and the file it is in.
func readNodeCert(dir string) (string, []byte, error) {
	var path string
	for _, name := range nodeCertFiles {
		path = filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return path, nil, fmt.Errorf("reading the certificate of this node: %w", err)
		}
		for rest := data; ; {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			switch {
			case block == nil:
				return path, nil, fmt.Errorf("%s holds no certificate", path)
			case block.Type == "CERTIFICATE":
				return path, block.Bytes, nil
			}
		}
	}
	return path, nil, fmt.Errorf("the certificate of this node is not there: %s does not exist", path)
}
