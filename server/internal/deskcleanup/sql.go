package deskcleanup

import "issue-desk-intake/server/internal/appsql"

// sqlDialect is the dialect of the application database that deskcleanup runs on.
// Statements against it go through bindSQL so the SQL stays portable.
// PostgreSQL (shared database) calls are written with their own placeholders.
var sqlDialect appsql.Active

// bindSQL converts the question-mark parameters of a statement built from
// constants. It panics on a malformed statement, a programming error.
func bindSQL(query string) string { return sqlDialect.MustBind(query) }
