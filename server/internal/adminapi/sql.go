package adminapi

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"
)

func queryMaps(ctx context.Context, tx queryRunner, query string, args ...any) ([]map[string]any, error) {
	rows, e := tx.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	columns, e := rows.Columns()
	if e != nil {
		return nil, e
	}
	out := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if e = rows.Scan(pointers...); e != nil {
			return nil, e
		}
		row := map[string]any{}
		for i, key := range columns {
			v := values[i]
			if raw, ok := v.([]byte); ok {
				v = string(raw)
			}
			if instant, ok := v.(time.Time); ok {
				v = iso(instant)
			}
			row[key] = v
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
func queryMap(ctx context.Context, tx queryRunner, query string, args ...any) (map[string]any, error) {
	rows, e := queryMaps(ctx, tx, query, args...)
	if e != nil {
		return nil, e
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

type queryRunner interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}
type repairTx struct{ *sql.Tx }

func (t *repairTx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return t.Tx.QueryContext(ctx, pgBind(q), args...)
}
func (t *repairTx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, pgBind(q), args...)
}
func pgBind(q string) string {
	var out strings.Builder
	index := 0
	for _, c := range q {
		if c == '?' {
			index++
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(index))
		} else {
			out.WriteRune(c)
		}
	}
	return out.String()
}
func (s *service) beginRepair(ctx context.Context) (*repairTx, error) {
	if s.Postgres == nil {
		return nil, bad(503, "修理と許可の共有DBが使えません。")
	}
	tx, e := s.Postgres.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	return &repairTx{tx}, nil
}
