package statistics

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"issue-desk-intake/server/internal/appsql"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestSQLGuard(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		checkSQLGuard(name, raw, t.Errorf)
	}
}

func checkSQLGuard(name string, raw []byte, report func(string, ...any)) {
	forbidden := regexp.MustCompile(`(?i)\b(INSERT\s+OR\s+IGNORE|rowid|BLOB|AUTOINCREMENT|json_extract|sqlite_master|BEGIN\s+IMMEDIATE)\b`)
	destructive := regexp.MustCompile(`(?i)\b(DELETE|DROP|TRUNCATE)\b`)
	applicationFiles := map[string]bool{"issue_usage_read.go": true, "scope.go": true, "sql.go": true, "usage_outcomes.go": true, "usage_review.go": true}
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, name, raw, 0)
	if err != nil {
		report("%s: %v", name, err)
		return
	}
	// Do not allow a local alias to hide an application DB receiver from the
	// call checks below. Readers use the field directly at the SQL boundary.
	ast.Inspect(f, func(n ast.Node) bool {
		var values []ast.Expr
		switch v := n.(type) {
		case *ast.AssignStmt:
			values = v.Rhs
		case *ast.ValueSpec:
			values = v.Values
		}
		for _, value := range values {
			r := receiver(value)
			if r == "s.AppDB" {
				report("%s:%d: アプリケーションDBの別名を使用できません", name, fs.Position(value.Pos()).Line)
			}
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				report("%s: %v", name, err)
				return true
			}
			if forbidden.MatchString(value) {
				report("%s:%d: 方言固有構文", name, fs.Position(lit.Pos()).Line)
			}
			if destructive.MatchString(value) {
				report("%s:%d: 削除SQL", name, fs.Position(lit.Pos()).Line)
			}
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "ExecContext", "QueryContext", "QueryRowContext", "Exec", "Query", "QueryRow":
		default:
			return true
		}
		recv := receiver(sel.X)
		if recv != "s.AppDB" && recv != "ltx" && recv != "db" {
			if strings.Contains(recv, "SQLite") || strings.Contains(recv, "AppDB") || strings.Contains(recv, "local") {
				report("%s:%d: アプリケーションDB接続の呼び出しを確認してください", name, fs.Position(call.Pos()).Line)
			}
			return true
		}
		if recv == "db" && name != "sql.go" {
			return true
		}
		if !applicationFiles[name] {
			report("%s:%d: アプリケーションDBへの呼び出しはbindSQL経由のファイルへ置いてください", name, fs.Position(call.Pos()).Line)
			return true
		}
		sqlIndex := 0
		if strings.HasSuffix(sel.Sel.Name, "Context") {
			sqlIndex = 1
		}
		if len(call.Args) <= sqlIndex {
			report("%s: SQL引数がありません", name)
			return true
		}
		bind, ok := call.Args[sqlIndex].(*ast.CallExpr)
		if !ok {
			report("%s:%d: bindSQLを経由していません", name, fs.Position(call.Pos()).Line)
			return true
		}
		if id, ok := bind.Fun.(*ast.Ident); ok && id.Name == "tableExistsSQL" && len(bind.Args) == 0 {
			if exists, err := appsql.Postgres.TableExists(); err != nil || len(regexp.MustCompile(`\$\d+`).FindAllString(exists, -1)) != len(call.Args)-sqlIndex-1 {
				report("%s:%d: SQL引数の数が一致しません", name, fs.Position(call.Pos()).Line)
			}
			return true
		}
		id, ok := bind.Fun.(*ast.Ident)
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
			report("%s:%d: PostgreSQLの展開と一致しません", name, fs.Position(call.Pos()).Line)
		}
		if strings.Count(query, "?") != len(call.Args)-sqlIndex-1 {
			report("%s:%d: SQL引数の数が一致しません", name, fs.Position(call.Pos()).Line)
		}
		return true
	})
}

func receiver(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return receiver(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		if len(v.Args) == 0 {
			return receiver(v.Fun) + "()"
		}
	}
	return ""
}

// constantSQL accepts a string literal, or literals joined with rowOrder(...)
// calls, and returns the statement with each rowOrder replaced by a placeholder
// in the PostgreSQL dialect.
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
		if id, ok := v.Fun.(*ast.Ident); ok && (id.Name == "jsonText" || id.Name == "jsonNumber") && len(v.Args) == 2 {
			column, ok1 := v.Args[0].(*ast.BasicLit)
			path, ok2 := v.Args[1].(*ast.BasicLit)
			if !ok1 || !ok2 || column.Kind != token.STRING || path.Kind != token.STRING {
				return "", false
			}
			c, err1 := strconv.Unquote(column.Value)
			p, err2 := strconv.Unquote(path.Value)
			if err1 != nil || err2 != nil {
				return "", false
			}
			expr, err := appsql.Postgres.JSONText(c, p)
			if id.Name == "jsonNumber" {
				expr, err = appsql.Postgres.JSONNumber(c, p)
			}
			return expr, err == nil
		}
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "rowOrder" && len(v.Args) == 1 {
			return "source_rowid ASC", true
		}
	}
	return "", false
}

// TestApplicationDBCallsStayInBoundFiles keeps bindSQL calls inside the
// application database SQL boundary files.
func TestApplicationDBCallsStayInBoundFiles(t *testing.T) {
	applicationFiles := map[string]bool{"issue_usage_read.go": true, "scope.go": true, "sql.go": true, "usage_outcomes.go": true, "usage_review.go": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || applicationFiles[name] {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "bindSQL(") {
			t.Errorf("%s: アプリケーションDBへの呼び出しはbindSQL経由のファイルへ置いてください", name)
		}
	}
}

func TestSQLGuardRejectsApplicationDBAliases(t *testing.T) {
	for _, recv := range []string{"s.AppDB"} {
		for _, name := range []string{"scope.go", "usage.go"} {
			source := "package statistics\nfunc f() { db := " + recv + "; db.QueryContext(ctx, \"SELECT 1\") }"
			var diagnostics []string
			checkSQLGuard(name, []byte(source), func(format string, args ...any) { diagnostics = append(diagnostics, fmt.Sprintf(format, args...)) })
			if !strings.Contains(strings.Join(diagnostics, "\n"), "別名") {
				t.Errorf("%s in %s: alias was not rejected: %v", recv, name, diagnostics)
			}
		}
	}
}

func TestSQLGuardApplicationDBPaths(t *testing.T) {
	for _, tc := range []struct{ name, file, statement, diagnostic string }{
		{"valid", "scope.go", `ExecContext(ctx, bindSQL("SELECT ?"), 1)`, ""},
		{"valid no context", "scope.go", `Exec(bindSQL("SELECT ?"), 1)`, ""},
		{"disallowed file", "usage.go", `ExecContext(ctx, bindSQL("SELECT ?"), 1)`, "bindSQL"},
		{"unbound", "scope.go", `ExecContext(ctx, "SELECT ?", 1)`, "bindSQL"},
		{"conversion", "scope.go", `ExecContext(ctx, other("SELECT ?"), 1)`, "不正なSQL変換"},
		{"arguments", "scope.go", `ExecContext(ctx, bindSQL("SELECT ?"))`, "SQL引数の数"},
		{"dialect", "scope.go", `ExecContext(ctx, bindSQL("SELECT rowid FROM x"))`, "方言固有構文"},
		{"delete", "scope.go", `ExecContext(ctx, bindSQL("DELETE FROM x"))`, "削除SQL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := "package statistics\nfunc f() { s.AppDB." + tc.statement + " }"
			var diagnostics []string
			checkSQLGuard(tc.file, []byte(source), func(format string, args ...any) { diagnostics = append(diagnostics, fmt.Sprintf(format, args...)) })
			if tc.diagnostic == "" && len(diagnostics) != 0 {
				t.Fatalf("unexpected diagnostics: %v", diagnostics)
			}
			if tc.diagnostic != "" && !strings.Contains(strings.Join(diagnostics, "\n"), tc.diagnostic) {
				t.Fatalf("missing %q in %v", tc.diagnostic, diagnostics)
			}
		})
	}
}
