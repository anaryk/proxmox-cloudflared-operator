package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/present"
)

// clean returns what a value prints as, with the text of it cleaned: a string,
// an error or a Stringer, whatever its type. Numbers and the like are not text.
func clean(arg any) any {
	switch v := arg.(type) {
	case string:
		return present.Printable(v)
	case error:
		return present.Printable(v.Error())
	case fmt.Stringer:
		return present.Printable(v.String())
	}
	if rv := reflect.ValueOf(arg); rv.Kind() == reflect.String {
		return present.Printable(rv.String())
	}
	return arg
}

// screen is where the commands write what the admin reads. Everything that
// is given to it as an argument or as a cell is cleaned by present.Printable,
// so that no command has to remember to; the formats are the commands' own
// text. A write that fails is remembered, and the first error is what done
// returns.
type screen struct {
	w   io.Writer
	err error
}

func (s *screen) printf(format string, args ...any) {
	if s.err != nil {
		return
	}
	cleaned := make([]any, len(args))
	for i, a := range args {
		cleaned[i] = clean(a)
	}
	_, s.err = fmt.Fprintf(s.w, format, cleaned...)
}

// println writes text and a newline.
func (s *screen) println(text string) { s.printf("%s\n", text) }

// done returns the first error a write had.
func (s *screen) done() error { return s.err }

// table is a table of the screen whose columns line up once it is flushed.
type table struct {
	s  *screen
	tw *tabwriter.Writer
}

func (s *screen) table() *table {
	return &table{s: s, tw: tabwriter.NewWriter(screenWriter{s}, 0, 0, 2, ' ', 0)}
}

// row writes a row of cells.
func (t *table) row(cells ...string) {
	for i, c := range cells {
		cells[i] = present.Printable(c)
	}
	_, _ = fmt.Fprintln(t.tw, strings.Join(cells, "\t"))
}

func (t *table) flush() { _ = t.tw.Flush() }

// screenWriter lets a tabwriter write through the screen, which keeps the
// errors. The cells are cleaned before they get there.
type screenWriter struct{ s *screen }

func (w screenWriter) Write(p []byte) (int, error) {
	if w.s.err != nil {
		return 0, w.s.err
	}
	n, err := w.s.w.Write(p)
	w.s.err = err
	return n, err
}

// printJSON writes a JSON document as the daemon sent it, indented by two
// spaces. Indenting changes white space only, so the order of the keys stays.
//
// A JSON string has no raw control characters of C0, but nothing stops it from
// holding DEL, the controls of C1 or the controls of direction, which the
// encoder of the daemon does not escape. They can only be inside strings, and
// are written as the escapes that mean the same.
func printJSON(w io.Writer, raw []byte) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return couldNotAsk{fmt.Errorf("the answer of the daemon is not JSON: %w", err)}
	}
	buf.WriteByte('\n')
	_, err := io.WriteString(w, escapeControls(buf.String()))
	return err
}

// escapeControls writes the characters that present.Printable replaces as
// JSON escapes, and leaves the line breaks of the indentation.
func escapeControls(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r != '\n' && (unicode.IsControl(r) || present.IsBidi(r)) {
			fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
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

// rawState asks the daemon for its state and returns the bytes it came as,
// without reading them.
func (a *app) rawState(ctx context.Context) ([]byte, error) {
	raw, err := a.client().StatusRaw(ctx)
	if err != nil {
		return nil, a.explain(ctx, err)
	}
	return raw, nil
}

// state asks the daemon for its state and reads it.
func (a *app) state(ctx context.Context) (engine.State, error) {
	raw, err := a.rawState(ctx)
	if err != nil {
		return engine.State{}, err
	}
	var st engine.State
	if err := json.Unmarshal(raw, &st); err != nil {
		return engine.State{}, couldNotAsk{fmt.Errorf("decoding the state of the daemon: %w", err)}
	}
	return st, nil
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
