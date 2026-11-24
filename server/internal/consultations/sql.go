package consultations

import (
	"errors"
	"strings"

	"issue-desk-intake/server/internal/appsql"
)

func draftProjectionSQL(purpose DraftReadPurpose) (string, error) {
	var keys string
	switch purpose {
	case DraftReadRecovery, DraftReadFiling:
		keys := "'status'"
		if purpose == DraftReadFiling {
			keys = "'id','project','status','parentSplit','parentConsultation','createdAt','issue','issues','ready','changeScope','splitChildren','splitIssues','dependsOnIssues'"
		}
		return `SELECT id, CASE WHEN json_typeof(json::json) = 'object' THEN (SELECT COALESCE(json_object_agg(key,value), '{}'::json) FROM json_each(json::json) WHERE key IN (` + keys + `))::text ELSE json END FROM drafts ORDER BY ` + rowOrder(true), nil
	case DraftReadPage:
		keys = "'id','owner','project','unassigned','status','title','shownTitle','createdAt','updatedAt','parentSplit','splitChildren','issue','finishedWithoutIssue'"
	case DraftReadAlerts:
		keys = "'id','project','title','updatedAt','status','parentSplit','splitScopeHeld','consultationBudget','stallCause','error','consultationStops'"
	case DraftReadFilingAlerts:
		keys = "'id','status','issue','revision','project','title','filingCheckCalls'"
	case DraftReadScreen:
		keys = "'id','owner','status','project','unassigned','title','shownTitle','createdAt','updatedAt','priority','retryAt','waitStartedAt','turnStopped','queued','continuationLimited','investigationNotice','investigations','priorityReviewRound','usageReviewRound','inventoryRound','parentSplit','parentConsultation','questions','issue','issues','projectHandoff','handoffRow','origin','followup','priorityChangeable','finishingTurn','run','splitChildren','finishedWithoutIssue','ready','kind','consultationChildren','basisConflicts','attempted','_stopped','revision','submissionRequest'"
	default:
		return "", errors.New("invalid draft read purpose")
	}
	// Non-objects are returned unchanged so decode rejects them just as List does.
	// json (unlike jsonb) preserves the spelling of numbers for UseNumber.
	//
	// Each row is parsed once: draft_json and draft_jsonb are computed in FROM
	// subqueries that OFFSET 0 keeps PostgreSQL from inlining into every use. A
	// draft is large, so repeating the conversion per field dominated the read.
	fields := "SELECT key,value FROM json_each(draft_json) WHERE key IN (" + keys + ")"
	projection := "'{}'::jsonb"
	parsed := `(SELECT json::json AS draft_json OFFSET 0) AS parsed`
	if purpose == DraftReadScreen {
		// Keep only the first user excerpt, role presence and image presence. A
		// 60-codepoint prefix contains every possible UTF-16 60-unit prefix.
		projection += ` || jsonb_build_object('messages', (
			SELECT COALESCE(jsonb_agg(m), '[]'::jsonb) FROM (
				(SELECT jsonb_build_object('role','user','text', CASE WHEN jsonb_typeof(value->'text') = 'string' THEN left(value->>'text',60) ELSE '' END) AS m, 1 AS n
				 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(draft_jsonb->'messages') = 'array' THEN draft_jsonb->'messages' ELSE '[]'::jsonb END) WITH ORDINALITY AS u(value, ord)
				 WHERE value->>'role' = 'user' ORDER BY ord LIMIT 1)
				UNION ALL SELECT '{"role":"proxy"}'::jsonb, 2 WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(draft_jsonb->'messages') = 'array' THEN draft_jsonb->'messages' ELSE '[]'::jsonb END) v WHERE v->>'role' = 'proxy')
				UNION ALL SELECT '{"role":"image-marker","images":[{"id":"00000000-0000-0000-0000-000000000000"}]}'::jsonb, 3 WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(draft_jsonb->'messages') = 'array' THEN draft_jsonb->'messages' ELSE '[]'::jsonb END) v CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(v->'images') = 'array' THEN v->'images' ELSE '[]'::jsonb END) img WHERE img->>'id' ~ '^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$')
				ORDER BY n) summary), 'body', CASE WHEN jsonb_typeof(draft_jsonb->'body') = 'string' AND position('<!-- issue-desk:followup -->' in draft_jsonb->>'body') > 0 THEN '<!-- issue-desk:followup -->' ELSE '' END,
			'usageMeasurementHold', CASE WHEN draft_jsonb->'usageMeasurementHold' IS NOT NULL AND draft_jsonb->'usageMeasurementHold' <> 'null'::jsonb THEN 'true'::jsonb ELSE 'null'::jsonb END,
			'consultationBudget', jsonb_build_object('stopped', CASE WHEN jsonb_typeof(draft_jsonb->'consultationBudget'->'stopped') = 'object' THEN '{}'::jsonb ELSE draft_jsonb->'consultationBudget'->'stopped' END,
				'pendingCall', CASE WHEN draft_jsonb->'consultationBudget'->'pendingCall' IS NOT NULL AND draft_jsonb->'consultationBudget'->'pendingCall' <> 'null'::jsonb THEN 'true'::jsonb ELSE 'null'::jsonb END,
				'unknownCalls', draft_jsonb->'consultationBudget'->'unknownCalls'))`
		// Retry actions need the number of saved parts, without their text.
		projection += ` || CASE WHEN jsonb_typeof(draft_jsonb->'consultationParts') = 'array'
			THEN jsonb_build_object('_consultationPartCount', jsonb_array_length(draft_jsonb->'consultationParts')) ELSE '{}'::jsonb END`
		fields += ` UNION ALL SELECT 'proposal', CASE WHEN json_typeof(draft_json->'proposal') = 'null' THEN 'null'::json ELSE json_build_object('kind', draft_json->'proposal'->'kind') END WHERE jsonb_exists(draft_jsonb, 'proposal')`
	}
	if purpose == DraftReadPage || purpose == DraftReadScreen {
		projection += `|| CASE WHEN jsonb_exists(draft_jsonb, 'splitIssues') THEN jsonb_build_object('splitIssues', CASE WHEN jsonb_typeof(draft_jsonb->'splitIssues') = 'array' THEN (SELECT COALESCE(jsonb_agg(CASE WHEN jsonb_typeof(v->'project') = 'string' THEN jsonb_build_object('project',v->'project') ELSE '{}'::jsonb END ORDER BY ord), '[]'::jsonb) FROM jsonb_array_elements(draft_jsonb->'splitIssues') WITH ORDINALITY AS entries(v,ord)) WHEN draft_jsonb->'splitIssues' = 'null'::jsonb THEN 'null'::jsonb ELSE 'true'::jsonb END) ELSE '{}'::jsonb END`
		// As before, only objects are converted to jsonb; the cast reads the
		// already validated text of draft_json.
		parsed = `(SELECT draft_json, CASE WHEN json_typeof(draft_json) = 'object' THEN draft_json::jsonb END AS draft_jsonb FROM ` + parsed + ` OFFSET 0) AS converted`
	}

	projection = "(SELECT COALESCE(json_object_agg(key,value), '{}'::json) FROM (" + fields + " UNION ALL SELECT key,value::json FROM jsonb_each(" + projection + ")) AS fields)"
	// PostgreSQL jsonb rejects escaped NUL even though json and Go accept it.
	// Return only those exceptional rows in full; decode still validates them.
	return `SELECT id, CASE WHEN position('\u0000' in json) > 0 THEN json ELSE (SELECT CASE WHEN json_typeof(draft_json) = 'object' THEN (` + projection + `)::text ` +
		"ELSE json END FROM " + parsed + ") END FROM drafts ORDER BY " + rowOrder(true), nil
}

func selectedDraftsSQL(ids []string) (string, []any) {
	marks := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		marks[i], args[i] = "?", id
	}
	return bindSQL("SELECT json FROM drafts WHERE id IN (" + strings.Join(marks, ",") + ") ORDER BY " + rowOrder(true)), args
}

func todoProjectionSQL() string {
	return `SELECT id, CASE WHEN json_typeof(json::json) = 'object' THEN (SELECT COALESCE(json_object_agg(key,value), '{}'::json) FROM json_each(json::json) WHERE key IN ('owner','project'))::text ELSE json END FROM todos ORDER BY ` + rowOrder(true)
}

func selectedTodosSQL(ids []string) (string, []any) {
	marks := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		marks[i], args[i] = "?", id
	}
	return bindSQL("SELECT json FROM todos WHERE id IN (" + strings.Join(marks, ",") + ") ORDER BY " + rowOrder(true)), args
}

// sqlDialect is the dialect of the database consultations currently run on.
// Statements go through it so the SQL stays portable; the caller of a database
// method stays the consultations function, not a relay, so conflict logs name
// the real operation.
var sqlDialect appsql.Active

// bindSQL converts the question-mark parameters of a statement built from
// constants. It panics on a malformed statement, which is a programming error
// that every test path of the statement would reveal.
func bindSQL(query string) string { return sqlDialect.MustBind(query) }

func rowOrder(desc bool) string { return sqlDialect.MustRowOrder(desc) }

func binaryType() string { return sqlDialect.MustBinary() }

func identityType() string { return sqlDialect.MustIdentity() }

func insertIgnoreSQL(table string, columns []string, source string) string {
	return sqlDialect.MustInsertIgnore(table, columns, source)
}
