package main

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func (a *app) eventsCmd() *cobra.Command {
	var since string
	cmd := &cobra.Command{
		Use:   "events [--since <duration|time>]",
		Short: "List what changed, as the daemon keeps it",
		Long: "List the events the daemon keeps, oldest first: routes that changed state, conflicts,\n" +
			"what was applied at Cloudflare, holds that began or ended, problems that appeared and what\n" +
			"admins did. The daemon keeps the last thousand since it started; the journal has them all\n" +
			"(journalctl -u pco). --since takes a duration such as 10m, or a time in RFC 3339 format.\n\n" +
			jsonHelp,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			from, err := a.parseSince(since)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if a.json {
				raw, err := a.client().EventsRaw(ctx, from)
				if err != nil {
					return a.explain(ctx, err)
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			events, err := a.client().Events(ctx, from)
			if err != nil {
				return a.explain(ctx, err)
			}
			return a.renderEvents(cmd.OutOrStdout(), events)
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "list only the events after this: a duration back from now, such as 10m, or a time in RFC 3339 format")
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

func (a *app) renderEvents(w io.Writer, events []engine.Event) error {
	s := &screen{w: w}
	if len(events) == 0 {
		s.println("No events.")
		return s.done()
	}
	t := s.table()
	t.row("TIME", "LEVEL", "KIND", "SUBJECT", "MESSAGE")
	for _, ev := range events {
		t.row(a.when(ev.At), ev.Level, ev.Kind, dash(ev.Subject), ev.Message)
	}
	t.flush()
	return s.done()
}
