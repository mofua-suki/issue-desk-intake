package startup

import (
	"context"
	"errors"
	"os"

	"issue-desk-intake/server/internal/appcutover"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/draftsummary"
	"issue-desk-intake/server/internal/store"
	"issue-desk-intake/server/internal/topicdb"
)

// openApplicationStore opens the application database for OpenApp on PostgreSQL.
// config.ApplicationSchema is empty in production; only tests set it to run the
// application on a throwaway schema.
func openApplicationStore(ctx context.Context, config *Config) (*store.SQLite, error) {
	schema := config.ApplicationSchema
	var s *store.SQLite
	var err error
	if schema == "" {
		s, err = OpenPostgresApplicationStore(ctx, config, "")
	} else {
		s, err = OpenPostgresApplicationStore(ctx, config, schema)
	}
	if err != nil {
		return nil, err
	}
	if schema == "" {
		schema = store.ApplicationPostgresSchema
	}
	if err := draftsummary.EnsureDraftSummary(ctx, s.DB(), schema); err != nil {
		s.Close()
		return nil, err
	}
	if _, err := draftsummary.BackfillDraftSummary(ctx, s.DB(), schema, 50); err != nil {
		s.Close()
		return nil, err
	}
	if err := topicdb.InstallTopicSummary(ctx, s.DB(), schema); err != nil {
		s.Close()
		return nil, err
	}
	// The search path is pinned by OpenApplicationPostgres to this schema.
	if _, err := s.DB().ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_proposals ON proposals (id text_pattern_ops)`); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// ApplicationDSN returns the PostgreSQL connection settings for the application
// store: ISSUE_DESK_DATABASE_URL, or the PG* environment variables (PGHOST must be
// set so that an empty environment never connects to a default host).
func ApplicationDSN(config *Config) string {
	if config.AppPostgresDSN != "" {
		return config.AppPostgresDSN
	}
	if os.Getenv("PGHOST") != "" {
		return "host=" + os.Getenv("PGHOST")
	}
	return ""
}

// OpenPostgresApplicationStore opens the application store on PostgreSQL and
// selects the PostgreSQL SQL dialect for the whole process. There is no other
// store: it fails (and the intake does not start) when the connection settings
// are missing, when PostgreSQL cannot be read, or when the switch-over flag is
// absent. In the last case it never starts on an empty database; it says where
// to read the background. A non-empty schema (tests only) skips the flag check.
func OpenPostgresApplicationStore(ctx context.Context, config *Config, schema string) (*store.SQLite, error) {
	dsn := ApplicationDSN(config)
	if dsn == "" {
		return nil, errors.New("保存先のPostgreSQLを指定してください。ISSUE_DESK_DATABASE_URL（またはPGHOST・PGUSER・PGDATABASE）が必要です")
	}
	s, err := store.OpenApplicationPostgres(ctx, dsn, schema)
	if err != nil {
		return nil, errors.New("受付のPostgreSQLを開けません")
	}
	if schema == "" {
		done, err := appcutover.Done(ctx, s.DB())
		if err != nil {
			s.Close()
			return nil, errors.New("切替の完了を確認できません")
		}
		if !done {
			s.Close()
			return nil, notMigratedError(config)
		}
	}
	appsql.SetActive(appsql.Postgres)
	return s, nil
}

// notMigratedError explains an environment that has not been moved to PostgreSQL.
// A leftover SQLite file is named; an empty state is never started silently.
func notMigratedError(config *Config) error {
	if config.SQLiteFile != "" {
		if _, err := os.Stat(config.SQLiteFile); err == nil {
			return errors.New("PostgreSQLへ未移行のSQLite（" + config.SQLiteFile + "）が残っています。起動を止めました。経緯は docs/pg-cutover.md を確認してください")
		}
	}
	return errors.New("PostgreSQLの保存先に切替の記録がありません。空の保存先では起動しません。経緯は docs/pg-cutover.md を確認してください")
}
