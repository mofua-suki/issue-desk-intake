package filingcheck

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
	for _, name := range []string{"recovery.go", "sql.go"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, problem := range sqlGuardProblems(name, raw) {
			t.Error(problem)
		}
	}
}

func sqlGuardProblems(name string, raw []byte) []string {
	var problems []string
	report := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	forbidden := regexp.MustCompile(`(?i)\b(INSERT\s+OR\s+IGNORE|rowid|BLOB|AUTOINCREMENT)\b`)
	destructive := regexp.MustCompile(`(?i)\b(DELETE|DROP|TRUNCATE)\b`)
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, name, raw, 0)
	if err != nil {
		return []string{err.Error()}
	}
	lines := strings.Split(string(raw), "\n")
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				problems = append(problems, err.Error())
				return true
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
	return problems
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

func TestSQLGuardDatabaseExpressions(t *testing.T) {
	for _, receiver := range []string{"s.options.AppDB"} {
		for _, tc := range []struct {
			name, call string
			allowed    bool
		}{
			{"bound", `QueryRowContext(ctx, bindSQL("SELECT x FROM t WHERE id=?"), id)`, true},
			{"row order", `QueryContext(ctx, bindSQL("SELECT x FROM t ORDER BY "+rowOrder("x")))`, true},
			{"insert helper", `ExecContext(ctx, insertIgnoreSQL())`, true},
			{"unbound", `QueryContext(ctx, "SELECT x FROM t")`, false},
			{"wrong count", `ExecContext(ctx, bindSQL("UPDATE t SET x=?"))`, false},
			{"dynamic", `ExecContext(ctx, bindSQL(query))`, false},
			{"forbidden", `ExecContext(ctx, bindSQL("DELETE FROM t"))`, false},
		} {
			t.Run(receiver+"/"+tc.name, func(t *testing.T) {
				source := []byte("package filingcheck\nfunc f() { " + receiver + "." + tc.call + " }")
				problems := sqlGuardProblems("fixture.go", source)
				if (len(problems) == 0) != tc.allowed {
					t.Errorf("problems = %v, allowed = %v", problems, tc.allowed)
				}
			})
		}
	}
}
