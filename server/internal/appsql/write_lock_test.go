package appsql_test

import (
	"context"
	"testing"

	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
)

// PostgreSQL takes a transaction-scoped advisory lock on the name, so a second
// writer for the same name cannot begin until the first one ends.
func TestBeginLockedWriteSerializesPostgresWriters(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	var dialect appsql.Active
	first, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err = dialect.BeginLockedWrite(ctx, first, "k"); err != nil {
		t.Fatal(err)
	}
	// Fail fast instead of waiting; an error keeps the connection usable.
	if _, err = second.ExecContext(ctx, "SET lock_timeout='300ms'"); err != nil {
		t.Fatal(err)
	}
	if err = dialect.BeginLockedWrite(ctx, second, "k"); err == nil {
		t.Fatal("second writer began while the first held the lock")
	}
	second.ExecContext(ctx, "ROLLBACK")
	if err = dialect.BeginLockedWrite(ctx, second, "other"); err != nil {
		t.Fatalf("a different lock name must not block: %v", err)
	}
	second.ExecContext(ctx, "ROLLBACK")
	if _, err = first.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err = dialect.BeginLockedWrite(ctx, second, "k"); err != nil {
		t.Fatal(err)
	}
	second.ExecContext(ctx, "ROLLBACK")
}
