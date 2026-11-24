package admincli

import "issue-desk-intake/server/internal/appsql"

// sqlDialect is the dialect of the database this package currently runs on.
var sqlDialect appsql.Active

// bindSQL converts the question-mark parameters of a statement built from constants.
func bindSQL(query string) string { return sqlDialect.MustBind(query) }
