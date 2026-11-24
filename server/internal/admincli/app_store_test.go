package admincli

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/store"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/appsql"
)

func TestStoreWithoutDSNFailsAndNeverOpensSQLite(t *testing.T) {
	args, file := fixture(t)
	t.Setenv("ISSUE_DESK_DATABASE_URL", "")
	t.Setenv("PGHOST", "")
	before := appsql.CurrentDialect()
	for _, c := range [][]string{{"cli-key", "issue"}, {"pc-token", "issue", "h"}, {"pc-token", "list"}} {
		if out, _, err := command(t, append(append([]string{}, args...), c...), ""); err == nil || out != "" || !strings.Contains(err.Error(), "ISSUE_DESK_DATABASE_URL") {
			t.Fatalf("%v: DSN無しはエラーが必要です: %q %v", c, out, err)
		}
	}
	if appsql.CurrentDialect() != before {
		t.Fatal("失敗した起動が方言を切り替えました")
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("SQLiteファイルを作りました")
	}
}

func TestPostgresAppStoreCLIKeyAndPCToken(t *testing.T) {
	db := pgFixture(t) // 専用DBと隔離スキーマ（t.Setenvで接続先を設定済み）
	ctx := context.Background()
	schema := "admincli_app_" + strings.ReplaceAll(strings.ToLower(t.Name()), "/", "_")
	schema = regexp.MustCompile(`[^a-z0-9_]`).ReplaceAllString(schema, "_")
	if len(schema) > 60 {
		schema = schema[:60]
	}
	for _, q := range []string{
		"CREATE SCHEMA " + schema,
		"CREATE TABLE " + schema + ".cli_keys(hash TEXT PRIMARY KEY, expires BIGINT NOT NULL)",
		"CREATE TABLE " + schema + ".api_tokens(id TEXT PRIMARY KEY,name TEXT NOT NULL UNIQUE,hash TEXT NOT NULL UNIQUE,issued_at TEXT NOT NULL,last_used_at TEXT)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { db.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	appPostgresSchema = schema
	previous := appsql.CurrentDialect()
	t.Cleanup(func() { appPostgresSchema = ""; appsql.SetActive(previous) })
	t.Setenv("ISSUE_DESK_APP_STORE", "postgres")
	args, _ := fixture(t)

	out, _, err := command(t, append(args, "cli-key", "issue"), "")
	key := strings.TrimSpace(out)
	if err != nil || !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(key) {
		t.Fatal("一時キーを発行できません", err)
	}
	var expires int64
	if err = db.QueryRowContext(ctx, "SELECT expires FROM "+schema+".cli_keys WHERE hash=$1", hash(key)).Scan(&expires); err != nil || expires <= 0 {
		t.Fatal("一時キーが保存されていません", err)
	}
	if out, _, err = command(t, append(args, "cli-key", "revoke"), key+"\n"); err != nil || out != "" {
		t.Fatal("一時キーを取り消せません", err)
	}
	var n int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM "+schema+".cli_keys").Scan(&n); err != nil || n != 0 {
		t.Fatal("取り消しが反映されません", n, err)
	}

	issued, _, err := command(t, append(args, "pc-token", "issue", "pg-host"), "")
	token := strings.TrimSpace(issued)
	if err != nil || !strings.HasPrefix(token, "idk_") {
		t.Fatal("PCトークンを発行できません", err)
	}
	listed, _, err := command(t, append(args, "pc-token", "list"), "")
	if err != nil || !strings.Contains(listed, "pg-host") || strings.Contains(listed, token) {
		t.Fatal("一覧が不正です", err)
	}
	if _, _, err = command(t, append(args, "pc-token", "revoke", "pg-host"), ""); err != nil {
		t.Fatal("失効できません", err)
	}
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM "+schema+".api_tokens").Scan(&n); err != nil || n != 0 {
		t.Fatal("失効が反映されません", n, err)
	}
}

// dedicatedDatabase creates a database of its own: the application schema has a
// fixed name, so sharing the test database with other packages would collide.
func dedicatedDatabase(t *testing.T) (string, *sql.DB) {
	t.Helper()
	base := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if base == "" {
		t.Skip("専用PostgreSQL未設定")
	}
	u, err := url.Parse(base)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("専用DBのみ使用できます")
	}
	ctx := context.Background()
	admin, err := store.OpenPostgres(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := uuid()
	name := "issue_desk_test_app_" + strings.ReplaceAll(id, "-", "")[:12]
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := store.OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	previousDialect := appsql.CurrentDialect()
	t.Cleanup(func() {
		db.Close()
		admin.ExecContext(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		admin.Close()
		appsql.SetActive(previousDialect)
	})
	if _, err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ISSUE_DESK_DATABASE_URL", u.String())
	return u.String(), db
}

func TestUnmigratedPostgresStopsWithMigrationGuidance(t *testing.T) {
	dedicatedDatabase(t)
	args, file := fixture(t)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][]string{{"cli-key", "issue"}, {"pc-token", "list"}} {
		out, _, err := command(t, append(append([]string{}, args...), c...), "")
		if err == nil || out != "" || !strings.Contains(err.Error(), "docs/pg-cutover.md") {
			t.Fatalf("%v: 未移行は案内つきで止まります: %q %v", c, out, err)
		}
	}
	if raw, err := os.ReadFile(file); err != nil || string(raw) != "stale" {
		t.Fatal("SQLiteファイルに触れました")
	}
}

func TestMigratedPostgresIsUsedWithoutAnyStoreSetting(t *testing.T) {
	_, db := dedicatedDatabase(t)
	ctx := context.Background()
	for _, q := range []string{
		"CREATE SCHEMA IF NOT EXISTS issue_desk_app_migration",
		"CREATE TABLE issue_desk_app_migration.app_store_cutover (id INTEGER PRIMARY KEY CHECK (id = 1), completed_at BIGINT NOT NULL)",
		"INSERT INTO issue_desk_app_migration.app_store_cutover VALUES (1, 1)",
		"CREATE TABLE IF NOT EXISTS issue_desk_app_migration.cli_keys(hash TEXT PRIMARY KEY, expires BIGINT NOT NULL)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	args, file := fixture(t)
	out, _, err := command(t, append(args, "cli-key", "issue"), "")
	if err != nil || out == "" {
		t.Fatal("切替済みのPostgreSQLへ発行できません", err)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("SQLiteファイルを作りました")
	}
}
