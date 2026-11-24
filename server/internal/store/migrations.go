package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

const MigrationLockKey int64 = 20261011

//go:embed migrations.json
var migrationsJSON []byte

type Migration struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	SQL     string `json:"sql"`
}

type AppliedMigration struct {
	Version int
	Name    string
}

type MigrationResult struct {
	Applied []int
	Version int
	Ahead   bool
}

func Migrations() ([]Migration, error) {
	var list []Migration
	if err := json.Unmarshal(migrationsJSON, &list); err != nil {
		return nil, err
	}
	for i := range list {
		list[i].SQL = strings.ReplaceAll(list[i].SQL, "{{dependency_readiness}}", dependencyReadinessSQL)
	}
	return list, validateMigrations(list)
}

func validateMigrations(list []Migration) error {
	if len(list) == 0 {
		return fmt.Errorf("migration list is empty")
	}
	for i, migration := range list {
		if migration.Version != i+1 || strings.TrimSpace(migration.Name) == "" {
			return fmt.Errorf("invalid migration at position %d", i+1)
		}
	}
	return nil
}

func PendingMigrations(list []Migration, applied []AppliedMigration) ([]Migration, bool, error) {
	if err := validateMigrations(list); err != nil {
		return nil, false, err
	}
	for i, row := range applied {
		if row.Version != i+1 {
			return nil, false, fmt.Errorf("applied migration sequence has a gap at %d", i+1)
		}
		if i < len(list) && row.Name != list[i].Name {
			return nil, false, fmt.Errorf("migration %d has a different name", row.Version)
		}
	}
	if len(applied) > len(list) {
		return nil, true, nil
	}
	return list[len(applied):], false, nil
}

func Migrate(ctx context.Context, db *sql.DB) (MigrationResult, error) {
	list, err := Migrations()
	if err != nil {
		return MigrationResult{}, err
	}
	return migrateList(ctx, db, list)
}

func migrateList(ctx context.Context, db *sql.DB, list []Migration) (MigrationResult, error) {
	if err := validateMigrations(list); err != nil {
		return MigrationResult{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return MigrationResult{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", MigrationLockKey); err != nil {
		return MigrationResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
version integer PRIMARY KEY,
name text NOT NULL,
applied_at timestamptz NOT NULL DEFAULT now()
)`); err != nil {
		return MigrationResult{}, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT version, name FROM schema_migrations ORDER BY version")
	if err != nil {
		return MigrationResult{}, err
	}
	var applied []AppliedMigration
	for rows.Next() {
		var row AppliedMigration
		if err := rows.Scan(&row.Version, &row.Name); err != nil {
			rows.Close()
			return MigrationResult{}, err
		}
		applied = append(applied, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MigrationResult{}, err
	}
	pending, ahead, err := PendingMigrations(list, applied)
	if err != nil {
		return MigrationResult{}, err
	}
	result := MigrationResult{Applied: []int{}, Version: max(len(list), len(applied)), Ahead: ahead}
	for _, migration := range pending {
		if strings.TrimSpace(migration.SQL) != "" {
			if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
				return MigrationResult{}, fmt.Errorf("migration %d failed: %w", migration.Version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", migration.Version, migration.Name); err != nil {
			return MigrationResult{}, err
		}
		result.Applied = append(result.Applied, migration.Version)
	}
	if err := tx.Commit(); err != nil {
		return MigrationResult{}, err
	}
	return result, nil
}
