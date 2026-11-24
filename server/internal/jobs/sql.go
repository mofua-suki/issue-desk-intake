package jobs

import "issue-desk-intake/server/internal/appsql"

// sqlDialect is the dialect of the application database (Options.AppDB, PostgreSQL in production).
// Statements against it go through bindSQL so the SQL stays portable. The
// shared PostgreSQL database is reached through Store.db and is not bound here.
var sqlDialect appsql.Active

// bindSQL converts the question-mark parameters of a statement built from
// constants. It panics on a malformed statement, a programming error.
func bindSQL(query string) string { return sqlDialect.MustBind(query) }

func rowOrder(desc bool) string { return sqlDialect.MustRowOrder(desc) }

// jsonText reads a constant JSON path of a JSON text column as text.
func jsonText(column, path string) string { return sqlDialect.MustJSONText(column, path) }
