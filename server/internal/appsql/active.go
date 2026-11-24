package appsql

import (
	"context"
	"database/sql"
	"sync/atomic"
)

// active is the dialect of the application database this process runs on. It is
// set once at startup (SetActive) and read by every package-level `sqlDialect`.
// An atomic keeps parallel tests and a late startup from racing. PostgreSQL is
// the only dialect and the zero value.
var active atomic.Int32

// SetActive selects the dialect used by every Active value and returns the
// previous one so that tests can restore it.
func SetActive(d Dialect) Dialect {
	if !d.valid() {
		panic("appsql: unknown SQL dialect")
	}
	return Dialect(active.Swap(int32(d)))
}

// CurrentDialect returns the dialect selected with SetActive (PostgreSQL by default).
func CurrentDialect() Dialect { return Dialect(active.Load()) }

// Active forwards each call to the dialect selected with SetActive. Packages
// declare `var sqlDialect appsql.Active`, so one startup call switches them all.
type Active struct{}

func (Active) Bind(s string) (string, error) { return CurrentDialect().Bind(s) }
func (Active) InsertIgnore(table string, columns []string, source string) (string, error) {
	return CurrentDialect().InsertIgnore(table, columns, source)
}
func (Active) RowOrder(desc bool) (string, error) { return CurrentDialect().RowOrder(desc) }
func (Active) Identity() (string, error)          { return CurrentDialect().Identity() }
func (Active) Binary() (string, error)            { return CurrentDialect().Binary() }
func (Active) JSONText(column, path string) (string, error) {
	return CurrentDialect().JSONText(column, path)
}
func (Active) JSONNumber(column, path string) (string, error) {
	return CurrentDialect().JSONNumber(column, path)
}
func (Active) TableExists() (string, error) { return CurrentDialect().TableExists() }
func (Active) BeginWrite() (string, error)  { return CurrentDialect().BeginWrite() }
func (Active) WriteLock() (string, error)   { return CurrentDialect().WriteLock() }

// BeginLockedWrite starts a read-modify-write transaction on conn (BeginWrite)
// and takes the transaction-scoped write lock named lock (WriteLock): BEGIN
// alone would let two read-modify-write transactions interleave. The caller
// still ends it with COMMIT or ROLLBACK.
func (a Active) BeginLockedWrite(ctx context.Context, conn *sql.Conn, lock string) error {
	begin, err := a.BeginWrite()
	if err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, begin); err != nil {
		return err
	}
	query, err := a.WriteLock()
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, query, lock)
	return err
}

// MustBind is Bind for a statement built from constants. A malformed statement
// is a programming error, so it panics instead of returning an error.
func (a Active) MustBind(s string) string {
	bound, err := a.Bind(s)
	if err != nil {
		panic("appsql: invalid SQL: " + err.Error())
	}
	return bound
}

// MustRowOrder is RowOrder that panics on an unknown dialect.
func (a Active) MustRowOrder(desc bool) string {
	order, err := a.RowOrder(desc)
	if err != nil {
		panic("appsql: invalid row order: " + err.Error())
	}
	return order
}

// MustInsertIgnore is InsertIgnore for constant table and column names. It
// panics on an invalid identifier.
func (a Active) MustInsertIgnore(table string, columns []string, source string) string {
	query, err := a.InsertIgnore(table, columns, source)
	if err != nil {
		panic("appsql: invalid insert: " + err.Error())
	}
	return query
}

// MustTableExists is TableExists that panics on an unknown dialect.
func (a Active) MustTableExists() string {
	query, err := a.TableExists()
	if err != nil {
		panic("appsql: invalid table check: " + err.Error())
	}
	return query
}

// MustJSONText is JSONText for a constant column and path. It panics on an
// invalid path.
func (a Active) MustJSONText(column, path string) string {
	expr, err := a.JSONText(column, path)
	if err != nil {
		panic("appsql: invalid JSON path: " + err.Error())
	}
	return expr
}

// MustJSONNumber is JSONNumber for a constant column and path. It panics on an
// invalid path.
func (a Active) MustJSONNumber(column, path string) string {
	expr, err := a.JSONNumber(column, path)
	if err != nil {
		panic("appsql: invalid JSON path: " + err.Error())
	}
	return expr
}

// MustIdentity is Identity that panics on an unknown dialect.
func (a Active) MustIdentity() string {
	name, err := a.Identity()
	if err != nil {
		panic("appsql: invalid identity type: " + err.Error())
	}
	return name
}

// MustBinary is Binary that panics on an unknown dialect.
func (a Active) MustBinary() string {
	name, err := a.Binary()
	if err != nil {
		panic("appsql: invalid binary type: " + err.Error())
	}
	return name
}

// MustBinaryCollation is BinaryCollation that panics on an unknown dialect.
func (a Active) MustBinaryCollation() string {
	collation, err := a.BinaryCollation()
	if err != nil {
		panic("appsql: invalid collation: " + err.Error())
	}
	return collation
}
