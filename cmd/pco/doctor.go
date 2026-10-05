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

func defaultDoctorEnv() doctorEnv {
	return doctorEnv{
		euid: os.Geteuid,
		node: localNode{
			HostEnv: &doctor.HostEnv{Systemd: connector.NewSystemctl(), Clock: time.Now, StoreCheck: nodeStoreReady},
			egress:  defaultEgressEnv(),
		},
	}
}

// nodeStoreReady opens the store of the node and says whether it is set up.
func nodeStoreReady() error {
	st, err := store.Open(store.DefaultPaths())
	if err != nil {
		return fmt.Errorf("opening the store: %w", err)
	}
	return daemon.StoreReady(st)()
}

// localNode is the node as the doctor reads it without the daemon.
type localNode struct {
	*doctor.HostEnv
	egress egressEnv
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

func (a *app) doctorCmd() *cobra.Command { return a.doctorCmdWith(defaultDoctorEnv()) }

func (a *app) doctorCmdWith(d doctorEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the installation",
		Long: "Check what pco needs: the mode, the last cycle, the inventory, the credentials,\n" +
			"cloudflared, the tunnels and their connectors, the way out to Cloudflare, the writer, the\n" +
			"records in the way, Proxmox, the store and the lock of the node, what waits for a\n" +
			"confirmation and the guests that wait for approval. Each finding comes with what to do\n" +
			"about it, and the exit status is 1 when a check fails.\n\n" +
			"When the daemon is not running, root still gets the checks that need no daemon: the units\n" +
			"of pco, the store, cloudflared and the egress table. The rest is said not to have been made,\n" +
			"and the exit status is 1 when one of these fails, as it is when pco.service does not run.\n\n" + jsonHelp,
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
