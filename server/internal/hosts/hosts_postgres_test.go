package hosts

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

	"issue-desk-intake/server/internal/store"
)

func heartbeatDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN is not configured")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("isolated test database required")
	}
	ctx := context.Background()
	admin, err := store.OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var id [8]byte
	if _, err = rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	schema := "heartbeat_" + hex.EncodeToString(id[:])
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, e := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); e != nil {
			t.Error(e)
		}
		admin.Close()
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := store.OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestHeartbeatChangedColumnsPostgres(t *testing.T) {
	db := heartbeatDB(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s := &Service{DB: db, Now: func() time.Time { return at }}
	exec := func(q string) {
		t.Helper()
		if _, e := db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	exec(`CREATE TABLE updated_columns(name text); CREATE FUNCTION record_column() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO updated_columns VALUES (TG_ARGV[0]); RETURN NEW; END $$`)
	cols := []string{"revision", "cli_versions", "load", "busy", "implementing", "publishing", "capabilities", "ready", "checks", "image_revision", "capacity", "intake_hold"}
	for _, c := range cols {
		exec(`CREATE TRIGGER track_` + c + ` AFTER UPDATE OF ` + c + ` ON hosts FOR EACH ROW EXECUTE FUNCTION record_column('` + c + `')`)
	}
	put := func(body map[string]any) {
		t.Helper()
		if _, e := s.Put(ctx, "pc", body); e != nil {
			t.Fatal(e)
		}
	}
	put(map[string]any{})
	exec("DELETE FROM updated_columns")
	put(map[string]any{})
	var count int
	if e := db.QueryRow("SELECT count(*) FROM updated_columns").Scan(&count); e != nil || count != 0 {
		t.Fatal(count, e)
	}
	cases := []struct {
		key, column string
		value       any
	}{
		{"revision", "revision", strings.Repeat("a", 40)}, {"cliVersions", "cli_versions", []any{map[string]any{"cli": "codex", "current": "1.0.0", "expected": "1.0.0", "at": float64(at.UnixMilli())}}},
		{"load", "load", map[string]any{"cpuPercent": float64(1)}}, {"busy", "busy", true}, {"implementing", "implementing", float64(1)}, {"publishing", "publishing", float64(1)}, {"capabilities", "capabilities", map[string]any{"a": float64(1)}}, {"ready", "ready", true}, {"checks", "checks", map[string]any{"a": float64(1)}}, {"imageRevision", "image_revision", "x"}, {"capacity", "capacity", float64(1)}, {"intakeHold", "intake_hold", map[string]any{"reasons": []any{}}},
	}
	body := map[string]any{}
	for _, c := range cases {
		body[c.key] = c.value
		exec("DELETE FROM updated_columns")
		put(body)
		var name string
		if e := db.QueryRow("SELECT name FROM updated_columns").Scan(&name); e != nil || name != c.column {
			t.Fatal(c.key, name, e)
		}
		if e := db.QueryRow("SELECT count(*) FROM updated_columns").Scan(&count); e != nil || count != 1 {
			t.Fatal(c.key, count, e)
		}
	}
	exec("DELETE FROM updated_columns")
	body["capabilities"] = map[string]any{"a": float64(1)}
	put(body)
	if e := db.QueryRow("SELECT count(*) FROM updated_columns").Scan(&count); e != nil || count != 0 {
		t.Fatal(count, e)
	}
	// Two full notifications race on the same row; the final row must equal one
	// complete notification rather than a mixture of both.
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, v := range []float64{2, 3} {
		go func(v float64) {
			<-start
			_, e := s.Put(ctx, "race", map[string]any{"implementing": v, "publishing": v})
			errs <- e
		}(v)
	}
	close(start)
	for i := 0; i < 2; i++ {
		if e := <-errs; e != nil {
			t.Fatal(e)
		}
	}
	var implementing, publishing int
	if e := db.QueryRow("SELECT implementing,publishing FROM hosts WHERE host='race'").Scan(&implementing, &publishing); e != nil || implementing != publishing || (implementing != 2 && implementing != 3) {
		t.Fatal(implementing, publishing, e)
	}
	for _, c := range cases {
		if c.key == "busy" || c.key == "ready" {
			continue
		}
		delete(body, c.key)
		exec("DELETE FROM updated_columns")
		put(body)
		var name string
		if e := db.QueryRow("SELECT name FROM updated_columns").Scan(&name); e != nil || name != c.column {
			t.Fatal(c.key, name, e)
		}
	}
}

func TestHeartbeatHistoryThrottlingPostgres(t *testing.T) {
	db := heartbeatDB(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s := &Service{DB: db, Now: func() time.Time { return at }}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := db.Exec(q, args...); e != nil {
			t.Fatal(e)
		}
	}
	put := func() {
		t.Helper()
		if _, e := s.Put(ctx, "pc", map[string]any{}); e != nil {
			t.Fatal(e)
		}
	}
	exec(`CREATE TABLE deletions(n int); CREATE FUNCTION track_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO deletions VALUES(1); RETURN NULL; END $$; CREATE TRIGGER track_delete BEFORE DELETE ON host_load_samples FOR EACH STATEMENT EXECUTE FUNCTION track_delete()`)
	put()
	at = at.Add(time.Minute)
	exec("INSERT INTO host_load_samples(host,received_at) VALUES('pc',$1),('pc',$2)", at.Add(-3*time.Hour-time.Millisecond), at.Add(-3*time.Hour))
	put()
	var n int
	if e := db.QueryRow("SELECT count(*) FROM deletions").Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	at = at.Add(9 * time.Minute)
	exec("INSERT INTO host_load_samples(host,received_at) VALUES ($1,$2)", "pc", at.Add(-3*time.Hour))
	put()
	if e := db.QueryRow("SELECT count(*) FROM deletions").Scan(&n); e != nil || n != 2 {
		t.Fatal(n, e)
	}
	if e := db.QueryRow("SELECT count(*) FROM host_load_samples WHERE host='pc'").Scan(&n); e != nil || n != 4 {
		t.Fatal(n, e)
	}
	if e := db.QueryRow("SELECT count(*) FROM host_load_samples WHERE host=$1 AND received_at=$2", "pc", at.Add(-3*time.Hour)).Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	exec(`CREATE FUNCTION fail_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'failure'; END $$; CREATE TRIGGER fail_delete BEFORE DELETE ON host_load_samples FOR EACH STATEMENT EXECUTE FUNCTION fail_delete()`)
	at = at.Add(10 * time.Minute)
	put()
	exec("DROP TRIGGER fail_delete ON host_load_samples")
	put()
	if e := db.QueryRow("SELECT count(*) FROM deletions").Scan(&n); e != nil || n != 3 {
		t.Fatal(n, e)
	}
	if e := db.QueryRow("SELECT count(*) FROM hosts WHERE host='pc' AND heartbeat_at=$1", at).Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
}
