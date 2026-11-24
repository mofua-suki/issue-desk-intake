package filingcheck

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/pgtest"
)

// Counts bytes returned by Next, not SQL argument sizes, decoded JSON sizes,
// storage/TOAST sizes or wire-protocol overhead. Keys are included in bytes.
type materialReadCount struct{ Calls, Rows, Bytes int }
type materialObservation struct {
	Projection, Selected, Fallback materialReadCount
	Keys                           map[string]bool
	Hold                           time.Duration
	Commits, Rollbacks             int
	ProjectionState                string
	Fail                           string
}
type materialConnector struct {
	driver.Connector
	observation *materialObservation
}

func (c materialConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &materialConn{PostgresConn: conn.(appsql.PostgresConn), observation: c.observation}, nil
}

type materialConn struct {
	appsql.PostgresConn
	observation *materialObservation
}

func (c *materialConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.PostgresConn.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	if opts.Isolation != driver.IsolationLevel(sql.LevelRepeatableRead) || !opts.ReadOnly {
		tx.Rollback()
		return nil, errors.New("filing snapshot must be read-only repeatable read")
	}
	return &materialTx{Tx: tx, observation: c.observation, start: time.Now()}, nil
}

type materialTx struct {
	driver.Tx
	observation *materialObservation
	start       time.Time
}

func (tx *materialTx) Commit() error {
	err := tx.Tx.Commit()
	tx.observation.Hold += time.Since(tx.start)
	tx.observation.Commits++
	return err
}
func (tx *materialTx) Rollback() error {
	err := tx.Tx.Rollback()
	tx.observation.Hold += time.Since(tx.start)
	tx.observation.Rollbacks++
	return err
}
func (c *materialConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	var count *materialReadCount
	switch {
	case strings.HasPrefix(q, "SELECT id, CASE WHEN"):
		count = &c.observation.Projection
	case strings.HasPrefix(q, "SELECT id, json FROM drafts WHERE id IN"):
		count = &c.observation.Selected
	case strings.HasPrefix(q, "SELECT id, json FROM drafts ORDER BY"):
		count = &c.observation.Fallback
	default:
		return c.PostgresConn.QueryContext(ctx, q, args)
	}
	count.Calls++
	if c.observation.Fail == "projection" && count == &c.observation.Projection || c.observation.Fail == "selected" && count == &c.observation.Selected {
		return nil, errors.New("injected material read failure")
	}
	rows, err := c.PostgresConn.QueryContext(ctx, q, args)
	if err != nil {
		var state interface{ SQLState() string }
		if count == &c.observation.Projection && errors.As(err, &state) {
			c.observation.ProjectionState = state.SQLState()
		}
		return nil, err
	}
	return &materialRows{Rows: rows, count: count, observation: c.observation, selected: count == &c.observation.Selected}, nil
}

type materialRows struct {
	driver.Rows
	count       *materialReadCount
	observation *materialObservation
	selected    bool
}

func (r *materialRows) Next(values []driver.Value) error {
	err := r.Rows.Next(values)
	if err != nil {
		return err
	}
	r.count.Rows++
	for _, v := range values {
		switch x := v.(type) {
		case string:
			r.count.Bytes += len(x)
		case []byte:
			r.count.Bytes += len(x)
		}
	}
	if r.selected {
		if r.observation.Keys == nil {
			r.observation.Keys = map[string]bool{}
		}
		switch x := values[0].(type) {
		case string:
			r.observation.Keys[x] = true
		case []byte:
			r.observation.Keys[string(x)] = true
		}
	}
	return nil
}
func observedMaterialRecords(t testing.TB, env pgtest.Env) (*consultations.Records, *materialObservation) {
	t.Helper()
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	observation := &materialObservation{}
	db := sql.OpenDB(materialConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), observation})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	// Warm the dedicated connection before any measured interval. Total read
	// time still includes database/sql acquisition; transaction hold excludes it.
	if err = db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &consultations.Records{DB: db}, observation
}
