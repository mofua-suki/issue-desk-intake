package appsql

import (
	"context"
	"database/sql"
)

type Result interface {
	LastInsertId() (int64, error)
	RowsAffected() (int64, error)
}
type Rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}
type Tx interface {
	Exec(context.Context, string, ...any) (Result, error)
	Query(context.Context, string, ...any) (Rows, error)
	Commit() error
	Rollback() error
}
type Store interface {
	Begin(context.Context, *sql.TxOptions) (Tx, error)
}
type SQLStore struct{ DB *sql.DB }

func (s SQLStore) Begin(ctx context.Context, opts *sql.TxOptions) (Tx, error) {
	tx, err := s.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return sqlTx{tx}, nil
}

type sqlTx struct{ tx *sql.Tx }

func (t sqlTx) Exec(ctx context.Context, q string, args ...any) (Result, error) {
	return t.tx.ExecContext(ctx, q, args...)
}
func (t sqlTx) Query(ctx context.Context, q string, args ...any) (Rows, error) {
	return t.tx.QueryContext(ctx, q, args...)
}
func (t sqlTx) Commit() error   { return t.tx.Commit() }
func (t sqlTx) Rollback() error { return t.tx.Rollback() }

// Within rolls back on callback failure or panic, and preserves commit errors.
func Within(ctx context.Context, s Store, opts *sql.TxOptions, fn func(Tx) error) error {
	tx, err := s.Begin(ctx, opts)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
