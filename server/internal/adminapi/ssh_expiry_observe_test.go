package adminapi

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Observe the actual pgx/database/sql boundary, including rows consumed from the
// server. Neither the old nor the shipped expiry implementation counts itself.
type sshSQLRecord struct {
	SQL  string
	Rows int64
	Err  error
}
type sshSQLTrace struct {
	mu      sync.Mutex
	records []sshSQLRecord
	start   func(*pgx.Conn, string)
	end     func(*pgx.Conn, string)
}
type sshSQLKey struct{}

func (o *sshSQLTrace) TraceQueryStart(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if o.start != nil {
		o.start(c, d.SQL)
	}
	return context.WithValue(ctx, sshSQLKey{}, d.SQL)
}
func (o *sshSQLTrace) TraceQueryEnd(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryEndData) {
	q, _ := ctx.Value(sshSQLKey{}).(string)
	o.mu.Lock()
	o.records = append(o.records, sshSQLRecord{q, d.CommandTag.RowsAffected(), d.Err})
	o.mu.Unlock()
	if o.end != nil {
		o.end(c, q)
	}
}
func (o *sshSQLTrace) take() []sshSQLRecord {
	o.mu.Lock()
	defer o.mu.Unlock()
	records := o.records
	o.records = nil
	return records
}
func sshObservedDB(t *testing.T, f *fixture, trace *sshSQLTrace) *sql.DB {
	t.Helper()
	var schema string
	if err := f.pg.QueryRow("SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	// The existing fixture already validated this disposable DB's DSN.
	cfg, err := pgx.ParseConfig(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	cfg.Tracer = trace
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { db.Close() })
	if err = db.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	trace.take()
	return db
}

// Exact former algorithm, using the shipped conversion and transaction adapter.
func legacySSHExpiry(ctx context.Context, tx *repairTx, now time.Time, ttl int) error {
	rows, err := queryMaps(ctx, tx, "SELECT id,decided_at FROM ssh_tickets WHERE status='approved'")
	if err != nil {
		return err
	}
	for _, row := range rows {
		if decided, ok := moment(row["decided_at"]); ok && !decided.After(now.Add(-time.Duration(ttl)*time.Minute)) {
			if _, err = tx.ExecContext(ctx, "UPDATE ssh_tickets SET status='expired' WHERE id=? AND status='approved'", row["id"]); err != nil {
				return err
			}
		}
	}
	return nil
}
func runSSHExpiry(t *testing.T, db *sql.DB, now time.Time, ttl int, legacy bool) error {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	runner := &repairTx{tx}
	if legacy {
		err = legacySSHExpiry(t.Context(), runner, now, ttl)
	} else {
		err = expireSSHTickets(t.Context(), runner, now, ttl)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
func isSSHExpirySQL(q string) bool {
	return strings.HasPrefix(q, "UPDATE ssh_tickets SET status='expired'")
}
func sshExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), q, args...); err != nil {
		t.Fatal(err)
	}
}
func insertSSHTicket(t *testing.T, f *fixture, state string, decided any) string {
	t.Helper()
	id := randomID()
	sshExec(t, f.pg, `INSERT INTO ssh_tickets(id,host,operation,reason,evidence,emergency,status,requester,requested_token,requested_at,approver,decided_at)
 VALUES($1,'test-host','repair','synthetic','synthetic',false,$2,'token:worker','aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',$3,'screen',$4)`, id, state, f.now, decided)
	return id
}
func sshSnapshot(t *testing.T, db *sql.DB) []map[string]any {
	t.Helper()
	rows, err := queryMaps(t.Context(), db, "SELECT * FROM ssh_tickets ORDER BY requested_at DESC,id")
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
