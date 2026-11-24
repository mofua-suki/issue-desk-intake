package draftsummary

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
)

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

type draftSummaryExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// EnsureDraftSummary installs the nullable saved screen projection. It accepts
// both *sql.DB and *sql.Tx; schema must already contain drafts. The DDL is one
// atomic PostgreSQL statement group, including replacement of the trigger.
// Existing rows are left for BackfillDraftSummary. This does not switch readers.
// Increment the format version in the trigger and backfill whenever the
// projection changes, so previously saved summaries are recomputed.
func EnsureDraftSummary(ctx context.Context, db draftSummaryExecer, schema string) error {
	if !schemaName.MatchString(schema) {
		return errors.New("schema name is invalid")
	}
	_, err := db.ExecContext(ctx, `ALTER TABLE `+schema+`.drafts ADD COLUMN IF NOT EXISTS screen_summary TEXT;
ALTER TABLE `+schema+`.drafts ADD COLUMN IF NOT EXISTS screen_summary_version bigint;
CREATE OR REPLACE FUNCTION `+schema+`.drafts_screen_summary("json" text) RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT SET search_path = pg_catalog AS $summary$
DECLARE
 source_json json;
 source_object jsonb;
BEGIN
 IF position('\u0000' in json) > 0 THEN
  RETURN NULL;
 END IF;
 -- json preserves number spellings and duplicate keys in retained fields.
 -- Convert once to jsonb only for the derived fields' existing semantics.
 source_json := json::json;
 IF json_typeof(source_json) <> 'object' THEN
  RETURN NULL;
 END IF;
 source_object := source_json::jsonb;
 RETURN (SELECT COALESCE(json_object_agg(key,value), '{}'::json) FROM (SELECT key,value FROM json_each(source_json) WHERE key IN ('id','owner','status','project','unassigned','title','shownTitle','createdAt','updatedAt','priority','retryAt','waitStartedAt','turnStopped','queued','continuationLimited','investigationNotice','investigations','priorityReviewRound','usageReviewRound','inventoryRound','parentSplit','questions','issue','issues','projectHandoff','handoffRow','origin','followup','priorityChangeable','finishingTurn','splitChildren','finishedWithoutIssue','ready','kind','consultationChildren','basisConflicts','attempted','_stopped') UNION ALL SELECT 'proposal', CASE WHEN json_typeof(source_json->'proposal') = 'null' THEN 'null'::json ELSE json_build_object('kind', source_json->'proposal'->'kind') END WHERE jsonb_exists(source_object, 'proposal') UNION ALL SELECT key,value::json FROM jsonb_each('{}'::jsonb || jsonb_build_object('messages', (
			SELECT COALESCE(jsonb_agg(m), '[]'::jsonb) FROM (
				(SELECT jsonb_build_object('role','user','text', CASE WHEN jsonb_typeof(value->'text') = 'string' THEN left(value->>'text',60) ELSE '' END) AS m, 1 AS n
				 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(source_object->'messages') = 'array' THEN source_object->'messages' ELSE '[]'::jsonb END) WITH ORDINALITY AS u(value, ord)
				 WHERE value->>'role' = 'user' ORDER BY ord LIMIT 1)
				UNION ALL SELECT '{"role":"proxy"}'::jsonb, 2 WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(source_object->'messages') = 'array' THEN source_object->'messages' ELSE '[]'::jsonb END) v WHERE v->>'role' = 'proxy')
				UNION ALL SELECT '{"role":"image-marker","images":[{"id":"00000000-0000-0000-0000-000000000000"}]}'::jsonb, 3 WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(source_object->'messages') = 'array' THEN source_object->'messages' ELSE '[]'::jsonb END) v CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(v->'images') = 'array' THEN v->'images' ELSE '[]'::jsonb END) img WHERE img->>'id' ~ '^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$')
				ORDER BY n) summary), 'body', CASE WHEN jsonb_typeof(source_object->'body') = 'string' AND position('<!-- issue-desk:followup -->' in source_object->>'body') > 0 THEN '<!-- issue-desk:followup -->' ELSE '' END,
			'usageMeasurementHold', CASE WHEN source_object->'usageMeasurementHold' IS NOT NULL AND source_object->'usageMeasurementHold' <> 'null'::jsonb THEN 'true'::jsonb ELSE 'null'::jsonb END,
			'consultationBudget', jsonb_build_object('stopped', CASE WHEN jsonb_typeof(source_object->'consultationBudget'->'stopped') = 'object' THEN '{}'::jsonb ELSE source_object->'consultationBudget'->'stopped' END,
                'pendingCall', CASE WHEN source_object->'consultationBudget'->'pendingCall' IS NOT NULL AND source_object->'consultationBudget'->'pendingCall' <> 'null'::jsonb THEN 'true'::jsonb ELSE 'null'::jsonb END,
                'unknownCalls', source_object->'consultationBudget'->'unknownCalls'))|| CASE WHEN jsonb_exists(source_object, 'splitIssues') THEN jsonb_build_object('splitIssues', CASE WHEN jsonb_typeof(source_object->'splitIssues') = 'array' THEN (SELECT COALESCE(jsonb_agg(CASE WHEN jsonb_typeof(v->'project') = 'string' THEN jsonb_build_object('project',v->'project') ELSE '{}'::jsonb END ORDER BY ord), '[]'::jsonb) FROM jsonb_array_elements(source_object->'splitIssues') WITH ORDINALITY AS entries(v,ord)) WHEN source_object->'splitIssues' = 'null'::jsonb THEN 'null'::jsonb ELSE 'true'::jsonb END) ELSE '{}'::jsonb END)) AS fields)::text;
EXCEPTION WHEN OTHERS THEN
 -- Invalid JSON, unsupported Unicode and conversion failures must not reject
 -- the original text write. Cancellation is deliberately not caught by OTHERS.
 RETURN NULL;
END;
$summary$;
CREATE OR REPLACE FUNCTION `+schema+`.drafts_set_screen_summary() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog AS $trigger$
BEGIN
 NEW.screen_summary_version := 2;
 NEW.screen_summary := `+schema+`.drafts_screen_summary(NEW.json);
 RETURN NEW;
END;
$trigger$;
DROP TRIGGER IF EXISTS drafts_screen_summary ON `+schema+`.drafts;
CREATE TRIGGER drafts_screen_summary BEFORE INSERT OR UPDATE OF json ON `+schema+`.drafts
FOR EACH ROW EXECUTE FUNCTION `+schema+`.drafts_set_screen_summary();`)
	if err != nil {
		return fmt.Errorf("ensure draft summary: %w", err)
	}
	return nil
}

type draftSummaryBeginner interface {
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

// BackfillDraftSummary fills missing or outdated summaries in primary-key order, committing
// each batch separately. It returns only committed row counts (including rows
// whose summary remains NULL). A NULL result advances the cursor within this
// invocation; a later invocation retries it. Current non-NULL summaries and
// json are untouched. Concurrent writes are serialized by row locks; this is not a
// snapshot of concurrent inserts, which may require another invocation.
// After restoring a pre-column backup, call EnsureDraftSummary first, then this
// function. A canceled/failed run can be resumed by calling it again.
func BackfillDraftSummary(ctx context.Context, db draftSummaryBeginner, schema string, batchSize int) (int64, error) {
	if !schemaName.MatchString(schema) {
		return 0, errors.New("schema name is invalid")
	}
	if batchSize <= 0 {
		return 0, errors.New("batch size must be positive")
	}
	var total int64
	var cursor *string
	for {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return total, fmt.Errorf("begin draft summary batch: %w", err)
		}
		var count int64
		var last sql.NullString
		err = tx.QueryRowContext(ctx, `WITH batch AS (
 SELECT id FROM `+schema+`.drafts
 WHERE (screen_summary IS NULL OR screen_summary_version IS DISTINCT FROM 2) AND ($1::text IS NULL OR id > $1)
 ORDER BY id LIMIT $2 FOR UPDATE
), updated AS (
 UPDATE `+schema+`.drafts AS d SET screen_summary = `+schema+`.drafts_screen_summary(d.json), screen_summary_version = 2
 FROM batch WHERE d.id = batch.id RETURNING d.id
)
SELECT count(*), (SELECT id FROM updated ORDER BY id DESC LIMIT 1) FROM updated`, cursor, batchSize).Scan(&count, &last)
		if err != nil {
			tx.Rollback()
			return total, fmt.Errorf("update draft summary batch: %w", err)
		}
		if err = tx.Commit(); err != nil {
			return total, fmt.Errorf("commit draft summary batch: %w", err)
		}
		total += count
		if count == 0 {
			return total, nil
		}
		cursor = &last.String
	}
}
