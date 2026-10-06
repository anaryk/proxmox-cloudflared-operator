package upgrade

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// ErrSignature is the error of a signature that does not hold.
var ErrSignature = errors.New("the signature of checksums.txt is not valid for the release key, or the key was revoked or has expired")

var fingerprintRe = regexp.MustCompile(`^[0-9A-Fa-f]{40}([0-9A-Fa-f]{24})?$`)

// Verifier checks a detached signature against a keyring and returns the
// fingerprint of the key that made it.
type Verifier interface {
	Verify(ctx context.Context, keyring, sig, file string) (fingerprint string, err error) // sqv, then gpgv
}

// NewVerifier returns the Verifier of the installer: sqv where it is
// installed, as on Debian 13, and gpgv where it is not.
func NewVerifier(run setup.Runner) Verifier { return toolVerifier{run} }

type toolVerifier struct{ run setup.Runner }

func (v toolVerifier) Verify(ctx context.Context, keyring, sig, file string) (string, error) {
	fpr, err := v.sqv(ctx, keyring, sig, file)
	if !errors.Is(err, setup.ErrCommandNotFound) {
		return fpr, err
	}
	fpr, err = v.gpgv(ctx, keyring, sig, file)
	if errors.Is(err, setup.ErrCommandNotFound) {
		return "", errors.New("neither sqv nor gpgv is installed to check the signature of the release: apt-get install sqv")
	}
	return fpr, err
}

// sqv prints the fingerprint of the signing key and exits 0 only for a good
// signature by a key of the keyring that is valid.
func (v toolVerifier) sqv(ctx context.Context, keyring, sig, file string) (string, error) {
	out, err := v.run.Run(ctx, "sqv", "--keyring", keyring, sig, file)
	if errors.Is(err, setup.ErrCommandNotFound) {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("%w (%w)", ErrSignature, err)
	}
	for line := range strings.Lines(out) {
		if line = strings.TrimSpace(line); fingerprintRe.MatchString(line) {
			return strings.ToUpper(line), nil
		}
	}
	return "", fmt.Errorf("%w: sqv accepted it but named no key", ErrSignature)
}

// gpgv exits 0 for a signature by a key that is revoked or has expired and
// says so only in its status lines, which are read for that reason. Given
// --keyring it does not fall back on the keys of a home directory.
func (v toolVerifier) gpgv(ctx context.Context, keyring, sig, file string) (string, error) {
	out, err := v.run.Run(ctx, "gpgv", "--status-fd", "1", "--keyring", keyring, sig, file)
	if errors.Is(err, setup.ErrCommandNotFound) {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("%w (%w)", ErrSignature, err)
	}
	good, fpr := false, ""
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "[GNUPG:]" {
			continue
		}
		switch fields[1] {
		case "BADSIG", "ERRSIG", "EXPSIG", "EXPKEYSIG", "REVKEYSIG":
			return "", fmt.Errorf("%w (gpgv: %s)", ErrSignature, fields[1])
		case "GOODSIG":
			good = true
		case "VALIDSIG":
			// The last field is the fingerprint of the primary key, which is
			// not the first when a subkey signed.
			if fpr == "" && len(fields) >= 3 {
				fpr = fields[2]
				if len(fields) >= 12 {
					fpr = fields[11]
				}
			}
		}
	}
	if !good {
		return "", ErrSignature
	}
	if !fingerprintRe.MatchString(fpr) {
		return "", fmt.Errorf("%w: gpgv accepted it but named no key", ErrSignature)
	}
	return strings.ToUpper(fpr), nil
}
