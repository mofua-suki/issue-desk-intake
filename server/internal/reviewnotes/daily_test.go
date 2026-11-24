package reviewnotes

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

// Migration 10 is the shipped PostgreSQL DDL. Execute it unchanged in an empty
// throwaway schema to test Run's actual daily-claim SQL against the real date
// predicate. Only this migration is applied, so repo_notes is deliberately absent.
func dailyDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv(pgtest.DSNEnv)
	if dsn == "" {
		t.Skip(pgtest.DSNEnv + "未設定のためPostgreSQLを使うテストを省略")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil || !strings.HasPrefix(config.Database, "issue_desk_test") {
		t.Fatal("専用のissue_desk_testデータベースが必要です")
	}
	admin, err := store.OpenPostgres(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	var id [8]byte
	if _, err = rand.Read(id[:]); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	schema := "reviewnotes_daily_" + hex.EncodeToString(id[:])
	if _, err = admin.ExecContext(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(2)
	t.Cleanup(func() {
		db.Close()
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("試験スキーマの片付け: %v", err)
		}
		admin.Close()
	})
	migrations, err := store.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.Version == 10 {
			if _, err := db.ExecContext(t.Context(), migration.SQL); err != nil {
				t.Fatal(err)
			}
			return db
		}
	}
	t.Fatal("配備する移行10がありません")
	return nil
}

func TestDailyRunSkipsSameJSTDate(t *testing.T) {
	db := dailyDB(t)
	clock := time.Date(2026, 11, 4, 14, 59, 0, 0, time.UTC)
	s := Service{DB: db, Now: func() time.Time { return clock }}
	config, err := ParseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy rows may have notes but no attempt_date. Cover the conflict-update
	// path without losing their state; owner/other below covers a fresh insert.
	if _, err := db.ExecContext(t.Context(), `INSERT INTO repo_note_processing(repo,notes,improvement_cursor)
VALUES('owner/repo','{"items":[{"kind":"legacy","text":"以前の注意点"}],"cursor":7}',17)`); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := db.QueryRowContext(t.Context(), "SELECT notes FROM repo_note_processing WHERE repo='owner/repo'").Scan(&before); err != nil {
		t.Fatal(err)
	}
	// Deliberately omit repo_notes: the first Run fails at the read AFTER the
	// autocommitted claim. A suppressed Run must never reach that read again.
	attempt := func(service *Service, repo string, wantAttempt bool, wantDate string) {
		t.Helper()
		attempted, err := service.Run(t.Context(), repo, config)
		if attempted != wantAttempt || (wantAttempt && (err == nil || !strings.Contains(err.Error(), `relation "repo_notes" does not exist`))) || (!wantAttempt && err != nil) {
			t.Fatalf("日次判定: attempted=%v want=%v err=%v", attempted, wantAttempt, err)
		}
		var date string
		if err := db.QueryRowContext(t.Context(), "SELECT attempt_date FROM repo_note_processing WHERE repo=$1", repo).Scan(&date); err != nil || date != wantDate {
			t.Fatalf("試行日が保存されていない: %q want=%q err=%v", date, wantDate, err)
		}
	}
	attempt(&s, "owner/repo", true, "2026-11-04")
	attempt(&s, "owner/repo", false, "2026-11-04")
	// A new Service has no process-local memory of the first attempt.
	other := Service{DB: db, Now: s.Now}
	attempt(&other, "owner/repo", false, "2026-11-04")
	attempt(&other, "owner/other", true, "2026-11-04")
	clock = clock.Add(2 * time.Minute) // Same UTC day, next JST day.
	attempt(&other, "owner/repo", true, "2026-11-05")
	attempt(&s, "owner/repo", false, "2026-11-05")
	clock = clock.Add(-24 * time.Hour)
	attempt(&s, "owner/repo", false, "2026-11-05")
	var after []byte
	if err := db.QueryRowContext(t.Context(), "SELECT notes FROM repo_note_processing WHERE repo='owner/repo'").Scan(&after); err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("失敗・抑止時にnotesが変わった: %v", err)
	}
	var improvementCursor int64
	if err := db.QueryRowContext(t.Context(), "SELECT improvement_cursor FROM repo_note_processing WHERE repo='owner/repo'").Scan(&improvementCursor); err != nil || improvementCursor != 17 {
		t.Fatalf("改善のcursorが変わった: %d %v", improvementCursor, err)
	}
}

func TestDailyRunAttemptSaveFailureStops(t *testing.T) {
	db := dailyDB(t)
	if _, err := db.ExecContext(t.Context(), `CREATE FUNCTION fail_attempt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'attempt save failed'; END $$;
CREATE TRIGGER fail_attempt BEFORE INSERT ON repo_note_processing FOR EACH ROW EXECUTE FUNCTION fail_attempt()`); err != nil {
		t.Fatal(err)
	}
	s := Service{DB: db, Now: func() time.Time { return testNow }}
	config, err := ParseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if attempted, err := s.Run(t.Context(), "owner/repo", config); attempted || err == nil || !strings.Contains(err.Error(), "attempt save failed") {
		t.Fatalf("試行日の保存失敗で処理が止まらない: %v %v", attempted, err)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM repo_note_processing").Scan(&count); err != nil || count != 0 {
		t.Fatalf("保存失敗なのに試行日が残った: %d %v", count, err)
	}
}
