package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/daemon"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// doctorEnv is what pco doctor takes from the node when the daemon is not
// running: the checks that need no daemon are made on the node itself, as
// root.
type doctorEnv struct {
	euid func() int
	node doctor.LocalEnv
}

// defaultDoctorEnv is the machine as it is: a Proxmox node, or the container of
// an appliance, which the profile marker says when the doctor looks.
func (a *app) defaultDoctorEnv() doctorEnv {
	return doctorEnv{
		euid: os.Geteuid,
		node: localNode{
			HostEnv: &doctor.HostEnv{Systemd: connector.NewSystemctl(), Clock: time.Now, StoreCheck: a.storeReady},
			egress:  defaultEgressEnv(),
			app:     a,
		},
	}
}

// onAppliance says whether this machine is the container of an appliance; a
// marker that cannot be read is no appliance here, as for pco status.
func (a *app) onAppliance() bool {
	profile, err := store.DetectProfile(a.profileFile)
	return err == nil && profile == store.ProfileAppliance
}

// volumeCheck is the check of the state volume of an appliance.
func (a *app) volumeCheck() func(path, marker string) error {
	if a.daemon.Appliance.Volume != nil {
		return a.daemon.Appliance.Volume
	}
	return appliance.VolumeMounted
}

// storeReady says whether the store of this machine is set up: that of the
// node, or in an appliance the volume with its marker, as the daemon checks it
// at its start, and the store on it.
func (a *app) storeReady() error {
	if !a.onAppliance() {
		return storeReadyAt(store.DefaultPaths())
	}
	if err := a.volumeCheck()(store.ApplianceLocal, store.VolumeMarker); err != nil {
		return err
	}
	paths, err := store.PathsFor(store.ProfileAppliance)
	if err != nil {
		return err
	}
	return storeReadyAt(paths)
}

// storeReadyAt says whether the store at p is mounted and set up. It only
// looks: the doctor changes nothing on the node, so the store is not made
// when it is missing.
func storeReadyAt(p store.Paths) error {
	st, err := store.OpenExisting(p)
	if err != nil {
		return fmt.Errorf("opening the store: %w", err)
	}
	return daemon.StoreReady(st)()
}

// localNode is the node as the doctor reads it without the daemon.
type localNode struct {
	*doctor.HostEnv
	egress egressEnv
	app    *app
}

// RepairFix is what puts the store of an appliance back, and empty on a host,
// whose store is the cluster filesystem.
func (n localNode) RepairFix() string {
	if !n.app.onAppliance() {
		return ""
	}
	return doctor.RepairFix(n.app.daemon.Appliance.System.VMIDHint(store.ApplianceLocal))
}

// Egress finds the egress filter as pco egress show does: switched off, its
// table not loaded, not as pco loads it, or on.
func (n localNode) Egress(ctx context.Context) (engine.EgressView, error) {
	since, off, err := egress.NewOverrides(n.egress.local).Off()
	switch {
	case err != nil:
		return engine.EgressView{}, err
	case off:
		return engine.EgressView{State: engine.EgressOff, Since: since}, nil
	}
	uid, err := n.egress.uid()
	if err != nil {
		return engine.EgressView{}, err
	}
	live, err := egress.ReadLive(ctx, n.egress.nft, uid)
	switch {
	case errors.Is(err, egress.ErrNotLoaded):
		return engine.EgressView{State: engine.EgressNotLoaded}, nil
	case errors.Is(err, egress.ErrUnreadable):
		return engine.EgressView{State: engine.EgressChanged}, nil
	case err != nil:
		return engine.EgressView{}, err
	case len(live.Differences) > 0:
		return engine.EgressView{State: engine.EgressChanged}, nil
	}
	return engine.EgressView{State: engine.EgressOn}, nil
}

func (a *app) doctorCmd() *cobra.Command { return a.doctorCmdWith(a.defaultDoctorEnv()) }

func (a *app) doctorCmdWith(d doctorEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the installation",
		Long: "Check what pco needs: the mode, the last cycle, the inventory, the credentials,\n" +
			"cloudflared, the tunnels and their connectors, the way out to Cloudflare, the writer, the\n" +
			"records in the way, Proxmox, the store and the lock of the node, what waits for a\n" +
			"confirmation and the guests that wait for approval. In the appliance it also checks what\n" +
			"the appliance depends on: its container and its volume, its identity, the token and the\n" +
			"access control around it, the way out and the egress filter as the user of the connectors,\n" +
			"DNS and the clock, the held packages and the versions, and the disk, the journal and the\n" +
			"memory. Each finding comes with what to do about it, and the exit status is 1 when a check\n" +
			"fails.\n\n" +
			"When the daemon is not running, root still gets the checks that need no daemon: the units\n" +
			"of pco, the store, cloudflared and the egress table. The rest is said not to have been made,\n" +
			"and the exit status is 1 when one of these fails, as it is when pco.service does not run.\n" +
			"It changes nothing.\n\n" + jsonHelp + "\n\n" +
			"It asks the daemon through its socket, which answers only root and pco-web, the user of the\n" +
			"web interface. The exit status is 2 when the daemon could not be asked, unless root got the\n" +
			"checks that need no daemon instead.",
		Example: "  # Check the installation\n" +
			"  pco doctor\n\n" +
			"  # The findings as JSON, for a script or a monitor\n" +
			"  pco doctor --json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			raw, err := a.client().DoctorRaw(ctx)
			if apiclient.NotRunning(err) && d.euid() == 0 {
				raw, err = json.Marshal(doctor.RunLocal(ctx, d.node))
			}
			if err != nil {
				return a.explain(ctx, err)
			}
			var findings []doctor.Finding
			decodeErr := json.Unmarshal(raw, &findings)
			if a.json {
				if err := printJSON(cmd.OutOrStdout(), raw); err != nil {
					return err
				}
			}
			if decodeErr != nil {
				return couldNotAsk{fmt.Errorf("decoding the answer of the daemon: %w", decodeErr)}
			}
			if !a.json {
				if err := renderFindings(cmd.OutOrStdout(), findings); err != nil {
					return err
				}
			}
			if doctor.Failed(findings) {
				return errReported
			}
			return nil
		},
	}
}

// renderFindings writes a finding a line, with what to do about it on the
// line below, and a summary.
func renderFindings(w io.Writer, findings []doctor.Finding) error {
	s := &screen{w: w}
	lines := make([]markedLine, len(findings))
	fails, warns := 0, 0
	for i, f := range findings {
		lines[i] = markedLine{level: f.Level, name: f.Check, detail: f.Detail, fix: f.Fix}
		switch f.Level {
		case doctor.LevelFail:
			fails++
		case doctor.LevelWarn:
			warns++
		}
	}
	renderMarked(s, lines)
	s.printf("\n%s\n", summary(fails, warns))
	return s.done()
}

func summary(fails, warns int) string {
	switch {
	case fails > 0:
		return fmt.Sprintf("%s, %s.", counted(fails, "failure"), counted(warns, "warning"))
	case warns > 0:
		return counted(warns, "warning") + ", no failure."
	}
	return "Everything is in order."
}

// counted says "no warning", "1 warning" or "2 warnings".
func counted(n int, what string) string {
	switch n {
	case 0:
		return "no " + what
	case 1:
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}
