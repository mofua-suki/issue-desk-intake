package projects

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"issue-desk-intake/server/internal/appsql"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestSQLGuard(t *testing.T) {
	for _, name := range []string{"conversation.go", "types.go", "sql.go"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		checkSQLGuard(name, raw, t.Errorf)
	}
}

// checkSQLGuard applies the same checks to every guarded production file.
func checkSQLGuard(name string, raw []byte, report func(string, ...any)) {
	forbidden := regexp.MustCompile(`(?i)\b(INSERT\s+OR\s+IGNORE|rowid|BLOB|AUTOINCREMENT)\b`)
	destructive := regexp.MustCompile(`(?i)\b(DELETE|DROP|TRUNCATE)\b`)
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
			_ = line
			if forbidden.MatchString(value) && !(name == "sql.go" && strings.Contains(value, "rowid")) {
				report("%s:%d: 方言固有構文", name, fs.Position(lit.Pos()).Line)
			}
			if destructive.MatchString(value) && !false {
				report("%s:%d: 削除SQL", name, fs.Position(lit.Pos()).Line)
			}
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "ExecContext" && sel.Sel.Name != "QueryContext" && sel.Sel.Name != "QueryRowContext" && sel.Sel.Name != "Exec") {
			return true
		}
		line := lines[fs.Position(call.Pos()).Line-1]
		_ = line
		if false {
			return true
		}
		first := 1
		if sel.Sel.Name == "Exec" {
			first = 0
		}
		if len(call.Args) < first+1 {
			report("%s: SQL引数がありません", name)
			return true
		}
		bind, ok := call.Args[first].(*ast.CallExpr)
		if !ok {
			report("%s:%d: bindSQLを経由していません", name, fs.Position(call.Pos()).Line)
			return true
		}
		id, ok := bind.Fun.(*ast.Ident)
		if ok && id.Name == "insertIgnoreSQL" {
			return true
		}
		if !ok || id.Name != "bindSQL" || len(bind.Args) != 1 {
			report("%s:%d: 不正なSQL変換", name, fs.Position(call.Pos()).Line)
			return true
		}
		query, ok := constantSQL(bind.Args[0])
		if !ok {
			report("%s:%d: SQLはリテラル（とrowOrder）で指定してください", name, fs.Position(call.Pos()).Line)
			return true
		}
		if want, err := appsql.Postgres.Bind(query); err != nil || bindSQL(query) != want {
			report("%s:%d: PostgreSQL用のSQL展開が想定と異なります", name, fs.Position(call.Pos()).Line)
		}
		if strings.Count(query, "?") != len(call.Args)-first-1 {
			report("%s:%d: SQL引数の数が一致しません", name, fs.Position(call.Pos()).Line)
		}
		return true
	})
}

// constantSQL accepts a string literal, or literals joined with rowOrder(...)
// calls, and returns the statement with each rowOrder replaced by a placeholder
// that expands like the PostgreSQL dialect.
func constantSQL(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		q, err := strconv.Unquote(v.Value)
		return q, err == nil
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, ok := constantSQL(v.X)
		if !ok {
			return "", false
		}
		r, ok := constantSQL(v.Y)
		return l + r, ok
	case *ast.CallExpr:
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "rowOrder" && len(v.Args) == 1 {
			return "rowid ASC", true
		}
	}
	return "", false
}

func TestSQLGuardApplicationDBPaths(t *testing.T) {
	for _, tc := range []struct{ name, statement, diagnostic string }{
		{"valid", `ExecContext(ctx, bindSQL("SELECT ?"), 1)`, ""},
		{"unbound", `ExecContext(ctx, "SELECT ?", 1)`, "bindSQL"},
		{"bad conversion", `ExecContext(ctx, other("SELECT ?"), 1)`, "不正なSQL変換"},
		{"arguments", `ExecContext(ctx, bindSQL("SELECT ?"))`, "SQL引数の数"},
		{"dialect", `ExecContext(ctx, bindSQL("SELECT rowid FROM x"))`, "方言固有構文"},
		{"delete", `ExecContext(ctx, bindSQL("DELETE FROM x"))`, "削除SQL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := "package projects\nfunc f() { o.AppDB." + tc.statement + " }"
			var errors []string
			checkSQLGuard("types.go", []byte(source), func(format string, args ...any) { errors = append(errors, fmt.Sprintf(format, args...)) })
			if tc.diagnostic == "" && len(errors) != 0 {
				t.Fatalf("unexpected diagnostics: %v", errors)
			}
			if tc.diagnostic != "" && !strings.Contains(strings.Join(errors, "\n"), tc.diagnostic) {
				t.Fatalf("missing %q in %v", tc.diagnostic, errors)
			}
		})
	}
}
