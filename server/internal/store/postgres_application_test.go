package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOpenApplicationPostgresRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	if _, err := OpenApplicationPostgres(ctx, "", ""); err == nil {
		t.Fatal("empty DSN must fail")
	}
	if _, err := OpenApplicationPostgres(ctx, "postgres://localhost/x", "bad schema;"); err == nil {
		t.Fatal("bad schema must fail")
	}
}

func TestOpenApplicationPostgresPinsSchemaAndBinds(t *testing.T) {
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("ISSUE_DESK_TEST_POSTGRES_DSN未設定のためPostgreSQL結合テストを省略")
	}
	if !strings.Contains(dsn, "issue_desk_test") {
		t.Fatal("専用のissue_desk_testで始まる試験DBが必要です")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := OpenApplicationPostgres(ctx, dsn, "public")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var schema string
	var n int
	if err := s.DB().QueryRowContext(ctx, "SELECT current_schema(), ?::int", 7).Scan(&schema, &n); err != nil || schema != "public" || n != 7 {
		t.Fatalf("schema=%q n=%d err=%v", schema, n, err)
	}
}
