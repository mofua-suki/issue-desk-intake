package jobs

import (
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// applicationDBFiles are the only files that run SQL on the application database
// (Options.AppDB, PostgreSQL in production);
// SQL calls there must go through bindSQL. Other files use the shared database.
var applicationDBFiles = map[string]bool{"transfer_match.go": true, "transfer_records.go": true}

func TestSQLGuard(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)\b(INSERT\s+OR\s+IGNORE|rowid|BLOB|AUTOINCREMENT|json_extract|sqlite_master|BEGIN\s+IMMEDIATE)\b`)
	destructive := regexp.MustCompile(`(?i)\b(DELETE|DROP|TRUNCATE)\b`)
	allowedDestructive := map[string]int{"transfer_gate.go": 1}
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
		fs := token.NewFileSet()
		f, err := parser.ParseFile(fs, name, raw, 0)
		if err != nil {
			t.Fatal(err)
		}
		deletes := 0
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
					deletes++
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
			checkApplicationDBCall(t, fs, name, call, sel)
			return true
		})
		if deletes != allowedDestructive[name] {
			t.Errorf("%s: 削除SQL %d 件、期待 %d", name, deletes, allowedDestructive[name])
		}
	}
}

type guardReporter interface {
	Helper()
	Errorf(string, ...any)
}

func checkApplicationDBCall(t guardReporter, fs *token.FileSet, name string, call *ast.CallExpr, sel *ast.SelectorExpr) {
	if !isApplicationDBReceiver(sel.X) {
		return
	}
	if !applicationDBFiles[name] {
		t.Errorf("%s:%d: application DB access outside allowed files", name, fs.Position(call.Pos()).Line)
	} else if sel.Sel.Name == "ExecContext" || sel.Sel.Name == "QueryContext" || sel.Sel.Name == "QueryRowContext" {
		checkBound(t, fs, name, call)
	}
}

func checkBound(t guardReporter, fs *token.FileSet, name string, call *ast.CallExpr) {
	t.Helper()
	// Calls on shared database handles (tx, db) carry no application DB receiver.
	if recv, ok := call.Fun.(*ast.SelectorExpr); ok {
		if !isApplicationDBReceiver(recv.X) {
			return
		}
	}
	line := fs.Position(call.Pos()).Line
	if len(call.Args) < 2 {
		t.Errorf("%s:%d: SQL引数がありません", name, line)
		return
	}
	if id, ok := call.Args[1].(*ast.Ident); ok && id.Name == "query" {
		return // bound at the switch above the call
	}
	bind, ok := call.Args[1].(*ast.CallExpr)
	if !ok {
		t.Errorf("%s:%d: bindSQLを経由していません", name, line)
		return
	}
	id, ok := bind.Fun.(*ast.Ident)
	if !ok || id.Name != "bindSQL" || len(bind.Args) != 1 {
		t.Errorf("%s:%d: 不正なSQL変換", name, line)
		return
	}
	query, ok := constantSQL(bind.Args[0])
	if !ok {
		t.Errorf("%s:%d: SQLはリテラル（とrowOrder）で指定してください", name, line)
		return
	}
	if want, err := appsql.Postgres.Bind(query); err != nil || bindSQL(query) != want {
		t.Errorf("%s:%d: PostgreSQLの展開と一致しません", name, line)
	}
	if strings.Count(query, "?") != len(call.Args)-2 {
		t.Errorf("%s:%d: SQL引数の数が一致しません", name, line)
	}
}

// isApplicationDBReceiver recognizes all supported direct application DB paths.
func isApplicationDBReceiver(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.ParenExpr:
		return isApplicationDBReceiver(v.X)
	case *ast.SelectorExpr:
		return v.Sel.Name == "AppDB" || isApplicationDBReceiver(v.X)
	case *ast.CallExpr:
		return isApplicationDBReceiver(v.Fun)
	}
	return false
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

type guardErrors struct{ count int }

func (g *guardErrors) Helper()                   {}
func (g *guardErrors) Errorf(_ string, _ ...any) { g.count++ }

func TestSQLGuardApplicationDBPaths(t *testing.T) {
	for _, path := range []string{"s.options.AppDB"} {
		for _, tc := range []struct {
			name, file, query string
			violations        int
		}{
			{"unbound", "transfer_match.go", `"SELECT ?"`, 1},
			{"bound", "transfer_match.go", `bindSQL("SELECT ?")`, 0},
			{"wrong file", "other.go", `bindSQL("SELECT ?")`, 1},
			{"wrong count", "transfer_match.go", `bindSQL("SELECT ?, ?")`, 1},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				fs := token.NewFileSet()
				expr, err := parser.ParseExprFrom(fs, "fixture.go", path+".QueryContext(ctx, "+tc.query+", arg)", 0)
				if err != nil {
					t.Fatal(err)
				}
				call := expr.(*ast.CallExpr)
				g := &guardErrors{}
				checkApplicationDBCall(g, fs, tc.file, call, call.Fun.(*ast.SelectorExpr))
				if g.count != tc.violations {
					t.Fatalf("got %d violations, want %d", g.count, tc.violations)
				}
			})
		}
	}
}

// TestTransferApplicationDBSelection exercises the actual read paths using the
// production application schema with independently populated databases.
func TestTransferApplicationDBSelection(t *testing.T) {
	oldDB, newDB := pgtest.New(t), pgtest.New(t)
	for _, item := range []struct {
		db    *sql.DB
		value string
	}{{oldDB, "old"}, {newDB, "new"}} {
		for _, table := range []string{"drafts", "proposals"} {
			query := "INSERT INTO " + table + "(id,json) VALUES(?,?)"
			if _, err := item.db.Exec(query, "id", `{"id":"id","repo":"owner/repo","number":1,"value":"`+item.value+`"}`); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx := context.Background()
	s := &Store{}
	for _, tc := range []struct {
		name    string
		options Options
		want    string
	}{
		{"set", Options{AppDB: newDB}, "new"},
		{"none", Options{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.options = tc.options
			for _, table := range []string{"drafts", "proposals"} {
				row, err := s.readTransferLocal(ctx, table, "id")
				if err != nil {
					t.Fatal(err)
				}
				if tc.want == "" {
					if row != nil {
						t.Fatalf("%s: got %v", table, row)
					}
					continue
				}
				if row["value"] != tc.want {
					t.Fatalf("%s: got %v", table, row)
				}
			}
			rows, err := s.readTransferObservations(ctx, "owner/repo#1", []string{"id", "other"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(rows) != 0 {
					t.Fatalf("got %v", rows)
				}
				return
			}
			if len(rows) != 1 || rows[0]["value"] != tc.want {
				t.Fatalf("got %v", rows)
			}
		})
	}
	saved := map[string]any{"source": "owner/repo#1"}
	s.options.AppDB = nil
	got, err := s.resolveTransferRecords(ctx, saved)
	if err != nil || got["source"] != saved["source"] {
		t.Fatalf("nil DB: %v, %v", got, err)
	}
	s.options.AppDB = oldDB
	row, err := s.readTransferLocal(ctx, "drafts", "id")
	if err != nil || row["value"] != "old" {
		t.Fatalf("replacement: %v, %v", row, err)
	}
}
