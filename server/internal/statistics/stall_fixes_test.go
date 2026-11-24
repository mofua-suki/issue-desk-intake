package statistics

import (
	"context"
	"testing"

	"issue-desk-intake/server/internal/pgtest"
)

// The migrated stall_fixes table has no json column; reading it must not fail.
func TestStallFixesReadFromTableColumns(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "INSERT INTO stall_fixes(draft,key,stall,filed_at) VALUES('d','k','s','2026-11-01T00:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	rows, err := (&Service{AppDB: db}).localRows(ctx, "stall_fixes")
	if err != nil || len(rows) != 1 || rows[0]["draft"] != "d" || rows[0]["at"] != "2026-11-01T00:00:00Z" {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
}
