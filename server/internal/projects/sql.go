package projects

import "issue-desk-intake/server/internal/appsql"

var sqlDialect appsql.Active

func bindSQL(query string) string { return sqlDialect.MustBind(query) }

func rowOrder(desc bool) string { return sqlDialect.MustRowOrder(desc) }
