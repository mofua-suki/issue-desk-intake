package usage

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// Keep each statement well below PostgreSQL's 65535 parameter limit.
const insertBatchRows = 200

type insertBatch struct {
	tx     *sql.Tx
	prefix string
	width  int
	args   []any
}

func newInsertBatch(tx *sql.Tx, kind string) *insertBatch {
	b := &insertBatch{tx: tx}
	switch kind {
	case "usage":
		b.prefix, b.width = "INSERT INTO usage_rows(key,at,host,source,repo,issue,step,provider,model,json,sender) VALUES", 11
	case "limits":
		b.prefix, b.width = "INSERT INTO usage_limit_rows(key,at,host,source,provider,\"limit\",json,sender) VALUES", 8
	case "fixedTests":
		b.prefix, b.width = "INSERT INTO fixed_test_usage(key,at,host,repo,scope,json,sender) VALUES", 7
	case "logLines":
		b.prefix, b.width = "INSERT INTO runner_log_lines(key,at,host,line,sender) VALUES", 5
	}
	return b
}

func (b *insertBatch) add(ctx context.Context, values []any, counts *Counts) error {
	b.args = append(b.args, values...)
	if len(b.args)/b.width == insertBatchRows {
		return b.flush(ctx, counts)
	}
	return nil
}

func (b *insertBatch) flush(ctx context.Context, counts *Counts) error {
	if len(b.args) == 0 {
		return nil
	}
	var query strings.Builder
	query.WriteString(b.prefix)
	for i := range b.args {
		if i%b.width == 0 {
			if i > 0 {
				query.WriteByte(',')
			}
			query.WriteByte('(')
		} else {
			query.WriteByte(',')
		}
		query.WriteByte('$')
		query.WriteString(strconv.Itoa(i + 1))
		if i%b.width == b.width-1 {
			query.WriteByte(')')
		}
	}
	query.WriteString(" ON CONFLICT(key) DO NOTHING")
	result, err := b.tx.ExecContext(ctx, query.String(), b.args...)
	if err != nil {
		return err
	}
	added, err := result.RowsAffected()
	if err != nil {
		return err
	}
	counts.Added += int(added)
	counts.Duplicate += len(b.args)/b.width - int(added)
	b.args = nil
	return nil
}
