package startup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"issue-desk-intake/server/internal/appcutover"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/store"
)

// cutoverDatabase creates a database of its own: the migration schema has a fixed
// name, so sharing the test database with other packages would make them collide.
func cutoverDatabase(t *testing.T) (string, *sql.DB) {
	t.Helper()
	base := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if base == "" {
		t.Skip("隔離した検証用PostgreSQLのDSNが未設定です")
	}
	u, err := url.Parse(base)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("明示的な検証DBが必要です")
	}
	admin, err := store.OpenPostgres(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 6)
	_, _ = rand.Read(nonce)
	name := "issue_desk_test_cutover_" + hex.EncodeToString(nonce)
	if _, err = admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := store.OpenPostgres(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		admin.Exec("DROP DATABASE " + name + " WITH (FORCE)")
		admin.Close()
	})
	return u.String(), db
}

func cutoverConfig(t *testing.T, dsn string) *Config {
	t.Helper()
	runtime := t.TempDir()
	return &Config{AppPostgresDSN: dsn, RuntimeDir: runtime, SQLiteFile: filepath.Join(runtime, "state", "intake.sqlite"), LedgerFile: filepath.Join(runtime, "state", "intake-desk-numbers.jsonl")}
}

func TestOpenPostgresApplicationStoreRequiresConnectionSettings(t *testing.T) {
	t.Setenv("PGHOST", "")
	config := cutoverConfig(t, "")
	before := appsql.CurrentDialect()
	_, err := OpenPostgresApplicationStore(context.Background(), config, "")
	if err == nil || !strings.Contains(err.Error(), "ISSUE_DESK_DATABASE_URL") {
		t.Fatalf("err=%v", err)
	}
	if appsql.CurrentDialect() != before {
		t.Fatal("失敗した起動が方言を切り替えました")
	}
	if _, statErr := os.Stat(config.SQLiteFile); statErr == nil {
		t.Fatal("SQLiteファイルを作りました")
	}
}

func TestOpenPostgresApplicationStoreRefusesUnmigratedStateWithGuidance(t *testing.T) {
	dsn, _ := cutoverDatabase(t)
	config := cutoverConfig(t, dsn)
	before := appsql.CurrentDialect()
	// A fresh state: no flag and no SQLite file. Never start on an empty database.
	_, err := OpenPostgresApplicationStore(context.Background(), config, "")
	if err == nil || !strings.Contains(err.Error(), "docs/pg-cutover.md") || !strings.Contains(err.Error(), "切替の記録がありません") {
		t.Fatalf("err=%v", err)
	}
	// A leftover SQLite file names the file and the document.
	if err = os.MkdirAll(filepath.Dir(config.SQLiteFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(config.SQLiteFile, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = OpenPostgresApplicationStore(context.Background(), config, "")
	if err == nil || !strings.Contains(err.Error(), "docs/pg-cutover.md") || !strings.Contains(err.Error(), config.SQLiteFile) {
		t.Fatalf("err=%v", err)
	}
	if appsql.CurrentDialect() != before {
		t.Fatal("失敗した起動が方言を切り替えました")
	}
	if raw, readErr := os.ReadFile(config.SQLiteFile); readErr != nil || string(raw) != "x" {
		t.Fatal("SQLiteファイルに触れました")
	}
}

func TestOpenPostgresApplicationStoreOpensMigratedStateAndSelectsPostgresDialect(t *testing.T) {
	dsn, db := cutoverDatabase(t)
	if _, err := db.Exec("CREATE SCHEMA IF NOT EXISTS issue_desk_app_migration"); err != nil {
		t.Fatal(err)
	}
	if err := markCutover(db, time.Now()); err != nil {
		t.Fatal(err)
	}
	config := cutoverConfig(t, dsn)
	s, err := OpenPostgresApplicationStore(context.Background(), config, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if appsql.CurrentDialect() != appsql.Postgres {
		t.Fatal("PostgreSQLの方言に切り替わっていません")
	}
	if _, statErr := os.Stat(config.SQLiteFile); statErr == nil {
		t.Fatal("SQLiteファイルを作りました")
	}
}

// The production opener must be the PostgreSQL one; only a test may replace it.
func TestOpenAppStartsOnlyOnPostgresInProduction(t *testing.T) {
	t.Setenv("PGHOST", "")
	t.Setenv("ISSUE_DESK_DATABASE_URL", "")
	file, dir, value := configFixture(t)
	writeConfig(t, file, value)
	config, err := Load(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = OpenApp(context.Background(), config, new(sql.DB), strings.Repeat("a", 40), nil)
	if err == nil || !strings.Contains(err.Error(), "ISSUE_DESK_DATABASE_URL") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(config.SQLiteFile); statErr == nil {
		t.Fatal("SQLiteファイルを作りました")
	}
	if _, statErr := os.Stat(filepath.Join(config.RuntimeDir, "state", "intake-lock.sqlite")); statErr == nil {
		t.Fatal("SQLiteの排他ファイルを作りました")
	}
}

// Only tests choose a throwaway application schema; no production file may set
// one (that would skip the switch-over flag check), and none may open SQLite.
func TestOnlyTestsChooseTheApplicationSchema(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if strings.Contains(text, "ApplicationSchema =") || strings.Contains(text, "ApplicationSchema:") {
			t.Errorf("%s が保存先のスキーマを指定しています", e.Name())
		}
		if strings.Contains(text, "OpenSQLite") || strings.Contains(text, `sql.Open("sqlite`) {
			t.Errorf("%s がSQLiteを開きます", e.Name())
		}
	}
	file, dir, value := configFixture(t)
	writeConfig(t, file, value)
	config, err := Load(file, dir, "")
	if err != nil || config.ApplicationSchema != "" {
		t.Fatalf("本番の設定が保存先のスキーマを指定: %+v %v", config, err)
	}
}

// The NAS compose file gives the shared database as PGHOST/PGUSER/PGDATABASE/PGPASSWORD
// and no URL. A key/value DSN without a password, plus PGPASSWORD, must resolve to the
// same connection, so the password never has to be written into a URL.
func TestKeyValueDSNTakesThePasswordFromTheEnvironment(t *testing.T) {
	t.Setenv("PGPASSWORD", "from-environment")
	config, err := pgx.ParseConfig("host=issue-desk-db user=issue_desk dbname=issue_desk")
	if err != nil || config.Host != "issue-desk-db" || config.User != "issue_desk" || config.Database != "issue_desk" || config.Password != "from-environment" {
		t.Fatalf("config=%+v err=%v", config, err)
	}
}

// markCutover records the finished switch-over the way the migration did; the
// production code only reads the flag (appcutover.Done).
func markCutover(db *sql.DB, now time.Time) error {
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+appcutover.FlagTable+` (id INTEGER PRIMARY KEY CHECK (id = 1), completed_at BIGINT NOT NULL)`); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `INSERT INTO `+appcutover.FlagTable+` (id, completed_at) VALUES (1, $1) ON CONFLICT (id) DO NOTHING`, now.UnixMilli())
	return err
}
