package consultations

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
)

type draftRaceConnector struct {
	base  driver.Connector
	after func()
}

func (c draftRaceConnector) Driver() driver.Driver { return c.base.Driver() }
func (c draftRaceConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &draftRaceConn{PostgresConn: conn.(appsql.PostgresConn), after: c.after}, nil
}

type draftRaceConn struct {
	appsql.PostgresConn
	after func()
}

func (c *draftRaceConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.PostgresConn.QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	if strings.Contains(q, "xmin::text") {
		return &draftRaceRows{Rows: rows, after: c.after}, nil
	}
	return rows, nil
}

type draftRaceRows struct {
	driver.Rows
	after func()
	once  sync.Once
}

func (r *draftRaceRows) Next(v []driver.Value) error {
	err := r.Rows.Next(v)
	if err == io.EOF {
		r.once.Do(r.after)
	}
	return err
}

func TestDraftCacheSnapshotUpdateDelete(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	writer, err := OpenRecords(ctx, env.DB)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err = writer.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, id, `{"id":"`+id+`","status":"old"}`); err != nil {
			t.Fatal(err)
		}
	}
	config, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = env.Schema
	var once sync.Once
	var updateErr error
	after := func() {
		once.Do(func() {
			_, updateErr = writer.DB.ExecContext(ctx, `UPDATE drafts SET json='{"id":"a","status":"new"}' WHERE id='a'`)
			if updateErr == nil {
				_, updateErr = writer.DB.ExecContext(ctx, `DELETE FROM drafts WHERE id='b'`)
			}
		})
	}
	db := sql.OpenDB(draftRaceConnector{base: appsql.NewPostgresBindingConnector(stdlib.GetConnector(*config)), after: after})
	defer db.Close()
	reader := &Records{DB: db}
	entries, err := reader.ListDraftEntries(ctx, DraftReadPage)
	if updateErr != nil || err != nil {
		t.Fatalf("update %v read %v", updateErr, err)
	}
	if len(entries) != 2 || entries[0].Key != "b" || entries[1].Record["status"] != "old" {
		t.Fatalf("snapshot: %v", entries)
	}
	entries, err = reader.ListDraftEntries(ctx, DraftReadPage)
	if err != nil || len(entries) != 1 || entries[0].Key != "a" || entries[0].Record["status"] != "new" {
		t.Fatalf("next: %v %v", entries, err)
	}
}
