package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/bundle"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func (a *app) importCmd() *cobra.Command { return a.importCmdWith(defaultBundleEnv()) }

func (a *app) importCmdWith(e bundleEnv) *cobra.Command {
	var passphraseFile string
	cmd := &cobra.Command{
		Use:   "import [--passphrase-file <path>] <file>",
		Short: "Restore the install of an encrypted bundle on this node",
		Long: "Restore the install of a bundle that pco export wrote, on a node where pco has none: its\n" +
			"identity, settings, manual routes, approvals, acknowledged segments, the registry of nodes,\n" +
			"the CA with its key and the previous CA of a rotation, leader.json, the networks, service\n" +
			"addresses, leases, consents, attachments, claims, tombstones, placements and moves, and the\n" +
			"Cloudflare credentials the bundle carries. It runs as root, with the daemon stopped, and\n" +
			"works on the store directly. A store that holds an install is refused and left as it is,\n" +
			"and so is a bundle that does not open or read: a restore that stops half way stores no\n" +
			"install, and is run again.\n\n" +
			"The passphrase is read from the one line of --passphrase-file, or asked for on a terminal\n" +
			"without being shown; it is never taken from an argument. A wrong passphrase and a file that\n" +
			"was changed cannot be told apart.\n\n" +
			"The install is stored in observe-only mode whatever it was: it publishes nothing until pco\n" +
			"apply, and the day in which the agent endpoint takes a certificate the store has no enrolment\n" +
			"for begins with the restore, whenever the exported install began to observe. What the\n" +
			"controller learns between its runs, the engine memory, is in no bundle: it is learnt again\n" +
			"in the observe-only window, before anything is removed. A bundle of an earlier shape carries\n" +
			"no pool of service addresses; the pool then starts above the addresses in use, and an\n" +
			"address that had been released may be given out again.\n\n" +
			"leader.json comes back as it was, so the controller writes nothing until pco promote\n" +
			"draws an epoch above the one of the install that was lost. The install remembers the cluster\n" +
			"it was made in: restored in another, the daemon writes nothing until pco promote makes that\n" +
			"cluster its writer, and the nodes of the lost cluster are admitted again after it.\n\n" +
			"Run it before pco setup on the node that is to run the controller, which keeps the install it\n" +
			"finds, and then run pco promote. It restores the install of a host: the install of an\n" +
			"appliance records the container it runs in, which a bundle does not carry.",
		Example: "  # Restore the install on a node where pco is not set up yet\n" +
			"  pco import /root/pco.bundle\n\n" +
			"  # With the passphrase read from a file\n" +
			"  pco import --passphrase-file /root/.pco-passphrase /root/pco.bundle",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			if e.euid() != 0 {
				return errors.New("pco import writes the store of the node: run it as root")
			}
			return a.importBundle(cmd, e, args[0], passphraseFile)
		},
	}
	cmd.Flags().StringVar(&passphraseFile, "passphrase-file", "", "read the passphrase from the one line of this file instead of asking for it")
	return cmd
}

func (a *app) importBundle(cmd *cobra.Command, e bundleEnv, file, passphraseFile string) error {
	profile, err := store.DetectProfile(a.profileFile)
	if err != nil {
		return err
	}
	if profile != store.ProfileHost {
		return errors.New("pco import restores the install of a host: the install of an appliance records the container it runs in, which a bundle does not carry")
	}
	paths, err := a.bundlePaths(e)
	if err != nil {
		return err
	}
	daemon := appliance.Daemon{Lock: paths.NodeLock()}
	if running, err := daemon.Running(); err != nil {
		return err
	} else if running {
		return fmt.Errorf("a pco daemon runs on this node (it holds %s): stop it first, with systemctl stop pco", paths.NodeLock())
	}
	// A store that holds an install is refused before a passphrase is asked
	// for. What cannot be read here is left to the restore, which reads it again.
	if existing, err := store.OpenExisting(paths); err == nil {
		if err := bundle.NoInstall(existing); errors.Is(err, bundle.ErrInstalled) {
			return err
		}
	}
	raw, err := readBundle(file)
	if err != nil {
		return err
	}
	pass, _, err := a.passphrase(cmd, e, passphraseQuestion{
		command: "pco import", file: passphraseFile, prompt: "Passphrase of the bundle: ",
	})
	if err != nil {
		return err
	}
	defer clear(pass)
	c, err := bundle.Open(raw, pass)
	if err != nil {
		return err
	}
	st, err := store.Open(paths)
	if err != nil {
		return fmt.Errorf("opening the store: %w", err)
	}
	if err := st.Init(); err != nil {
		return fmt.Errorf("making the roots of the store: %w", err)
	}
	defer c.ClearCredentials()
	if err := bundle.Restore(cmd.Context(), st, c); err != nil {
		return err
	}
	s := &screen{w: cmd.OutOrStdout()}
	s.printf("Restored the install from %s.\n", file)
	printBundle(s, c)
	s.println("pco only observes until pco apply.")
	s.println("Next: run pco setup if this node has none, then run pco promote.")
	return s.done()
}

// readBundle reads a bundle file, which is at most maxBundleSize.
func readBundle(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err == nil && info.Size() > maxBundleSize {
		return nil, fmt.Errorf("%s is too large to be a pco bundle", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxBundleSize+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(raw) > maxBundleSize {
		return nil, fmt.Errorf("%s is too large to be a pco bundle", path)
	}
	return raw, nil
}
