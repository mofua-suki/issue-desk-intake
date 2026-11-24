package proposals

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/store"
)

func isolatedPostgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("隔離した検証用PostgreSQLのDSNが未設定です")
	}
	u, e := url.Parse(dsn)
	if e != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("明示的な検証DBが必要です")
	}
	ctx := context.Background()
	admin, e := store.OpenPostgres(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	random := make([]byte, 8)
	_, _ = rand.Read(random)
	schema := "proposal_" + hex.EncodeToString(random)
	if _, e = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	params := u.Query()
	params.Set("search_path", schema)
	u.RawQuery = params.Encode()
	db, e := store.OpenPostgres(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		db.Close()
		if _, e := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); e != nil {
			t.Error(e)
		}
		admin.Close()
	})
	if _, e = store.Migrate(ctx, db); e != nil {
		t.Fatal(e)
	}
	return db
}
func TestRepostPersistsCommandAndReplacesOnlyUnavailableDestination(t *testing.T) {
	s, _, restart := fixture(t)
	pg := isolatedPostgres(t)
	s.o.Postgres = pg
	ctx := context.Background()
	for _, host := range []string{"host-a", "host-b"} {
		if _, e := pg.ExecContext(ctx, `INSERT INTO hosts(host,heartbeat_at,ready,capabilities) VALUES($1,$2,true,'{"workerTokenKind":"worker"}')`, host, s.now()); e != nil {
			t.Fatal(e)
		}
	}
	input := Object{"project": "owner/repo", "number": 1, "commentId": 10, "instructionAt": "2026-11-01T03:00:00Z"}
	first, e := s.Repost(ctx, input, "screen")
	if e != nil || first["targetHost"] != "host-a" || first["created"] != true {
		t.Fatalf("first %v %v", first, e)
	}
	restart()
	same, e := s.Repost(ctx, input, "screen")
	if e != nil || same["commandId"] != first["commandId"] || same["created"] != false {
		t.Fatalf("restart duplicate %v %v", same, e)
	}
	var count int
	if e = pg.QueryRowContext(ctx, `SELECT count(*) FROM job_commands`).Scan(&count); e != nil || count != 1 {
		t.Fatalf("count %d %v", count, e)
	}
	// Persisted-before-enqueue recovery reuses exactly the saved command ID.
	if _, e = pg.ExecContext(ctx, `DELETE FROM job_commands WHERE id=$1`, first["commandId"]); e != nil {
		t.Fatal(e)
	}
	replay, e := s.Repost(ctx, input, "screen")
	if e != nil || replay["commandId"] != first["commandId"] || replay["created"] != true {
		t.Fatalf("saved command replay %v %v", replay, e)
	}
	if _, e = pg.ExecContext(ctx, `UPDATE hosts SET ready=false`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Repost(ctx, input, "screen"); status(e) != 409 {
		t.Fatalf("no alternative %v", e)
	}
	var state string
	if e = pg.QueryRowContext(ctx, `SELECT status FROM job_commands WHERE id=$1`, first["commandId"]).Scan(&state); e != nil || state != "queued" {
		t.Fatal("existing command cancelled without eligible replacement")
	}
	if _, e = pg.ExecContext(ctx, `UPDATE hosts SET ready=true WHERE host='host-b'`); e != nil {
		t.Fatal(e)
	}
	replacement, e := s.Repost(ctx, input, "token:pc")
	if e != nil || replacement["targetHost"] != "host-b" || replacement["commandId"] == first["commandId"] {
		t.Fatalf("replacement %v %v", replacement, e)
	}
	if e = pg.QueryRowContext(ctx, `SELECT status FROM job_commands WHERE id=$1`, first["commandId"]).Scan(&state); e != nil || state != "failed" {
		t.Fatal("superseded command remains active")
	}
	if _, e = pg.ExecContext(ctx, `UPDATE job_commands SET status='done' WHERE id=$1`, replacement["commandId"]); e != nil {
		t.Fatal(e)
	}
	next, e := s.Repost(ctx, input, "screen")
	if e != nil || next["commandId"] == replacement["commandId"] {
		t.Fatalf("completed command not replaced %v %v", next, e)
	}
	mark := Object{"type": "dismissal", "status": "dismissed", "fingerprint": "same"}
	r, _ := s.read(ctx, "rejection:owner/repo#1:10")
	r["fingerprint"] = "same"
	_ = s.save(ctx, "rejection:owner/repo#1:10", r)
	_ = s.save(ctx, "dismissal:owner/repo#1:followup-1", mark)
	if _, e = s.Repost(ctx, input, "screen"); status(e) != 409 {
		t.Fatalf("dismissal ignored %v", e)
	}
}
