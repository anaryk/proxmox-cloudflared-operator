package upgrade

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

const (
	fprPrimary = "0123456789ABCDEF0123456789ABCDEF01234567"
	fprSubkey  = "89ABCDEF0123456789ABCDEF0123456789ABCDEF"
)

func goodGPGV() string {
	return "[GNUPG:] NEWSIG\n" +
		"[GNUPG:] GOODSIG 0123456789ABCDEF Release Test\n" +
		"[GNUPG:] VALIDSIG " + fprSubkey + " 2026-10-01 1790000000 0 4 0 22 10 00 " + fprPrimary + "\n"
}

func TestSqvChecksTheSignatureWhenItIsThere(t *testing.T) {
	host := &fakeRunner{do: func(name string, _ ...string) (string, error) {
		require.Equal(t, "sqv", name)
		return "warning: something\n" + fprPrimary + "\n", nil
	}}

	fpr, err := NewVerifier(host).Verify(t.Context(), "/k.gpg", "/w/checksums.txt.sig", "/w/checksums.txt")

	require.NoError(t, err)
	require.Equal(t, fprPrimary, fpr)
	require.Equal(t, []string{"sqv --keyring /k.gpg /w/checksums.txt.sig /w/checksums.txt"}, host.lines())
}

func TestGpgvChecksTheSignatureWithoutSqv(t *testing.T) {
	host := &fakeRunner{do: func(name string, _ ...string) (string, error) {
		if name == "sqv" {
			return "", fmt.Errorf("sqv: %w", setup.ErrCommandNotFound)
		}
		return goodGPGV(), nil
	}}

	fpr, err := NewVerifier(host).Verify(t.Context(), "/k.gpg", "/w/checksums.txt.sig", "/w/checksums.txt")

	require.NoError(t, err)
	require.Equal(t, fprPrimary, fpr, "the key, not the subkey that signed")
	require.Equal(t, []string{
		"sqv --keyring /k.gpg /w/checksums.txt.sig /w/checksums.txt",
		"gpgv --status-fd 1 --keyring /k.gpg /w/checksums.txt.sig /w/checksums.txt",
	}, host.lines())
}

func TestASignatureThatIsNotGoodIsRefused(t *testing.T) {
	missing := func(name string) error {
		if name == "sqv" {
			return fmt.Errorf("sqv: %w", setup.ErrCommandNotFound)
		}
		return nil
	}
	for _, tt := range []struct {
		name string
		do   func(name string, args ...string) (string, error)
	}{
		{"sqv refuses it", func(string, ...string) (string, error) {
			return "", errors.New("sqv: exit status 1: Error verifying signature")
		}},
		{"sqv names no key", func(string, ...string) (string, error) { return "ok\n", nil }},
		{"gpgv refuses it", func(name string, _ ...string) (string, error) {
			if err := missing(name); err != nil {
				return "", err
			}
			return "[GNUPG:] BADSIG 0123 Test\n", errors.New("gpgv: exit status 1")
		}},
		{"gpgv says the key expired", func(name string, _ ...string) (string, error) {
			if err := missing(name); err != nil {
				return "", err
			}
			return "[GNUPG:] EXPKEYSIG 0123 Test\n" + goodGPGV(), nil
		}},
		{"gpgv says the key was revoked", func(name string, _ ...string) (string, error) {
			if err := missing(name); err != nil {
				return "", err
			}
			return "[GNUPG:] REVKEYSIG 0123 Test\n" + goodGPGV(), nil
		}},
		{"gpgv does not call it good", func(name string, _ ...string) (string, error) {
			if err := missing(name); err != nil {
				return "", err
			}
			return "[GNUPG:] VALIDSIG " + fprPrimary + "\n", nil
		}},
		{"gpgv names no key", func(name string, _ ...string) (string, error) {
			if err := missing(name); err != nil {
				return "", err
			}
			return "[GNUPG:] GOODSIG 0123 Test\n", nil
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host := &fakeRunner{do: tt.do}
			_, err := NewVerifier(host).Verify(t.Context(), "/k.gpg", "/w/s.sig", "/w/s")
			require.ErrorIs(t, err, ErrSignature)
		})
	}
}

// A signature by a key the keyring does not hold is told apart from one that
// does not hold: the keyring may be the wrong one.
func TestASignatureByAKeyOutsideTheKeyringIsToldApart(t *testing.T) {
	for _, tt := range []struct {
		name string
		do   func(name string, args ...string) (string, error)
	}{
		{"sqv", func(string, ...string) (string, error) {
			return "", errors.New("sqv: exit status 1: Missing key 0123456789ABCDEF0123456789ABCDEF01234567, which is needed to verify signature.")
		}},
		{"gpgv", func(name string, _ ...string) (string, error) {
			if name == "sqv" {
				return "", fmt.Errorf("sqv: %w", setup.ErrCommandNotFound)
			}
			return "[GNUPG:] NEWSIG\n[GNUPG:] ERRSIG 0123456789ABCDEF 22 10 00 1790000000 9 -\n[GNUPG:] NO_PUBKEY 0123456789ABCDEF\n",
				errors.New("gpgv: exit status 2")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewVerifier(&fakeRunner{do: tt.do}).Verify(t.Context(), "/k.gpg", "/w/s.sig", "/w/s")
			require.ErrorIs(t, err, ErrUnknownKey)
			require.NotErrorIs(t, err, ErrSignature)
		})
	}
}

func TestWithoutAVerifierNothingIsChecked(t *testing.T) {
	host := &fakeRunner{do: func(name string, _ ...string) (string, error) {
		return "", fmt.Errorf("%s: %w", name, setup.ErrCommandNotFound)
	}}

	_, err := NewVerifier(host).Verify(t.Context(), "/k.gpg", "/w/s.sig", "/w/s")

	require.EqualError(t, err, "neither sqv nor gpgv is installed to check the signature of the release: apt-get install sqv")
}

// pathRunner runs commands from the PATH of the test, for the tools of the
// real signature checks, which may live outside the fixed PATH of the host
// runner. hide makes a command missing.
type pathRunner struct{ hide string }

func (p pathRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == p.hide {
		return "", fmt.Errorf("%s: %w", name, setup.ErrCommandNotFound)
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, setup.ErrCommandNotFound)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(errOut.String()))
	}
	return out.String(), nil
}

// The verifier with the real sqv and gpgv, on checksums signed by gpg with a
// key of the test. With PCO_REQUIRE_CRYPTO_TESTS=1 a missing tool fails.
func TestTheRealToolsCheckTheSignature(t *testing.T) {
	need := func(tool string) bool {
		if _, err := exec.LookPath(tool); err == nil {
			return true
		}
		if os.Getenv("PCO_REQUIRE_CRYPTO_TESTS") == "1" {
			t.Fatalf("PCO_REQUIRE_CRYPTO_TESTS=1 but %s is not installed", tool)
		}
		return false
	}
	if !need("gpg") {
		t.Skip("gpg is not installed")
	}
	// A short path: the socket of gpg-agent must fit into the limit of the system.
	home, err := os.MkdirTemp("/tmp", "pco-up-gpg.")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = exec.Command("gpgconf", "--homedir", home, "--kill", "all").Run()
		_ = os.RemoveAll(home)
	})
	require.NoError(t, os.Chmod(home, 0o700))
	gpg := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("gpg", append([]string{"--homedir", home, "--batch", "--quiet", "--passphrase", ""}, args...)...)
		out, err := cmd.Output()
		require.NoError(t, err, "gpg %v", args)
		return string(out)
	}
	key := func(email string) string {
		gpg("--quick-generate-key", "Test <"+email+">", "ed25519", "sign", "never")
		for line := range strings.Lines(gpg("--with-colons", "--fingerprint", email)) {
			if f := strings.Split(line, ":"); f[0] == "fpr" {
				return f[9]
			}
		}
		t.Fatal("no fingerprint")
		return ""
	}
	dir := t.TempDir()
	good, other := key("release@example.invalid"), key("other@example.invalid")
	sums := filepath.Join(dir, "checksums.txt")
	require.NoError(t, os.WriteFile(sums, []byte(strings.Repeat("a", 64)+"  pco_1.2.4_amd64.deb\n"), 0o600))
	sign := func(fpr, out string) {
		gpg("--yes", "--local-user", fpr, "--detach-sign", "--output", out, sums)
	}
	sign(good, filepath.Join(dir, "good.sig"))
	sign(other, filepath.Join(dir, "other.sig"))
	keyring := filepath.Join(dir, "release.gpg")
	require.NoError(t, os.WriteFile(keyring, []byte(gpg("--export", good)), 0o600))

	for _, tool := range []string{"sqv", "gpgv"} {
		t.Run(tool, func(t *testing.T) {
			if !need(tool) {
				t.Skip(tool + " is not installed")
			}
			hide := "sqv"
			if tool == "sqv" {
				hide = "gpgv"
			}
			v := NewVerifier(pathRunner{hide: hide})

			fpr, err := v.Verify(t.Context(), keyring, filepath.Join(dir, "good.sig"), sums)
			require.NoError(t, err)
			require.Equal(t, good, fpr)

			_, err = v.Verify(t.Context(), keyring, filepath.Join(dir, "other.sig"), sums)
			require.ErrorIs(t, err, ErrUnknownKey, "a signature by another key")

			tampered := filepath.Join(dir, "tampered.txt")
			require.NoError(t, os.WriteFile(tampered, []byte(strings.Repeat("b", 64)+"  pco_1.2.4_amd64.deb\n"), 0o600))
			_, err = v.Verify(t.Context(), keyring, filepath.Join(dir, "good.sig"), tampered)
			require.ErrorIs(t, err, ErrSignature, "checksums that were changed")
		})
	}
}
