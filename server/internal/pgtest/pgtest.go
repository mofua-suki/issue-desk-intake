// Package pgtest is the shared PostgreSQL fixture of the tests. It is test
// support code: no production file may import it (guard_test.go), and the intake
// binary does not link it (cmd/intake/no_sqlite_test.go).
//
// Usage (the minimal case):
//
//	func TestSomething(t *testing.T) {
//		db := pgtest.New(t) // t.Skip when ISSUE_DESK_TEST_POSTGRES_DSN is unset
//		if _, err := db.Exec("INSERT INTO settings(id,json) VALUES(?,?)", "k", "{}"); err != nil {
//			t.Fatal(err)
//		}
//	}
//
// New returns a *sql.DB on a throwaway schema (t_<hex>) that holds the complete
// production application schema (appdb.CreateSchema: every table, the shared
// source_rowid sequence, the identity column and the indexes). The connection
// layer is the production one (appsql.NewPostgresBindingConnector): `?` becomes
// `$n`. search_path is pinned to the schema on every connection, so tests
// that run in parallel (t.Parallel, or other packages at the same time) never see
// each other's rows. The schema is dropped by t.Cleanup.
//
// Inside package store, build the store with applicationSQLite(pgtest.New(t), appsql.Postgres).
// Code outside package store opens the application store with
// store.OpenApplicationPostgres(ctx, env.DSN, env.Schema) using NewIsolated.
package pgtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"issue-desk-intake/server/internal/appdb"
	"issue-desk-intake/server/internal/appsql"
)

// DSNEnv is the environment variable that points at the PostgreSQL test server.
// The database name must start with issue_desk_test.
const DSNEnv = "ISSUE_DESK_TEST_POSTGRES_DSN"

// Env describes one isolated schema.
type Env struct {
	// DB is the connection layer of the application store, search_path pinned to Schema.
	DB *sql.DB
	// DSN is the test server's URL (without search_path); Schema is the throwaway schema.
	// Pass both to store.OpenApplicationPostgres to open the application store on it.
	DSN, Schema string
}

// New returns a database holding the application schema alone. See NewIsolated.
func New(t testing.TB) *sql.DB { return NewIsolated(t).DB }

// NewIsolated creates a fresh schema with the production tables and returns it.
// It skips the test when DSNEnv is unset and fails it when the DSN is not a
// dedicated issue_desk_test database.
func NewIsolated(t testing.TB) Env {
	t.Helper()
	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skip(DSNEnv + "未設定のためPostgreSQLを使うテストを省略")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("postgres:// または postgresql:// のURL形式で、専用のissue_desk_testで始まる試験DBが必要です")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("試験DBを開けません")
	}
	defer admin.Close()
	var name string
	if err := admin.QueryRowContext(ctx, "SELECT current_database()").Scan(&name); err != nil || !strings.HasPrefix(name, "issue_desk_test") {
		t.Fatal("試験DBを確認できません")
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal("試験スキーマ名を生成できません")
	}
	schema := "t_" + hex.EncodeToString(random[:])
	tx, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("試験スキーマのトランザクションを開始できません")
	}
	if err := appdb.CreateSchema(ctx, tx, schema); err != nil {
		tx.Rollback()
		t.Fatalf("試験スキーマを作成できません: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal("試験スキーマを確定できません")
	}
	t.Cleanup(func() { dropSchema(t, dsn, schema) })

	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("試験DBの接続先を解釈できません")
	}
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["search_path"] = schema
	db := sql.OpenDB(appsql.NewPostgresBindingConnector(stdlib.GetConnector(*config)))
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { db.Close() })
	var active string
	if err := db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&active); err != nil || active != schema {
		t.Fatal("試験別スキーマの分離を確認できません")
	}
	return Env{DB: db, DSN: dsn, Schema: schema}
}

// dropSchema removes the throwaway schema; it only ever receives a name made by NewIsolated.
func dropSchema(t testing.TB, dsn, schema string) {
	cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Error("試験スキーマの削除接続を開けません")
		return
	}
	defer admin.Close()
	if _, err := admin.ExecContext(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
		t.Error("試験スキーマを削除できません")
	}
}
