package consultations

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
)

var errInjectedDraftFailure = errors.New("injected read failure")
var errInjectedDraftCloseFailure = errors.New("injected close failure")

type draftFailureControl struct {
	mode           string
	stage          string
	projections    int
	projectionArgs []int
	closes         int
}
type draftFailureConnector struct {
	base    driver.Connector
	control *draftFailureControl
}

func (c draftFailureConnector) Driver() driver.Driver { return c.base.Driver() }
func (c draftFailureConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &draftFailureConnection{PostgresConn: conn.(appsql.PostgresConn), control: c.control}, nil
}

type draftFailureConnection struct {
	appsql.PostgresConn
	control *draftFailureControl
}

func (c *draftFailureConnection) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	projection := strings.HasPrefix(q, "SELECT id, CASE")
	if projection {
		c.control.projections++
		c.control.projectionArgs = append(c.control.projectionArgs, len(args))
	}
	inject := projection && c.control.stage != "keys" || strings.Contains(q, "xmin::text") && c.control.stage == "keys"
	if inject && c.control.mode == "query" {
		c.control.mode = ""
		return nil, errInjectedDraftFailure
	}
	rows, err := c.PostgresConn.QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	if inject && c.control.mode != "" {
		mode := c.control.mode
		c.control.mode = ""
		return &draftFailureResult{Rows: rows, mode: mode, control: c.control}, nil
	}
	return rows, nil
}

type draftFailureResult struct {
	driver.Rows
	mode    string
	seen    int
	control *draftFailureControl
}

func (r *draftFailureResult) Next(v []driver.Value) error {
	if r.seen == 1 && strings.HasPrefix(r.mode, "rows") {
		r.seen++
		return errInjectedDraftFailure
	}
	err := r.Rows.Next(v)
	if err != nil {
		return err
	}
	r.seen++
	if r.seen == 2 {
		switch strings.TrimSuffix(r.mode, "-close") {
		case "scan":
			v[1] = nil
		case "decode":
			v[1] = "{"
		}
	}
	return nil
}

func (r *draftFailureResult) Close() error {
	r.control.closes++
	err := r.Rows.Close()
	if strings.Contains(r.mode, "close") {
		return errInjectedDraftCloseFailure
	}
	return err
}

func TestDraftCacheProjectionFailureRetry(t *testing.T) {
	testDraftCacheReadFailureRetry(t, "projection", []string{"query", "scan", "rows", "decode", "close", "scan-close", "decode-close"})
}

func TestDraftCacheKeyFailureRetry(t *testing.T) {
	testDraftCacheReadFailureRetry(t, "keys", []string{"query", "scan", "rows", "close", "scan-close"})
}

func testDraftCacheReadFailureRetry(t *testing.T, stage string, modes []string) {
	for _, mode := range modes {
		for _, purpose := range []DraftReadPurpose{DraftReadPage, DraftReadAlerts, DraftReadScreen, DraftReadRecovery, DraftReadFiling} {
			t.Run(mode+"/"+string(purpose), func(t *testing.T) {
				ctx := context.Background()
				env := pgtest.NewIsolated(t)
				writer, err := OpenRecords(ctx, env.DB)
				if err != nil {
					t.Fatal(err)
				}
				for _, id := range []string{"a", "b", "c"} {
					if _, err = writer.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, id, `{"id":"`+id+`","status":"old"}`); err != nil {
						t.Fatal(err)
					}
				}
				cfg, err := pgx.ParseConfig(env.DSN)
				if err != nil {
					t.Fatal(err)
				}
				cfg.RuntimeParams["search_path"] = env.Schema
				control := &draftFailureControl{stage: stage}
				db := sql.OpenDB(draftFailureConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), control})
				defer db.Close()
				reader := &Records{DB: db}
				if _, err = reader.ListDraftEntries(ctx, purpose); err != nil {
					t.Fatal(err)
				}
				if _, err = writer.DB.ExecContext(ctx, `UPDATE drafts SET json=replace(json,'old','new') WHERE id IN ('a','b')`); err != nil {
					t.Fatal(err)
				}
				held := make(map[string]cachedDraft)
				for key, value := range reader.draftCache[purpose] {
					held[key] = cachedDraft{version: value.version, record: copyDraftRecord(value.record), bytes: value.bytes}
				}
				control.mode = mode
				before := control.projections
				failed, err := reader.ListDraftEntries(ctx, purpose)
				if err == nil || failed != nil {
					t.Fatalf("incomplete result published: %v %v", failed, err)
				}
				if (mode == "query" || strings.HasPrefix(mode, "rows")) && !errors.Is(err, errInjectedDraftFailure) {
					t.Fatalf("wrong error for %s: %v", mode, err)
				}
				if strings.HasPrefix(mode, "scan") && !strings.Contains(err.Error(), "converting NULL to string") {
					t.Fatalf("wrong scan error: %v", err)
				}
				if strings.HasPrefix(mode, "decode") && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("wrong decode error: %v", err)
				}
				if mode == "close" && !errors.Is(err, errInjectedDraftCloseFailure) {
					t.Fatalf("wrong close error: %v", err)
				}
				if mode != "query" && control.closes != 1 {
					t.Fatalf("failed rows not closed exactly once: %d", control.closes)
				}
				failedProjections := 1
				if stage == "keys" {
					failedProjections = 0
				}
				if control.projections != before+failedProjections {
					t.Fatalf("unexpected projection count: %d", control.projections-before)
				}
				if !reflect.DeepEqual(reader.draftCache[purpose], held) {
					t.Fatal("failed read changed retained records or versions")
				}
				entries, err := reader.ListDraftEntries(ctx, purpose)
				if err != nil {
					t.Fatal(err)
				}
				if control.projections != before+failedProjections+1 {
					t.Fatal("retry did not fetch projections")
				}
				query, err := draftProjectionSQL(purpose)
				if err != nil {
					t.Fatal(err)
				}
				wantEntries, err := writer.readDraftEntries(ctx, query)
				if err != nil || !reflect.DeepEqual(entries, wantEntries) {
					t.Fatalf("recovery differs from latest projection: %v %v", entries, err)
				}
				for _, entry := range entries {
					want := "new"
					if entry.Key == "c" {
						want = "old"
					}
					if entry.Record["status"] != want {
						t.Fatalf("retry %s: %#v", entry.Key, entry.Record)
					}
				}
				again, err := reader.ListDraftEntries(ctx, purpose)
				if err != nil || !reflect.DeepEqual(entries, again) {
					t.Fatalf("cached retry differs: %v %v", again, err)
				}
				if control.projections != before+failedProjections+1 {
					t.Fatal("successful retry was not cached")
				}
			})
		}
	}
}
