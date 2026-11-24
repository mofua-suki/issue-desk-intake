package adminapi

import "strings"

// SQL fragments of the decision lists use the application database dialect
// (PostgreSQL). jsonPage takes them as arguments.

func jsonTextSQL(column, path string) string { return sqlDialect.MustJSONText(column, path) }

func rowOrderSQL(desc bool) string { return sqlDialect.MustRowOrder(desc) }

func binaryCollationSQL() string { return sqlDialect.MustBinaryCollation() }

// decisionsWhere lists the decisions that are not removed and belong to the
// project (or to no project). It has two ? parameters, both the project.
func decisionsWhere() string {
	project := jsonTextSQL("json", "$.project")
	return jsonTextSQL("json", "$.removedAt") + " IS NULL AND (? = '' OR " + project + " IS NULL OR " + project + " = ?)"
}

// decisionsOrder lists the newest decision first.
func decisionsOrder() string { return rowOrderSQL(true) }

// aiDecisionsWhere filters by kind and who. It has four ? parameters.
func aiDecisionsWhere() string {
	return "(? = '' OR " + jsonTextSQL("json", "$.kind") + " = ?) AND (? = '' OR " + jsonTextSQL("json", "$.who") + " = ?)"
}

// aiDecisionsOrder sorts by time, then id (byte order), then insertion order.
func aiDecisionsOrder() string {
	collate := " COLLATE " + binaryCollationSQL()
	return "COALESCE(" + jsonTextSQL("json", "$.at") + ", '')" + collate + ", COALESCE(" + jsonTextSQL("json", "$.id") + ", '')" + collate + ", " + rowOrderSQL(false)
}

// aiDecisionStatsSQL has no parameters or filters: stats always covers all rows.
// MATERIALIZED prevents repeated full JSONB casts for the four fields. Sort in
// Go by the projected at/id strings; insertion order breaks exact ties, just as
// jsonList + SliceStable does. The database primary key is deliberately unused.
func aiDecisionStatsSQL() string {
	fields := []string{}
	for _, key := range []string{"kind", "score", "at", "id"} {
		fields = append(fields, "CASE WHEN jsonb_typeof(d->'"+key+"') = 'string' THEN d->>'"+key+"' ELSE '' END")
	}
	// API-normalized records have scalar fields and flat string arrays. Nested
	// containers or duplicate keys can hide input beyond Go's nesting limit after
	// JSONB conversion; use the original decoder for those stored legacy values.
	flat := `NOT EXISTS (SELECT 1 FROM jsonb_each(CASE WHEN jsonb_typeof(d) = 'object' THEN d ELSE '{}'::jsonb END) AS v WHERE
 jsonb_typeof(v.value) = 'object' OR EXISTS (
 SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(v.value) = 'array' THEN v.value ELSE '[]'::jsonb END) AS item
 WHERE jsonb_typeof(item) IN ('object', 'array')))`
	return "WITH records AS MATERIALIZED (SELECT json::jsonb AS d, json IS JSON WITH UNIQUE KEYS AS unique_keys, source_rowid FROM ai_decisions) SELECT " +
		strings.Join(fields, ", ") + ", jsonb_typeof(d) IN ('object', 'null') AND unique_keys AND " + flat + " FROM records ORDER BY " + rowOrderSQL(false)
}
