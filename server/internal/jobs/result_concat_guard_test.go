package jobs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var unsafeResultConcat = regexp.MustCompile(`(?i)\bcoalesce\s*\(\s*(?:[a-z_][a-z_0-9]*\s*\.\s*)?result\s*,\s*'\{\}'\s*::\s*jsonb\s*\)\s*\|\|`)

func sqlConstant(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			s, err := strconv.Unquote(x.Value)
			return s, err == nil
		}
	case *ast.ParenExpr:
		return sqlConstant(x.X)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			a, ok := sqlConstant(x.X)
			b, ok2 := sqlConstant(x.Y)
			return a + b, ok && ok2
		}
	}
	return "", false
}

func TestResultConcatGuardExamples(t *testing.T) {
	for _, tc := range []struct {
		sql    string
		unsafe bool
	}{
		{"coalesce(result,'{}'::jsonb) || jsonb_build_object('error','x')", true},
		{"COALESCE( j.result,\n '{}' :: JSONB )\n|| '{}'", true},
		{"(CASE WHEN jsonb_typeof(result)='object' THEN result ELSE '{}'::jsonb END) || '{}'", false},
		{"SELECT coalesce(result,'{}'::jsonb) FROM jobs", false},
		{"coalesce(hostd_health,'{}'::jsonb) || '{}'", false},
	} {
		if got := unsafeResultConcat.MatchString(tc.sql); got != tc.unsafe {
			t.Errorf("%q: %v", tc.sql, got)
		}
	}
	expr, err := parser.ParseExpr("`coalesce(j.` + `result, '{}'::jsonb)` + ` || '{}'`")
	if err != nil {
		t.Fatal(err)
	}
	text, ok := sqlConstant(expr)
	if !ok || !unsafeResultConcat.MatchString(text) {
		t.Fatalf("concatenated SQL not detected: %q", text)
	}
}

func TestResultConcatGuardServer(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			e, ok := n.(ast.Expr)
			if !ok {
				return true
			}
			text, ok := sqlConstant(e)
			if ok && unsafeResultConcat.MatchString(text) {
				t.Errorf("unsafe result concatenation: %s", fset.Position(e.Pos()))
				return false
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
