package images

import (
	"fmt"
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
	for _, name := range []string{"http.go", "store.go", "write_scope.go", "sql.go"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		checkSQLGuard(t, name, raw, t.Errorf)
	}
}

func checkSQLGuard(t *testing.T, name string, raw []byte, report func(string, ...any)) {
	t.Helper()
	forbidden := regexp.MustCompile(`(?i)\b(INSERT\s+OR\s+IGNORE|rowid|BLOB|AUTOINCREMENT)\b`)
	destructive := regexp.MustCompile(`(?i)\b(DELETE|DROP|TRUNCATE)\b`)
	const deletion = "\tresult, e := tx.ExecContext(ctx, \"DELETE FROM images WHERE id=? AND scope=? AND bound=0\", id, scope)"
	want := 0
	if name == "store.go" {
		want = 1
	}
	if got := strings.Count(string(raw), deletion+"\n"); got != want {
		report("%s: 削除行 %d、期待 %d", name, got, want)
	}
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, name, raw, 0)
	if err != nil {
		report("%s: %v", name, err)
		return
	}
	lines := strings.Split(string(raw), "\n")
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				report("%s: %v", name, err)
				return false
			}
			line := lines[fs.Position(lit.Pos()).Line-1]
			if forbidden.MatchString(value) {
				report("%s:%d: 方言固有構文", name, fs.Position(lit.Pos()).Line)
			}
			if destructive.MatchString(value) && !(name == "store.go" && line == deletion) {
				report("%s:%d: 削除SQL", name, fs.Position(lit.Pos()).Line)
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
		if name == "store.go" && line == deletion {
			return true
		}
		if len(call.Args) < 2 {
			report("%s: SQL引数がありません", name)
			return true
		}
		bind, ok := call.Args[1].(*ast.CallExpr)
		if !ok {
			report("%s:%d: bindSQLを経由していません", name, fs.Position(call.Pos()).Line)
			return true
		}
		id, ok := bind.Fun.(*ast.Ident)
		if !ok || id.Name != "bindSQL" || len(bind.Args) != 1 {
			report("%s:%d: 不正なSQL変換", name, fs.Position(call.Pos()).Line)
			return true
		}
		query, ok := literalSQL(bind.Args[0])
		if !ok {
			report("%s:%d: SQLはリテラル(とrowOrder)で指定してください", name, fs.Position(call.Pos()).Line)
			return true
		}
		if got := postgresBound(t, query); strings.Count(got, "$") != strings.Count(query, "?") || strings.Contains(got, "?") {
			report("%s:%d: PostgreSQLへの展開が不正です", name, fs.Position(call.Pos()).Line)
		}
		if strings.Count(query, "?") != len(call.Args)-2 {
			report("%s:%d: SQL引数の数が一致しません", name, fs.Position(call.Pos()).Line)
		}
		return true
	})
}

// literalSQL は文字列リテラルと rowOrder() 呼び出しだけの連結を受け付ける。
func literalSQL(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		l, ok1 := literalSQL(v.X)
		r, ok2 := literalSQL(v.Y)
		return l + r, v.Op == token.ADD && ok1 && ok2
	case *ast.CallExpr:
		id, ok := v.Fun.(*ast.Ident)
		return "", ok && id.Name == "rowOrder" && len(v.Args) == 1
	}
	return "", false
}

// postgresBound expands the statement with the PostgreSQL dialect, the only one
// production uses, and restores the previous dialect.
func postgresBound(t *testing.T, query string) string {
	t.Helper()
	previous := appsql.SetActive(appsql.Postgres)
	defer appsql.SetActive(previous)
	return bindSQL(query)
}

func TestSQLGuardDBAccessPaths(t *testing.T) {
	for _, path := range []string{"opts.AppDB"} {
		for _, tc := range []struct {
			name, query string
			wantError   bool
		}{
			{"valid", `bindSQL("SELECT ?")`, false},
			{"unbound", `"SELECT ?"`, true},
			{"forbidden", `bindSQL("SELECT rowid FROM images WHERE id=?")`, true},
		} {
			t.Run(fmt.Sprintf("%s/%s", path, tc.name), func(t *testing.T) {
				source := fmt.Sprintf("package images\nfunc f() { %s.QueryContext(ctx, %s, id) }\n", path, tc.query)
				var errors []string
				checkSQLGuard(t, "fixture.go", []byte(source), func(format string, args ...any) { errors = append(errors, fmt.Sprintf(format, args...)) })
				if (len(errors) > 0) != tc.wantError {
					t.Fatalf("errors = %v, want error %v", errors, tc.wantError)
				}
			})
		}
	}
}
