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
		Long: "List the routes sorted by hostname. The note is the reason a route is not served,\n" +
			"or its first warning.\n\n" +
			"With --json the whole state of the daemon is printed, as it was sent, and --state\n" +
			"cannot be used.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			want, err := parseRouteState(state)
			if err != nil {
				return err
			}
			if a.json && want != "" {
				return errors.New("--state cannot be used with --json: the JSON is the whole state of the daemon")
			}
			st, raw, err := a.state(cmd.Context())
			if err != nil {
				return err
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), raw)
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
	shown := slices.DeleteFunc(slices.Clone(routes), func(r engine.RouteView) bool {
		return state != "" && r.State != state
	})
	if len(shown) == 0 {
		if state != "" {
			_, err := fmt.Fprintf(w, "No routes in state %s.\n", state)
			return err
		}
		_, err := fmt.Fprintln(w, "No routes.")
		return err
	}
	slices.SortStableFunc(shown, func(x, y engine.RouteView) int {
		return cmp.Or(cmp.Compare(x.Hostname, y.Hostname), cmp.Compare(x.Owner, y.Owner))
	})
	t := newTable(w)
	_, _ = fmt.Fprintln(t, "HOSTNAME\tSTATE\tSERVICE\tOWNER\tZONE\tNOTE")
	for _, r := range shown {
		_, _ = fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Hostname, r.State, dash(r.Service), dash(r.Owner), dash(r.Zone), dash(routeNote(r)))
	}
	return t.Flush()
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
