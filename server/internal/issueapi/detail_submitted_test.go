package issueapi

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/consultations"
	"testing"
	"time"
)

func TestSubmittedDraftSelectsNewestBoundRowWithoutParsingAllRows(t *testing.T) {
	f := newFixture(t)
	insert := func(id string, record Object) {
		t.Helper()
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.app.DB().ExecContext(f.ctx, bindSQL("INSERT INTO drafts(id,json) VALUES(?,?)"), id, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	insert("old", Object{"project": "owner/repo", "issue": Object{"number": 1}, "consultationProvider": "old"})
	insert("new", Object{"project": "owner/repo", "issue": Object{"project": "", "number": "1"}, "consultationProvider": "new"})
	insert("other-number", Object{"project": "owner/repo", "issue": Object{"number": 2}, "consultationProvider": "wrong-number"})
	insert("other-repo", Object{"project": "owner/repo", "issue": Object{"project": "other/repo", "number": 1}, "consultationProvider": "wrong-repo"})
	insert("bad", Object{"project": "owner/repo", "issue": Object{"number": "abc"}, "consultationProvider": "bad"})
	check := func(want string) {
		t.Helper()
		code, value := f.call("GET", "/api/issue?project=owner%2Frepo&number=1", "screen", nil)
		if code != 200 || fleet.ObjectOrEmpty(value)["consultationProvider"] != want {
			t.Fatalf("detail: status=%d value=%v, want provider %s", code, value, want)
		}
	}
	check("new")
	raw := `{"project":"owner/repo","issue":{"project":null,"number":"1"},"consultationProvider":"updated"}`
	if _, err := f.app.DB().ExecContext(f.ctx, bindSQL("UPDATE drafts SET json=? WHERE id=?"), raw, "new"); err != nil {
		t.Fatal(err)
	}
	check("updated")
	raw = `{"project":"owner/repo","issue":{"number":2},"consultationProvider":"updated"}`
	if _, err := f.app.DB().ExecContext(f.ctx, bindSQL("UPDATE drafts SET json=? WHERE id=?"), raw, "new"); err != nil {
		t.Fatal(err)
	}
	check("old")
}

func TestIssueDetailMatchesNumericDraftsAndIgnoresNonObjects(t *testing.T) {
	f := newFixture(t)
	for _, raw := range []string{`"1.0"`, `1.0`, `1e0`, `" 1"`} {
		t.Run(raw, func(t *testing.T) {
			_, err := f.app.DB().ExecContext(f.ctx, bindSQL(`DELETE FROM drafts`))
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.app.DB().ExecContext(f.ctx, bindSQL(`INSERT INTO drafts(id,json) VALUES(?,?),(?,?)`), "match", `{"project":"owner/repo","issue":{"number":`+raw+`},"consultationProvider":"matched"}`, "array", `[]`)
			if err != nil {
				t.Fatal(err)
			}
			code, value := f.call("GET", "/api/issue?project=owner%2Frepo&number=1", "screen", nil)
			if code != 200 || fleet.ObjectOrEmpty(value)["consultationProvider"] != "matched" {
				t.Fatalf("status=%d detail=%v", code, value)
			}
		})
	}
}

func TestIssueActionsIgnoreInvalidNumbersInDetailAndClose(t *testing.T) {
	f := newFixture(t)
	for id, raw := range map[string]string{
		"invalid": `{"payload":{"repo":"owner/repo","number":"invalid"}}`,
		"other":   `{"payload":{"repo":"owner/repo","number":2},"attempted":true}`,
		"match":   `{"payload":{"repo":"owner/repo","number":"1.0","action":"reply"},"postedComment":{},"attempted":true}`,
	} {
		if _, err := f.app.DB().ExecContext(f.ctx, bindSQL(`INSERT INTO actions(id,json) VALUES(?,?)`), id, raw); err != nil {
			t.Fatal(err)
		}
	}
	code, value := f.call("GET", "/api/issue?project=owner%2Frepo&number=1", "screen", nil)
	if code != 200 || fleet.ObjectOrEmpty(value)["transferPending"] != true {
		t.Fatalf("status=%d detail=%v", code, value)
	}
	// 送信が残っているだけ（issueのロックが空いている）なら閉じるのを止めない。
	if got := f.s.closeBlocker(f.ctx, Object{"status": "completed", "project": "owner/repo", "number": int64(1)}); got != "" {
		t.Fatalf("leftover close blocker = %q", got)
	}
	unlock := lock(&f.s.locks, "issue:owner/repo#1")
	got := f.s.closeBlocker(f.ctx, Object{"status": "completed", "project": "owner/repo", "number": int64(1)})
	unlock()
	if got != "この依頼への操作を送信中です。" {
		t.Fatalf("close blocker = %q", got)
	}
	if _, err := f.app.DB().ExecContext(f.ctx, bindSQL(`DELETE FROM actions WHERE id=?`), "match"); err != nil {
		t.Fatal(err)
	}
	if got := f.s.closeBlocker(f.ctx, Object{"status": "completed", "project": "owner/repo", "number": int64(1)}); got != "" {
		t.Fatalf("close blocker after removal = %q", got)
	}
}

// The list reader owns its gate while its database connection is stalled.
type stalledDraftConnector struct{ entered chan struct{} }

func (c stalledDraftConnector) Connect(context.Context) (driver.Conn, error) {
	return stalledDraftConn(c), nil
}
func (c stalledDraftConnector) Driver() driver.Driver { return stalledDraftDriver{} }

type stalledDraftDriver struct{}

func (stalledDraftDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("unexpected open")
}

type stalledDraftConn struct{ entered chan struct{} }

func (c stalledDraftConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c stalledDraftConn) Close() error              { return nil }
func (c stalledDraftConn) Begin() (driver.Tx, error) { return nil, errors.New("unexpected begin") }
func (c stalledDraftConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	close(c.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestIssueDetailProceedsWhileDraftListHoldsGate(t *testing.T) {
	f := newFixture(t)
	_, err := f.app.DB().ExecContext(f.ctx, bindSQL(`INSERT INTO drafts(id,json) VALUES(?,?)`), "draft", `{"project":"owner/repo","issue":{"number":1},"consultationProvider":"matched"}`)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	db := sql.OpenDB(stalledDraftConnector{entered})
	defer db.Close()
	f.s.o.Drafts = &consultations.Records{DB: db}
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() { _, err := f.s.o.Drafts.ListDraftEntries(ctx, consultations.DraftReadPage); done <- err }()
	defer cancel()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("draft list returned before reaching BeginTx: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("draft list did not reach BeginTx within 5 seconds")
	}
	result := make(chan struct {
		code  int
		value any
	}, 1)
	go func() {
		code, value := f.call("GET", "/api/issue?project=owner%2Frepo&number=1", "screen", nil)
		result <- struct {
			code  int
			value any
		}{code, value}
	}()
	select {
	case got := <-result:
		if got.code != 200 || fleet.ObjectOrEmpty(got.value)["consultationProvider"] != "matched" {
			t.Fatalf("status=%d detail=%v", got.code, got.value)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("issue detail waited for the draft list gate")
	}
}

func TestIssueDetailHandlesLegacyUnicodeDrafts(t *testing.T) {
	f := newFixture(t)
	for _, row := range []struct{ id, raw string }{
		{"match", `{"project":"owner/repo","issue":{"number":1},"consultationProvider":"matched","body":"a\u0000b"}`},
		{"other", `{"project":"owner/repo","issue":{"number":2},"body":"\ud800"}`},
	} {
		if _, err := f.app.DB().ExecContext(f.ctx, bindSQL(`INSERT INTO drafts(id,json) VALUES(?,?)`), row.id, row.raw); err != nil {
			t.Fatal(err)
		}
	}
	code, value := f.call("GET", "/api/issue?project=owner%2Frepo&number=1", "screen", nil)
	if code != 200 || fleet.ObjectOrEmpty(value)["consultationProvider"] != "matched" {
		t.Fatalf("status=%d detail=%v", code, value)
	}
}

func TestSubmittedDraftSelectionEquivalence(t *testing.T) {
	f := newFixture(t)
	for _, row := range []struct{ id, raw string }{
		{"a", `{"project":"owner/repo","issue":{"number":1},"consultationProvider":"first"}`},
		{"b", `{"project":"other","issue":{"project":"owner/repo","number":"1e0"},"consultationProvider":"second"}`},
		{"c", `{"project":"owner/repo","issue":{"number":2},"consultationProvider":"other"}`},
	} {
		if _, err := f.app.DB().ExecContext(f.ctx, bindSQL(`INSERT INTO drafts(id,json) VALUES(?,?)`), row.id, row.raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ number, want string }{{"1", "second"}, {"2", "other"}, {"3", "codex"}} {
		code, v := f.call("GET", "/api/issue?project=owner%2Frepo&number="+tc.number, "screen", nil)
		if code != 200 || fleet.ObjectOrEmpty(v)["consultationProvider"] != tc.want {
			t.Fatalf("number %s: %d %v", tc.number, code, v)
		}
	}
}

func TestSubmittedDraftLegacyEquivalence(t *testing.T) {
	f := newFixture(t)
	for _, row := range []struct{ id, raw string }{
		{"first", `{"project":"owner/repo","issue":{"number":1},"consultationProvider":"older"}`},
		{"legacy", `{"project":"owner/repo","issue":{"number":"1.0"},"consultationProvider":"legacy","body":"\ud800"}`},
		{"bad", `{"project":"owner/repo","issue":{"number":1}`},
	} {
		if _, err := f.app.DB().ExecContext(f.ctx, bindSQL(`INSERT INTO drafts(id,json) VALUES(?,?)`), row.id, row.raw); err != nil {
			t.Fatal(err)
		}
	}
	code, v := f.call("GET", "/api/issue?project=owner%2Frepo&number=1", "screen", nil)
	if code != 200 || fleet.ObjectOrEmpty(v)["consultationProvider"] != "legacy" {
		t.Fatalf("%d %v", code, v)
	}
}

func TestSubmittedDraftMutationAndErrors(t *testing.T) {
	f := newFixture(t)
	check := func(want string) {
		t.Helper()
		code, v := f.call("GET", "/api/issue?project=owner%2Frepo&number=1", "screen", nil)
		if code != 200 || fleet.ObjectOrEmpty(v)["consultationProvider"] != want {
			t.Fatalf("%d %v, want %s", code, v, want)
		}
	}
	check("codex")
	for _, q := range []struct {
		query string
		args  []any
		want  string
	}{
		{`INSERT INTO drafts(id,json) VALUES(?,?)`, []any{"d", `{"project":"owner/repo","issue":{"number":1},"consultationProvider":"added"}`}, "added"},
		{`UPDATE drafts SET json=? WHERE id=?`, []any{`{"project":"owner/repo","issue":{"number":1},"consultationProvider":"updated"}`, "d"}, "updated"},
		{`DELETE FROM drafts WHERE id=?`, []any{"d"}, "codex"},
	} {
		if _, err := f.app.DB().ExecContext(f.ctx, bindSQL(q.query), q.args...); err != nil {
			t.Fatal(err)
		}
		check(q.want)
	}
	db := sql.OpenDB(stalledDraftConnector{make(chan struct{})})
	db.Close()
	f.s.o.AppDB = db
	code, _ := f.call("GET", "/api/issue?project=owner%2Frepo&number=1", "screen", nil)
	if code < 500 {
		t.Fatalf("database error: status %d", code)
	}
}

// recordingDraftConnector observes the values returned by the real PostgreSQL driver.
type recordingDraftConnector struct {
	driver.Connector
	bodies map[string]int
}

func (c *recordingDraftConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &recordingDraftConn{Conn: conn, bodies: c.bodies}, nil
}

type recordingDraftConn struct {
	driver.Conn
	bodies map[string]int
}

func (c *recordingDraftConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *recordingDraftConn) CheckNamedValue(v *driver.NamedValue) error {
	if check, ok := c.Conn.(driver.NamedValueChecker); ok {
		return check.CheckNamedValue(v)
	}
	return driver.ErrSkip
}
func (c *recordingDraftConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	if strings.Contains(query, "FROM drafts ORDER BY") {
		return &recordingDraftRows{Rows: rows, bodies: c.bodies, selection: true}, nil
	}
	if strings.Contains(query, "SELECT json FROM drafts WHERE id=") {
		return &recordingDraftRows{Rows: rows, bodies: c.bodies, id: fmt.Sprint(args[0].Value)}, nil
	}
	return rows, nil
}

type recordingDraftRows struct {
	driver.Rows
	bodies    map[string]int
	selection bool
	id        string
}

func (r *recordingDraftRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err != nil {
		return err
	}
	if r.selection && dest[2] != nil {
		r.bodies[fmt.Sprint(dest[0])]++
	} else if !r.selection && dest[0] != nil {
		r.bodies[r.id]++
	}
	return nil
}
func TestSubmittedDraftExcludesUnrelatedBodies(t *testing.T) {
	f := newFixture(t)
	insert := func(id, raw string) {
		t.Helper()
		if _, err := f.app.DB().ExecContext(f.ctx, bindSQL(`INSERT INTO drafts(id,json) VALUES(?,?)`), id, raw); err != nil {
			t.Fatal(err)
		}
	}
	insert("match", `{"project":"owner/repo","issue":{"number":1},"consultationProvider":"found"}`)
	insert("legacy", `{"project":"other","issue":{"number":1},"body":"\ud800"}`)
	var schema string
	if err := f.app.DB().QueryRowContext(f.ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	recorder := &recordingDraftConnector{Connector: appsql.NewPostgresBindingConnector(stdlib.GetConnector(*config)), bodies: map[string]int{}}
	db := sql.OpenDB(recorder)
	defer db.Close()
	f.s.o.AppDB = db
	check := func() {
		t.Helper()
		clear(recorder.bodies)
		code, v := f.call("GET", "/api/issue?project=owner%2Frepo&number=1", "screen", nil)
		if code != 200 || fleet.ObjectOrEmpty(v)["consultationProvider"] != "found" {
			t.Fatalf("%d %v", code, v)
		}
		if len(recorder.bodies) != 2 || recorder.bodies["match"] != 1 || recorder.bodies["legacy"] != 1 {
			t.Fatalf("body rows: %v", recorder.bodies)
		}
	}
	check()
	for i := 0; i < 100; i++ {
		insert(fmt.Sprintf("unrelated-%d", i), `{"project":"owner/repo","issue":{"number":2},"body":"unrelated"}`)
	}
	check()
}
