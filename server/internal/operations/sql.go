package operations

import "issue-desk-intake/server/internal/appsql"

var sqlDialect appsql.Active

func bindSQL(query string) string { return sqlDialect.MustBind(query) }

func rowOrder(desc bool) string { return sqlDialect.MustRowOrder(desc) }

// tableExistsSQL is the already-bound statement counting the table named by
// its one parameter (0 or 1 in the single result row).
func tableExistsSQL() string { return sqlDialect.MustTableExists() }
