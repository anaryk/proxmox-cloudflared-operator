package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/require"
	"golang.org/x/term"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

func TestPrintableReplacesWhatATerminalWouldObey(t *testing.T) {
	for _, tt := range []struct {
		name, in, want string
	}{
		{"plain text", "www.example.com: connection refused", "www.example.com: connection refused"},
		{"text that is not ASCII", "služba é 日本語 🙂", "služba é 日本語 🙂"},
		{"escape and bell", "a\x1b[2Jb\x07c", "a?[2Jb?c"},
		{"the operating system command that sets a title", "\x1b]0;pwned\x07", "?]0;pwned?"},
		{"every C0 control", "\x00\x01\x08\t\n\v\f\r\x1a\x1f", "??????????"},
		{"delete", "a\x7fb", "a?b"},
		{"C1 controls", runes(0x80, 0x85, 0x9b, 0x9f), "????"},
		{"a bidirectional override", "a" + runes(0x202e) + "b" + runes(0x202a) + "c" + runes(0x202b) + "d" + runes(0x202c) + "d" + runes(0x202d) + "e", "a?b?c?d?d?e"},
		{"bidirectional isolates", runes(0x2066) + "x" + runes(0x2067) + "y" + runes(0x2068) + "z" + runes(0x2069), "?x?y?z?"},
		{"marks of direction", runes(0x200e) + "A" + runes(0x200f) + "B" + runes(0x61c) + "C", "?A?B?C"},
		{"line and paragraph separators", "a" + runes(0x2028) + "b" + runes(0x2029) + "c", "a?b?c"},
		{"zero-width characters", "w" + runes(0x200b) + "w" + runes(0x200c) + "w" + runes(0x200d) + "w" + runes(0x2060) + "w", "w?w?w?w?w"},
		{"a byte order mark", runes(0xfeff) + "bom", "?bom"},
		{"a soft hyphen", "ex" + runes(0xad) + "ample", "ex?ample"},
		{"tag characters", "flag" + runes(0xe0001, 0xe0065, 0xe006e, 0xe007f), "flag????"},
		{"other format characters", runes(0x600) + runes(0x180e) + runes(0xfff9), "???"},
		{"spaces that are not ASCII stay", "a" + runes(0xa0) + "b" + runes(0x3000) + "c", "a" + runes(0xa0) + "b" + runes(0x3000) + "c"},
		{"bytes that are no UTF-8", "a\xffb", "a�b"},
		{"nothing", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := printable(tt.in)

			require.Equal(t, tt.want, got)
			for _, r := range got {
				require.True(t, unicode.IsPrint(r) || unicode.IsSpace(r), "U+%04X is neither printable nor a space", r)
			}
		})
	}
}

func TestPrintableLeavesNothingToChance(t *testing.T) {
	// Every rune that is neither printable nor a space is gone, whatever the
	// category: this walks the first planes.
	for r := rune(0); r < 0x3000; r++ {
		out := printable(string(r))
		for _, got := range out {
			require.False(t, unicode.IsControl(got), "U+%04X came out as a control character", r)
			require.False(t, isBidi(got), "U+%04X came out as a bidirectional control", r)
			require.False(t, unicode.Is(unicode.Cf, got), "U+%04X came out as a format character", r)
		}
	}
}

func TestTheScreenCleansWhatItIsGiven(t *testing.T) {
	var buf bytes.Buffer
	s := &screen{w: &buf}

	s.printf("%s %s %s %q %d|%-6s|\n", "a\x1bb", planner.RouteState("x\x07y"), errors.New("e\x00rr"), "q\x1b", 42, "z")
	s.println("line\x1b[31m")
	tb := s.table()
	tb.row("HEAD", "H\x1b2")
	tb.row("a\x1b", "b")
	tb.flush()

	require.NoError(t, s.done())
	require.Equal(t, "a?b x?y e?rr \"q?\" 42|z     |\nline?[31m\nHEAD  H?2\na?    b\n", buf.String())
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the pipe is closed") }

func TestTheScreenKeepsTheFirstWriteError(t *testing.T) {
	s := &screen{w: failingWriter{}}

	s.println("one")
	s.printf("two %s\n", "x")

	require.EqualError(t, s.done(), "the pipe is closed")
}

// hostileText has an escape sequence, a bell, a delete, a C1 control and a
// right-to-left override: what a guest, another party's DNS record or
// Cloudflare may put in a name or a message.
var hostileText = "\x1b[2J\x1b]0;pwned\x07 \x7f " + runes(0x9b) + "31m " + runes(0x202e) + "evil" + runes(0x2069)

// requireClean fails when text holds a character that a terminal would act on.
func requireClean(t *testing.T, text string, what string) {
	t.Helper()
	for i, r := range text {
		if r == '\n' {
			continue
		}
		require.False(t, unicode.IsControl(r), "%s: control character U+%04X at byte %d of %q", what, r, i, text)
		require.False(t, isBidi(r), "%s: bidirectional control U+%04X at byte %d of %q", what, r, i, text)
		require.False(t, unicode.Is(unicode.Cf, r), "%s: format character U+%04X at byte %d of %q", what, r, i, text)
	}
}

// hostileState carries hostileText wherever the state holds text that others
// control.
func hostileState() engine.State {
	st := planState()
	st.Mode = "enforce"
	st.Complete = false
	st.Problems = []string{
		"agent of qemu/101: " + hostileText,
		"the inventory is incomplete; run pco apply --confirm-deletes if " + hostileText,
	}
	st.Routes = []engine.RouteView{
		routeView("evil"+hostileText+".example.com", "qemu/101", planner.StateUnreachable, "", "zone"+hostileText, "refused "+hostileText),
		routeView("warn.example.com", "qemu/102", planner.StateActive, "http://10.0.0.1:80", "example.com", "", hostileText),
	}
	st.Issues = []planner.Issue{
		{Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Line: 2, Col: 5, Msg: "broken " + hostileText},
		{Msg: "settings " + hostileText},
	}
	st.Tunnels[0].Name = "pco" + hostileText
	st.Actions = []reconcile.Action{
		{Kind: reconcile.DeleteRecord, Target: "gone" + hostileText, Detail: "in zone " + hostileText, Destructive: true, Held: "guard " + hostileText},
		{Kind: reconcile.UpdateRecord, Target: "shop" + hostileText, Detail: "in zone " + hostileText, Destructive: true, Held: "adoption " + hostileText},
	}
	st.Waiting = []engine.Waiting{
		{Kind: engine.WaitingRemovals, Detail: "guard " + hostileText, Items: []string{"gone" + hostileText}},
		{Kind: engine.WaitingZone, Subject: "zone" + hostileText, Detail: "zone" + hostileText + " left its listing", Items: []string{}},
	}
	st.Conflicts = []reconcile.Conflict{{Zone: "zone" + hostileText, Name: "shop.example.com", Type: "TXT", Content: "content" + hostileText}}
	st.Lost = []string{"lost" + hostileText}
	st.Credentials = []engine.CredentialView{hostileCredential()}
	return st
}

func hostileCredential() engine.CredentialView {
	v := failingCredential("a1b2c3d4", "label"+hostileText)
	v.Report.Accounts[0].Name = "account" + hostileText
	v.Report.Zones[0].Name = "zone" + hostileText
	v.Report.Checks[3].Scope = "zone" + hostileText
	v.Report.Checks[3].Detail = "grant " + hostileText
	v.Report.Leftovers = []string{"probe" + hostileText}
	return v
}

func TestNothingTheDaemonSendsReachesTheTerminalAsAControlCharacter(t *testing.T) {
	st := hostileState()
	for _, tt := range []struct {
		name string
		args []string
		in   string
	}{
		{"status", []string{"status"}, ""},
		{"routes", []string{"routes"}, ""},
		{"routes of a state", []string{"routes", "--state", "unreachable"}, ""},
		{"plan", []string{"plan"}, ""},
		{"apply --confirm-deletes", []string{"apply", "--confirm-deletes"}, "n\n"},
		{"adopt a conflict", []string{"adopt", "shop.example.com"}, "n\n"},
		{"adopt a lost name", []string{"adopt", "lost.example.com", "--yes"}, ""},
		{"credential list", []string{"credential", "list"}, ""},
		{"credential check", []string{"credential", "check", "a1b2c3d4"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := &fakeEngine{state: st, checkView: hostileCredential()}
			r := newRunner(t, serveFake(t, e)).tty()

			res := r.run(tt.in, tt.args...)

			requireClean(t, res.out, "stdout")
			requireClean(t, res.errOut, "stderr")
			if res.err != nil {
				require.Equal(t, 1, exitCode(res.err, io.Discard))
			}
		})
	}
}

func TestHostileStatusGolden(t *testing.T) {
	r, _ := daemonWith(t, hostileState())

	res := r.run("", "status")

	requireClean(t, res.out, "stdout")
	requireGolden(t, "status_hostile.golden", res.out)
}

func TestHostileRoutesAndPlanGolden(t *testing.T) {
	r, _ := daemonWith(t, hostileState())

	routes := r.run("", "routes")
	plan := r.run("", "plan")

	requireGolden(t, "routes_hostile.golden", routes.out)
	requireGolden(t, "plan_hostile.golden", plan.out)
}

func TestHostileChecklistGolden(t *testing.T) {
	e := &fakeEngine{state: hostileState(), checkView: hostileCredential()}
	r := newRunner(t, serveFake(t, e))

	res := r.run("", "credential", "check", "a1b2c3d4")

	requireGolden(t, "credential_check_hostile.golden", res.out)
}

func TestHostileConfirmationsGolden(t *testing.T) {
	r, _ := daemonWith(t, hostileState())

	apply := r.tty().run("n\n", "apply", "--confirm-deletes")
	adopt := r.tty().run("n\n", "adopt", "shop.example.com")

	requireGolden(t, "apply_hostile.golden", apply.out+"--- stderr\n"+apply.errOut)
	requireGolden(t, "adopt_hostile.golden", adopt.out+"--- stderr\n"+adopt.errOut)
}

func TestJSONNeedsNoCleaning(t *testing.T) {
	// The payload is printed as it is, its characters escaped as JSON says.
	r, _ := daemonWith(t, hostileState())

	res := r.run("", "--json", "plan")

	require.NoError(t, res.err)
	require.Contains(t, res.out, `\u001b[2J`)
	requireClean(t, res.out, "json")
}

func TestJSONEscapesWhatTheEncoderOfTheDaemonDoesNot(t *testing.T) {
	// encoding/json escapes C0, and nothing else that a terminal acts on.
	raw := "{\"a\":\"x\x7fy\u009bz\u202ew\u2066v\u0085\",\"b\":[1,\"\\u001b\"]}"

	var out bytes.Buffer
	require.NoError(t, printJSON(&out, []byte(raw)))

	requireClean(t, out.String(), "json")
	require.Contains(t, out.String(), `"x\u007fy\u009bz\u202ew\u2066v\u0085"`)
	var before, after any
	require.NoError(t, json.Unmarshal([]byte(raw), &before))
	require.NoError(t, json.Unmarshal(out.Bytes(), &after))
	require.Equal(t, before, after, "the escapes mean what the characters meant")
	require.True(t, strings.HasPrefix(out.String(), "{\n  \"a\": "), "indented, in the order of the daemon: %q", out.String())
}

func TestTheExitCodeAndWhatIsPrintedWithIt(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		code   int
		stderr string
	}{
		{"no error", nil, 0, ""},
		{"a command that reported its own problems", errReported, 1, ""},
		{"the same, wrapped", fmt.Errorf("status: %w", errReported), 1, ""},
		{"an error", errors.New("cannot reach the pco daemon"), 1, "pco: cannot reach the pco daemon\n"},
		{"an error that carries what others control", errors.New("refused: " + hostileText), 1, "pco: refused: ?[2J?]0;pwned? ? ?31m ?evil?\n"},
		{"an aborted confirmation", errAborted, 1, "pco: aborted: nothing was changed\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer

			require.Equal(t, tt.code, exitCode(tt.err, &stderr))
			require.Equal(t, tt.stderr, stderr.String())
		})
	}
}

func TestTheTerminalIsRestoredWhenTheNoEchoReadIsInterrupted(t *testing.T) {
	state := &term.State{}
	var restored []int
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	ops := termOps{
		getState: func(int) (*term.State, error) { return state, nil },
		restore: func(fd int, got *term.State) error {
			require.Same(t, state, got)
			restored = append(restored, fd)
			return nil
		},
		readPassword: func(int) ([]byte, error) {
			<-release
			return nil, nil
		},
	}
	signals := make(chan os.Signal, 1)
	signals <- os.Interrupt

	token, err := readPasswordRestoring(7, ops, signals)

	require.Nil(t, token)
	require.EqualError(t, err, "interrupted")
	require.Equal(t, []int{7}, restored, "the terminal was given back")
}

func TestTheNoEchoReadLeavesTheTerminalToTheReaderWhenItEnds(t *testing.T) {
	ops := termOps{
		getState: func(int) (*term.State, error) { return &term.State{}, nil },
		restore:  func(int, *term.State) error { t.Fatal("restored a terminal that the read restores itself"); return nil },
	}

	t.Run("a token", func(t *testing.T) {
		ops.readPassword = func(fd int) ([]byte, error) { return []byte("secret"), nil }
		token, err := readPasswordRestoring(7, ops, make(chan os.Signal))
		require.NoError(t, err)
		require.Equal(t, "secret", string(token))
	})

	t.Run("an error of the read", func(t *testing.T) {
		ops.readPassword = func(int) ([]byte, error) { return nil, errors.New("inappropriate ioctl") }
		_, err := readPasswordRestoring(7, ops, make(chan os.Signal))
		require.EqualError(t, err, "inappropriate ioctl")
	})

	t.Run("a terminal whose state cannot be read is not read at all", func(t *testing.T) {
		ops.getState = func(int) (*term.State, error) { return nil, errors.New("not a terminal") }
		ops.readPassword = func(int) ([]byte, error) { t.Fatal("read"); return nil, nil }
		_, err := readPasswordRestoring(7, ops, make(chan os.Signal))
		require.EqualError(t, err, "not a terminal")
	})
}

func TestEveryLineOfAGoldenFileIsClean(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	require.NoError(t, err)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".golden") {
			continue
		}
		raw, err := os.ReadFile("testdata/" + e.Name())
		require.NoError(t, err)
		requireClean(t, string(raw), e.Name())
	}
}

// runes makes a string of the code points given, for the ones a source file
// had better not hold as they are.
func runes(code ...rune) string { return string(code) }
