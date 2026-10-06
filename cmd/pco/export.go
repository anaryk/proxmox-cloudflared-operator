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
		Long: "Write the install to an encrypted bundle: its identity, settings, manual routes, approvals,\n" +
			"acknowledged segments, the registry of nodes, the CA with its key and leader.json, and with\n" +
			"--credentials the Cloudflare tokens too. pco import restores the install from it on another\n" +
			"node or in another cluster, which is what a restore after a lost cluster or controller, and\n" +
			"the conversion of a node, need. Run tokens, bindings and the caches are left out: the daemon\n" +
			"builds them again.\n\n" +
			"The bundle is encrypted with AES-256-GCM under a key that Argon2id derives from a passphrase\n" +
			"of at least 16 characters, which makes a file that is attacked offline expensive. The\n" +
			"passphrase is read from the one line of --passphrase-file, which may be /dev/stdin for a\n" +
			"pipe, or asked for twice on a terminal without being shown. An empty answer has pco draw\n" +
			"one of 24 characters and show it once, after the file is written. It is never taken from an\n" +
			"argument, which others could read, and no log or error repeats it.\n\n" +
			"With its passphrase the bundle can impersonate the controller to every agent of the install,\n" +
			"as it holds the key of the CA that signs their certificates, and with --credentials it holds\n" +
			"the Cloudflare tokens: keep the passphrase apart from the file. The file is written for root\n" +
			"only, and an existing file is never replaced.\n\n" +
			"It runs as root on the node and reads the store directly, so it needs no daemon. On a host\n" +
			"the store is shared by the nodes of a cluster, and any node can export it.",
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

func (a *app) export(cmd *cobra.Command, e bundleEnv, target string, credentials bool, passphraseFile string) error {
	switch _, err := os.Lstat(target); {
	case err == nil:
		return fmt.Errorf("%s already exists: pco export never replaces a file, remove it or name another", target)
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
	c, err := bundle.Collect(st, credentials, a.now(), a.version)
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
	raw, err := bundle.Seal(c, pass, e.rand)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(target, raw, atomicfile.Options{Mode: 0o600}); err != nil {
		return fmt.Errorf("writing the bundle: %w", err)
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
