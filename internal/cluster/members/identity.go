package members

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
)

// CAFingerprint is the SHA-256 of the DER of the first certificate in the
// file, normally RootCAFile, as lower-case hex. Proxmox makes that certificate
// once for a cluster, or for a node that stands alone, and does not rotate
// it, which makes it what tells one cluster from another.
func CAFingerprint(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			return "", fmt.Errorf("%s holds no certificate", path)
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return "", fmt.Errorf("%s: the first certificate: %w", path, err)
		}
		sum := sha256.Sum256(block.Bytes)
		return hex.EncodeToString(sum[:]), nil
	}
}
