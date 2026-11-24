package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"issue-desk-intake/server/internal/appsql"
)

// ApplicationPostgresSchema is the schema the application tables live in while
// the store moves from SQLite to PostgreSQL (the schema appdb/migrate fills).
const ApplicationPostgresSchema = "issue_desk_app_migration"

const applicationPostgresMaxConns = 8

var schemaIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// OpenApplicationPostgres opens the application store on PostgreSQL. search_path
// is pinned to schema (ApplicationPostgresSchema when empty) on every connection,
// and every statement passes through appsql.Postgres.Bind at the driver, so the
// existing statements that still use `?` run unchanged (deletion statements
// included: their text is not touched, only the placeholders are numbered).
func OpenApplicationPostgres(ctx context.Context, dsn, schema string) (*SQLite, error) {
	if dsn == "" {
		return nil, errors.New("PostgreSQLの接続先が必要です")
	}
	if schema == "" {
		schema = ApplicationPostgresSchema
	}
	if !schemaIdentifier.MatchString(schema) {
		return nil, errors.New("PostgreSQLのスキーマ名が不正です")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("PostgreSQLの接続先を解釈できません")
	}
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["search_path"] = schema
	db := sql.OpenDB(appsql.NewPostgresBindingConnector(stdlib.GetConnector(*config)))
	db.SetMaxOpenConns(applicationPostgresMaxConns)
	db.SetMaxIdleConns(applicationPostgresMaxConns)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return applicationSQLite(db, appsql.Postgres), nil
}
