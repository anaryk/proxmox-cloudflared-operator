package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// applianceCmd is the group of the commands of the appliance: those run on
// the node that installs, repairs and removes it, and those run inside it.
func (a *app) applianceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "appliance",
		Short: "Commands of the pco appliance",
		Long: "Commands of the pco appliance, the container that runs pco and its connectors on a\n" +
			"Proxmox VE node in place of the host install.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(a.applianceInitCmd(), a.applianceRecoverCmd())
	return cmd
}

func (a *app) applianceInitCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Make the store of this appliance from the bootstrap the installer pushed",
		Long: "Make the store of this appliance from the bootstrap the installer on the node pushed into\n" +
			"it, and remove the bootstrap, which holds the secrets. pco appliance install and repair run\n" +
			"it; it runs as root inside the appliance. A bootstrap made for another container, by its\n" +
			"MACs or the VMID its state volume names, changes nothing, and is removed. In mode install\n" +
			"the daemon must not run, and a bootstrap turned away for that stays for the next run; in\n" +
			"modes repair and recover pco.service is stopped first. Every mode restarts pco.service at\n" +
			"the end. A step that fails is named, and an init of the same mode finishes what it left.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			if err := a.insideAppliance(cmd, "pco setup sets pco up"); err != nil {
				return err
			}
			return a.applianceInit(cmd, path)
		},
	}
	cmd.Flags().StringVar(&path, "bootstrap", "", "the bootstrap the installer pushed, as /var/lib/pco/bootstrap.json")
	_ = cmd.MarkFlagRequired("bootstrap")
	return cmd
}

func (a *app) applianceRecoverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "recover",
		Short: "Draw a writer epoch above the last write at Cloudflare, after a rollback or a restore",
		Long: "Draw a new writer epoch for this appliance, above the highest generation the sentinels of\n" +
			"its tunnels at Cloudflare carry and above the stored one, after a snapshot rollback or a\n" +
			"restore put an older state on its volume. It reads Cloudflare with the stored credentials,\n" +
			"so one must be stored (pco credential add). pco.service is stopped while it runs and\n" +
			"started after; the install only observes until pco apply. It runs as root inside the\n" +
			"appliance; on a host, pco setup --recover adopts an install after its store was lost.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			if err := a.insideAppliance(cmd, "pco setup --recover adopts an install after its store was lost"); err != nil {
				return err
			}
			return a.applianceRecover(cmd)
		},
	}
}

// insideAppliance refuses a command of the appliance on another machine, and
// one that does not run as root. elsewhere says what to run on a host.
func (a *app) insideAppliance(cmd *cobra.Command, elsewhere string) error {
	profile, err := store.DetectProfile(a.profileFile)
	if err != nil {
		return err
	}
	if profile != store.ProfileAppliance {
		return fmt.Errorf("%s runs inside the appliance; on a host, %s", cmd.CommandPath(), elsewhere)
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("%s changes the store of the appliance: run it as root", cmd.CommandPath())
	}
	return nil
}

// applianceVolume returns the roots of the store on the state volume, which
// must be mounted with its marker.
func (a *app) applianceVolume() (store.Paths, error) {
	paths, err := store.PathsFor(store.ProfileAppliance)
	if err != nil {
		return store.Paths{}, err
	}
	deps := a.daemon.Appliance
	volume := deps.Volume
	if volume == nil {
		volume = appliance.VolumeMounted
	}
	if err := volume(paths.Local, store.VolumeMarker); err != nil {
		return store.Paths{}, errors.New(appliance.VolumeLine(paths.Local, err, deps.System.VMIDHint(paths.Local)))
	}
	return paths, nil
}

func applianceDaemon(paths store.Paths) appliance.Daemon {
	run := setup.NewHostRunner()
	return appliance.Daemon{
		Lock: paths.NodeLock(),
		Systemctl: func(ctx context.Context, args ...string) (string, error) {
			return run.Run(ctx, "systemctl", args...)
		},
	}
}

func (a *app) applianceInit(cmd *cobra.Command, path string) error {
	ctx := cmd.Context()
	out, errOut := &screen{w: cmd.OutOrStdout()}, &screen{w: cmd.ErrOrStderr()}
	override, err := a.cloudflareOverride()
	if err != nil {
		return err
	}
	if override != "" {
		errOut.printf("warning: %s\n", overrideLine(override))
	}
	paths, err := a.applianceVolume()
	if err != nil {
		return err
	}
	b, err := appliance.ReadBootstrap(path)
	if err != nil {
		return err
	}
	deps := a.initDeps(override, out)
	d := applianceDaemon(paths)
	stopped, err := d.ReadyForInit(ctx, b, paths.Local, deps)
	if err == nil {
		err = a.runInit(ctx, paths, b, deps)
	}
	if err != nil && stopped {
		startAgain(ctx, d, errOut)
	}
	if err != nil {
		return err
	}
	return out.done()
}

// runInit runs Init, which removes the bootstrap, on the store of the volume.
func (a *app) runInit(ctx context.Context, paths store.Paths, b appliance.Bootstrap, deps appliance.InitDeps) error {
	st, err := store.Open(paths)
	if err != nil {
		return fmt.Errorf("opening the store: %w", err)
	}
	return appliance.Init(ctx, st, b, deps)
}

// initDeps are what Init works with, the Cloudflare API at override when it
// is not empty.
func (a *app) initDeps(override string, out *screen) appliance.InitDeps {
	sys := a.daemon.Appliance.System
	return appliance.InitDeps{
		CheckToken:     appliance.CheckToken,
		NewClient:      cloudflareClients(override),
		Systemd:        connector.NewSystemctl(),
		Now:            a.now,
		Rand:           rand.Reader,
		Incarnation:    sys.Incarnation,
		Links:          sys.Links,
		MountSource:    sys.MountSource,
		RecoverInstall: setup.RecoverInstall,
		Info:           func(format string, args ...any) { out.printf(format+"\n", args...) },
	}
}

// startAgain starts pco.service again after a failure, as it ran before.
func startAgain(ctx context.Context, d appliance.Daemon, errOut *screen) {
	if err := d.Start(ctx); err != nil {
		errOut.printf("warning: %s was running before and could not be started again: %v\n", appliance.Unit, err)
		return
	}
	errOut.printf("warning: %s was running before and is started again\n", appliance.Unit)
}

func (a *app) applianceRecover(cmd *cobra.Command) error {
	ctx := cmd.Context()
	out, errOut := &screen{w: cmd.OutOrStdout()}, &screen{w: cmd.ErrOrStderr()}
	override, err := a.cloudflareOverride()
	if err != nil {
		return err
	}
	if override != "" {
		errOut.printf("warning: %s\n", overrideLine(override))
	}
	paths, err := a.applianceVolume()
	if err != nil {
		return err
	}
	st, err := store.Open(paths)
	if err != nil {
		return fmt.Errorf("opening the store: %w", err)
	}
	// The daemon adds a credential; it is not stopped for nothing.
	switch creds, err := st.Credentials(); {
	case err != nil:
		return fmt.Errorf("reading the credentials: %w", err)
	case len(creds) == 0:
		return appliance.ErrNoCredential
	}
	d := applianceDaemon(paths)
	stopped, err := d.Stop(ctx)
	if err != nil {
		if stopped {
			startAgain(ctx, d, errOut)
		}
		return err
	}
	newClient := cloudflareClients(override)
	sys := a.daemon.Appliance.System
	w, err := appliance.Recover(ctx, st, appliance.RecoverDeps{
		NewClient:      func(c store.Credential) (cfapi.API, error) { return newClient(c.Token.Reveal()) },
		RecoverInstall: setup.RecoverInstall,
		Incarnation:    sys.Incarnation,
		Now:            a.now,
		Rand:           rand.Reader,
	})
	if err != nil {
		if stopped {
			startAgain(ctx, d, errOut)
		}
		return err
	}
	out.printf("writer: install %s, generation %d, drawn for this start of the container; it only observes until pco apply\n",
		w.InstallID, w.Generation)
	if err := d.Start(ctx); err != nil {
		return err
	}
	out.printf("%s: started\n", appliance.Unit)
	return out.done()
}
