package attachments

import "issue-desk-intake/server/internal/appsql"

// sqlDialect binds queries for the application database (PostgreSQL).
var sqlDialect appsql.Active

func bindSQL(query string) string { return sqlDialect.MustBind(query) }
