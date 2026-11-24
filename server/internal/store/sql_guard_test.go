package store

import (
	"errors"
	"fmt"
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

func TestApplicationSQLGuard(t *testing.T) {
	for _, problem := range checkApplicationSQL(".") {
		t.Error(problem)
	}
}

// checkApplicationSQL examines every existing canonical filename as well as
// the files that are always required. It is shared with the filesystem tests.
func checkApplicationSQL(dir string) []string {
	var problems []string
	files := []string{"sqlite.go", "application.go", "tokens.go", "sessions.go", "session-create.go"}
	canonical := 0
	forbidden := regexp.MustCompile(`(?i)\b(INSERT\s+OR\s+IGNORE|rowid|AUTOINCREMENT|BLOB)\b`)
	destructive := regexp.MustCompile(`(?i)\b(DELETE|DROP|TRUNCATE)\b`)
	// Exact original source lines, not a diff against an unavailable .git tree.
	// Moving SQL behind the common execution path must not broaden its predicates.
	allowed := map[string]int{
		"\tresult, err := s.db.ExecContext(ctx, \"DELETE FROM api_tokens WHERE name=?\", name)":                               1,
		"\t\tresult, err := s.db.ExecContext(ctx, \"DELETE FROM api_token_grants WHERE token_id=?\", id)":                     1,
		"\tresult, err := s.db.ExecContext(ctx, \"DELETE FROM cli_keys WHERE hash=? AND expires>=?\", hash, now.UnixMilli())": 1,
	}
	for _, file := range files {
		raw, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			if (file == "sqlite.go" || file == "application.go") && errors.Is(err, os.ErrNotExist) {
				continue
			}
			problems = append(problems, fmt.Sprintf("%s: 読取失敗: %v", file, err))
			continue
		}
		if file == "sqlite.go" || file == "application.go" {
			canonical++
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, raw, 0)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: parse失敗: %v", file, err))
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if literal, ok := n.(*ast.BasicLit); ok && literal.Kind == token.STRING {
				text, err := strconv.Unquote(literal.Value)
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s: 文字列の読取失敗: %v", file, err))
					return true
				}
				if forbidden.MatchString(text) {
					problems = append(problems, fmt.Sprintf("%s: SQL部品を通らない方言固有構文", file))
				}
			}
			return true
		})
		for _, line := range strings.Split(string(raw), "\n") {
			if destructive.MatchString(line) {
				if file != "tokens.go" || allowed[line] != 1 {
					problems = append(problems, fmt.Sprintf("%s: 許可されていない削除文の追加・変更", file))
				} else {
					allowed[line]--
				}
			}
		}
	}
	if canonical == 0 {
		problems = append(problems, "sqlite.go と application.go がありません")
	}
	for _, remaining := range allowed {
		if remaining != 0 {
			problems = append(problems, "既存の削除文の行が維持されていません")
		}
	}
	return problems
}

func TestApplicationSQLGuardFixtures(t *testing.T) {
	// Copy shipped sources so the rename test does not depend on a type alias
	// or a synthetic approximation of the actual SQL and deletion allowlist.
	fixture := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		for _, name := range []string{"tokens.go", "sessions.go", "session-create.go"} {
			raw, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
		// Either canonical source can be present when the repository itself is renamed.
		var raw []byte
		for _, name := range []string{"sqlite.go", "application.go"} {
			var err error
			raw, err = os.ReadFile(name)
			if err == nil {
				break
			}
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		}
		if raw == nil {
			t.Fatal("正本ファイルがありません")
		}
		if err := os.WriteFile(filepath.Join(dir, "sqlite.go"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	cases := []struct {
		name   string
		change func(*testing.T, string)
		want   string
	}{
		{"old only", func(*testing.T, string) {}, ""},
		{"new only", func(t *testing.T, d string) {
			if err := os.Rename(filepath.Join(d, "sqlite.go"), filepath.Join(d, "application.go")); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"both", func(t *testing.T, d string) { copyFixture(t, d, "sqlite.go", "application.go") }, ""},
		{"neither", func(t *testing.T, d string) {
			if err := os.Remove(filepath.Join(d, "sqlite.go")); err != nil {
				t.Fatal(err)
			}
		}, "sqlite.go と application.go がありません"},
		{"forbidden old with both", func(t *testing.T, d string) {
			copyFixture(t, d, "sqlite.go", "application.go")
			appendFixture(t, d, "sqlite.go", "\nvar forbidden = `BLOB`\n")
		}, "sqlite.go: SQL部品"},
		{"forbidden new with both", func(t *testing.T, d string) {
			copyFixture(t, d, "sqlite.go", "application.go")
			appendFixture(t, d, "application.go", "\nvar forbidden = `rowid`\n")
		}, "application.go: SQL部品"},
		{"read failure", func(t *testing.T, d string) {
			if err := os.Mkdir(filepath.Join(d, "application.go"), 0700); err != nil {
				t.Fatal(err)
			}
		}, "application.go: 読取失敗"},
		{"parse failure", func(t *testing.T, d string) { appendFixture(t, d, "sqlite.go", "\nfunc {\n") }, "sqlite.go: parse失敗"},
		{"missing required", func(t *testing.T, d string) {
			if err := os.Remove(filepath.Join(d, "sessions.go")); err != nil {
				t.Fatal(err)
			}
		}, "sessions.go: 読取失敗"},
		{"extra deletion", func(t *testing.T, d string) {
			appendFixture(t, d, "application.go", "package store\nvar q = `DELETE FROM x`\n")
		}, "application.go: 許可されていない削除文"},
		{"duplicate allowed deletion", func(t *testing.T, d string) {
			appendFixture(t, d, "tokens.go", "\nfunc duplicate() {\n\tresult, err := s.db.ExecContext(ctx, \"DELETE FROM api_tokens WHERE name=?\", name)\n}\n")
		}, "tokens.go: 許可されていない削除文"},
		{"missing allowed deletion", func(t *testing.T, d string) {
			p := filepath.Join(d, "tokens.go")
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			raw = []byte(strings.Replace(string(raw), "DELETE FROM api_tokens WHERE name=?", "SELECT name FROM api_tokens WHERE name=?", 1))
			if err := os.WriteFile(p, raw, 0600); err != nil {
				t.Fatal(err)
			}
		}, "既存の削除文の行が維持されていません"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := fixture(t)
			tc.change(t, d)
			got := strings.Join(checkApplicationSQL(d), "\n")
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
		})
	}
}

func copyFixture(t *testing.T, dir, from, to string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, from))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, to), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func appendFixture(t *testing.T, dir, name, text string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
