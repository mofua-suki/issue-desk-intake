package appsql

import (
	"context"
	"database/sql/driver"
	"fmt"
)

// postgresBindingConnector wraps connections so that `?` becomes `$n` before the driver sees it.
type postgresBindingConnector struct{ base driver.Connector }

// NewPostgresBindingConnector wraps a pgx connector so that every statement goes
// through BindPostgresQuery (the connection layer of the application store).
func NewPostgresBindingConnector(base driver.Connector) driver.Connector {
	return postgresBindingConnector{base: base}
}

func (c postgresBindingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	full, ok := conn.(PostgresConn)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("PostgreSQLの接続が想定の形ではありません: %T", conn)
	}
	return &postgresBindingConn{PostgresConn: full}, nil
}

func (c postgresBindingConnector) Driver() driver.Driver { return c.base.Driver() }

// PostgresConn lists what the pgx connection provides (pgx v5 stdlib.Conn has no
// driver.Validator, so it must not be required here); embedding keeps database/sql
// on the same code paths as with the driver used directly.
type PostgresConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ExecerContext
	driver.QueryerContext
	driver.ConnPrepareContext
	driver.Pinger
	driver.SessionResetter
	driver.NamedValueChecker
}

type postgresBindingConn struct{ PostgresConn }

// BindPostgresQuery is idempotent: a statement without `?` (or one already using
// $n) comes back unchanged. Quotes, comments and dollar quotes are left intact.
func BindPostgresQuery(query string) (string, error) {
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			return Postgres.Bind(query)
		}
	}
	return query, nil
}

func (c *postgresBindingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	bound, err := BindPostgresQuery(query)
	if err != nil {
		return nil, err
	}
	return c.PostgresConn.ExecContext(ctx, bound, args)
}

func (c *postgresBindingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	bound, err := BindPostgresQuery(query)
	if err != nil {
		return nil, err
	}
	return c.PostgresConn.QueryContext(ctx, bound, args)
}

func (c *postgresBindingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	bound, err := BindPostgresQuery(query)
	if err != nil {
		return nil, err
	}
	return c.PostgresConn.PrepareContext(ctx, bound)
}

func (c *postgresBindingConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}
