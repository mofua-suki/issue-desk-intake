package topicdb

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
)

type Execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// InstallTopicSummary installs the projection used by topic links.
// NULL indicates that the original document must be decoded by the reader.
func InstallTopicSummary(ctx context.Context, db Execer, schema string) error {
	if !schemaName.MatchString(schema) {
		return fmt.Errorf("invalid schema name")
	}
	for _, table := range []string{"drafts", "todos"} {
		fields := "'topicLinks','status','finishedWithoutIssue','issue','issues'"
		if table == "todos" {
			fields = "''"
		}
		_, err := db.ExecContext(ctx, `ALTER TABLE `+schema+`.`+table+` ADD COLUMN IF NOT EXISTS topic_summary text, ADD COLUMN IF NOT EXISTS topic_summary_processed boolean NOT NULL DEFAULT false;
CREATE OR REPLACE FUNCTION `+schema+`.`+table+`_topic_summary(source text) RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT SET search_path = pg_catalog AS $body$
DECLARE j jsonb; result text;
BEGIN
 IF NOT (source IS JSON WITH UNIQUE KEYS) OR NOT pg_input_is_valid(source, 'jsonb') THEN RETURN NULL; END IF;
 j := source::jsonb;
 IF jsonb_typeof(j) <> 'object' OR EXISTS (SELECT 1 FROM jsonb_path_query(j, '$.**') v WHERE CASE WHEN jsonb_typeof(v) = 'number' THEN (v::text)::numeric >= 1e308 OR (v::text)::numeric <= -1e308 ELSE false END) THEN RETURN NULL; END IF;
 SELECT COALESCE(json_object_agg(key,value), '{}'::json)::text INTO result FROM json_each(source::json) WHERE key IN (`+fields+`);
 RETURN result;
EXCEPTION WHEN OTHERS THEN RETURN NULL;
END $body$;
CREATE OR REPLACE FUNCTION `+schema+`.`+table+`_set_topic_summary() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog AS $body$
BEGIN NEW.topic_summary := `+schema+`.`+table+`_topic_summary(NEW.json); NEW.topic_summary_processed := true; RETURN NEW; END $body$;
DROP TRIGGER IF EXISTS `+table+`_topic_summary ON `+schema+`.`+table+`;
CREATE TRIGGER `+table+`_topic_summary BEFORE INSERT OR UPDATE OF json ON `+schema+`.`+table+` FOR EACH ROW EXECUTE FUNCTION `+schema+`.`+table+`_set_topic_summary();`)
		if err != nil {
			return fmt.Errorf("ensure %s topic summary: %w", table, err)
		}
	}
	return nil
}

// BackfillTopicSummary processes bounded batches after InstallTopicSummary has
// committed the DDL. A NULL projection remains a valid fallback. The processed
// marker preserves progress even when the original document cannot be projected.
// It can resume after interruption; writes continue during each batch.
// Walk the existing primary key without building an index over legacy rows at startup.
// Only pending rows are locked and updated.
func BackfillTopicSummary(ctx context.Context, db *sql.DB, schema string) error {
	if !schemaName.MatchString(schema) {
		return fmt.Errorf("invalid schema name")
	}
	for _, table := range []string{"drafts", "todos"} {
		var cursor sql.NullString
		for {
			var last sql.NullString
			err := db.QueryRowContext(ctx, `WITH batch AS (
 SELECT id FROM `+schema+`.`+table+` WHERE NOT topic_summary_processed AND ($1::text IS NULL OR id > $1) ORDER BY id LIMIT 50 FOR UPDATE
 ), updated AS (
 UPDATE `+schema+`.`+table+` t SET topic_summary = `+schema+`.`+table+`_topic_summary(t.json), topic_summary_processed = true FROM batch WHERE t.id = batch.id RETURNING t.id
 ) SELECT max(id) FROM updated`, cursor).Scan(&last)
			if err != nil {
				return fmt.Errorf("backfill %s topic summary: %w", table, err)
			}
			if !last.Valid {
				break
			}
			cursor = last
		}
	}
	return nil
}
