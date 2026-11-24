package startup

import (
	"context"
	"strings"

	"issue-desk-intake/server/internal/schemadefaults"
	"issue-desk-intake/server/internal/store"
)

// repairSchemaDefaults gives the auto-numbered columns of an already migrated
// PostgreSQL schema their identity default (see schemadefaults.EnsureDefaults). It runs
// once the leadership lease is held and the application store is open, before
// anything writes. A failure never stops the intake: only the table names are
// logged, never values.
func repairSchemaDefaults(ctx context.Context, s *store.SQLite, schema string, log func(string)) {
	if schema == "" {
		schema = store.ApplicationPostgresSchema
	}
	failed, err := schemadefaults.EnsureDefaults(ctx, s.DB(), schema)
	if err != nil {
		log("スキーマの自動採番の補修に失敗しました（起動は続けます）。対象の表: " + strings.Join(failed, ", "))
	}
}
