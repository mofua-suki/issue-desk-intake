package store

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The application runs on PostgreSQL only. No non-test file may link a SQLite
// driver or open a SQLite database.
func TestProductionCodeDoesNotUseSQLite(t *testing.T) {
	root := filepath.Join("..", "..")
	forbidden := []string{
		`"modernc.org/sqlite"`,
		`internal/appdb"`,
		"OpenSQLite(",
		`sql.Open("sqlite`,
		"sql.Register(",
	}
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		slash := filepath.ToSlash(path)
		if d.IsDir() {
			if strings.HasSuffix(slash, "/internal/pgtest") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		checked++
		for _, word := range forbidden {
			if !strings.Contains(string(raw), word) {
				continue
			}
			t.Errorf("本番コード %s が SQLite を使っています: %s", slash, word)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 100 {
		t.Fatalf("検査した本番ファイルが少なすぎます: %d", checked)
	}
}

// OpenApp selects PostgreSQL unconditionally.
func TestApplicationStartupAlwaysSelectsPostgresDialect(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "startup", "cutover.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{"appsql.SetActive(appsql.Postgres)", "OpenPostgresApplicationStore(ctx, config, \"\")", "appcutover.Done("} {
		if !strings.Contains(text, required) {
			t.Errorf("起動時のPostgreSQL固定の契約がありません: %s", required)
		}
	}
}

// The SQLite driver is gone from the module, so no test can link it either.
func TestModuleHasNoSQLiteDriver(t *testing.T) {
	for _, name := range []string{"go.mod", "go.sum"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "modernc.org/sqlite") {
			t.Errorf("%s に SQLite ドライバが残っています", name)
		}
	}
}
