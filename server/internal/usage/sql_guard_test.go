package usage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestSQLGuard(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)\b(INSERT\s+OR\s+IGNORE|rowid|BLOB|AUTOINCREMENT)\b`)
	destructive := regexp.MustCompile(`(?i)\b(DELETE|DROP|TRUNCATE)\b`)
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
			return true
		})
	}
}

// TestNoSQLiteCalls keeps usage on the shared PostgreSQL database only.
// The application database was migrated from SQLite; do not reintroduce its
// driver or route application DB queries through this shared package.
func TestNoSQLiteCalls(t *testing.T) {
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
		if forbiddenApplicationDBAccess(string(raw)) {
			t.Errorf("%s: application DBへのアクセスは禁止です。usageは共有PostgreSQLだけを使ってください", name)
		}
	}
}

// Check references as well as calls, including DB values assigned to aliases.
// Raw-text matching also preserves the original bindSQL check outside calls.
func forbiddenApplicationDBAccess(source string) bool {
	for _, forbidden := range []string{".SQLite", ".AppDB", "bindSQL"} {
		if strings.Contains(source, forbidden) {
			return true
		}
	}
	return false
}

func TestApplicationDBBoundaryFixtures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
	}{
		{"legacy call", `o.SQLite.Query("SELECT 1")`},
		{"legacy field", `db := o.SQLite; db.Query("SELECT 1")`},
		{"application call", `o.AppDB.Query("SELECT 1")`},
		{"application field", `db := o.AppDB; db.Query("SELECT 1")`},
		{"binding", `query := bindSQL("SELECT ?")`},
		{"binding reference", `binding := bindSQL`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !forbiddenApplicationDBAccess(tc.source) {
				t.Errorf("application DBへの禁止経路を検出できません: %s", tc.source)
			}
		})
	}
	if forbiddenApplicationDBAccess(`s.DB.QueryContext(ctx, "SELECT key,value FROM settings")`) {
		t.Error("共有PostgreSQLへの正当なアクセスを拒否しました")
	}
}
