package operations

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
)

type recoveryCountConnector struct {
	base    driver.Connector
	queries *atomic.Int64
}

func (c recoveryCountConnector) Driver() driver.Driver { return c.base.Driver() }
func (c recoveryCountConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &recoveryCountConn{PostgresConn: conn.(appsql.PostgresConn), queries: c.queries}, nil
}

type recoveryCountConn struct {
	appsql.PostgresConn
	queries *atomic.Int64
}

func (c *recoveryCountConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.queries.Add(1)
	return c.PostgresConn.QueryContext(ctx, query, args)
}

func (c *recoveryCountConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.queries.Add(1)
	return c.PostgresConn.ExecContext(ctx, query, args)
}

// 原因の無い依頼は周期ごとに候補の読取へ現れ続ける。除外を記録済みなら、
// その件数が増えても候補の読取がデータベースへ送る問合せは増えない。
func TestRecoveryRecordedMissingCauseAddsNoQueriesPostgres(t *testing.T) {
	r := recoveryFixture(t)
	ctx := context.Background()
	var schema string
	if e := r.Service.PostgreSQL.QueryRow("SELECT current_schema()").Scan(&schema); e != nil {
		t.Fatal(e)
	}
	config, e := pgx.ParseConfig(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if e != nil {
		t.Fatal(e)
	}
	config.RuntimeParams["search_path"] = schema
	var queries atomic.Int64
	counted := sql.OpenDB(recoveryCountConnector{stdlib.GetConnector(*config), &queries})
	defer counted.Close()
	r.Service.PostgreSQL = counted
	candidates := func() int64 {
		t.Helper()
		queries.Store(0)
		list, e := r.Candidates(ctx)
		if e != nil || len(list) != 0 {
			t.Fatal(list, e)
		}
		return queries.Load()
	}
	empty := candidates()
	const missing = 6
	for n := 1; n <= missing; n++ {
		recoveryInsert(t, r, n, "held", "question", "normal", "owner/repo", `{}`, `{"questions":[]}`, 0, nil, "1 hour")
	}
	// 未記録の依頼は保存済みの台帳を読み直してから記録する。
	if first := candidates(); first <= empty {
		t.Fatalf("未記録の除外を台帳へ記録していない: 問合せ %d 回（依頼なしは %d 回）", first, empty)
	}
	records, e := r.records(ctx)
	if e != nil || len(records) != missing {
		t.Fatal(records, e)
	}
	for _, row := range records {
		if row.ExclusionReason != "原因なし" || row.ExclusionAt.IsZero() {
			t.Fatalf("除外を記録していない: %+v", row)
		}
	}
	if recorded := candidates(); recorded != empty {
		t.Fatalf("記録済みの除外 %d 件で問合せが %d 回（依頼なしは %d 回）", missing, recorded, empty)
	}
	after, e := r.records(ctx)
	if e != nil || len(after) != missing {
		t.Fatal(after, e)
	}
	for i := range after {
		if !after[i].ExclusionAt.Equal(records[i].ExclusionAt) {
			t.Fatalf("記録済みの除外を書き直した: %+v -> %+v", records[i], after[i])
		}
	}
}
