package models

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"issue-desk-intake/server/internal/store"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func testPostgres(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN is not configured")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("tests require an explicitly named issue_desk_test database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := store.OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var database string
	if err = admin.QueryRowContext(ctx, "SELECT current_database()").Scan(&database); err != nil || !strings.HasPrefix(database, "issue_desk_test") {
		admin.Close()
		t.Fatal("test database verification failed")
	}
	var id [8]byte
	if _, err = rand.Read(id[:]); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	schema := "models_test_" + hex.EncodeToString(id[:])
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	params := u.Query()
	params.Set("search_path", schema)
	u.RawQuery = params.Encode()
	db, err := store.OpenPostgres(ctx, u.String())
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if _, err := admin.ExecContext(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("test schema cleanup failed: %v", err)
		}
		admin.Close()
	})
	if _, err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db, ctx
}

func TestPostgresProviderDisableFencesAndResolution(t *testing.T) {
	db, ctx := testPostgres(t)
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s := Service{DB: db, Now: func() time.Time { return now }}
	ready := []map[string]any{{"id": "codex", "ready": true}, {"id": "claude", "ready": true}}
	var wg sync.WaitGroup
	var mu sync.Mutex
	count := 0
	for _, p := range []string{"codex", "claude"} {
		wg.Go(func() {
			_, e := s.SetDisable(ctx, p, map[string]any{}, "test", ready)
			if e == nil {
				mu.Lock()
				count++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if count != 1 {
		t.Fatalf("simultaneous disables committed: %d", count)
	}
	view, e := s.Disables(ctx, ready)
	if e != nil {
		t.Fatal(e)
	}
	entries := view["disables"].([]any)
	if len(entries) != 1 {
		t.Fatal(view)
	}
	disabled := entries[0].(map[string]any)["provider"].(string)
	resolved, e := s.Resolve(ctx, testConfig(), map[string]any{"consultationProvider": disabled, "tier": map[string]any{"level": "light"}}, "intake")
	if e != nil || resolved.Provider == disabled || resolved.ProviderDisable["from"] != disabled {
		t.Fatal(resolved, e)
	}
	if _, e = s.Cancel(ctx, "providerDisable."+disabled); e != nil {
		t.Fatal(e)
	}
	if _, e = s.SetDisable(ctx, "claude", map[string]any{}, "test", ready); e != nil {
		t.Fatal(e)
	}
	resolved, e = s.Resolve(ctx, testConfig(), map[string]any{"consultationProvider": "claude", "tier": map[string]any{"level": "light"}}, "intake")
	if e != nil || resolved.Model != "gpt-light" || resolved.Provider != "codex" {
		t.Fatal(resolved, e)
	}
	s.Now = func() time.Time { return now.Add(25 * time.Hour) }
	resolved, e = s.Resolve(ctx, testConfig(), map[string]any{"consultationProvider": "claude"}, "intake")
	if e != nil || resolved.Provider != "claude" || resolved.ProviderDisable != nil {
		t.Fatal(resolved, e)
	}
	if _, e = s.Resolve(ctx, testConfig(), map[string]any{"executionSettings": map[string]any{"provider": "claude"}, "provider": "codex"}, "review"); e == nil {
		t.Fatal("conflicting review provenance accepted")
	}
}
