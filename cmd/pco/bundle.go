package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/bundle"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	// maxBundleSize is more than any bundle needs: an install holds files of
	// at most 1 MiB each, and pmxcfs takes no more.
	maxBundleSize = 64 << 20
	// maxPassphraseFile is more than a passphrase on one line needs.
	maxPassphraseFile = 4 << 10
)

// bundleEnv is what pco export and pco import take from the machine. They
// work on its store directly, as root.
type bundleEnv struct {
	euid func() int
	rand io.Reader
	// paths are the roots of the store; nil takes those of the profile of
	// the machine.
	paths func() (store.Paths, error)
}

func defaultBundleEnv() bundleEnv { return bundleEnv{euid: os.Geteuid, rand: rand.Reader} }

// bundlePaths returns the roots of the store of this machine.
func (a *app) bundlePaths(e bundleEnv) (store.Paths, error) {
	if e.paths != nil {
		return e.paths()
	}
	profile, err := store.DetectProfile(a.profileFile)
	if err != nil {
		return store.Paths{}, err
	}
	if profile == store.ProfileAppliance {
		return a.applianceVolume()
	}
	return store.DefaultPaths(), nil
}

// passphraseQuestion is how a command gets the passphrase of a bundle.
type passphraseQuestion struct {
	command string // "pco export"
	file    string // --passphrase-file
	prompt  string
	// confirm asks twice, and generate takes an empty answer to mean that
	// pco draws one.
	confirm, generate bool
}

// passphrase returns the passphrase of a bundle: the line of the file, or what
// is typed at the terminal without being shown, and whether pco drew it. It is
// never taken from an argument, and no error repeats it.
func (a *app) passphrase(cmd *cobra.Command, e bundleEnv, q passphraseQuestion) (pass []byte, drawn bool, err error) {
	if q.file != "" {
		pass, err = readPassphraseFile(cmd, q.file)
		return pass, false, err
	}
	fd, terminal := a.stdinTerminal(cmd.InOrStdin())
	if !terminal {
		return nil, false, fmt.Errorf("there is no passphrase: pass --passphrase-file, or run %s on a terminal", q.command)
	}
	if pass, err = a.promptPassphrase(cmd, fd, q.prompt); err != nil {
		return nil, false, err
	}
	switch {
	case len(pass) == 0 && q.generate:
		made, err := bundle.Generate(e.rand)
		return []byte(made), true, err
	case q.confirm:
		again, err := a.promptPassphrase(cmd, fd, "Passphrase again: ")
		defer clear(again)
		if err != nil {
			clear(pass)
			return nil, false, err
		}
		if !bytes.Equal(pass, again) {
			clear(pass)
			return nil, false, errors.New("the two entries of the passphrase differ")
		}
	}
	return pass, false, nil
}

func (a *app) promptPassphrase(cmd *cobra.Command, fd int, prompt string) ([]byte, error) {
	s := &screen{w: cmd.ErrOrStderr()}
	s.printf("%s", prompt)
	if err := s.done(); err != nil {
		return nil, err
	}
	raw, err := a.readPassword(fd)
	// The newline the admin typed was not echoed.
	s.println("")
	return raw, err
}

// readPassphraseFile reads the passphrase from a file that holds it on one
// line, and warns when others can read the file.
func readPassphraseFile(cmd *cobra.Command, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// A pipe or a device has no mode of its own to speak of.
	if info, err := f.Stat(); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o077 != 0 {
		s := &screen{w: cmd.ErrOrStderr()}
		s.printf("warning: %s can be read by others; restrict it with chmod 600\n", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxPassphraseFile+1))
	defer clear(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(raw) > maxPassphraseFile {
		return nil, fmt.Errorf("%s is too long for a passphrase", path)
	}
	line := bytes.TrimRight(raw, "\r\n")
	switch {
	case len(line) == 0:
		return nil, fmt.Errorf("%s holds no passphrase", path)
	case bytes.ContainsAny(line, "\r\n"):
		return nil, fmt.Errorf("%s holds more than one line: the passphrase is the one line", path)
	}
	return bytes.Clone(line), nil
}

// printBundle writes what a bundle holds.
func printBundle(s *screen, c bundle.Contents) {
	t := s.table()
	t.row("install", c.InstallID)
	t.row("manual routes", fmt.Sprint(len(c.ManualRoutes)))
	t.row("approvals", fmt.Sprint(len(c.Approvals)))
	t.row("acknowledged segments", fmt.Sprint(len(c.Segments)+len(c.NodeSegments)))
	t.row("nodes", fmt.Sprint(len(c.Nodes)))
	t.row("CA and its key", caHeld(c))
	t.row("controller objects", controllerObjects(c))
	t.row("Cloudflare credentials", credentialsHeld(c))
	t.flush()
}

func caHeld(c bundle.Contents) string {
	switch {
	case c.CA == nil:
		return "no"
	case c.PreviousCA != nil:
		return "yes, with the previous CA of a rotation"
	}
	return "yes"
}

// controllerObjects counts what the controller wrote besides the decisions
// of the admin, leaving out what the bundle has none of.
func controllerObjects(c bundle.Contents) string {
	var tombstones map[string]json.RawMessage
	_ = json.Unmarshal(c.Tombstones, &tombstones)
	var parts []string
	for _, p := range []struct {
		n                int
		singular, plural string
	}{
		{len(c.Networks), "network", "networks"},
		{len(c.ServiceAddrs) + len(c.NodeServiceAddrs), "service address", "service addresses"},
		{len(c.Leases), "lease", "leases"},
		{len(c.Consents), "consent", "consents"},
		{len(c.Attachments), "attachment", "attachments"},
		{len(c.Claims), "claim", "claims"},
		{len(tombstones), "tombstone", "tombstones"},
		{len(c.Placements), "placement", "placements"},
		{len(c.Moves), "move", "moves"},
	} {
		switch {
		case p.n == 1:
			parts = append(parts, "1 "+p.singular)
		case p.n > 1:
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.plural))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func credentialsHeld(c bundle.Contents) string {
	if len(c.Credentials) == 0 {
		return "none"
	}
	return fmt.Sprint(len(c.Credentials))
}
