package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/bundle"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func (a *app) exportCmd() *cobra.Command { return a.exportCmdWith(defaultBundleEnv()) }

func (a *app) exportCmdWith(e bundleEnv) *cobra.Command {
	var credentials bool
	var passphraseFile string
	cmd := &cobra.Command{
		Use:   "export [--credentials] [--passphrase-file <path>] <file>",
		Short: "Write the install to an encrypted bundle, the backup to keep",
		Long: "Write the install to an encrypted bundle: its identity and the cluster it was made in, the\n" +
			"settings, manual routes, approvals and acknowledged segments, the registry of nodes, the CA\n" +
			"with its key, and the previous CA while a rotation runs, leader.json, and every other object\n" +
			"the controller writes: the networks and service addresses, the leases, consents and\n" +
			"attachments of the managed network, the claims, tombstones, placements and moves. With\n" +
			"--credentials the Cloudflare tokens are in it too. pco import restores the install from it on\n" +
			"another node or in another cluster, which is what a restore after a lost cluster or\n" +
			"controller, and the conversion of a node, need. Run tokens, bindings and the caches are left\n" +
			"out: the daemon builds them again.\n\n" +
			"It runs as root on the node and reads the store directly, while the daemon runs: nothing has to\n" +
			"be stopped. What it reads is the store at one moment; when the CA changed before the file was\n" +
			"written, as it does during a rotation, it refuses and is run again. On a host the store is\n" +
			"shared by the nodes of a cluster, and any node can export it.\n\n" +
			"The bundle is encrypted with AES-256-GCM under a key that Argon2id derives from a passphrase\n" +
			"of at least 16 characters, which makes a file that is attacked offline expensive. The\n" +
			"passphrase is read from the one line of --passphrase-file, which may be /dev/stdin for a\n" +
			"pipe, or asked for twice on a terminal without being shown. An empty answer has pco draw\n" +
			"one of 24 characters and show it once, after the file is written. It is never taken from an\n" +
			"argument, which others could read, and no log or error repeats it.\n\n" +
			"With its passphrase the bundle can impersonate the controller to every agent of the install,\n" +
			"as it holds the key of the CA that signs their certificates, and with --credentials it holds\n" +
			"the Cloudflare tokens: keep the passphrase apart from the file. The file is written for root\n" +
			"only, and an existing file is never replaced.",
		Example: "  # Back up the install; the passphrase is asked for twice\n" +
			"  pco export /root/pco.bundle\n\n" +
			"  # With the Cloudflare tokens, the passphrase read from a file\n" +
			"  pco export --credentials --passphrase-file /root/.pco-passphrase /root/pco.bundle",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			if e.euid() != 0 {
				return errors.New("pco export reads the secrets of the install: run it as root")
			}
			return a.export(cmd, e, args[0], credentials, passphraseFile)
		},
	}
	cmd.Flags().BoolVar(&credentials, "credentials", false, "carry the Cloudflare tokens in the bundle as well")
	cmd.Flags().StringVar(&passphraseFile, "passphrase-file", "", "read the passphrase from the one line of this file instead of asking for it")
	return cmd
}

var errNothingToExport = errors.New("there is no install to export: pco is not set up on this node")

func errExists(target string) error {
	return fmt.Errorf("%s already exists: pco export never replaces a file, remove it or name another", target)
}

func (a *app) export(cmd *cobra.Command, e bundleEnv, target string, credentials bool, passphraseFile string) error {
	// Refused before anything is asked; the write refuses again what appeared
	// in the meantime.
	switch _, err := os.Lstat(target); {
	case err == nil:
		return errExists(target)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if _, err := os.Stat(filepath.Dir(target)); err != nil {
		return err
	}
	paths, err := a.bundlePaths(e)
	if err != nil {
		return err
	}
	st, err := store.OpenExisting(paths)
	if err != nil {
		if errors.Is(err, store.ErrNoRoot) {
			return errNothingToExport
		}
		return fmt.Errorf("opening the store: %w", err)
	}
	c, err := bundle.Collect(cmd.Context(), st, credentials, a.now(), a.version)
	switch {
	case errors.Is(err, bundle.ErrNoInstall), errors.Is(err, store.ErrNoRoot):
		return errNothingToExport
	case err != nil:
		return err
	}
	pass, drawn, err := a.passphrase(cmd, e, passphraseQuestion{
		command: "pco export", file: passphraseFile, confirm: true, generate: true,
		prompt: "Passphrase of the bundle, at least 16 characters (empty to have one made): ",
	})
	if err != nil {
		return err
	}
	defer clear(pass)
	// The daemon runs meanwhile, and a rotation that moved the CA since it
	// was read would leave a bundle of a CA the agents are about to leave.
	switch same, err := bundle.SameCA(st, c); {
	case err != nil:
		return err
	case !same:
		return errors.New("the CA of the install changed while pco export ran, as it does during a rotation: run pco export again")
	}
	raw, err := bundle.Seal(c, pass, e.rand)
	if err != nil {
		return err
	}
	if err := writeNew(target, raw); err != nil {
		return err
	}
	s := &screen{w: cmd.OutOrStdout()}
	s.printf("Wrote %s.\n", target)
	printBundle(s, c)
	if !credentials {
		s.println("The Cloudflare credentials are not in it: --credentials adds them.")
	}
	if drawn {
		s.printf("\nPassphrase: %s\nIt is shown only now. Keep it apart from the file.\n", string(pass))
	}
	return s.done()
}

// writeNew writes raw to target, for root only, and never over a file: the
// text goes to a temporary file beside it, which is then linked to the name,
// and the link fails when something took the name in the meantime. The
// temporary name is removed either way.
func writeNew(target string, raw []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".*"+atomicfile.TempExt)
	if err != nil {
		return fmt.Errorf("writing the bundle: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("writing the bundle: %w", err)
	}
	switch err := os.Link(f.Name(), target); {
	case errors.Is(err, fs.ErrExist):
		return errExists(target)
	case err != nil:
		return fmt.Errorf("writing the bundle: %w", err)
	}
	return nil
}
