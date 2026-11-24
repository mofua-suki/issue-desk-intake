package startup

import "issue-desk-intake/server/internal/appsql"

// referenceDialect is the dialect of the application database that the
// reference acceptance reads.
var referenceDialect appsql.Active

func referenceTableExistsSQL() string { return referenceDialect.MustTableExists() }

// referenceAttentionSQL selects the newest attention case of an issue.
func referenceAttentionSQL() string {
	project := referenceDialect.MustJSONText("json", "$.project")
	number := referenceDialect.MustJSONNumber("json", "$.number")
	order := referenceDialect.MustRowOrder(true)
	return referenceDialect.MustBind("SELECT json FROM attention_cases WHERE " + project + "=? AND " + number + "=? ORDER BY " + order + " LIMIT 1")
}

// referenceAttentionForIssuesSQL returns the same newest row as the single
// issue query for each requested number, in one scan of the repository's cases.
func referenceAttentionForIssuesSQL() string {
	project := referenceDialect.MustJSONText("json", "$.project")
	number := referenceDialect.MustJSONNumber("json", "$.number")
	order := referenceDialect.MustRowOrder(true)
	return referenceDialect.MustBind("SELECT DISTINCT ON (" + number + ") (" + number + ")::bigint, json FROM attention_cases WHERE " + project + "=? AND " + number + " IN (SELECT value::bigint FROM jsonb_array_elements_text(?::jsonb)) ORDER BY " + number + ", " + order)
}
