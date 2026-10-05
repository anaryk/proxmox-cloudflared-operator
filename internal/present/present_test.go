package present

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// wordCase is a case of testdata/words.json: a function, by the name its twin
// in the web interface has, what it is given, as the API sends it, and what
// it returns. The tests of the web interface run the same cases.
type wordCase struct {
	Fn    string          `json:"fn"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	Want  json.RawMessage `json:"want"`
}

// wordFuncs run the function a case names on its input.
var wordFuncs = map[string]func(t *testing.T, in json.RawMessage) any{
	"modeText":      func(t *testing.T, in json.RawMessage) any { return ModeText(decode[engine.State](t, in)) },
	"inventoryText": func(t *testing.T, in json.RawMessage) any { return InventoryText(decode[engine.State](t, in)) },
	"egressText":    func(t *testing.T, in json.RawMessage) any { return EgressText(decode[engine.EgressView](t, in)) },
	"writerText":    func(t *testing.T, in json.RawMessage) any { return WriterText(decode[string](t, in)) },
	"verifiedText":  func(t *testing.T, in json.RawMessage) any { return VerifiedText(decode[engine.TunnelView](t, in)) },
	"connectorText": func(t *testing.T, in json.RawMessage) any { return ConnectorText(decode[connector.Status](t, in)) },
	"credentialState": func(t *testing.T, in json.RawMessage) any {
		return CredentialState(decode[engine.CredentialView](t, in))
	},
	"identityNow": func(t *testing.T, in json.RawMessage) any { return IdentityNow(decode[engine.ApprovalView](t, in)) },
	"routeNote":   func(t *testing.T, in json.RawMessage) any { return RouteNote(decode[engine.RouteView](t, in)) },
	"nextStep":    func(t *testing.T, in json.RawMessage) any { return NextStep(decode[engine.State](t, in)) },
	"unaffected": func(t *testing.T, in json.RawMessage) any {
		if actions := Unaffected(decode[engine.State](t, in)); actions != nil {
			return actions
		}
		return []reconcile.Action{}
	},
	"routeStateOrder": func(*testing.T, json.RawMessage) any { return RouteStateOrder },
	"commandArg": func(t *testing.T, in json.RawMessage) any {
		arg := decode[struct{ Kind, Value string }](t, in)
		value, refused := CommandArg(arg.Kind, arg.Value)
		return map[string]string{"value": value, "refused": refused}
	},
	"rotateCommand": func(t *testing.T, in json.RawMessage) any {
		arg := decode[struct {
			Tunnels []engine.TunnelView
			Account string
		}](t, in)
		cmd, refused := RotateCommand(arg.Tunnels, arg.Account)
		return map[string]string{"command": cmd, "refused": refused}
	},
	"budgetWait": func(t *testing.T, in json.RawMessage) any {
		changes, matched := BudgetWait(decode[string](t, in))
		return map[string]any{"changes": changes, "matched": matched}
	},
	"printable": func(t *testing.T, in json.RawMessage) any {
		got := Printable(decode[string](t, in))
		for _, r := range got {
			require.True(t, unicode.IsPrint(r) || unicode.IsSpace(r), "U+%04X is neither printable nor a space", r)
		}
		return got
	},
	"isBidi": func(t *testing.T, in json.RawMessage) any {
		s := decode[string](t, in)
		require.Equal(t, 1, utf8.RuneCountInString(s), "the input is one character")
		r, _ := utf8.DecodeRuneInString(s)
		return IsBidi(r)
	},
}

// decode reads the input of a case as the type a function takes; a key the
// type does not have is a mistake of the case.
func decode[T any](t *testing.T, in json.RawMessage) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&v))
	return v
}

func readWordCases(t *testing.T) []wordCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/words.json")
	require.NoError(t, err)
	var cases []wordCase
	require.NoError(t, json.Unmarshal(raw, &cases))
	return cases
}

func TestEveryCaseOfTheWords(t *testing.T) {
	cases := readWordCases(t)
	names := make(map[string]bool)
	for _, c := range cases {
		run, known := wordFuncs[c.Fn]
		require.True(t, known, "words.json names a function that is not known: %s", c.Fn)
		require.False(t, names[c.Fn+"/"+c.Name], "two cases are called %s/%s", c.Fn, c.Name)
		names[c.Fn+"/"+c.Name] = true

		t.Run(c.Fn+"/"+c.Name, func(t *testing.T) {
			got, err := json.Marshal(run(t, c.Input))
			require.NoError(t, err)
			require.JSONEq(t, string(c.Want), string(got))
		})
	}
}

func TestEveryFunctionHasCases(t *testing.T) {
	has := make(map[string]bool)
	for _, c := range readWordCases(t) {
		has[c.Fn] = true
	}
	for fn := range wordFuncs {
		require.True(t, has[fn], "words.json has no case of %s", fn)
	}
}

// The line BudgetWait matches is the one the reconcilers write: what it
// says waits is read back in full.
func TestBudgetWaitReadsBackTheLineOfWhatWaits(t *testing.T) {
	for _, w := range []reconcile.Waiting{
		{Changes: 1},
		{Changes: 2},
		{Changes: 1500},
		{Reads: []string{reconcile.ZoneListingRead + "example.com"}},
		{Reads: []string{reconcile.TunnelRead + "0123456789abcdef0123456789abcdef"}, Changes: 1},
		{Reads: []string{reconcile.TunnelRead + "acc1", reconcile.ZoneListingRead + "example.com", reconcile.ZoneListingRead + "example.net"}, Changes: 7},
	} {
		line := w.Line()
		t.Run(line, func(t *testing.T) {
			changes, matched := BudgetWait(line)

			require.True(t, matched)
			require.Equal(t, w.Changes, changes)
		})
	}
}

// No character a shell gives a meaning to gets into a command, wherever it is
// in the value.
func TestCommandArgRefusesWhatAShellReads(t *testing.T) {
	shell := []string{" ", "\t", "\n", "'", "\"", "`", "$", ";", "&", "|", "<", ">", "(", ")", "{", "}", "\\", "*", "?", "#", "~", "!", "=", "\x00", "\u202e"}
	for kind, valid := range map[string]string{
		ArgAccount:  "0123456789abcdef0123456789abcdef",
		ArgOwner:    "qemu/101",
		ArgHostname: "www.example.com",
	} {
		got, refused := CommandArg(kind, valid)
		require.Equal(t, valid, got)
		require.Empty(t, refused)
		for _, c := range shell {
			for _, v := range []string{c + valid, valid + c, valid[:5] + c + valid[5:]} {
				got, refused := CommandArg(kind, v)
				require.Empty(t, got, "%s %q", kind, v)
				require.Equal(t, "the "+kind+" has an unexpected form", refused)
			}
		}
	}
}
