package main

import (
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func (a *app) eventsCmd() *cobra.Command {
	var (
		since string
		kinds []string
	)
	cmd := &cobra.Command{
		Use:   "events [--since <duration|time>] [--kind <kind>]",
		Short: "List what changed, as the daemon keeps it",
		Long: "List the events the daemon keeps, oldest first: routes that changed state, conflicts,\n" +
			"what was applied at Cloudflare, holds that began or ended, problems that appeared and what\n" +
			"admins did. The daemon keeps the last thousand since it started; the journal has them all\n" +
			"(journalctl -u pco). --since takes a duration such as 10m, or a time in RFC 3339 format.\n\n" +
			"--kind lists the events of that kind alone, and may be given more than once. --kind security\n" +
			"lists the audit trail instead: join tokens, admissions, forgets, promotions, hand-offs,\n" +
			"rotations, what happened to the lease and what admins did, with who did it, as every node\n" +
			"keeps the last 500 of its own in the cluster root, merged by time. It answers on any node of\n" +
			"a cluster, also without a controller.\n\n" +
			jsonHelp + "\n\n" + askHelp,
		Example: "  # Every event the daemon keeps\n" +
			"  pco events\n\n" +
			"  # Those of the last ten minutes\n" +
			"  pco events --since 10m\n\n" +
			"  # Those after noon UTC on 1 October 2026, as JSON\n" +
			"  pco events --since 2026-10-01T12:00:00Z --json\n\n" +
			"  # The audit trail of every node of the last day\n" +
			"  pco events --kind security --since 24h",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			from, err := a.parseSince(since)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if a.json {
				raw, err := a.client().EventsRaw(ctx, from, kinds...)
				if err != nil {
					return a.explain(ctx, err)
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			events, err := a.client().Events(ctx, from, kinds...)
			if err != nil {
				return a.explain(ctx, err)
			}
			return a.renderEvents(cmd.OutOrStdout(), events, slices.Equal(kinds, []string{engine.KindSecurity}))
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "list only the events after this: a duration back from now, such as 10m, or a time in RFC 3339 format")
	cmd.Flags().StringArrayVar(&kinds, "kind", nil, "list only the events of this kind; security lists the audit trail of every node")
	return cmd
}

// parseSince reads --since: a positive duration back from now, or a time in
// RFC 3339 format. Empty is the zero time, which asks for every event.
func (a *app) parseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return a.now().Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since %q: want a duration such as 10m or a time in RFC 3339 format such as 2026-10-01T12:00:00Z", s)
}

// renderEvents lists events a line each. The node that made an event shows
// once events of another node are among them, and who acted in the audit
// trail.
func (a *app) renderEvents(w io.Writer, events []engine.Event, trail bool) error {
	s := &screen{w: w}
	if len(events) == 0 {
		s.println("No events.")
		return s.done()
	}
	nodes := trail || slices.ContainsFunc(events, func(ev engine.Event) bool { return ev.Node != "" })
	t := s.table()
	head := []string{"TIME", "LEVEL"}
	if nodes {
		head = append(head, "NODE")
	}
	head = append(head, "KIND", "SUBJECT")
	if trail {
		head = append(head, "ACTOR")
	}
	t.row(append(head, "MESSAGE")...)
	for _, ev := range events {
		row := []string{a.when(ev.At), ev.Level}
		if nodes {
			row = append(row, dash(ev.Node))
		}
		row = append(row, ev.Kind, dash(ev.Subject))
		if trail {
			row = append(row, dash(ev.Actor))
		}
		t.row(append(row, ev.Message)...)
	}
	t.flush()
	return s.done()
}
