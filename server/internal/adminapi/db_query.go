package adminapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

const dbQueryMaxBytes = 262144

// Same limit as lexDBQuery: longer SQL is always refused before execution.
const dbQueryMaxSQLBytes = 65536

// Shared by all registrations in this process, including both target pools.
var dbQuerySlot sync.Mutex

type dbQueryColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}
type dbQueryResult struct {
	Columns   []dbQueryColumn `json:"columns"`
	Rows      [][]any         `json:"rows"`
	Truncated struct {
		Rows  bool `json:"rows"`
		Bytes bool `json:"bytes"`
	} `json:"truncated"`
}

func (s *service) diagnosticsDBQuery(r *http.Request, p auth.Principal) (any, int, error) {
	return s.dbQuery(r, p, nil)
}

// Authorization failures also enter here for one audit row, but never execute SQL.
func (s *service) dbQuery(r *http.Request, p auth.Principal, denial error) (value any, status int, err error) {
	var target, query, grantID string
	defer func() {
		if auditErr := s.auditDBQuery(p, target, query, grantID, status, err); auditErr != nil {
			value, status, err = nil, 0, bad(503, "DB照会の監査を保存できません。")
		}
	}()
	data, parseErr := body(r)
	if data != nil {
		target, query = str(data["target"]), str(data["sql"])
	}
	if denial != nil {
		return nil, 0, denial
	}
	if r.Method != http.MethodPost {
		return nil, 0, bad(400, "POSTでDB照会を指定してください。")
	}
	if parseErr != nil {
		return nil, 0, parseErr
	}
	maxRows, e := dbQueryRowLimit(data, target, query)
	if e != nil {
		return nil, 0, e
	}
	if !dbQuerySlot.TryLock() {
		return nil, 0, bad(409, "別のDB照会を実行中です。")
	}
	defer dbQuerySlot.Unlock()
	if s.Postgres == nil {
		return nil, 0, bad(503, "DB照会の共有DBが使えません。")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	conn, connErr := s.Postgres.Conn(ctx)
	if connErr != nil {
		return nil, 0, dbQueryError(connErr)
	}
	defer conn.Close()
	// Re-read only after obtaining the execution slot. Permanent grants do not
	// authorize this operation even if a malformed row bypassed grant creation.
	err = conn.QueryRowContext(ctx, `SELECT id FROM public.permission_grants
WHERE kind='db-read' AND target=$1 AND revoked_at IS NULL AND expires_at > $2
ORDER BY created_at,id LIMIT 1`, target, s.Now()).Scan(&grantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, bad(403, "対象に有効なdb-readの許可が必要です。")
	}
	if err != nil {
		return nil, 0, dbQueryError(err)
	}
	guarded, guardErr := guardDBQuery(target, query)
	if guardErr != nil {
		return nil, 0, bad(400, guardErr.Error())
	}
	result, execErr := executeDBQuery(ctx, conn, target, guarded, maxRows)
	if execErr != nil {
		return nil, 0, execErr
	}
	return result, 200, nil
}

// auditDBQuery records the request with its outcome. Only the caller's SQL is
// kept, never result values or PostgreSQL error details.
func (s *service) auditDBQuery(p auth.Principal, target, query, grantID string, status int, err error) error {
	result := status
	if err != nil {
		result = 503
		var problem *contract.Problem
		if errors.As(err, &problem) {
			result = problem.Status
		}
	}
	// Keep the SQL supplied by the caller, not its normalized execution form.
	// Never put result values or PostgreSQL error details into this record.
	// Only the two valid targets are recorded: any other caller-supplied
	// text here would let a denied request store an unbounded audit row.
	auditTarget := target
	if auditTarget != "shared" && auditTarget != "app" {
		auditTarget = ""
	}
	record := map[string]any{"grantId": grantID, "target": auditTarget, "sql": query, "status": result}
	if len(query) > dbQueryMaxSQLBytes {
		// Never executed (the guard refuses this length). Keep a bounded
		// prefix; the size and digest still identify the full text.
		end := dbQueryMaxSQLBytes
		for end > 0 && !utf8.RuneStart(query[end]) {
			end--
		}
		digest := sha256.Sum256([]byte(query))
		record["sql"], record["sqlBytes"], record["sqlSha256"] = query[:end], len(query), hex.EncodeToString(digest[:])
	}
	detail, _ := json.Marshal(record)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tx, auditErr := s.AppDB.BeginTx(ctx, nil)
	if auditErr == nil {
		defer tx.Rollback()
		auditErr = s.audit(ctx, tx, p, "db-query", string(detail))
		if auditErr == nil {
			auditErr = tx.Commit()
		}
	}
	return auditErr
}

// dbQueryRowLimit validates the request body and returns the row limit.
func dbQueryRowLimit(data map[string]any, target, query string) (int64, error) {
	if problem := contract.OnlyKeys(data, "target", "sql", "maxRows"); problem != nil {
		return 0, problem
	}
	if target != "shared" && target != "app" || query == "" {
		return 0, bad(400, "対象shared/appとSQLを指定してください。")
	}
	maxRows := int64(100)
	if supplied, exists := data["maxRows"]; exists {
		var ok bool
		maxRows, ok = integer(supplied)
		if !ok || maxRows < 1 || maxRows > 1000 {
			return 0, bad(400, "maxRowsは1〜1000の整数で指定してください。")
		}
	}
	return maxRows, nil
}

func dbQueryError(err error) error {
	var problem *contract.Problem
	if errors.As(err, &problem) {
		return problem
	}
	var pg *pgconn.PgError
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &pg) && pg.Code == "57014" {
		return bad(504, "DB照会が時間上限を超えました。")
	}
	// SQLSTATE classes caused by the submitted statement itself: unsupported
	// feature, cardinality, data, syntax/name and program-limit errors, plus
	// read-only, lock-timeout and out-of-memory for this statement. Everything
	// else (connection, shutdown, internal) stays a 503.
	if pg != nil && (dbQueryListed("0A 21 22 42 54", pg.Code[:min(2, len(pg.Code))]) || dbQueryListed("25006 55P03 53200", pg.Code)) {
		return bad(400, "DBが照会を拒否しました。")
	}
	return bad(503, "DB照会を実行できません。")
}

func executeDBQuery(ctx context.Context, conn *sql.Conn, target, query string, maxRows int64) (*dbQueryResult, error) {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, dbQueryError(err)
	}
	defer tx.Rollback()
	schema := "public"
	if target == "app" {
		schema = "issue_desk_app_migration"
	}
	for _, setting := range []string{
		"SET LOCAL statement_timeout = '5s'",
		"SET LOCAL lock_timeout = '500ms'",
		"SET LOCAL search_path = pg_catalog, " + schema + ", pg_temp",
	} {
		if _, err = tx.ExecContext(ctx, setting); err != nil {
			return nil, dbQueryError(err)
		}
	}
	// Describe first: LIMIT 0 plans the statement without producing rows, so
	// names and types are refused before any value is computed.
	described, err := tx.QueryContext(ctx, fmt.Sprintf("SELECT * FROM (%s) AS q LIMIT 0", query))
	if err != nil {
		return nil, dbQueryError(err)
	}
	types, err := described.ColumnTypes()
	if closeErr := described.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, dbQueryError(err)
	}
	if len(types) == 0 || len(types) > 1600 {
		return nil, bad(400, "結果の列数は1〜1600にしてください。")
	}
	result := &dbQueryResult{Columns: []dbQueryColumn{}, Rows: [][]any{}}
	names, sizes, values := make([]string, len(types)), make([]string, len(types)), make([]string, len(types))
	for i, column := range types {
		kind := column.DatabaseTypeName()
		// Positive scalar list excludes composite, record, bytea and unknown
		// wire types, regardless of aliases or empty results.
		if dbQueryCredentialName(column.Name()) || !dbQueryListed("BOOL INT2 INT4 INT8 FLOAT4 FLOAT8 NUMERIC TEXT VARCHAR BPCHAR NAME DATE TIMESTAMP TIMESTAMPTZ TIME TIMETZ INTERVAL UUID JSON JSONB", kind) {
			return nil, bad(400, "結果の列名または型は許可されていません。")
		}
		result.Columns = append(result.Columns, dbQueryColumn{Name: column.Name(), Type: kind})
		names[i] = fmt.Sprintf("c%d", i+1)
		sizes[i] = fmt.Sprintf("COALESCE(pg_catalog.octet_length(q.c%d::pg_catalog.text), 0)::pg_catalog.int8", i+1)
		values[i] = fmt.Sprintf("CASE WHEN s.z <= %d THEN s.c%d END", dbQueryMaxBytes, i+1)
	}
	// Account for metadata, JSON escaping, flags and Encoder's final newline.
	raw, err := json.Marshal(result)
	if err != nil || len(raw)+1 > dbQueryMaxBytes {
		return nil, bad(400, "結果の列情報が大きすぎます。")
	}
	budget := dbQueryMaxBytes - len(raw) - 1
	// PostgreSQL measures each row (z = bytes of its values as text) and sends
	// the values only when the row could fit the response at all. A larger row
	// arrives as NULLs, so the intake never buffers more than about one response
	// limit per row, whatever expression builds the value. Positional aliases
	// keep duplicate column labels apart.
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("SELECT s.z, %s FROM (SELECT q.*, %s AS z FROM (%s) AS q(%s) LIMIT %d) AS s",
		strings.Join(values, ", "), strings.Join(sizes, " + "), query, strings.Join(names, ", "), maxRows+1))
	if err != nil {
		return nil, dbQueryError(err)
	}
	defer rows.Close()
	for rows.Next() {
		if int64(len(result.Rows)) == maxRows {
			result.Truncated.Rows = true
			break
		}
		var size int64
		values, pointers := make([]any, len(types)), make([]any, len(types)+1)
		pointers[0] = &size
		for i := range values {
			pointers[i+1] = &values[i]
		}
		if err = rows.Scan(pointers...); err != nil {
			return nil, dbQueryError(err)
		}
		if size > dbQueryMaxBytes {
			result.Truncated.Bytes = true
			break
		}
		for i, v := range values {
			if bytes, ok := v.([]byte); ok {
				values[i] = string(bytes)
			}
		}
		raw, err = json.Marshal(values)
		if err != nil {
			return nil, bad(400, "結果をJSONで表現できません。")
		}
		cost := len(raw)
		if len(result.Rows) > 0 {
			cost++
		}
		if cost > budget {
			result.Truncated.Bytes = true
			break
		}
		budget -= cost
		result.Rows = append(result.Rows, values)
	}
	if err = rows.Err(); err != nil {
		return nil, dbQueryError(err)
	}
	if err = rows.Close(); err != nil {
		return nil, dbQueryError(err)
	}
	if err = tx.Commit(); err != nil {
		return nil, dbQueryError(err)
	}
	return result, nil
}
