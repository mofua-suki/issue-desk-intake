package hostd

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/store"
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
	var id [8]byte
	if _, err = rand.Read(id[:]); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	schema := "hostd_test_" + hex.EncodeToString(id[:])
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

// 失敗した版は hostd が選ばないので、install は同じ版の失敗の記録を消して再試行できる状態にする。
func TestInstallClearsOnlyTheFailedRecordOfThatTag(t *testing.T) {
	db, ctx := testPostgres(t)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO hosts(host,heartbeat_at,hostd_health) VALUES('gt12',now(),'{}'::jsonb)")
	exec("INSERT INTO client_release_history(host,flavor,revision,kind) VALUES('*','-python','aaaaaaaaaaaa1111111111111111111111111111','align')")
	for _, row := range [][4]string{
		{"gt12", "aaaaaaaaaaaa-client-python", "-python", "failed"},
		{"gt12", "bbbbbbbbbbbb-client-python", "-python", "failed"},
		{"gt12", "aaaaaaaaaaaa-client", "", "failed"},
		{"other", "aaaaaaaaaaaa-client-python", "-python", "failed"},
		{"gt12", "aaaaaaaaaaaa-success", "-python", "success"},
	} {
		exec("INSERT INTO host_image_results(host,tag,flavor,digest,status) VALUES($1,$2,$3,'sha256:x',$4)", row[0], row[1], row[2], row[3])
	}
	exec(`INSERT INTO host_image_history(host,tag,flavor,digest,status,stage,reason,detail,self_check_failed,from_tag,started_at)
		VALUES('gt12','aaaaaaaaaaaa-client-python','-python','sha256:x','failed','swap','error','detail','["check"]','old', '2026-11-01T00:00:00Z'),
		('gt12','aaaaaaaaaaaa-client-python','-python','sha256:x','success',NULL,NULL,NULL,NULL,NULL,NULL)`)
	if _, err := db.ExecContext(ctx, "INSERT INTO host_image_history(host,tag,flavor,digest,status) VALUES('gt12','x','','x','unknown')"); err == nil {
		t.Fatal("invalid history status must be rejected")
	}
	var indexExists bool
	err := db.QueryRowContext(ctx, `WITH own AS MATERIALIZED (
		SELECT indexrelid FROM pg_index WHERE indrelid = to_regclass('host_image_history'))
		SELECT EXISTS (SELECT 1 FROM own WHERE pg_get_indexdef(indexrelid) LIKE '%(host, received_at)%')`).Scan(&indexExists)
	if err != nil || !indexExists {
		t.Fatalf("history index missing: %v", err)
	}
	service := &Service{DB: db, Now: time.Now}
	body := map[string]any{"id": "0123456789abcdef0123456789abcdef", "kind": "install", "tag": "aaaaaaaaaaaa-python"}
	if _, err := service.Commands(ctx, "gt12", "test", body, true); err != nil {
		t.Fatal(err)
	}
	var remaining []string
	rows, err := db.QueryContext(ctx, "SELECT host || '/' || tag || '/' || flavor || '/' || status FROM host_image_results ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var tag string
		if err = rows.Scan(&tag); err != nil {
			t.Fatal(err)
		}
		remaining = append(remaining, tag)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"gt12/aaaaaaaaaaaa-client//failed", "gt12/aaaaaaaaaaaa-success/-python/success", "gt12/bbbbbbbbbbbb-client-python/-python/failed", "other/aaaaaaaaaaaa-client-python/-python/failed"}
	if !reflect.DeepEqual(remaining, want) {
		t.Fatal("install must clear only the failed record of the installed tag", remaining)
	}
	var count, detailed, received int
	if err := db.QueryRowContext(ctx, `SELECT count(*), count(*) FILTER (WHERE status='failed' AND stage='swap' AND reason='error' AND detail='detail' AND self_check_failed='["check"]'::jsonb AND from_tag='old' AND started_at='2026-11-01T00:00:00Z'), count(received_at) FROM host_image_history`).Scan(&count, &detailed, &received); err != nil || count != 2 || detailed != 1 || received != 2 {
		t.Fatalf("history changed or defaults missing: count=%d detailed=%d received=%d err=%v", count, detailed, received, err)
	}
}

func TestImageResultHistorySurvivesInstall(t *testing.T) {
	db, ctx := testPostgres(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,heartbeat_at,hostd_health) VALUES('gt12',now(),'{}'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO client_release_history(host,flavor,revision,kind) VALUES('*','-python','aaaaaaaaaaaa1111111111111111111111111111','align')`); err != nil {
		t.Fatal(err)
	}
	service := &Service{DB: db, Now: time.Now}
	if _, err := service.ImageResult(ctx, "gt12", map[string]any{"tag": "aaaaaaaaaaaa-client-python", "flavor": "-python", "digest": "sha256:" + strings.Repeat("f", 64), "status": "failed", "reason": "network"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Commands(ctx, "gt12", "test", map[string]any{"id": "0123456789abcdef0123456789abcdef", "kind": "install", "tag": "aaaaaaaaaaaa-python"}, true); err != nil {
		t.Fatal(err)
	}
	var current, history int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM host_image_results WHERE host='gt12'), (SELECT count(*) FROM host_image_history WHERE host='gt12' AND reason='network')`).Scan(&current, &history); err != nil || current != 0 || history != 1 {
		t.Fatalf("current=%d history=%d err=%v", current, history, err)
	}
}
