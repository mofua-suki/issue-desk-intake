package statistics

import (
	"context"
	"database/sql"
	"errors"

	"issue-desk-intake/server/internal/appsql"
)

// sqlDialect is the PostgreSQL dialect for the application database read via
// Service.AppDB and the caller's local snapshot. Its statements go
// through bindSQL.
// Other shared database statements use PostgreSQL placeholders directly.
var sqlDialect appsql.Active

// bindSQL converts the question-mark parameters of a statement built from
// constants. It panics on a malformed statement, a programming error.
func bindSQL(query string) string { return sqlDialect.MustBind(query) }

func rowOrder(desc bool) string { return sqlDialect.MustRowOrder(desc) }

// queryLocalRows reads the JSON rows of one known application table. The
// table name never reaches the SQL text except through the literals below.
func queryLocalRows(ctx context.Context, db *sql.DB, table string) (*sql.Rows, error) {
	switch table {
	case "todos":
		return db.QueryContext(ctx, bindSQL("SELECT json FROM todos ORDER BY "+rowOrder(false)))
	case "stall_fixes":
		// The table the retired Node application left has no json column
		// (draft, key, stall, filed_at), so build the record from its columns.
		return db.QueryContext(ctx, bindSQL("SELECT json_build_object('draft',draft,'key',key,'stall',stall,'at',filed_at)::text FROM stall_fixes ORDER BY "+rowOrder(false)))
	case "actions":
		return db.QueryContext(ctx, bindSQL("SELECT json FROM actions ORDER BY "+rowOrder(false)))
	case "proposals":
		return db.QueryContext(ctx, bindSQL("SELECT json FROM proposals ORDER BY "+rowOrder(false)))
	case "scope_records":
		return db.QueryContext(ctx, bindSQL("SELECT json FROM scope_records ORDER BY "+rowOrder(false)))
	case "usage_review_rounds":
		return db.QueryContext(ctx, bindSQL("SELECT json FROM usage_review_rounds ORDER BY "+rowOrder(false)))
	case "usage_review_days":
		return db.QueryContext(ctx, bindSQL("SELECT json FROM usage_review_days ORDER BY "+rowOrder(false)))
	case "inventory_rounds":
		return db.QueryContext(ctx, bindSQL("SELECT json FROM inventory_rounds ORDER BY "+rowOrder(false)))
	case "priority_review_rounds":
		return db.QueryContext(ctx, bindSQL("SELECT json FROM priority_review_rounds ORDER BY "+rowOrder(false)))
	case "priority_review_days":
		return db.QueryContext(ctx, bindSQL("SELECT json FROM priority_review_days ORDER BY "+rowOrder(false)))
	case "review_notes_rounds":
		return db.QueryContext(ctx, bindSQL("SELECT json FROM review_notes_rounds ORDER BY "+rowOrder(false)))
	}
	return nil, errors.New("unknown statistics table")
}

// tableExistsSQL is the already-bound statement counting the table named by
// its one parameter (0 or 1 in the single result row).
func tableExistsSQL() string { return sqlDialect.MustTableExists() }
