package consultations

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/screenapi"
)

func TestDraftProjectedReads(t *testing.T) {
	ctx := context.Background()
	store := openPostgresStore(t)
	r, err := OpenRecords(ctx, store.DB())
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []struct{ id, json string }{
		{"a", `{"id":"a","owner":null,"project":"x","status":"draft","consultationBudget":{"stopped":{"calls":9007199254740993}},"body":"` + strings.Repeat("x", 4096) + `"}`},
		{"b", `{"id":"b","project":1e3,"status":null,"messages":[{"role":"user","text":"secret"}]}`},
	} {
		if _, err := r.DB.ExecContext(ctx, bindSQL("INSERT INTO drafts(id,json) VALUES(?,?)"), raw.id, raw.json); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := r.ListDraftEntries(ctx, DraftReadPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Key != "b" || entries[1].Key != "a" {
		t.Fatalf("database keys/order: %v", entries)
	}
	all, err := r.List(ctx, "drafts")
	if err != nil {
		t.Fatal(err)
	}
	page, err := r.ListDraftFields(ctx, DraftReadPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != len(all) {
		t.Fatal("row count changed")
	}
	for i := range all {
		for _, key := range []string{"id", "owner", "project", "status"} {
			a, exists := all[i][key]
			b, got := page[i][key]
			if exists != got || !reflect.DeepEqual(a, b) {
				t.Fatalf("%s: %#v != %#v", key, a, b)
			}
		}
		if _, ok := page[i]["body"]; ok {
			t.Fatal("body included")
		}
		if _, ok := page[i]["messages"]; ok {
			t.Fatal("messages included")
		}
	}
	alerts, err := r.ListDraftFields(ctx, DraftReadAlerts)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range alerts {
		if d["id"] == "a" && record(record(d["consultationBudget"])["stopped"])["calls"] != json.Number("9007199254740993") {
			t.Fatal("number changed")
		}
	}
	selected, err := r.GetDrafts(ctx, []string{"a", "missing", "a"})
	if err != nil || len(selected) != 1 || selected[0]["body"] != strings.Repeat("x", 4096) {
		t.Fatalf("selected: %v %v", selected, err)
	}
	if _, err := r.Change(ctx, "drafts", "a", func(_ *sql.Tx, d Record) (Record, error) { d["status"] = "closed"; return d, nil }); err != nil {
		t.Fatal(err)
	}
	selected, err = r.GetDrafts(ctx, []string{"a"})
	if err != nil || selected[0]["status"] != "closed" {
		t.Fatalf("update: %v %v", selected, err)
	}
	page, err = r.ListDraftFields(ctx, DraftReadPage)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range page {
		if d["id"] == "a" && d["status"] != "closed" {
			t.Fatal("projected update missing")
		}
	}
	if _, err := r.DB.ExecContext(ctx, bindSQL("DELETE FROM drafts WHERE id=?"), "a"); err != nil {
		t.Fatal(err)
	}
	selected, err = r.GetDrafts(ctx, []string{"a"})
	if err != nil || len(selected) != 0 {
		t.Fatalf("delete: %v %v", selected, err)
	}
	page, err = r.ListDraftFields(ctx, DraftReadPage)
	if err != nil || len(page) != 1 || page[0]["id"] != "b" {
		t.Fatalf("projected delete: %v %v", page, err)
	}
	if _, err := r.DB.ExecContext(ctx, bindSQL("INSERT INTO drafts(id,json) VALUES(?,?)"), "bad", `null`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ListDraftFields(ctx, DraftReadPage); err == nil {
		t.Fatal("non-object accepted")
	}
	if _, err := r.DB.ExecContext(ctx, bindSQL("UPDATE drafts SET json=? WHERE id=?"), `{`, "bad"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ListDraftFields(ctx, DraftReadPage); err == nil {
		t.Fatal("malformed JSON accepted")
	}
	if _, err := r.GetDrafts(ctx, []string{"bad"}); err == nil {
		t.Fatal("malformed full record accepted")
	}
}

func TestDraftScreenProjectionCompatibility(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	s.options.FamilyJobs = func(context.Context, []FamilyIssue) ([]FamilyJob, error) {
		return []FamilyJob{{Project: "owner/app", Number: 7, Status: "merged"}}, nil
	}
	rows := []Record{
		{"id": "p", "project": "owner/app", "status": "draft", "ready": true, "createdAt": "same", "splitChildren": []any{"c"}, "splitIssues": []any{Record{"project": "owner/app", "body": strings.Repeat("x", 4096)}}, "messages": []any{Record{"role": "proxy"}, Record{"role": "user", "text": strings.Repeat("😀", 50), "images": []any{Record{"id": "00000000-0000-0000-0000-000000000000"}}}}, "body": "x <!-- issue-desk:followup --> y"},
		{"id": "interrupted-split", "project": "owner/app", "status": "error", "consultationChildren": []any{"split-a", "split-b"}, "consultationParts": []any{"分割本文A", "分割本文B"}},
		{"id": "c", "project": "owner/app", "status": "submitted", "createdAt": "same", "parentSplit": "p", "issue": Record{"number": 7}},
		{"id": "empty", "project": 17, "status": nil, "messages": false, "splitIssues": false, "proposal": 5, "questions": nil},
		{"id": "bad-image", "project": "owner/app", "status": "thinking", "_stopped": true, "messages": []any{Record{"role": "user", "text": 42, "images": []any{Record{"id": "bad"}}}}, "consultationBudget": Record{"stopped": true}},
		{"id": "missing", "project": "owner/app"},
		{"id": "number-proposal", "project": "owner/app", "proposal": Record{"kind": json.Number("1e3")}},
		{"id": "nul", "project": "owner/app", "status": "draft", "parentSplit": "p", "body": "body\x00text", "savedMaterials": "saved\x00text", "messages": []any{Record{"role": "user", "text": "hello\x00world"}}},
	}
	for _, d := range rows {
		id := text(d["id"])
		if _, err := s.options.Records.Change(ctx, "drafts", id, func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
			t.Fatal(err)
		}
	}
	full, err := s.options.Records.List(ctx, "drafts")
	if err != nil {
		t.Fatal(err)
	}
	projected, err := s.options.Records.ListDraftFields(ctx, DraftReadScreen)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != len(projected) {
		t.Fatal("row count")
	}
	page, err := s.options.Records.ListDraftFields(ctx, DraftReadPage)
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := s.options.Records.ListDraftFields(ctx, DraftReadAlerts)
	if err != nil || len(alerts) != len(full) {
		t.Fatalf("alerts: %v, rows=%d", err, len(alerts))
	}
	for i := range full {
		if full[i]["id"] != page[i]["id"] || full[i]["id"] != alerts[i]["id"] {
			t.Fatal("projection order differs")
		}
	}
	page = s.FamilyProgress(ctx, page)
	full = s.FamilyProgress(ctx, full)
	projected = s.FamilyProgress(ctx, projected)
	find := func(records []Record, id string) Record {
		for _, d := range records {
			if d["id"] == id {
				return d
			}
		}
		t.Fatalf("missing %s", id)
		return nil
	}
	interrupted := find(projected, "interrupted-split")
	if interrupted["consultationParts"] != nil || draftRefusal(interrupted, "retry", false) != "" {
		t.Fatalf("一覧に分割本文を載せずに再試行を案内できません: %v", interrupted)
	}
	for _, action := range []string{"todo", "discard", "finish", "resubmit"} {
		a := s.familyRefusal(find(full, "p"), find(full, "p"), []Record{find(full, "c")}, action)
		b := s.familyRefusal(find(projected, "p"), find(projected, "p"), []Record{find(projected, "c")}, action)
		if a != b {
			t.Fatalf("family refusal %s differs", action)
		}
	}
	for i := range full {
		if !reflect.DeepEqual(full[i]["familyProgress"], page[i]["familyProgress"]) {
			t.Fatalf("page family progress differs for %v", full[i]["id"])
		}
		for _, principal := range []auth.Principal{{Kind: auth.Screen}, {Kind: auth.PC, Name: "reader"}, {Kind: auth.PC, Name: "reader", Delegated: true}} {
			if (s.draftAccess(full[i], principal) == nil) != (s.draftAccess(page[i], principal) == nil) {
				t.Fatalf("access changed for %v", full[i]["id"])
			}
		}
		if full[i]["id"] != projected[i]["id"] {
			t.Fatal("tie ordering changed")
		}
		for _, busy := range []bool{false, true} {
			for _, action := range []string{"chat", "retry", "continue_budget", "submit", "resubmit", "comment", "finish", "edit", "todo", "discard", "proxy_undo", "stop"} {
				if draftRefusal(full[i], action, busy) != draftRefusal(projected[i], action, busy) {
					t.Fatalf("%s %s", full[i]["id"], action)
				}
			}
		}
		a := screenapi.Summarize("draft", screenapi.Object(s.DraftView(full[i], auth.Principal{Kind: auth.Screen}, "")))
		b := screenapi.Summarize("draft", screenapi.Object(s.DraftView(projected[i], auth.Principal{Kind: auth.Screen}, "")))
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s: full=%#v projected=%#v", full[i]["id"], a, b)
		}
	}
}

// The generated benchmark fixture is reproducible: 6702 drafts, 4436 issues;
// even-indexed rows are unfinished (3351 drafts, 2218 issues). Each draft has
// an 8192-byte body, four 4096-byte messages and 8192 bytes of saved material.
// Issue rows use an 8192-byte body. External fixtures and route timing belong
// to the subsequent request-path work; no timing threshold is asserted here.
func TestDraftProjectionTransferVolume(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	r, e := OpenRecords(ctx, env.DB)
	if e != nil {
		t.Fatal(e)
	}
	// Small sample exercises the same production SQL and counts bytes actually scanned.
	for i := 0; i < 8; i++ {
		id := fmt.Sprint(i)
		d := Record{"id": id, "status": "draft", "project": "owner/app", "body": strings.Repeat("b", 8192), "messages": []any{Record{"role": "user", "text": strings.Repeat("m", 4096)}}, "savedMaterials": strings.Repeat("s", 8192)}
		if _, e = r.Change(ctx, "drafts", id, func(*sql.Tx, Record) (Record, error) { return d, nil }); e != nil {
			t.Fatal(e)
		}
	}
	config, e := pgx.ParseConfig(env.DSN)
	if e != nil {
		t.Fatal(e)
	}
	config.RuntimeParams["search_path"] = env.Schema
	counts := &readCounts{}
	db := sql.OpenDB(countConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*config)), counts})
	defer db.Close()
	reader := &Records{DB: db}
	if _, e = reader.List(ctx, "drafts"); e != nil {
		t.Fatal(e)
	}
	full := *counts
	*counts = readCounts{}
	if _, e = reader.ListDraftFields(ctx, DraftReadScreen); e != nil {
		t.Fatal(e)
	}
	screen := *counts
	*counts = readCounts{}
	if _, e = reader.GetDrafts(ctx, []string{"1", "2"}); e != nil {
		t.Fatal(e)
	}
	selected := *counts
	*counts = readCounts{}
	if empty, e := reader.GetDrafts(ctx, nil); e != nil || len(empty) != 0 || counts.calls != 0 {
		t.Fatalf("empty selection: %v %v %+v", empty, e, counts)
	}
	if full.calls != 1 || full.rows != 8 || screen.calls != 2 || screen.rows != 16 || selected.calls != 1 || selected.rows != 2 || screen.bytes >= full.bytes/4 || selected.bytes >= full.bytes {
		t.Fatalf("full %+v screen %+v selected %+v", full, screen, selected)
	}
	t.Logf("full %+v screen %+v selected %+v", full, screen, selected)
}

type readCounts struct{ calls, rows, bytes int }
type countConnector struct {
	base   driver.Connector
	counts *readCounts
}

func (c countConnector) Driver() driver.Driver { return c.base.Driver() }
func (c countConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &countConn{PostgresConn: conn.(appsql.PostgresConn), counts: c.counts}, nil
}

type countConn struct {
	appsql.PostgresConn
	counts *readCounts
}

func (c *countConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.counts.calls++
	rows, err := c.PostgresConn.QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	return &countRows{Rows: rows, counts: c.counts}, nil
}

type countRows struct {
	driver.Rows
	counts *readCounts
}

func (r *countRows) Next(v []driver.Value) error {
	err := r.Rows.Next(v)
	if err == nil {
		r.counts.rows++
		for _, x := range v {
			switch x := x.(type) {
			case string:
				r.counts.bytes += len(x)
			case []byte:
				r.counts.bytes += len(x)
			}
		}
	}
	return err
}

type readFailureDriver struct{}

var registerReadFailureDriver sync.Once

type readFailureConn struct {
	mode    string
	queries *[]string
}
type readFailureRows struct {
	mode      string
	sent      bool
	projected bool
	versions  bool
}

type readFailureTx struct{}

func (readFailureTx) Commit() error   { return nil }
func (readFailureTx) Rollback() error { return nil }

func (readFailureDriver) Open(name string) (driver.Conn, error) {
	return &readFailureConn{mode: name}, nil
}
func (*readFailureConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (*readFailureConn) Close() error              { return nil }
func (*readFailureConn) Begin() (driver.Tx, error) { return nil, errors.New("unexpected transaction") }
func (*readFailureConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return readFailureTx{}, nil
}
func (c *readFailureConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if c.queries != nil {
		*c.queries = append(*c.queries, q)
	}
	if strings.HasPrefix(c.mode, "state:") && !strings.Contains(q, "xmin::text") {
		if strings.HasPrefix(q, "SELECT id, json FROM") {
			return nil, errFullRead
		}
		return nil, &pgconn.PgError{Code: strings.TrimPrefix(c.mode, "state:")}
	}
	if c.mode == "query" {
		return nil, errors.New("query failure")
	}
	return &readFailureRows{mode: c.mode, projected: strings.HasPrefix(q, "SELECT id,"), versions: strings.Contains(q, "xmin::text")}, nil
}
func (r *readFailureRows) Columns() []string {
	if r.projected {
		return []string{"id", "json"}
	}
	return []string{"json"}
}
func (*readFailureRows) Close() error { return nil }
func (r *readFailureRows) Next(v []driver.Value) error {
	if r.mode == "rows" {
		return errors.New("rows failure")
	}
	if r.sent {
		return io.EOF
	}
	r.sent = true
	if r.versions {
		v[0], v[1] = "id", "1"
		return nil
	}
	if r.projected {
		v[0] = "id"
		v = v[1:]
	}
	if r.mode == "scan" {
		v[0] = nil
	} else {
		v[0] = "{"
	}
	return nil
}
func TestDraftReadFailures(t *testing.T) {
	registerReadFailureDriver.Do(func() { sql.Register("draft-read-failures", readFailureDriver{}) })
	for _, mode := range []string{"query", "scan", "decode", "rows"} {
		t.Run(mode, func(t *testing.T) {
			db, e := sql.Open("draft-read-failures", mode)
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			r := &Records{DB: db}
			if _, e = r.ListDraftFields(context.Background(), DraftReadPage); e == nil {
				t.Fatal("failure hidden")
			}
			if _, e = r.GetDrafts(context.Background(), []string{"a"}); e == nil {
				t.Fatal("failure hidden")
			}
		})
	}
}

func TestDraftSelectionUsesDatabaseKey(t *testing.T) {
	ctx := context.Background()
	st := openPostgresStore(t)
	r, e := OpenRecords(ctx, st.DB())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.DB.ExecContext(ctx, bindSQL("INSERT INTO drafts(id,json) VALUES(?,?)"), "database-key", `{"id":null,"body":"full"}`); e != nil {
		t.Fatal(e)
	}
	entries, e := r.ListDraftEntries(ctx, DraftReadPage)
	if e != nil || len(entries) != 1 || entries[0].Key != "database-key" || entries[0].Record["id"] != nil {
		t.Fatalf("entries %v: %v", entries, e)
	}
	rows, e := r.GetDrafts(ctx, []string{entries[0].Key})
	if e != nil || len(rows) != 1 || rows[0]["body"] != "full" {
		t.Fatalf("rows %v: %v", rows, e)
	}
}

func TestDraftProjectionInvalidPostgresJSONFallback(t *testing.T) {
	ctx := context.Background()
	r := &Records{DB: openPostgresStore(t).DB()}
	for _, row := range []struct{ id, raw string }{
		{"normal", `{"id":"normal","project":"owner/app","status":"draft"}`},
		{"surrogate", `{"id":"s","project":"owner/app","status":"draft","title":"\ud800","messages":[{"role":"user","text":"a\udc00b"}]}`},
		{"nul", `{"id":"nul","project":"owner/app","status":"submitting","body":"a\u0000b"}`},
	} {
		if _, err := r.DB.ExecContext(ctx, bindSQL("INSERT INTO drafts(id,json) VALUES(?,?)"), row.id, row.raw); err != nil {
			t.Fatal(err)
		}
	}
	full, err := r.List(ctx, "drafts")
	if err != nil {
		t.Fatal(err)
	}
	for _, purpose := range []DraftReadPurpose{DraftReadPage, DraftReadAlerts, DraftReadScreen, DraftReadRecovery, DraftReadFiling} {
		entries, err := r.ListDraftEntries(ctx, purpose)
		if err != nil {
			t.Fatalf("%s: %v", purpose, err)
		}
		if len(entries) != len(full) {
			t.Fatalf("%s: row count", purpose)
		}
		for i, entry := range entries {
			if !reflect.DeepEqual(entry.Record, full[i]) {
				t.Fatalf("%s row %d differs", purpose, i)
			}
		}
	}
	if _, err := r.DB.ExecContext(ctx, bindSQL("UPDATE drafts SET json=? WHERE id=?"), `{"id":"t","project":"owner/app"} trailing`, "surrogate"); err != nil {
		t.Fatal(err)
	}
	full, err = r.List(ctx, "drafts")
	if err != nil {
		t.Fatal(err)
	}
	for _, purpose := range []DraftReadPurpose{DraftReadPage, DraftReadAlerts, DraftReadScreen, DraftReadRecovery, DraftReadFiling} {
		entries, err := r.ListDraftEntries(ctx, purpose)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != len(full) {
			t.Fatal("row count")
		}
		for i, entry := range entries {
			if !reflect.DeepEqual(entry.Record, full[i]) {
				t.Fatalf("%s row %d differs", purpose, i)
			}
		}
	}

}

func TestDraftProjectionEscapedNULFallback(t *testing.T) {
	ctx := context.Background()
	r := &Records{DB: openPostgresStore(t).DB()}
	for _, row := range []struct{ id, raw string }{
		{"normal", `{"id":"normal","status":"draft"}`},
		{"nul", `{"id":"nul","status":"submitting","body":"a\u0000b"}`},
	} {
		if _, err := r.DB.ExecContext(ctx, bindSQL("INSERT INTO drafts(id,json) VALUES(?,?)"), row.id, row.raw); err != nil {
			t.Fatal(err)
		}
	}
	full, err := r.List(ctx, "drafts")
	if err != nil {
		t.Fatal(err)
	}
	for _, purpose := range []DraftReadPurpose{DraftReadRecovery, DraftReadFiling} {
		entries, err := r.ListDraftEntries(ctx, purpose)
		if err != nil {
			t.Fatalf("%s: %v", purpose, err)
		}
		if len(entries) != len(full) {
			t.Fatalf("%s: row count", purpose)
		}
		for i, entry := range entries {
			if !reflect.DeepEqual(entry.Record, full[i]) {
				t.Fatalf("%s row %d differs", purpose, i)
			}
		}
	}
}

func TestReceiptReloadAfterResponseLoss(t *testing.T) {
	ctx := context.Background()
	s, db, _ := fixture(t)
	s.options.Start = nil
	p := auth.Principal{Kind: auth.Screen}
	input := Record{"id": todoID, "project": "owner/app", "tier": "light", "text": "保存された本文", "hold": true}
	if _, err := s.AcceptIssue(ctx, input, p); err != nil {
		t.Fatal(err)
	}
	// A fresh service and Records instance represent the surviving process.
	records, err := OpenRecords(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	opts := s.options
	opts.Records = records
	survivor, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	view, err := survivor.readDraft(ctx, todoID, p)
	if err != nil || view["receiptId"] != todoID || view["receiptState"] != "accepted" {
		t.Fatalf("reload: %v %v", view, err)
	}
	replay, err := survivor.AcceptIssue(ctx, input, p)
	if err != nil || replay["receiptId"] != todoID {
		t.Fatalf("replay: %v %v", replay, err)
	}
	var raw string
	if err := records.DB.QueryRowContext(ctx, bindSQL("SELECT json FROM intake_receipts WHERE id=?"), todoID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	saved, err := decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if saved["phase"] != "accepted" || saved["kind"] != "issue" || saved["sendId"] != todoID || saved["draftId"] != todoID || record(saved["input"])["text"] != "保存された本文" {
		t.Fatalf("recovery state: %v", saved)
	}
}

var errFullRead = errors.New("full read failed")

type readFailureConnector struct {
	mode    string
	queries *[]string
}

func (c readFailureConnector) Driver() driver.Driver { return readFailureDriver{} }
func (c readFailureConnector) Connect(context.Context) (driver.Conn, error) {
	return &readFailureConn{mode: c.mode, queries: c.queries}, nil
}

func TestDraftProjectionSQLStateFailureBoundaries(t *testing.T) {
	for _, code := range []string{"57014", "08006", "22P02", "22P05"} {
		t.Run(code, func(t *testing.T) {
			var queries []string
			db := sql.OpenDB(readFailureConnector{mode: "state:" + code, queries: &queries})
			defer db.Close()
			_, err := (&Records{DB: db}).ListDraftFields(context.Background(), DraftReadPage)
			conversion := code == "22P02" || code == "22P05"
			if conversion {
				if !errors.Is(err, errFullRead) {
					t.Fatalf("full-read failure hidden: %v", err)
				}
			} else {
				var state *pgconn.PgError
				if !errors.As(err, &state) || state.Code != code {
					t.Fatalf("database failure changed: %v", err)
				}
			}
			full := 0
			for _, q := range queries {
				if strings.HasPrefix(q, "SELECT id, json FROM") {
					full++
				}
			}
			want := 0
			if conversion {
				want = 1
			}
			if full != want {
				t.Fatalf("full-read attempts=%d want=%d; queries=%v", full, want, queries)
			}
		})
	}
}
