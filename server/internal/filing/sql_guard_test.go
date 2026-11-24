package filing

import (
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
	forbidden := regexp.MustCompile(`(?i)\b(INSERT\s+OR\s+IGNORE|rowid|BLOB|AUTOINCREMENT|json_extract|sqlite_master|BEGIN\s+IMMEDIATE)\b`)
	destructive := regexp.MustCompile(`(?i)\b(DELETE|DROP|TRUNCATE)\b`)
	const deletion = "\t\t_, err = s.options.Records.DB.ExecContext(ctx, \"DELETE FROM job_filings WHERE key=?\", key)"
	const deletionIf = "\tif _, err := s.options.Records.DB.ExecContext(ctx, \"DELETE FROM job_filings WHERE key=?\", key); err != nil {"
	for _, name := range []string{"outbox.go", "policy.go", "service.go", "split.go", "sql.go"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		wantDelete := 0
		if name == "outbox.go" {
			wantDelete = 3
		}
		if got := strings.Count(string(raw), deletion+"\n") + strings.Count(string(raw), deletionIf+"\n"); got != wantDelete {
			t.Errorf("%s: 削除行 %d、期待 %d", name, got, wantDelete)
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
				if forbidden.MatchString(value) && !(name == "sql.go" && strings.Contains(value, "rowid")) {
					t.Errorf("%s:%d: 方言固有構文", name, fs.Position(lit.Pos()).Line)
				}
				if destructive.MatchString(value) && !(name == "outbox.go" && (line == deletion || line == deletionIf)) {
					t.Errorf("%s:%d: 削除SQL", name, fs.Position(lit.Pos()).Line)
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
			if name == "outbox.go" && (line == deletion || line == deletionIf) {
				return true
			}
			first := 1
			if sel.Sel.Name == "Exec" {
				first = 0
			}
			if len(call.Args) < first+1 {
				t.Errorf("%s: SQL引数がありません", name)
				return true
			}
			bind, ok := call.Args[first].(*ast.CallExpr)
			if !ok {
				t.Errorf("%s:%d: bindSQLを経由していません", name, fs.Position(call.Pos()).Line)
				return true
			}
			if id, ok := bind.Fun.(*ast.Ident); ok && id.Name == "tableExistsSQL" && len(bind.Args) == 0 {
				if exists, err := appsql.Postgres.TableExists(); err != nil || strings.Count(exists, "$") != len(call.Args)-first-1 {
					t.Errorf("%s:%d: SQL引数の数が一致しません", name, fs.Position(call.Pos()).Line)
				}
				return true
			}
			id, ok := bind.Fun.(*ast.Ident)
			if ok && id.Name == "insertIgnoreSQL" {
				return true
			}
			if !ok || id.Name != "bindSQL" || len(bind.Args) != 1 {
				t.Errorf("%s:%d: 不正なSQL変換", name, fs.Position(call.Pos()).Line)
				return true
			}
			query, ok := constantSQL(bind.Args[0])
			if !ok {
				t.Errorf("%s:%d: SQLはリテラル（とrowOrder）で指定してください", name, fs.Position(call.Pos()).Line)
				return true
			}
			if want, err := appsql.Postgres.Bind(query); err != nil || bindSQL(query) != want {
				t.Errorf("%s:%d: PostgreSQL用のSQL展開が想定と異なります", name, fs.Position(call.Pos()).Line)
			}
			if strings.Count(query, "?") != len(call.Args)-first-1 {
				t.Errorf("%s:%d: SQL引数の数が一致しません", name, fs.Position(call.Pos()).Line)
			}
			return true
		})
	}
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
			return "rowid ASC", true
		}
	}
	return "", false
}
