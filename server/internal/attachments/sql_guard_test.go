package attachments

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

var forbiddenAttachmentSQL = regexp.MustCompile(`(?i)\b(INSERT\s+OR\s+IGNORE|rowid|AUTOINCREMENT|BLOB|DELETE|DROP|TRUNCATE)\b`)

// attachmentSQLViolations checks all string literals independently of the DB receiver.
func attachmentSQLViolations(name string, source []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, source, 0)
	if err != nil {
		return nil, err
	}
	var violations []string
	ast.Inspect(f, func(n ast.Node) bool {
		if literal, ok := n.(*ast.BasicLit); ok && literal.Kind == token.STRING {
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				violations = append(violations, fmt.Sprintf("%s: %v", fset.Position(literal.Pos()), err))
			} else if forbiddenAttachmentSQL.MatchString(value) {
				violations = append(violations, fset.Position(literal.Pos()).String())
			}
		}
		return true
	})
	return violations, nil
}

func TestAttachmentSQLHasNoDialectSpecificOrDestructiveSyntax(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		violations, err := attachmentSQLViolations(name, raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, position := range violations {
			t.Errorf("%s: 禁止されたSQL構文", position)
		}
	}
}

func TestAttachmentSQLGuardExamples(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		count        int
	}{
		{"delete", "package p; func f() { o.AppDB.Exec(`DELETE FROM images`) }", 1},
		{"drop", "package p; func f() { o.AppDB.Exec(`DROP TABLE images`) }", 1},
		{"truncate", "package p; func f() { o.AppDB.Exec(`TRUNCATE images`) }", 1},
		{"transaction", "package p; func f() { tx.Exec(`INSERT OR IGNORE INTO images VALUES (1)`); tx.Exec(`SELECT rowid FROM images`); tx.Exec(`CREATE TABLE t (id AUTOINCREMENT, data BLOB)`) }", 3},
		{"allowed", "package p; func f() { o.AppDB.Exec(`SELECT id FROM images WHERE id=?`); o.AppDB.Exec(`UPDATE images SET bound=1`); o.AppDB.Exec(`INSERT INTO images(id) VALUES (?)`); tx.Exec(`SELECT id FROM images`) }", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := attachmentSQLViolations(tc.name+".go", []byte(tc.source))
			if err != nil || len(got) != tc.count {
				t.Fatalf("violations=%v, err=%v, want %d", got, err, tc.count)
			}
		})
	}
}

func TestBindSQLExpandsPostgreSQLParameters(t *testing.T) {
	previous := appsql.SetActive(appsql.Postgres)
	defer appsql.SetActive(previous)
	query := "UPDATE drafts SET json=? WHERE id=?"
	if got, want := bindSQL(query), "UPDATE drafts SET json=$1 WHERE id=$2"; got != want {
		t.Fatalf("PostgreSQLの問い合わせが不正です: %q", got)
	}
}
