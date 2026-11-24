package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestPendingMigrationsChecksSequenceAndKnownNames(t *testing.T) {
	list := []Migration{{Version: 1, Name: "first"}, {Version: 2, Name: "second"}}
	pending, ahead, err := PendingMigrations(list, []AppliedMigration{{Version: 1, Name: "first"}})
	if err != nil || ahead || len(pending) != 1 || pending[0].Version != 2 {
		t.Fatalf("pending: %v %v %v", pending, ahead, err)
	}
	for _, applied := range [][]AppliedMigration{
		{{Version: 1, Name: "wrong"}},
		{{Version: 2, Name: "second"}},
		{{Version: 1, Name: "first"}, {Version: 3, Name: "unknown"}},
	} {
		if _, _, err := PendingMigrations(list, applied); err == nil {
			t.Fatalf("invalid sequence accepted: %v", applied)
		}
	}
	pending, ahead, err = PendingMigrations(list, []AppliedMigration{{Version: 1, Name: "first"}, {Version: 2, Name: "second"}, {Version: 3, Name: "future"}})
	if err != nil || !ahead || len(pending) != 0 {
		t.Fatalf("newer database rejected: %v %v %v", pending, ahead, err)
	}
}

// Each database test uses a separate schema in an explicitly supplied test DB.
// No application's configured DSN is read or inferred.
func testPostgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("ISSUE_DESK_TEST_POSTGRES_DSN is not configured")
	}
	parsed, err := pgx.ParseConfig(dsn)
	if err != nil || !strings.HasPrefix(parsed.Database, "issue_desk_test") {
		t.Fatal("PostgreSQL tests require a dedicated issue_desk_test database")
	}
	ctx := context.Background()
	admin, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	schema := fmt.Sprintf("intake_store_%x", nonce)
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(5)
	t.Cleanup(func() {
		db.Close()
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("test schema cleanup: %v", err)
		}
		admin.Close()
	})
	return db
}

func TestPostgresMigrationsApplyAndPreserveNewerVersions(t *testing.T) {
	db := testPostgres(t)
	ctx := context.Background()
	list, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	result, err := Migrate(ctx, db)
	if err != nil || result.Version != len(list) || len(result.Applied) != len(list) || result.Ahead {
		t.Fatalf("first apply: %#v %v", result, err)
	}
	health := NewHealthChecker(healthDSN(t, db))
	defer health.Close()
	if err := health.Check(ctx); err != nil {
		t.Fatalf("applied database is unhealthy: %v", err)
	}
	rows, err := db.QueryContext(ctx, "SELECT version,name FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	var applied []AppliedMigration
	for rows.Next() {
		var migration AppliedMigration
		if err := rows.Scan(&migration.Version, &migration.Name); err != nil {
			t.Fatal(err)
		}
		applied = append(applied, migration)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for i, migration := range applied {
		if migration.Version != list[i].Version || migration.Name != list[i].Name {
			t.Fatalf("stored migration differs: %#v", migration)
		}
	}
	result, err = Migrate(ctx, db)
	if err != nil || len(result.Applied) != 0 {
		t.Fatalf("re-applied migrations: %#v %v", result, err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO schema_migrations(version,name) VALUES($1,'future')", len(list)+1); err != nil {
		t.Fatal(err)
	}
	result, err = Migrate(ctx, db)
	if err != nil || !result.Ahead || result.Version != len(list)+1 || len(result.Applied) != 0 {
		t.Fatalf("ahead: %#v %v", result, err)
	}
}

func healthDSN(t *testing.T, db *sql.DB) string {
	t.Helper()
	connectionURL, err := url.Parse(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil || (connectionURL.Scheme != "postgres" && connectionURL.Scheme != "postgresql") {
		t.Fatal("invalid test connection")
	}
	var schema string
	if err := db.QueryRow("SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	query := connectionURL.Query()
	query.Set("search_path", schema)
	connectionURL.RawQuery = query.Encode()
	return connectionURL.String()
}

func TestPostgresHealthDoesNotWaitForBusyWorkPool(t *testing.T) {
	db := testPostgres(t)
	ctx := context.Background()
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	dsn := healthDSN(t, db)
	db.SetMaxOpenConns(1)
	occupied, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	health := NewHealthChecker(dsn)
	defer health.Close()
	if err := health.Check(ctx); err != nil {
		t.Fatalf("healthy database rejected with occupied work pool: %v", err)
	}
}

func TestPostgresMigrationFailureRollsBackDDLAndVersionRecords(t *testing.T) {
	db := testPostgres(t)
	ctx := context.Background()
	list := []Migration{
		{Version: 1, Name: "first", SQL: "CREATE TABLE temporary_feature(id integer PRIMARY KEY)"},
		{Version: 2, Name: "failure", SQL: "THIS IS NOT VALID SQL"},
	}
	if _, err := migrateList(ctx, db, list); err == nil {
		t.Fatal("invalid SQL accepted")
	}
	var versions, feature sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT to_regclass('schema_migrations')::text,to_regclass('temporary_feature')::text").Scan(&versions, &feature); err != nil {
		t.Fatal(err)
	}
	if versions.Valid || feature.Valid {
		t.Fatalf("partial transaction survived: %v %v", versions, feature)
	}
}

func TestPostgresMigrationMismatchDoesNotModifyExistingData(t *testing.T) {
	db := testPostgres(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations(version integer PRIMARY KEY,name text NOT NULL,applied_at timestamptz DEFAULT now()); INSERT INTO schema_migrations(version,name) VALUES(1,'unexpected-name')`); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, db); err == nil {
		t.Fatal("mismatched name accepted")
	}
	var name string
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT name FROM schema_migrations WHERE version=1").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if count != 1 || name != "unexpected-name" {
		t.Fatalf("existing records changed: %d %s", count, name)
	}
}

func TestPostgresMigrationWaitsForTransactionLock(t *testing.T) {
	db := testPostgres(t)
	ctx := context.Background()
	lock, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err := lock.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", MigrationLockKey); err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := Migrate(wait, db); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("migration did not wait on the lock: %v", err)
	}
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
}

// healthQueryCounter observes the actual PostgreSQL query used by the checker.
type healthQueryCounter struct{ queries atomic.Int32 }

func (c *healthQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.queries.Add(1)
	return ctx
}
func (c *healthQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// waiterContext signals when Check has reached the in-flight wait.
type waiterContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *waiterContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestHealthCheckWaiterRetriesAfterLeaderCancellation(t *testing.T) {
	db := testPostgres(t)
	if _, err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	h := NewHealthChecker(healthDSN(t, db))
	defer h.Close()
	connecting := make(chan struct{})
	var calls atomic.Int32
	h.connect = func(ctx context.Context, dsn string) (*pgx.Conn, error) {
		if calls.Add(1) == 1 {
			close(connecting)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return pgx.Connect(ctx, dsn)
	}
	leaderCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leader := make(chan error, 1)
	go func() { leader <- h.Check(leaderCtx) }()
	<-connecting
	waiterCtx := &waiterContext{Context: context.Background(), waiting: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { waiter <- h.Check(waiterCtx) }()
	<-waiterCtx.waiting
	cancel()
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader: %v", err)
	}
	if err := <-waiter; err != nil {
		t.Fatalf("waiter: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("connection attempts: %d", calls.Load())
	}
}

func TestHealthCheckWaiterRetriesWithoutCachingLeaderCancellation(t *testing.T) {
	h := NewHealthChecker("")
	defer h.Close()
	connecting := make(chan struct{})
	want := errors.New("database unavailable")
	var calls atomic.Int32
	h.connect = func(ctx context.Context, _ string) (*pgx.Conn, error) {
		if calls.Add(1) == 1 {
			close(connecting)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, want
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leader := make(chan error, 1)
	go func() { leader <- h.Check(ctx) }()
	<-connecting
	waiterCtx := &waiterContext{Context: context.Background(), waiting: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { waiter <- h.Check(waiterCtx) }()
	<-waiterCtx.waiting
	cancel()
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader: %v", err)
	}
	if err := <-waiter; !errors.Is(err, want) {
		t.Fatalf("waiter: %v", err)
	}
	if err := h.Check(context.Background()); !errors.Is(err, want) || calls.Load() != 2 {
		t.Fatalf("cached retry: %v, connection attempts: %d", err, calls.Load())
	}
}

func TestHealthCheckReusesConnections(t *testing.T) {
	db := testPostgres(t)
	ctx := context.Background()
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	h := NewHealthChecker(healthDSN(t, db))
	defer h.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	var connects atomic.Int32
	tracer := &healthQueryCounter{}
	connecting := make(chan struct{})
	release := make(chan struct{})
	h.connect = func(ctx context.Context, dsn string) (*pgx.Conn, error) {
		connects.Add(1)
		close(connecting)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		config, err := pgx.ParseConfig(dsn)
		if err != nil {
			return nil, err
		}
		config.Tracer = tracer
		return pgx.ConnectConfig(ctx, config)
	}
	leader := make(chan error, 1)
	go func() { leader <- h.Check(ctx) }()
	<-connecting
	// A cancelled waiter must leave without interrupting the shared check.
	waitctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := h.Check(waitctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	waiter := &waiterContext{Context: ctx, waiting: make(chan struct{})}
	waiterResult := make(chan error, 1)
	go func() { waiterResult <- h.Check(waiter) }()
	<-waiter.waiting
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; errs <- h.Check(ctx) }()
	}
	close(start)
	close(release)
	if err := <-leader; err != nil {
		t.Fatal(err)
	}
	if err := <-waiterResult; err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		if err := h.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if connects.Load() != 1 || tracer.queries.Load() != 1 {
		t.Fatalf("connections=%d queries=%d", connects.Load(), tracer.queries.Load())
	}
	now = now.Add(2 * time.Second)
	if err := h.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if connects.Load() != 1 || tracer.queries.Load() != 2 {
		t.Fatalf("expired: connections=%d queries=%d", connects.Load(), tracer.queries.Load())
	}
}

func TestHealthCheckCacheExpiryAndFailure(t *testing.T) {
	db := testPostgres(t)
	ctx := context.Background()
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	h := NewHealthChecker(healthDSN(t, db))
	defer h.Close()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := h.Check(cancelled); err == nil {
		t.Fatal("cancelled check succeeded")
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	if err := h.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE schema_migrations RENAME TO schema_migrations_offline"); err != nil {
		t.Fatal(err)
	}
	if err := h.Check(ctx); err != nil {
		t.Fatalf("cached result: %v", err)
	}
	now = now.Add(2 * time.Second)
	if err := h.Check(ctx); err == nil {
		t.Fatal("expired result hid database failure")
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE schema_migrations_offline RENAME TO schema_migrations"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if err := h.Check(ctx); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	h.Close()
	if err := h.Check(ctx); err == nil {
		t.Fatal("closed checker accepted request")
	}
}

func TestPostgresJobsSearchIndexes(t *testing.T) {
	db := testPostgres(t)
	if _, err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, first, second, predicate string }{
		{"jobs_repo_issue_lower", "lower(repo)", "issue", ""},
		{"jobs_repo_status_open", "lower(repo)", "status", "(status <> ALL (ARRAY['merged'::text, 'closed'::text]))"},
	} {
		var first, second, predicate, method string
		var valid, ready, unique bool
		var columns int
		err := db.QueryRow(`SELECT pg_get_indexdef(i.indexrelid, 1, true), pg_get_indexdef(i.indexrelid, 2, true), COALESCE(pg_get_expr(i.indpred, i.indrelid), ''), am.amname, i.indisvalid, i.indisready, i.indisunique, i.indnkeyatts
    FROM pg_indexes p JOIN pg_class c ON c.relname=p.indexname JOIN pg_namespace n ON n.oid=c.relnamespace AND n.nspname=p.schemaname
    JOIN pg_index i ON i.indexrelid=c.oid JOIN pg_class tbl ON tbl.oid=i.indrelid JOIN pg_am am ON am.oid=c.relam
    WHERE p.schemaname=current_schema() AND p.tablename='jobs' AND tbl.relname='jobs' AND p.indexname=$1`, tc.name).Scan(&first, &second, &predicate, &method, &valid, &ready, &unique, &columns)
		if err != nil {
			t.Fatal(err)
		}
		if first != tc.first || second != tc.second || predicate != tc.predicate || method != "btree" || !valid || !ready || unique || columns != 2 {
			t.Fatalf("%s: %q %q %q %s valid=%v ready=%v unique=%v columns=%d", tc.name, first, second, predicate, method, valid, ready, unique, columns)
		}
	}
}

func jobsSearchSnapshot(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	var result string
	if err := db.QueryRow(query).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func jobsSearchBeforeUpgrade(t *testing.T, db *sql.DB) {
	t.Helper()
	list, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) < 35 || list[34].Version != 35 {
		t.Fatal("migration 35 missing")
	}
	if _, err := migrateList(context.Background(), db, list[:34]); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresJobsSearchIndexUpgradePreservesRows(t *testing.T) {
	db := testPostgres(t)
	jobsSearchBeforeUpgrade(t, db)
	_, err := db.Exec(`INSERT INTO jobs(repo,issue,status,hold_reason,title) SELECT CASE WHEN n % 2 = 0 THEN 'Org/Repo' ELSE 'org/repo' END, n, s, CASE WHEN s='held' THEN 'question' END, 'job ' || n FROM unnest(ARRAY['queued','running','held','failed','stopped','merged','closed']) WITH ORDINALITY AS x(s,n)`)
	if err != nil {
		t.Fatal(err)
	}
	rowsQuery := `SELECT COALESCE(jsonb_agg(to_jsonb(j) ORDER BY id),'[]'::jsonb)::text FROM jobs j`
	versionsQuery := `SELECT COALESCE(jsonb_agg(to_jsonb(m) ORDER BY version),'[]'::jsonb)::text FROM schema_migrations m WHERE version <= 34`
	rows, versions := jobsSearchSnapshot(t, db, rowsQuery), jobsSearchSnapshot(t, db, versionsQuery)
	result, err := Migrate(context.Background(), db)
	list, listErr := Migrations()
	if err != nil || listErr != nil || len(result.Applied) != len(list)-34 || len(result.Applied) == 0 || result.Applied[0] != 35 || result.Version != len(list) {
		t.Fatalf("upgrade: %#v %v", result, err)
	}
	if got := jobsSearchSnapshot(t, db, rowsQuery); got != rows {
		t.Fatalf("rows changed: %s", got)
	}
	if got := jobsSearchSnapshot(t, db, versionsQuery); got != versions {
		t.Fatalf("migration records changed: %s", got)
	}
	result, err = Migrate(context.Background(), db)
	if err != nil || len(result.Applied) != 0 {
		t.Fatalf("reapply: %#v %v", result, err)
	}
}

func TestPostgresJobsSearchIndexPlans(t *testing.T) {
	db := testPostgres(t)
	jobsSearchBeforeUpgrade(t, db)
	_, err := db.Exec(`INSERT INTO jobs(repo,issue,status) SELECT 'org/repo' || (n % 200), n, CASE WHEN n % 5 = 0 THEN 'closed' ELSE 'queued' END FROM generate_series(1,20000) n; ANALYZE jobs`)
	if err != nil {
		t.Fatal(err)
	}
	queries := []struct{ where, index string }{
		{"lower(repo)=lower('ORG/REPO7') AND issue=7", "jobs_repo_issue_lower"},
		{"lower(repo)=lower('ORG/REPO7') AND status NOT IN ('merged','closed') AND status='queued'", "jobs_repo_status_open"},
	}
	before := make([]string, len(queries))
	for i, q := range queries {
		before[i] = jobsSearchSnapshot(t, db, `SELECT COALESCE(jsonb_agg(to_jsonb(j) ORDER BY id),'[]'::jsonb)::text FROM jobs j WHERE `+q.where)
	}
	if _, err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ANALYZE jobs`); err != nil {
		t.Fatal(err)
	}
	// Disable sequential scans on this connection to check index availability, not planner preference.
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	for i, q := range queries {
		if got := jobsSearchSnapshot(t, db, `SELECT COALESCE(jsonb_agg(to_jsonb(j) ORDER BY id),'[]'::jsonb)::text FROM jobs j WHERE `+q.where); got != before[i] || got == "[]" {
			t.Fatalf("results changed or empty: %s", got)
		}
		var plan string
		if err := conn.QueryRowContext(context.Background(), `EXPLAIN (FORMAT JSON) SELECT * FROM jobs WHERE `+q.where).Scan(&plan); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan, `"Index Name": "`+q.index+`"`) {
			t.Fatalf("index %s unavailable: %s", q.index, plan)
		}
	}
}
