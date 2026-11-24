package filing

import "issue-desk-intake/server/internal/appsql"

var sqlDialect appsql.Active

func bindSQL(query string) string { return sqlDialect.MustBind(query) }

func rowOrder(desc bool) string { return sqlDialect.MustRowOrder(desc) }

func insertIgnoreSQL(table string, columns []string, source string) string {
	return sqlDialect.MustInsertIgnore(table, columns, source)
}

// jsonText reads a constant JSON path of a JSON text column as text.
func jsonText(column, path string) string { return sqlDialect.MustJSONText(column, path) }

// tableExistsSQL is the already-bound statement counting the table named by
// its one parameter (0 or 1 in the single result row).
func tableExistsSQL() string { return sqlDialect.MustTableExists() }
