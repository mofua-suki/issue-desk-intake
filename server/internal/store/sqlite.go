package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"issue-desk-intake/server/internal/appsql"
)

// SQLite is the application store handle, opened on PostgreSQL by
// OpenApplicationPostgres. The name is a leftover of the SQLite era.
type SQLite struct{ db *applicationDB }

// applicationRunner is shared by autocommit connections, transactions and Fake.
// Single-statement operations must not acquire a writable transaction just to
// use appsql: in particular, reads must remain possible while another writer runs.
type applicationRunner interface {
	Exec(context.Context, string, ...any) (appsql.Result, error)
	Query(context.Context, string, ...any) (appsql.Rows, error)
}

type connectionRunner struct{ db *sql.DB }

func (r connectionRunner) Exec(ctx context.Context, q string, args ...any) (appsql.Result, error) {
	return r.db.ExecContext(ctx, q, args...)
}
func (r connectionRunner) Query(ctx context.Context, q string, args ...any) (appsql.Rows, error) {
	return r.db.QueryContext(ctx, q, args...)
}

// Embedding preserves access to the shared database/sql handle inside store.
// ExecContext also binds the existing deletion statements without changing them.
type applicationDB struct {
	*sql.DB
	dialect      appsql.Dialect
	runner       applicationRunner
	transactions appsql.Store
}

func applicationSQLite(db *sql.DB, dialect appsql.Dialect) *SQLite {
	return &SQLite{db: &applicationDB{
		DB: db, dialect: dialect, runner: connectionRunner{db},
		transactions: appsql.SQLStore{DB: db},
	}}
}

func (db *applicationDB) ExecContext(ctx context.Context, q string, args ...any) (appsql.Result, error) {
	bound, err := db.dialect.Bind(q)
	if err != nil {
		return nil, err
	}
	return db.runner.Exec(ctx, bound, args...)
}

// applicationRow mirrors QueryRow's single-row contract and releases Rows before
// a session transaction writes. Neither SQL nor argument values are logged.
type applicationRow struct {
	rows appsql.Rows
	err  error
}

func (db *applicationDB) QueryRowContext(ctx context.Context, q string, args ...any) applicationRow {
	bound, err := db.dialect.Bind(q)
	if err != nil {
		return applicationRow{err: err}
	}
	rows, err := db.runner.Query(ctx, bound, args...)
	return applicationRow{rows: rows, err: err}
}

func (r applicationRow) Scan(dest ...any) (err error) {
	if r.err != nil {
		return r.err
	}
	defer func() {
		if closeErr := r.rows.Close(); closeErr != nil && (err == nil || errors.Is(err, sql.ErrNoRows)) {
			err = closeErr
		}
	}()
	if !r.rows.Next() {
		if err = r.rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if err = r.rows.Scan(dest...); err != nil {
		return err
	}
	return r.rows.Err()
}

func (s *SQLite) Close() error { return s.db.Close() }

// DB is the shared database handle. Callers must keep multi-record changes in one
// transaction and preserve JSON fields owned by other services.
func (s *SQLite) DB() *sql.DB { return s.db.DB }

func (s *SQLite) Setting(ctx context.Context, id string) (json.RawMessage, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT json FROM settings WHERE id=?", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !json.Valid([]byte(raw)) {
		return nil, false, fmt.Errorf("setting %q contains invalid JSON", id)
	}
	return json.RawMessage(raw), true, nil
}

func (s *SQLite) PutSetting(ctx context.Context, id string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO settings(id,json) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET json=excluded.json", id, string(raw))
	return err
}
