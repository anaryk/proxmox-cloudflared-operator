package docscheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/require"
)

// goFiles parses the Go files of a package of the repository that are not
// tests, in the order of their names, and returns the positions they share.
func goFiles(t *testing.T, dir string) ([]*ast.File, *token.FileSet) {
	t.Helper()
	entries, err := os.ReadDir(repo(dir))
	require.NoError(t, err)
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, repo(dir+"/"+name), nil, 0)
		require.NoError(t, err)
		files = append(files, f)
	}
	require.NotEmpty(t, files, "no Go files in %s", dir)
	return files, fset
}

// constString is the string a constant expression says, when it is made of
// string literals, +, and constants read before it.
func constString(e ast.Expr, known map[string]string) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return constString(e.X, known)
	case *ast.Ident:
		s, ok := known[e.Name]
		return s, ok
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		x, ok := constString(e.X, known)
		if !ok {
			return "", false
		}
		y, ok := constString(e.Y, known)
		return x + y, ok
	}
	return "", false
}

// stringConsts returns the string constants of a package whose names are
// prefix, or prefix and then a capital letter, and when typeName is not empty,
// whose declared type is that. They are found in the source, so that a
// constant added later is found too, and one whose value is not a string the
// test can read fails it.
func stringConsts(t *testing.T, dir, prefix, typeName string) map[string]string {
	t.Helper()
	files, _ := goFiles(t, dir)
	known := make(map[string]string)
	for _, f := range files {
		for _, vs := range constSpecs(f) {
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					if v, ok := constString(vs.Values[i], known); ok {
						known[name.Name] = v
					}
				}
			}
		}
	}
	out := make(map[string]string)
	for _, f := range files {
		for _, vs := range constSpecs(f) {
			for _, name := range vs.Names {
				if !named(name.Name, prefix) || typeName != "" && !hasType(vs, typeName) {
					continue
				}
				v, ok := known[name.Name]
				require.True(t, ok, "%s: cannot read the value of the constant %s: teach docscheck its form", dir, name.Name)
				out[name.Name] = v
			}
		}
	}
	return out
}

// constSpecs returns the specs of the constants a file declares at its top.
func constSpecs(f *ast.File) []*ast.ValueSpec {
	var out []*ast.ValueSpec
	for _, decl := range f.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
			for _, spec := range gen.Specs {
				out = append(out, spec.(*ast.ValueSpec))
			}
		}
	}
	return out
}

// named says that name is prefix, or prefix and then a capital letter.
func named(name, prefix string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
	return ok && (rest == "" || unicode.IsUpper([]rune(rest)[0]))
}

func hasType(vs *ast.ValueSpec, typeName string) bool {
	id, ok := vs.Type.(*ast.Ident)
	return ok && id.Name == typeName
}
