package adminapi

import "issue-desk-intake/server/internal/appsql"

// sqlDialect is the dialect of the application database that the admin API
// files other than api.go and sql.go run on. Statements against it go through
// bindSQL so the SQL stays portable. The shared PostgreSQL statements keep
// their own conversion in repairTx (sql.go).
var sqlDialect appsql.Active

// bindSQL converts the question-mark parameters of a statement built from
// constants. It panics on a malformed statement, a programming error.
func bindSQL(query string) string { return sqlDialect.MustBind(query) }

// insertIgnoreSQL builds an insert that skips rows that already exist.
func insertIgnoreSQL(table string, columns []string, source string) string {
	return sqlDialect.MustInsertIgnore(table, columns, source)
}
