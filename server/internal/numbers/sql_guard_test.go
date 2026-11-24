package numbers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/appsql"
)

func TestSQLGuard(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)\b(INSERT\s+OR\s+IGNORE|rowid|BLOB|AUTOINCREMENT)\b`)
	destructive := regexp.MustCompile(`(?i)\b(DELETE|DROP|TRUNCATE)\b`)
	const deletion = "\t\tif _, err = tx.ExecContext(ctx, \"DELETE FROM desk_projects WHERE project=?\", to); err != nil {"
	for _, name := range []string{"assign.go", "reconcile.go", "store.go", "sql.go"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if name == "assign.go" {
			want = 1
		}
		if got := strings.Count(string(raw), deletion+"\n"); got != want {
			t.Errorf("%s: 削除行 %d、期待 %d", name, got, want)
		}
		fs := token.NewFileSet()
		f, err := parser.ParseFile(fs, name, raw, 0)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(raw), "\n")
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				line := lines[fs.Position(lit.Pos()).Line-1]
				if forbidden.MatchString(value) {
					t.Errorf("%s:%d: 方言固有構文", name, fs.Position(lit.Pos()).Line)
				}
				if destructive.MatchString(value) && !(name == "assign.go" && line == deletion) {
					t.Errorf("%s:%d: 削除SQL", name, fs.Position(lit.Pos()).Line)
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "ExecContext" && sel.Sel.Name != "QueryContext" && sel.Sel.Name != "QueryRowContext") {
				return true
			}
			line := lines[fs.Position(call.Pos()).Line-1]
			if name == "assign.go" && line == deletion {
				return true
			}
			if len(call.Args) < 2 {
				t.Errorf("%s: SQL引数がありません", name)
				return true
			}
			bind, ok := call.Args[1].(*ast.CallExpr)
			if !ok {
				t.Errorf("%s:%d: bindSQLを経由していません", name, fs.Position(call.Pos()).Line)
				return true
			}
			id, ok := bind.Fun.(*ast.Ident)
			if !ok || id.Name != "bindSQL" || len(bind.Args) != 1 {
				t.Errorf("%s:%d: 不正なSQL変換", name, fs.Position(call.Pos()).Line)
				return true
			}
			lit, ok := bind.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s:%d: SQLはリテラルで指定してください", name, fs.Position(call.Pos()).Line)
				return true
			}
			query, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			if got := postgresBound(t, query); strings.Count(got, "$") != strings.Count(query, "?") || strings.Contains(got, "?") {
				t.Errorf("%s:%d: PostgreSQLへの展開が不正です", name, fs.Position(call.Pos()).Line)
			}
			if strings.Count(query, "?") != len(call.Args)-2 {
				t.Errorf("%s:%d: SQL引数の数が一致しません", name, fs.Position(call.Pos()).Line)
			}
			return true
		})
	}
}

// postgresBound expands the statement with the PostgreSQL dialect, the only one
// production uses, and restores the previous dialect.
func postgresBound(t *testing.T, query string) string {
	t.Helper()
	previous := appsql.SetActive(appsql.Postgres)
	defer appsql.SetActive(previous)
	return bindSQL(query)
}
