package docscheck

import (
	"go/ast"
	"go/token"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// problems is docs/problems.md, and the sections of it each kind of word has.
// Problem lines are free text from many places, so what is keyed is what is
// fixed: the event kinds, the doctor checks, the named lines, the states and
// reasons that have a name. A line that is only a format at one call site is
// for review to find, and for a row of its own.
func problems(t *testing.T) string { return page(t, "problems.md") }

// has says that one of the code spans of the first cells of a section is the
// word, or begins with it and a space or a colon, as "credential <id>" does
// the check "credential".
func has(spans []string, word string) bool {
	return slices.ContainsFunc(spans, func(s string) bool {
		return s == word || strings.HasPrefix(s, word+" ") || strings.HasPrefix(s, word+":")
	})
}

func TestEveryEventKindHasARow(t *testing.T) {
	rows := spans(section(t, problems(t), "Event kinds"))
	kinds := stringConsts(t, "internal/engine", "kind", "")
	require.NotEmpty(t, kinds)
	for _, kind := range slices.Sorted(maps.Values(kinds)) {
		require.Contains(t, rows, kind,
			"docs/problems.md has no row for the event kind %q: add one to the table under \"Event kinds\"", kind)
	}
}

func TestEveryDoctorCheckHasARow(t *testing.T) {
	rows := spans(section(t, problems(t), "Doctor checks"))
	names := checkNames(t)
	require.NotEmpty(t, names)
	for _, name := range names {
		require.True(t, has(rows, name),
			"docs/problems.md has no row for the doctor check %q: add one to the table under \"Doctor checks\"", name)
	}
}

// A line the engine names, a constant of problem and then a capital letter,
// is in the page, in the words it has: the longest part of it that is not a
// placeholder.
func TestEveryNamedProblemLineIsListed(t *testing.T) {
	text := normalized(problems(t))
	lines := stringConsts(t, "internal/engine", "problem", "")
	require.NotEmpty(t, lines)
	for _, name := range slices.Sorted(maps.Keys(lines)) {
		fixed := longestFixed(lines[name])
		require.Contains(t, text, fixed,
			"docs/problems.md does not list the line of %s, %q: add a row for it under the area it belongs to", name, lines[name])
	}
}

var verb = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)

// longestFixed is the longest part of a format string between its verbs,
// with the white space around it trimmed.
func longestFixed(format string) string {
	var best string
	for _, part := range verb.Split(format, -1) {
		if part = strings.TrimSpace(part); len(part) > len(best) {
			best = part
		}
	}
	return normalized(best)
}

func TestEveryRouteStateHasARow(t *testing.T) {
	rows := spans(section(t, problems(t), "Route states"))
	states := stringConsts(t, "internal/planner", "State", "RouteState")
	maps.Copy(states, stringConsts(t, "internal/engine", "RouteFrozen", ""))
	require.GreaterOrEqual(t, len(states), 8)
	for _, state := range slices.Sorted(maps.Values(states)) {
		require.Contains(t, rows, state,
			"docs/problems.md has no row for the route state %q: add one to the table under \"Route states\"", state)
	}
}

func TestEveryHeldReasonHasARow(t *testing.T) {
	rows := spans(section(t, problems(t), "Why a change is held"))
	reasons := stringConsts(t, "internal/reconcile", "Held", "")
	require.NotEmpty(t, reasons)
	for _, reason := range slices.Sorted(maps.Values(reasons)) {
		require.True(t, has(rows, reason),
			"docs/problems.md has no row for the held reason %q: add one to the table under \"Why a change is held\"", reason)
	}
}

func TestEveryWaitingKindHasARow(t *testing.T) {
	rows := spans(section(t, problems(t), "What waits for a confirmation"))
	kinds := stringConsts(t, "internal/engine", "Waiting", "")
	require.NotEmpty(t, kinds)
	for _, kind := range slices.Sorted(maps.Values(kinds)) {
		require.Contains(t, rows, kind,
			"docs/problems.md has no row for the kind of what waits, %q: add one to the table under \"What waits for a confirmation\"", kind)
	}
}

func TestEveryWriterVerdictHasARow(t *testing.T) {
	rows := spans(section(t, problems(t), "Writer verdicts"))
	verdicts := stringConsts(t, "internal/engine", "Verdict", "")
	require.NotEmpty(t, verdicts)
	for _, verdict := range slices.Sorted(maps.Values(verdicts)) {
		require.Contains(t, rows, verdict,
			"docs/problems.md has no row for the writer verdict %q: add one to the table under \"Writer verdicts\"", verdict)
	}
}

// checkNames are the names of the checks of pco doctor, read from the calls
// of ok, warn and fail in its package: the first argument, or the name it was
// given in the function, up to the part that is not fixed ("credential " and
// then an id); and from the two tables that name checks, stateChecks and the
// checks of the appliance.
func checkNames(t *testing.T) []string {
	t.Helper()
	found := make(map[string]bool)
	files, fset := goFiles(t, "internal/doctor")
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					collectChecks(t, fset, d, found)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok && len(vs.Names) == 1 {
						collectTable(vs, found)
					}
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(found))
}

// collectTable adds the names of the checks that a table lists: stateChecks,
// the checks that read the state, which are listed for the time before the
// first cycle, and applianceRuns, whose elements start with the name of the
// check.
func collectTable(vs *ast.ValueSpec, found map[string]bool) {
	switch vs.Names[0].Name {
	case "stateChecks":
		for _, v := range vs.Values {
			ast.Inspect(v, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, ok := constString(lit, nil); ok {
						found[strings.TrimSpace(s)] = true
					}
				}
				return true
			})
		}
	case "applianceRuns":
		for _, v := range vs.Values {
			list, ok := v.(*ast.CompositeLit)
			if !ok {
				continue
			}
			for _, elt := range list.Elts {
				if row, ok := elt.(*ast.CompositeLit); ok && len(row.Elts) > 0 {
					if s, ok := constString(row.Elts[0], nil); ok {
						found[strings.TrimSpace(s)] = true
					}
				}
			}
		}
	}
}

// collectChecks adds the check names of the calls of ok, warn and fail in a
// function. A name that is a parameter of the function, a variable of a loop
// over a table, or a field of the table's row comes from where the table or
// the caller says it.
func collectChecks(t *testing.T, fset *token.FileSet, fn *ast.FuncDecl, found map[string]bool) {
	t.Helper()
	local := make(map[string]string) // names given to a check in the function
	elsewhere := make(map[string]bool)
	for _, field := range fn.Type.Params.List {
		for _, id := range field.Names {
			elsewhere[id.Name] = true
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.RangeStmt:
			if id, ok := n.Value.(*ast.Ident); ok {
				elsewhere[id.Name] = true
			}
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && i < len(n.Rhs) {
					if s, ok := leading(n.Rhs[i], local); ok {
						local[id.Name] = s
					}
				}
			}
		case *ast.ValueSpec:
			for i, id := range n.Names {
				if i < len(n.Values) {
					if s, ok := leading(n.Values[i], local); ok {
						local[id.Name] = s
					}
				}
			}
		}
		return true
	})
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "ok" && id.Name != "warn" && id.Name != "fail" {
			return true
		}
		switch arg := call.Args[0].(type) {
		case *ast.Ident:
			if elsewhere[arg.Name] {
				return true
			}
		case *ast.SelectorExpr:
			if arg.Sel.Name == "name" {
				return true
			}
		}
		s, ok := leading(call.Args[0], local)
		require.True(t, ok, "docscheck cannot tell the name of the doctor check given at %s: teach it the form", fset.Position(call.Pos()))
		found[strings.TrimSpace(s)] = true
		return true
	})
}

// leading is the fixed text an expression starts with: a string literal, a
// name that was given one, or the first of a sum.
func leading(e ast.Expr, local map[string]string) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		return constString(e, nil)
	case *ast.Ident:
		s, ok := local[e.Name]
		return s, ok
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return leading(e.X, local)
		}
	}
	return "", false
}
