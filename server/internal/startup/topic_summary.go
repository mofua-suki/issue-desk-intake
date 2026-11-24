package startup

import (
	"context"
	"database/sql"
	"time"

	"issue-desk-intake/server/internal/store"
	"issue-desk-intake/server/internal/topicdb"
)

// The reader can use the original document until each projection is ready.
func (a *App) startTopicSummaryBackfill(schema string) {
	if schema == "" {
		schema = store.ApplicationPostgresSchema
	}
	a.periodically("topic-summary", time.Minute, topicSummaryBackfill(a.SQLite.DB(), schema))
}

// periodically serializes calls. After the legacy rows have been processed,
// the write triggers maintain projections without repeating a full key scan.
func topicSummaryBackfill(db *sql.DB, schema string) func(context.Context) error {
	complete := false
	return func(ctx context.Context) error {
		if complete {
			return nil
		}
		if err := topicdb.BackfillTopicSummary(ctx, db, schema); err != nil {
			return err
		}
		complete = true
		return nil
	}
}
