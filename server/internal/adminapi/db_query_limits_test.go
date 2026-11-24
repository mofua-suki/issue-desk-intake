package adminapi

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"issue-desk-intake/server/internal/contract"
)

func TestDBQueryLimits(t *testing.T) {
	f := newDBQueryFixture(t)
	dbQueryGrant(t, f, "shared")
	t.Run("rows", func(t *testing.T) {
		input := dbQueryInput("shared", "SELECT generate_series(1,1001) AS n")
		input["maxRows"] = 100
		code, value := f.call("POST", dbQueryPath, "delegate", input)
		status(t, code, 200, value)
		result := obj(value)
		if len(result["rows"].([]any)) != 100 || obj(result["truncated"])["rows"] != true {
			t.Fatalf("行上限: %v", result)
		}
		for _, n := range []int{1, 1000} {
			input["maxRows"] = n
			code, value = f.call("POST", dbQueryPath, "delegate", input)
			status(t, code, 200, value)
			if len(obj(value)["rows"].([]any)) != n {
				t.Fatal("指定行数を守っていません")
			}
		}
		code, value = f.call("POST", dbQueryPath, "delegate", dbQueryInput("shared", "SELECT 1 WHERE false"))
		status(t, code, 200, value)
		if len(obj(value)["rows"].([]any)) != 0 || obj(obj(value)["truncated"])["rows"] != false {
			t.Fatal("空の結果")
		}
	})
	t.Run("bytes", func(t *testing.T) {
		for _, query := range []string{
			"SELECT repeat('x',307200) AS value",
			"SELECT repeat('受付',100000) AS value",
			"SELECT repeat('\"<&',60000) AS value",
			"SELECT repeat('x',4000) AS value FROM generate_series(1,100)",
		} {
			request, _ := json.Marshal(dbQueryInput("shared", query))
			r := httptest.NewRequest("POST", "http://127.0.0.1:8787"+dbQueryPath, strings.NewReader(string(request)))
			r.Header.Set("Authorization", "Bearer "+f.token["delegate"])
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			f.mux.ServeHTTP(w, r)
			if w.Code != 200 || w.Body.Len() > 262144 {
				t.Fatalf("本文上限: status=%d bytes=%d", w.Code, w.Body.Len())
			}
			var result dbQueryResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if !result.Truncated.Bytes {
				t.Fatal("bytesの切り詰め表示がありません")
			}
		}
	})
	t.Run("timeout", func(t *testing.T) {
		start := time.Now()
		code, value := f.call("POST", dbQueryPath, "delegate", dbQueryInput("shared", "SELECT count(*) FROM generate_series(1,100000) a, generate_series(1,100000) b"))
		elapsed := time.Since(start)
		status(t, code, 504, value)
		if elapsed < 5*time.Second || elapsed > 7*time.Second {
			t.Fatalf("所要時間: %s", elapsed)
		}
		t.Logf("504応答まで %s", elapsed)
		code, value = f.call("POST", dbQueryPath, "delegate", dbQueryInput("shared", "SELECT 1"))
		status(t, code, 200, value)
	})
	t.Run("concurrent", func(t *testing.T) {
		// Hold the first request at the driver, after it took the execution slot,
		// so the check does not depend on lock_timeout or scheduling.
		observed, observation := observeDBQuery(t, f.pg)
		f.pg = observed
		f.restart()
		hold := &dbQueryHold{entered: make(chan struct{}), release: make(chan struct{})}
		observation.hold.Store(hold)
		type response struct {
			code  int
			value any
			err   error
		}
		done := make(chan response, 1)
		go func() {
			code, value, err := f.callResult("POST", dbQueryPath, "delegate", dbQueryInput("shared", "SELECT count(*) FROM jobs"))
			done <- response{code, value, err}
		}()
		select {
		case <-hold.entered:
		case first := <-done:
			t.Fatalf("最初の要求が待機せず終了: %+v", first)
		}
		code, value := f.call("POST", dbQueryPath, "delegate", dbQueryInput("shared", "SELECT 1"))
		status(t, code, 409, value)
		close(hold.release)
		first := <-done
		if first.err != nil {
			t.Fatal(first.err)
		}
		status(t, first.code, 200, first.value)
	})
}

// A huge cell or row must be cut off in PostgreSQL: the intake never receives
// a value larger than the response limit, whatever expression builds it.
func TestDBQueryHugeValueNotFetched(t *testing.T) {
	f := newDBQueryFixture(t)
	dbQueryGrant(t, f, "shared")
	observed, observation := observeDBQuery(t, f.pg)
	f.pg = observed
	f.restart()
	for _, tc := range []struct {
		name, query string
		rows        int
	}{
		{"cell", "SELECT repeat('x',20000000) AS value", 0},
		{"nested", "SELECT replace(repeat('x',4000000),'x','yy') AS value", 0},
		{"columns", "SELECT repeat('x',200000) AS a, repeat('y',200000) AS b", 0},
		{"second_row", "SELECT repeat('x', CASE WHEN n = 1 THEN 10 ELSE 20000000 END) AS value FROM generate_series(1,2) AS n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observation.maxValue.Store(0)
			request, _ := json.Marshal(dbQueryInput("shared", tc.query))
			r := httptest.NewRequest("POST", "http://127.0.0.1:8787"+dbQueryPath, strings.NewReader(string(request)))
			r.Header.Set("Authorization", "Bearer "+f.token["delegate"])
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			f.mux.ServeHTTP(w, r)
			if w.Code != 200 || w.Body.Len() > 262144 {
				t.Fatalf("本文上限: status=%d bytes=%d", w.Code, w.Body.Len())
			}
			var result dbQueryResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if !result.Truncated.Bytes || len(result.Rows) != tc.rows {
				t.Fatalf("切り詰め: bytes=%v rows=%d", result.Truncated.Bytes, len(result.Rows))
			}
			if got := observation.maxValue.Load(); got > 262144 {
				t.Fatalf("上限を超える値をDBから受け取りました: %d bytes", got)
			}
		})
	}
	// A value that fits is still returned in full through the same path.
	code, value := f.call("POST", dbQueryPath, "delegate", dbQueryInput("shared", "SELECT repeat('x',1000) AS value"))
	status(t, code, 200, value)
	if got := str(obj(value)["rows"].([]any)[0].([]any)[0]); len(got) != 1000 || observation.maxValue.Load() != 1000 {
		t.Fatalf("収まる値: %d bytes, 受信 %d bytes", len(got), observation.maxValue.Load())
	}
}

func TestDBQueryErrorStatus(t *testing.T) {
	for code, want := range map[string]int{
		"21000": 400, "0A000": 400, "54000": 400, "54001": 400, "22012": 400, "42601": 400, "42P01": 400, "25006": 400, "55P03": 400, "53200": 400,
		"57014": 504,
		"08006": 503, "53300": 503, "57P01": 503, "XX000": 503, "40001": 503,
	} {
		problem, ok := dbQueryError(&pgconn.PgError{Code: code, Message: "detail-must-not-leak"}).(*contract.Problem)
		if !ok || problem.Status != want || strings.Contains(problem.Error(), "detail-must-not-leak") {
			t.Fatalf("SQLSTATE %s: %v", code, problem)
		}
	}
	if problem := dbQueryError(context.DeadlineExceeded).(*contract.Problem); problem.Status != 504 {
		t.Fatalf("期限切れ: %v", problem)
	}
	if problem := dbQueryError(driver.ErrBadConn).(*contract.Problem); problem.Status != 503 {
		t.Fatalf("接続障害: %v", problem)
	}
}

func TestDBQueryOutputAndInput(t *testing.T) {
	f := newDBQueryFixture(t)
	dbQueryGrant(t, f, "shared")
	for _, tc := range []struct {
		name, query string
		want        int
	}{
		{"credential_label", "SELECT 1 AS access_token", 400},
		{"record", "SELECT q FROM (SELECT 1 AS n) q", 400},
		{"composite", "SELECT j FROM jobs j", 400},
		{"invalid_sql", "SELECT FROM", 400},
		{"cardinality", "SELECT (SELECT generate_series(1,2)) AS n", 400},
		{"too_large", "SELECT repeat('x',2000000000) AS value", 400},
		{"no_columns", "SELECT", 400},
		{"undefined_table", "SELECT * FROM missing", 400},
		{"scalar_types", "SELECT 1 AS n, 2.5 AS amount, true AS b, NULL AS empty, now() AS at", 200},
		{"duplicate_labels", "SELECT 1 AS n, 2 AS n", 200},
		{"with", "WITH q AS (SELECT 1 AS n) SELECT * FROM q; -- trailing", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, value := f.call("POST", dbQueryPath, "delegate", dbQueryInput("shared", tc.query))
			status(t, code, tc.want, value)
			if tc.name == "duplicate_labels" && len(obj(value)["rows"].([]any)[0].([]any)) != 2 {
				t.Fatal("同名列の値が失われました")
			}
		})
	}
	for _, max := range []any{0, 1001, 1.5, "100", nil} {
		input := dbQueryInput("shared", "SELECT 1")
		input["maxRows"] = max
		code, value := f.call("POST", dbQueryPath, "delegate", input)
		status(t, code, 400, value)
	}
	for _, input := range []map[string]any{dbQueryInput("other", "SELECT 1"), dbQueryInput("shared", ""), {"target": "shared", "sql": "SELECT 1", "extra": true}} {
		code, value := f.call("POST", dbQueryPath, "delegate", input)
		status(t, code, 400, value)
	}
	// Inspect the actual BEGIN options while executing against the shipping DDL.
	observed, observation := observeDBQuery(t, f.pg)
	defer observed.Close()
	f.pg = observed
	f.restart()
	code, value := f.call("POST", dbQueryPath, "delegate", dbQueryInput("shared", "SELECT length('abc') AS n"))
	status(t, code, 200, value)
	if !observation.readOnly.Load() {
		t.Fatal("読み取り専用ではありません")
	}
	// bytea on a shipping table is rejected even when it returns no rows.
	dbQueryGrant(t, f, "app")
	code, value = f.call("POST", dbQueryPath, "delegate", dbQueryInput("app", "SELECT bytes FROM images"))
	status(t, code, 400, value)
	// search_path must select pg_catalog functions before same-name user functions.
	if _, err := observed.Exec("CREATE FUNCTION public.length(text) RETURNS integer LANGUAGE SQL AS 'SELECT 999'"); err != nil {
		t.Fatal(err)
	}
	code, value = f.call("POST", dbQueryPath, "delegate", dbQueryInput("shared", "SELECT length('abc') AS n"))
	status(t, code, 200, value)
	if fmt.Sprint(obj(value)["rows"].([]any)[0].([]any)[0]) != "3" {
		t.Fatal("関数がshadowされました")
	}
	// Connection failure remains a public 503, without database error details.
	observed.Close()
	code, value = f.call("POST", dbQueryPath, "delegate", dbQueryInput("shared", "SELECT 1"))
	status(t, code, 503, value)
}

// Observe actual driver calls without changing SQL, results or transaction options.
type dbQueryObservation struct {
	queries  atomic.Int64
	readOnly atomic.Bool
	maxValue atomic.Int64 // largest single value received from PostgreSQL
	hold     atomic.Pointer[dbQueryHold]
}

// dbQueryHold stops the next query at the driver until release is closed.
type dbQueryHold struct{ entered, release chan struct{} }
type dbQueryConnector struct {
	driver.Connector
	observation *dbQueryObservation
}

func (c dbQueryConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return dbQueryObservedConn{Conn: conn, observation: c.observation}, nil
}

type dbQueryObservedConn struct {
	driver.Conn
	observation *dbQueryObservation
}

func (c dbQueryObservedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	first := strings.HasPrefix(query, "SELECT * FROM (")
	if first {
		c.observation.queries.Add(1)
		if hold := c.observation.hold.Swap(nil); hold != nil {
			close(hold.entered)
			<-hold.release
		}
	}
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil || !first && !strings.HasPrefix(query, "SELECT s.z") {
		return rows, err
	}
	return dbQueryObservedRows{Rows: rows, observation: c.observation}, nil
}

type dbQueryObservedRows struct {
	driver.Rows
	observation *dbQueryObservation
}

func (r dbQueryObservedRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	for _, value := range dest {
		size := int64(0)
		switch v := value.(type) {
		case string:
			size = int64(len(v))
		case []byte:
			size = int64(len(v))
		}
		for old := r.observation.maxValue.Load(); size > old && !r.observation.maxValue.CompareAndSwap(old, size); old = r.observation.maxValue.Load() {
		}
	}
	return err
}
func (r dbQueryObservedRows) ColumnTypeDatabaseTypeName(index int) string {
	return r.Rows.(driver.RowsColumnTypeDatabaseTypeName).ColumnTypeDatabaseTypeName(index)
}
func (c dbQueryObservedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.observation.readOnly.Store(opts.ReadOnly)
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func observeDBQuery(t *testing.T, db *sql.DB) (*sql.DB, *dbQueryObservation) {
	t.Helper()
	config, err := pgx.ParseConfig(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT current_database()").Scan(&config.Database); err != nil {
		t.Fatal(err)
	}
	observation := &dbQueryObservation{}
	observed := sql.OpenDB(dbQueryConnector{Connector: stdlib.GetConnector(*config), observation: observation})
	t.Cleanup(func() { observed.Close() })
	return observed, observation
}
