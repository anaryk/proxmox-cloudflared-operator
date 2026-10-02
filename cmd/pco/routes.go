package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

func (a *app) routesCmd() *cobra.Command {
	var state string
	cmd := &cobra.Command{
		Use:   "routes",
		Short: "List the routes and how each fares",
		Long: "List the routes sorted by hostname. The level is how strongly the address of the target\n" +
			"was proven to be the guest's: port, observed, or manual for a route to an address. The note\n" +
			"is the reason a route is not served, or its first warning.\n\n" +
			"With --json the whole state of the daemon is printed as the daemon sent it, re-indented,\n" +
			"with control and bidirectional characters escaped, and --state cannot be used.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			want, err := parseRouteState(state)
			if err != nil {
				return err
			}
			if a.json {
				if want != "" {
					return errors.New("--state cannot be used with --json: the JSON is the whole state of the daemon")
				}
				raw, err := a.rawState(cmd.Context())
				if err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			st, err := a.state(cmd.Context())
			if err != nil {
				return err
			}
			return renderRoutes(cmd.OutOrStdout(), st.Routes, want)
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "show only the routes in this state: "+routeStateNames())
	return cmd
}

func routeStateNames() string {
	names := make([]string, len(routeStateOrder))
	for i, s := range routeStateOrder {
		names[i] = string(s)
	}
	return strings.Join(names, ", ")
}

func parseRouteState(s string) (planner.RouteState, error) {
	if s == "" {
		return "", nil
	}
	for _, known := range routeStateOrder {
		if string(known) == s {
			return known, nil
		}
	}
	return "", fmt.Errorf("unknown route state %q: want one of %s", s, routeStateNames())
}

func renderRoutes(w io.Writer, routes []engine.RouteView, state planner.RouteState) error {
	s := &screen{w: w}
	shown := slices.DeleteFunc(slices.Clone(routes), func(r engine.RouteView) bool {
		return state != "" && r.State != state
	})
	if len(shown) == 0 {
		if state != "" {
			s.printf("No routes in state %s.\n", state)
		} else {
			s.println("No routes.")
		}
		return s.done()
	}
	slices.SortStableFunc(shown, func(x, y engine.RouteView) int {
		return cmp.Or(cmp.Compare(x.Hostname, y.Hostname), cmp.Compare(x.Owner, y.Owner))
	})
	t := s.table()
	t.row("HOSTNAME", "STATE", "LEVEL", "SERVICE", "OWNER", "ZONE", "NOTE")
	for _, r := range shown {
		t.row(r.Hostname, string(r.State), dash(r.Level), dash(r.Service), dash(r.Owner), dash(r.Zone), dash(routeNote(r)))
	}
	t.flush()
	return s.done()
}

// routeNote is the reason a route is in its state, or else its first warning.
func routeNote(r engine.RouteView) string {
	if r.Reason != "" {
		return r.Reason
	}
	if len(r.Warnings) > 0 {
		return r.Warnings[0]
	}
	return ""
}
