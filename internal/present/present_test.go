package present

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
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
	"rogueText":     func(t *testing.T, in json.RawMessage) any { return decode[engine.RogueConnector](t, in).Text() },
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
	"commandArgEveryPosition": commandArgEveryPosition,
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

// The pattern of a kind without one refuses whatever it is given, in the
// web interface too.
func TestArgFormOfAKindWithoutOneMatchesNothing(t *testing.T) {
	re := regexp.MustCompile(ArgForm("path"))
	for _, v := range []string{"", "a", "/etc", "-", "\n", "0123456789abcdef0123456789abcdef", "qemu/101", "www.example.com"} {
		require.False(t, re.MatchString(v), "%q", v)
	}
	for kind, form := range argForms {
		require.Equal(t, form.String(), ArgForm(kind))
	}
}

// commandArgEveryPosition puts each of the characters in place of each
// character of each value, and between any two of them, and lists what
// CommandArg did not refuse as of an unexpected form. Each value must pass
// as it is.
func commandArgEveryPosition(t *testing.T, in json.RawMessage) any {
	arg := decode[struct {
		Chars  []string
		Values map[string]string
	}](t, in)
	wrong := []string{}
	for _, kind := range slices.Sorted(maps.Keys(arg.Values)) {
		v := arg.Values[kind]
		if got, _ := CommandArg(kind, v); got != v {
			wrong = append(wrong, kind+" "+strconv.Quote(v)+" is refused")
		}
		for _, c := range arg.Chars {
			var spliced []string
			for i := range len(v) + 1 {
				if i < len(v) && v[i:i+1] != c {
					spliced = append(spliced, v[:i]+c+v[i+1:])
				}
				spliced = append(spliced, v[:i]+c+v[i:])
			}
			for _, s := range spliced {
				if got, refused := CommandArg(kind, s); got != "" || refused != "the "+kind+" has an unexpected form" {
					wrong = append(wrong, kind+" "+strconv.Quote(s))
				}
			}
		}
	}
	return wrong
}
