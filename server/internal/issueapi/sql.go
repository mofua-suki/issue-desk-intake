package issueapi

import "issue-desk-intake/server/internal/appsql"

// sqlDialect is the dialect of the application database that issueapi runs on.
// Statements against it go through bindSQL so the SQL stays portable.
var sqlDialect appsql.Active

// bindSQL converts the question-mark parameters of a statement built from
// constants. It panics on a malformed statement, a programming error.
func bindSQL(query string) string { return sqlDialect.MustBind(query) }

func rowOrder(desc bool) string { return sqlDialect.MustRowOrder(desc) }

// jsonText reads a constant JSON path of a JSON text column as text.
func jsonText(column, path string) string { return sqlDialect.MustJSONText(column, path) }

// safeJSONNumber treats malformed and out-of-range numeric text as unmatched.
func safeJSONNumber(column, path string) string {
	value := jsonText(column, path)
	return "(CASE WHEN pg_input_is_valid(" + value + ", 'numeric') THEN " + value + "::numeric END)"
}
