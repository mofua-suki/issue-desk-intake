package adminapi

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

// The application statements of these files go through bindSQL / insertIgnoreSQL.
// api.go and sql.go are permission-scope files and are not checked here; the
// shared PostgreSQL statements (grants.go, ssh.go) go through repairTx.
var sqlGuardFiles = []string{"ai_decisions.go", "bind.go", "decision_mirror.go", "decisions.go", "json_page.go", "tokens.go"}

const (
	tokenDeletion = "\t\t_, e = tx.ExecContext(r.Context(), \"DELETE FROM api_token_grants WHERE token_id=?\", parts[0])"
	pragmaLine    = "\trows, e := s.AppDB.QueryContext(ctx, statement)"
)

func TestSQLGuard(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)\b(INSERT\s+OR\s+IGNORE|rowid|BLOB|AUTOINCREMENT|json_extract|COLLATE|PRAGMA)\b`)
	destructive := regexp.MustCompile(`(?i)\b(DELETE|DROP|TRUNCATE)\b`)
	for _, name := range sqlGuardFiles {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		deletions := 0
		for _, line := range strings.Split(string(raw), "\n") {
			if destructive.MatchString(line) {
				if name == "tokens.go" && line == tokenDeletion {
					deletions++
				} else {
					t.Errorf("%s: 許可されていない削除文の追加・変更: %s", name, line)
				}
			}
		}
		if want := map[bool]int{true: 1, false: 0}[name == "tokens.go"]; deletions != want {
			t.Errorf("%s: 削除行 %d、期待 %d", name, deletions, want)
		}
		fs := token.NewFileSet()
		f, err := parser.ParseFile(fs, name, raw, 0)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(raw), "\n")
		// The compact statistics reader is covered by real PostgreSQL compatibility,
		// transaction recovery, and saved-row invariance tests. Its SQL call's source
		// shape is not a contract. Destructive/dialect checks still cover this body.
		var statsBody *ast.BlockStmt
		if name == "ai_decisions.go" {
			for _, decl := range f.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "aiDecisionStatsFields" {
					statsBody = fn.Body
				}
			}
		}
		consts := map[string]string{}
		for _, decl := range f.Decls {
			if g, ok := decl.(*ast.GenDecl); ok && g.Tok == token.CONST {
				for _, spec := range g.Specs {
					v := spec.(*ast.ValueSpec)
					if len(v.Names) == 1 && len(v.Values) == 1 {
						if q, ok := constantSQL(v.Values[0], consts); ok {
							consts[v.Names[0].Name] = q
						}
					}
				}
			}
		}
		found := map[string]int{}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range forbidden.FindAllString(value, -1) {
					found[strings.ToLower(m)]++
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
			if statsBody != nil && call.Pos() >= statsBody.Pos() && call.End() <= statsBody.End() {
				return true
			}
			pos := fs.Position(call.Pos())
			line := lines[pos.Line-1]
			if (name == "tokens.go" && line == tokenDeletion) || (name == "decision_mirror.go" && line == pragmaLine) {
				return true
			}
			if len(call.Args) < 2 {
				t.Errorf("%s:%d: SQL引数がありません", name, pos.Line)
				return true
			}
			wrap, ok := call.Args[1].(*ast.CallExpr)
			id, isIdent := ast.Expr(nil), false
			if ok {
				id = wrap.Fun
				_, isIdent = id.(*ast.Ident)
			}
			if !isIdent || (id.(*ast.Ident).Name != "bindSQL" && id.(*ast.Ident).Name != "insertIgnoreSQL") {
				t.Errorf("%s:%d: bindSQLを経由していません", name, pos.Line)
				return true
			}
			if id.(*ast.Ident).Name == "bindSQL" && name != "json_page.go" {
				query, ok := constantSQL(wrap.Args[0], consts)
				if !ok {
					t.Errorf("%s:%d: SQLはリテラルで指定してください", name, pos.Line)
				} else if got := postgresBound(t, query); strings.Contains(got, "?") || strings.Count(got, "$") != strings.Count(query, "?") || strings.Count(query, "?") != len(call.Args)-2 {
					t.Errorf("%s:%d: PostgreSQLへの展開が不正か引数の数が一致しません", name, pos.Line)
				}
			}
			return true
		})
		// jsonPage builds its statement from the caller's table, where and order,
		// which must be literals.
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "jsonPage" && len(call.Args) >= 5 {
				for _, i := range []int{2, 3, 4} {
					if builder(call.Args[i]) {
						continue
					}
					if lit, ok := call.Args[i].(*ast.BasicLit); !ok || lit.Kind != token.STRING {
						t.Errorf("%s:%d: jsonPageのtable・where・orderはリテラルか、引数なしの組み立て関数で指定してください", name, fs.Position(call.Pos()).Line)
					}
				}
			}
			return true
		})
		for word, n := range found {
			t.Errorf("%s: 方言固有構文 %s が %d 件（一律禁止。方言の部品を使ってください）", name, word, n)
		}
	}
}

// sqlBuilders are the argument-less functions of dialect_sql.go that build a
// jsonPage fragment from the dialect parts; their own literals are checked
// above, so no dialect word can hide in them.
var sqlBuilders = map[string]bool{"decisionsWhere": true, "decisionsOrder": true, "aiDecisionsWhere": true, "aiDecisionsOrder": true}

func builder(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && sqlBuilders[id.Name]
}

// jsonListSQL reconstructs the known SQL expression, rejecting unknown builders
// so that an indirect or split literal cannot bypass the rowid check.
func jsonListSQL(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.ParenExpr:
		return jsonListSQL(v.X)
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			s, err := strconv.Unquote(v.Value)
			return s, err == nil
		}
	case *ast.Ident:
		if v.Name == "table" {
			return "<table>", true
		}
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			l, lok := jsonListSQL(v.X)
			r, rok := jsonListSQL(v.Y)
			return l + r, lok && rok
		}
	case *ast.CallExpr:
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "rowOrderSQL" && len(v.Args) == 1 {
			if arg, ok := v.Args[0].(*ast.Ident); ok && arg.Name == "false" {
				return "<rowOrderSQL(false)>", true
			}
		}
	}
	return "", false
}

var standaloneRowID = regexp.MustCompile(`(?i)\browid\b`)

func validJSONListSQL(e ast.Expr) bool {
	query, ok := jsonListSQL(e)
	return ok && !standaloneRowID.MatchString(query) && query == "SELECT json FROM <table> ORDER BY <rowOrderSQL(false)>"
}

func TestJSONListSQLGuard(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "api.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	queries := 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "jsonList" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if ok && (sel.Sel.Name == "QueryContext" || sel.Sel.Name == "QueryRowContext" || sel.Sel.Name == "ExecContext") {
				queries++
				if len(call.Args) < 2 || !validJSONListSQL(call.Args[1]) {
					t.Error("jsonListのSQLは方言の追加順を使い、独立したrowidを含めないでください")
				}
			}
			return true
		})
	}
	if queries != 1 {
		t.Errorf("jsonListのSQL呼び出し数 = %d、期待 1", queries)
	}
}

func TestJSONListSQLGuardExpressions(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want bool
	}{
		{`"SELECT json FROM "+table+" ORDER BY "+rowOrderSQL(false)`, true},
		{`"SELECT source_rowid FROM "+table+" ORDER BY "+rowOrderSQL(false)`, false},
		{`"SELECT json FROM "+table+" ORDER BY rowid"`, false},
		{`"SELECT json FROM "+table+" ORDER BY RoW"+"Id"`, false},
	} {
		e, err := parser.ParseExpr(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		if got := validJSONListSQL(e); got != tc.want {
			t.Errorf("%s: got %v", tc.expr, got)
		}
	}
	if standaloneRowID.MatchString("source_rowid") || !standaloneRowID.MatchString("RoWiD") {
		t.Error("rowidの識別に失敗")
	}
}

// constantSQL accepts string literals, known constants and their sums.
func constantSQL(e ast.Expr, consts map[string]string) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		q, err := strconv.Unquote(v.Value)
		return q, err == nil
	case *ast.Ident:
		q, ok := consts[v.Name]
		return q, ok
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, ok := constantSQL(v.X, consts)
		if !ok {
			return "", false
		}
		r, ok := constantSQL(v.Y, consts)
		return l + r, ok
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
