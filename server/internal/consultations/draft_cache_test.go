package consultations

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
)

func TestDraftCacheIncremental(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	writer, err := OpenRecords(ctx, env.DB)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, err = writer.DB.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES ($1,$2)", id, `{"id":"`+id+`","status":"draft","splitChildren":[{"value":"original"}]}`); err != nil {
			t.Fatal(err)
		}
	}
	config, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = env.Schema
	counts := &readCounts{}
	db := sql.OpenDB(countConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*config)), counts})
	defer db.Close()
	reader := &Records{DB: db}
	for _, purpose := range []DraftReadPurpose{DraftReadPage, DraftReadAlerts, DraftReadScreen} {
		first, err := reader.ListDraftEntries(ctx, purpose)
		if err != nil {
			t.Fatal(err)
		}
		*counts = readCounts{}
		again, err := reader.ListDraftEntries(ctx, purpose)
		if err != nil || !reflect.DeepEqual(first, again) {
			t.Fatalf("%s: %v", purpose, err)
		}
		if counts.calls != 1 || counts.rows != 3 {
			t.Fatalf("%s unchanged: %+v", purpose, counts)
		}
		if purpose == DraftReadPage {
			first[0].Record["splitChildren"].([]any)[0].(map[string]any)["value"] = "modified"
			after, err := reader.ListDraftEntries(ctx, purpose)
			if err != nil || !reflect.DeepEqual(again, after) {
				t.Fatalf("copy: %v", err)
			}
		}
	}
	if _, err = writer.DB.ExecContext(ctx, `UPDATE drafts SET json='{"id":"a","status":"closed"}' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	for _, purpose := range []DraftReadPurpose{DraftReadPage, DraftReadAlerts, DraftReadScreen} {
		*counts = readCounts{}
		entries, err := reader.ListDraftEntries(ctx, purpose)
		if err != nil {
			t.Fatal(err)
		}
		if counts.calls != 2 || counts.rows != 4 {
			t.Fatalf("%s updated: %+v", purpose, counts)
		}
		if entries[2].Key != "a" || entries[2].Record["status"] != "closed" {
			t.Fatalf("%s: %v", purpose, entries)
		}
	}
	if _, err = writer.DB.ExecContext(ctx, `DELETE FROM drafts WHERE id='b'`); err != nil {
		t.Fatal(err)
	}
	if _, err = writer.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES ('d','{"id":"d"}')`); err != nil {
		t.Fatal(err)
	}
	entries, err := reader.ListDraftEntries(ctx, DraftReadPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].Key != "d" || entries[1].Key != "c" || entries[2].Key != "a" {
		t.Fatalf("order: %v", entries)
	}
	// The cache serializes queries through this counting connection.
	*counts = readCounts{}
	concurrent := &Records{DB: db}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := concurrent.ListDraftEntries(ctx, DraftReadPage); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if counts.calls != 13 || counts.rows != 39 {
		t.Fatalf("concurrent reads fetched projections more than once: %+v", counts)
	}
	if _, err = writer.DB.ExecContext(ctx, `UPDATE drafts SET json='null' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	if _, err = concurrent.ListDraftEntries(ctx, DraftReadPage); err == nil {
		t.Fatal("invalid record accepted")
	}
	if _, err = writer.DB.ExecContext(ctx, `UPDATE drafts SET json='{"id":"a","status":"recovered"}' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	entries, err = concurrent.ListDraftEntries(ctx, DraftReadPage)
	if err != nil || entries[2].Record["status"] != "recovered" {
		t.Fatalf("retry: %v %v", entries, err)
	}
}

func TestDraftCacheConcurrentPurposes(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	reader, err := OpenRecords(ctx, env.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES ('a','{"id":"a","status":"old"}')`); err != nil {
		t.Fatal(err)
	}
	purposes := []DraftReadPurpose{DraftReadPage, DraftReadAlerts, DraftReadScreen, DraftReadRecovery, DraftReadFiling}
	readAll := func(status string) {
		t.Helper()
		start := make(chan struct{})
		errs := make(chan error, len(purposes)*4)
		for _, purpose := range purposes {
			for i := 0; i < 4; i++ {
				go func(p DraftReadPurpose) {
					<-start
					entries, err := reader.ListDraftEntries(ctx, p)
					if err == nil && (len(entries) != 1 || entries[0].Key != "a" || entries[0].Record["status"] != status) {
						err = fmt.Errorf("%s: unexpected entries %v", p, entries)
					}
					if err == nil {
						entries[0].Record["status"] = "caller change"
					}
					errs <- err
				}(purpose)
			}
		}
		close(start)
		for i := 0; i < cap(errs); i++ {
			if err := <-errs; err != nil {
				t.Error(err)
			}
		}
		entries, err := reader.ListDraftEntries(ctx, DraftReadPage)
		if err != nil || len(entries) != 1 || entries[0].Record["status"] != status {
			t.Fatalf("caller changes affected cached record: %v %v", entries, err)
		}
	}
	readAll("old")
	if _, err := reader.DB.ExecContext(ctx, `UPDATE drafts SET json='{"id":"a","status":"new"}' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	readAll("new")
}

// This driver stops the first key query until the test explicitly releases it.
// It does not depend on a running PostgreSQL instance for cancellation checks.
type draftWaitConnector struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *draftWaitConnector) Driver() driver.Driver { return draftWaitDriver{} }
func (c *draftWaitConnector) Connect(context.Context) (driver.Conn, error) {
	return &draftWaitConn{control: c}, nil
}

type draftWaitDriver struct{}

func (draftWaitDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type draftWaitConn struct{ control *draftWaitConnector }

func (*draftWaitConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (*draftWaitConn) Close() error              { return nil }
func (*draftWaitConn) Begin() (driver.Tx, error) { return draftWaitTx{}, nil }
func (c *draftWaitConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if opts.Isolation != driver.IsolationLevel(sql.LevelRepeatableRead) || !opts.ReadOnly {
		return nil, errors.New("snapshot options changed")
	}
	return c.Begin()
}
func (c *draftWaitConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	value := `{"id":"json-id","status":"draft"}`
	if strings.Contains(query, "xmin::text") {
		c.control.once.Do(func() {
			close(c.control.started)
			<-c.control.release
		})
		value = "1"
	}
	return &draftWaitRows{value: value}, nil
}

type draftWaitTx struct{}

func (draftWaitTx) Commit() error   { return nil }
func (draftWaitTx) Rollback() error { return nil }

type draftWaitRows struct {
	value string
	done  bool
}

func (*draftWaitRows) Columns() []string { return []string{"id", "value"} }
func (*draftWaitRows) Close() error      { return nil }
func (r *draftWaitRows) Next(v []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	v[0], v[1] = "database-key", r.value
	return nil
}

// Observing Done proves the read has reached its cancellable wait, without sleeps.
type draftWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *draftWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestDraftCacheWaitCancellation(t *testing.T) {
	for _, purpose := range []DraftReadPurpose{DraftReadPage, DraftReadAlerts, DraftReadScreen, DraftReadRecovery, DraftReadFiling} {
		t.Run(string(purpose), func(t *testing.T) {
			control := &draftWaitConnector{started: make(chan struct{}), release: make(chan struct{})}
			db := sql.OpenDB(control)
			defer db.Close()
			var release sync.Once
			defer release.Do(func() { close(control.release) })
			reader := &Records{DB: db}
			read := func(ctx context.Context, purpose DraftReadPurpose) error {
				entries, err := reader.ListDraftEntries(ctx, purpose)
				if err == nil && (len(entries) != 1 || entries[0].Key != "database-key" || entries[0].Record["id"] != "json-id") {
					return fmt.Errorf("wrong entries: %v", entries)
				}
				if err != nil && entries != nil {
					return fmt.Errorf("failed read published entries: %v", entries)
				}
				return err
			}
			first := make(chan error, 1)
			go func() { first <- read(context.Background(), DraftReadPage) }()
			<-control.started

			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			// Must finish while the first query remains stopped.
			if err := read(cancelled, purpose); !errors.Is(err, context.Canceled) {
				t.Fatalf("pre-cancelled read: %v", err)
			}
			ctx, cancelWait := context.WithCancel(context.Background())
			defer cancelWait()
			waiting := &draftWaitContext{Context: ctx, waiting: make(chan struct{})}
			second := make(chan error, 1)
			go func() { second <- read(waiting, purpose) }()
			<-waiting.waiting
			cancelWait()
			if err := <-second; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled waiting read: %v", err)
			}

			// A normal reader queued after cancellation must still get the gate.
			following := make(chan error, 1)
			go func() { following <- read(context.Background(), purpose) }()
			release.Do(func() { close(control.release) })
			if err := <-first; err != nil {
				t.Fatal(err)
			}
			if err := <-following; err != nil {
				t.Fatal(err)
			}
			if err := read(context.Background(), purpose); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDraftCacheLargePartialChanges(t *testing.T) {
	for _, changed := range []int{65535, 65536} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			ctx := context.Background()
			env := pgtest.NewIsolated(t)
			writer, err := OpenRecords(ctx, env.DB)
			if err != nil {
				t.Fatal(err)
			}
			// One extra row remains unchanged in both boundary cases.
			if _, err = writer.DB.ExecContext(ctx, `INSERT INTO drafts(id,json)
				SELECT i::text, json_build_object('id','json-'||i,'status','old')::text
				FROM generate_series(0,$1::integer) AS i`, changed); err != nil {
				t.Fatal(err)
			}
			cfg, err := pgx.ParseConfig(env.DSN)
			if err != nil {
				t.Fatal(err)
			}
			cfg.RuntimeParams["search_path"] = env.Schema
			control := &draftFailureControl{}
			db := sql.OpenDB(draftFailureConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), control})
			defer db.Close()
			reader := &Records{DB: db}
			if _, err = reader.ListDraftEntries(ctx, DraftReadPage); err != nil {
				t.Fatal(err)
			}
			if _, err = writer.DB.ExecContext(ctx, `UPDATE drafts SET json=replace(json,'old','new') WHERE id <> '0'`); err != nil {
				t.Fatal(err)
			}
			// Use the shipped projection and database ordering as the oracle.
			query, err := draftProjectionSQL(DraftReadPage)
			if err != nil {
				t.Fatal(err)
			}
			want, err := writer.readDraftEntries(ctx, query)
			if err != nil || len(want) != changed+1 {
				t.Fatalf("baseline: rows=%d err=%v", len(want), err)
			}
			for _, entry := range want {
				status := "new"
				if entry.Key == "0" {
					status = "old"
				}
				if entry.Record["id"] != "json-"+entry.Key || entry.Record["status"] != status {
					t.Fatalf("baseline key/content mismatch: %v", entry)
				}
			}
			for pass := 0; pass < 2; pass++ {
				got, err := reader.ListDraftEntries(ctx, DraftReadPage)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("partial update pass %d: rows=%d err=%v", pass, len(got), err)
				}
			}
			wantArgs := changed
			if changed > 65535 {
				wantArgs = 0
			}
			if !reflect.DeepEqual(control.projectionArgs, []int{0, wantArgs}) {
				t.Fatalf("projection bind counts: %v, want [0 %d]", control.projectionArgs, wantArgs)
			}
		})
	}
}
