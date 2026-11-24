package operations

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

// PostgreSQL向けの呼び出し(receiverがv.Name == "tx")は対象外。
func TestSQLGuard(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)\b(INSERT\s+OR\s+IGNORE|rowid|BLOB|AUTOINCREMENT|json_extract|sqlite_master|BEGIN\s+IMMEDIATE)\b`)
	destructive := regexp.MustCompile(`(?i)\b(DELETE|DROP|TRUNCATE)\b`)
	for _, name := range []string{"attention.go", "incidents.go", "service.go", "sql.go"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		fs := token.NewFileSet()
		f, err := parser.ParseFile(fs, name, raw, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				if forbidden.MatchString(value) {
					t.Errorf("%s:%d: 方言固有構文", name, fs.Position(lit.Pos()).Line)
				}
				if destructive.MatchString(value) {
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
			if postgresReceiver(sel.X) {
				return true
			}
			line := fs.Position(call.Pos()).Line
			if len(call.Args) < 2 {
				t.Errorf("%s:%d: SQL引数がありません", name, line)
				return true
			}
			bind, ok := call.Args[1].(*ast.CallExpr)
			if !ok {
				if id, ok := call.Args[1].(*ast.Ident); ok && id.Name == "query" && name == "attention.go" {
					return true // switchのリテラル分岐でbindSQL済み(下の検査が分岐を見る)
				}
				t.Errorf("%s:%d: bindSQLを経由していません", name, line)
				return true
			}
			if id, ok := bind.Fun.(*ast.Ident); ok && id.Name == "tableExistsSQL" && len(bind.Args) == 0 {
				if exists, err := appsql.Postgres.TableExists(); err != nil || len(regexp.MustCompile(`\$\d+`).FindAllString(exists, -1)) != len(call.Args)-2 {
					t.Errorf("%s:%d: SQL引数の数が一致しません", name, line)
				}
				return true
			}
			id, ok := bind.Fun.(*ast.Ident)
			if !ok || id.Name != "bindSQL" || len(bind.Args) != 1 {
				t.Errorf("%s:%d: 不正なSQL変換", name, line)
				return true
			}
			query, ok := literalSQL(bind.Args[0])
			if !ok {
				t.Errorf("%s:%d: SQLはリテラル(とrowOrder)で指定してください", name, line)
				return true
			}
			if want, err := appsql.Postgres.Bind(query); err != nil || bindSQL(query) != want {
				t.Errorf("%s:%d: PostgreSQLの展開と一致しません", name, line)
			}
			if strings.Count(query, "?") != len(call.Args)-2 {
				t.Errorf("%s:%d: SQL引数の数が一致しません", name, line)
			}
			return true
		})
	}
}

// postgresReceiver は PostgreSQL 向け接続(PostgreSQL フィールド、そこから開いた tx)を判別する。
func postgresReceiver(x ast.Expr) bool {
	switch v := x.(type) {
	case *ast.SelectorExpr:
		return v.Sel.Name == "PostgreSQL"
	case *ast.Ident:
		return v.Name == "tx"
	}
	return false
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
