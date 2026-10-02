package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// newTable returns a writer that lines up the columns of what is written to
// it, tab separated, once it is flushed.
func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

// printJSON writes a JSON document as the daemon sent it, indented by two
// spaces. Indenting changes white space only, so the order of the keys stays.
func printJSON(w io.Writer, raw []byte) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return fmt.Errorf("the answer of the daemon is not JSON: %w", err)
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

// printCredentialJSON writes a credential view as JSON. The client hands the
// view over decoded, so this is its encoding, which has the order of the keys
// the daemon wrote them in.
func printCredentialJSON(w io.Writer, v engine.CredentialView) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding the credential: %w", err)
	}
	return printJSON(w, raw)
}

// state asks the daemon for its state and returns it decoded together with the
// bytes it came as.
func (a *app) state(ctx context.Context) (engine.State, []byte, error) {
	raw, err := a.client().StatusRaw(ctx)
	if err != nil {
		return engine.State{}, nil, a.explain(ctx, err)
	}
	var st engine.State
	if err := json.Unmarshal(raw, &st); err != nil {
		return engine.State{}, nil, fmt.Errorf("decoding the state of the daemon: %w", err)
	}
	return st, raw, nil
}

// when formats a time in local time as RFC 3339, without fractions of a
// second. The zero time is a dash.
func (a *app) when(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.In(a.loc).Format(time.RFC3339)
}

// dash stands for a value that is empty.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
