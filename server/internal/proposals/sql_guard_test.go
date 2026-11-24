package proposals

import (
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

// TestSQLGuard keeps the SQL statements of this package portable: every
// call goes through bindSQL (or insertIgnoreSQL) with a literal statement, no
// dialect-specific syntax is reintroduced, and no deletion SQL is added.
// Calls on the PostgreSQL (shared database) handle keep their own $n syntax.
func TestSQLGuard(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		checkSQLGuard(t, name, raw)
	}
}

type sqlGuardReporter interface {
	Errorf(string, ...any)
	Fatal(...any)
}

func checkSQLGuard(t sqlGuardReporter, name string, raw []byte) {
	forbidden := regexp.MustCompile(`(?i)\b(INSERT\s+OR\s+IGNORE|rowid|BLOB|AUTOINCREMENT)\b`)
	destructive := regexp.MustCompile(`(?i)\b(DELETE|DROP|TRUNCATE)\b`)
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
				t.Errorf("%s:%d: 削除SQLの追加は承認が必要です", name, fs.Position(lit.Pos()).Line)
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
		if strings.Contains(string(raw[fs.Position(sel.X.Pos()).Offset:fs.Position(sel.X.End()).Offset]), "Postgres") {
			return true
		}
		line := fs.Position(call.Pos()).Line
		if len(call.Args) < 2 {
			t.Errorf("%s:%d: SQL引数がありません", name, line)
			return true
		}
		wrap, ok := call.Args[1].(*ast.CallExpr)
		if !ok {
			t.Errorf("%s:%d: bindSQLを経由していません", name, line)
			return true
		}
		id, ok := wrap.Fun.(*ast.Ident)
		if !ok {
			t.Errorf("%s:%d: 不正なSQL変換", name, line)
			return true
		}
		switch id.Name {
		case "bindSQL":
			if len(wrap.Args) != 1 {
				t.Errorf("%s:%d: 不正なSQL変換", name, line)
				return true
			}
			lit, ok := wrap.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s:%d: SQLはリテラルで指定してください", name, line)
				return true
			}
			query, _ := strconv.Unquote(lit.Value)
			if want, err := appsql.Postgres.Bind(query); err != nil || bindSQL(query) != want {
				t.Errorf("%s:%d: PostgreSQL用のSQL展開が想定と異なります", name, line)
			}
			if strings.Count(query, "?") != len(call.Args)-2 {
				t.Errorf("%s:%d: SQL引数の数が一致しません", name, line)
			}
		case "insertIgnoreSQL":
			if len(wrap.Args) != 3 {
				t.Errorf("%s:%d: 不正なSQL変換", name, line)
				return true
			}
			if _, ok := wrap.Args[2].(*ast.BasicLit); !ok {
				t.Errorf("%s:%d: INSERTの値はリテラルで指定してください", name, line)
			}
		default:
			t.Errorf("%s:%d: bindSQLを経由していません", name, line)
		}
		return true
	})
}

type sqlGuardCapture struct{ errors int }

func (c *sqlGuardCapture) Errorf(string, ...any) { c.errors++ }
func (c *sqlGuardCapture) Fatal(args ...any)     { panic(args) }

func TestSQLGuardApplicationDBPaths(t *testing.T) {
	for _, receiver := range []string{"s.o.AppDB"} {
		for _, method := range []string{"ExecContext", "QueryContext", "QueryRowContext"} {
			for _, tc := range []struct {
				name, query string
				rejected    bool
			}{
				{"bound", "bindSQL(`SELECT ?`), 1", false},
				{"unwrapped", "`SELECT ?`, 1", true},
				{"nonliteral", "bindSQL(query), 1", true},
				{"wrong argument count", "bindSQL(`SELECT ?`)", true},
				{"forbidden dialect", "bindSQL(`SELECT rowid`)", true},
				{"destructive", "bindSQL(`DELETE FROM things`)", true},
			} {
				t.Run(receiver+"/"+method+"/"+tc.name, func(t *testing.T) {
					source := "package proposals\nfunc f() { " + receiver + "." + method + "(ctx, " + tc.query + ") }"
					c := &sqlGuardCapture{}
					checkSQLGuard(c, "fixture.go", []byte(source))
					if (c.errors > 0) != tc.rejected {
						t.Errorf("errors = %d, rejected = %v", c.errors, tc.rejected)
					}
				})
			}
		}
	}
}
