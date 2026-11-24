package consultations

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

type routeReadControl struct {
	counts                                     readCounts
	fail                                       bool
	failList, failProjection                   bool
	listCalls, projectionCalls                 int
	beforeFull                                 func()
	fullCalls, fullRows, fullBytes             int
	singleCalls, singleRows, singleBytes       int
	fallbackCalls, fallbackRows, fallbackBytes int
	familyCalls, familyRows, familyBytes       int
	pageProjectionRows, pageProjectionBytes    int
	failFamily, failSingle                     bool
	beforeFamily                               func()
}
type routeReadConnector struct {
	base    driver.Connector
	control *routeReadControl
}

func (c routeReadConnector) Driver() driver.Driver { return c.base.Driver() }
func (c routeReadConnector) Connect(ctx context.Context) (driver.Conn, error) {
	v, e := c.base.Connect(ctx)
	if e != nil {
		return nil, e
	}
	return &routeReadConn{PostgresConn: v.(appsql.PostgresConn), control: c.control}, nil
}

type routeReadConn struct {
	appsql.PostgresConn
	control *routeReadControl
}

func (c *routeReadConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	list := strings.HasPrefix(q, "SELECT json FROM drafts ORDER BY")
	projection := strings.HasPrefix(q, "SELECT id, CASE WHEN")
	family := strings.HasPrefix(q, "WITH parent_ids AS")
	single := strings.HasPrefix(q, "SELECT json FROM drafts WHERE id=")
	fallback := strings.HasPrefix(q, "SELECT id, json FROM drafts ORDER BY") || list
	if fallback {
		c.control.fallbackCalls++
	}
	if single {
		c.control.singleCalls++
		if c.control.failSingle {
			return nil, errors.New("family read failed")
		}
	}
	if family {
		c.control.familyCalls++
		if c.control.beforeFamily != nil {
			f := c.control.beforeFamily
			c.control.beforeFamily = nil
			f()
		}
		if c.control.failFamily {
			return nil, errors.New("family read failed")
		}
	}
	if list {
		c.control.listCalls++
	}
	if projection {
		c.control.projectionCalls++
	}
	if c.control.failList && list || c.control.failProjection && projection {
		return nil, errors.New("draft read failed")
	}
	full := strings.HasPrefix(q, "SELECT id, json FROM drafts WHERE id IN") || strings.HasPrefix(q, "SELECT json FROM drafts WHERE id IN")
	if full {
		c.control.fullCalls++
		if c.control.beforeFull != nil {
			f := c.control.beforeFull
			c.control.beforeFull = nil
			f()
		}
		if c.control.fail {
			return nil, errors.New("selected read failed")
		}
	}
	c.control.counts.calls++
	rows, e := c.PostgresConn.QueryContext(ctx, q, args)
	if e != nil {
		return nil, e
	}
	return &routeReadRows{Rows: rows, control: c.control, full: full, single: single, family: family, projection: projection, fallback: fallback}, nil
}

type routeReadRows struct {
	driver.Rows
	control                              *routeReadControl
	full                                 bool
	single, family, projection, fallback bool
}

func (r *routeReadRows) Next(v []driver.Value) error {
	e := r.Rows.Next(v)
	if e == nil {
		r.control.counts.rows++
		for _, x := range v {
			n := 0
			switch value := x.(type) {
			case string:
				n = len(value)
			case []byte:
				n = len(value)
			}
			r.control.counts.bytes += n
			if r.full {
				r.control.fullBytes += n
			}
			if r.single {
				r.control.singleBytes += n
			}
			if r.family {
				r.control.familyBytes += n
			}
			if r.projection {
				r.control.pageProjectionBytes += n
			}
			if r.fallback {
				r.control.fallbackBytes += n
			}
		}
		if r.full {
			r.control.fullRows++
		}
		if r.single {
			r.control.singleRows++
		}
		if r.family {
			r.control.familyRows++
		}
		if r.projection {
			r.control.pageProjectionRows++
		}
		if r.fallback {
			r.control.fallbackRows++
		}
	}
	return e
}

func TestDraftRouteReadVolumeFailuresAndUpdates(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	if _, e := OpenRecords(ctx, env.DB); e != nil {
		t.Fatal(e)
	}
	for _, row := range []struct{ id, json string }{{"parent", `{"id":"parent","project":"owner/app","status":"draft","body":"old","splitChildren":["child"],"splitIssues":[{}]}`}, {"child", `{"id":"child","project":"owner/app","status":"submitted","parentSplit":"parent","issue":{"number":7}}`}, {"other", `{"id":"other","project":"other/app","status":"draft","body":"` + strings.Repeat("x", 8192) + `"}`}} {
		if _, e := env.DB.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES($1,$2)", row.id, row.json); e != nil {
			t.Fatal(e)
		}
	}
	cfg, e := pgx.ParseConfig(env.DSN)
	if e != nil {
		t.Fatal(e)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	control := &routeReadControl{}
	readDB := sql.OpenDB(routeReadConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), control})
	defer readDB.Close()
	s, db, _ := fixture(t)
	s.options.Records = &Records{DB: readDB}
	s.options.FamilyJobs = func(context.Context, []FamilyIssue) ([]FamilyJob, error) {
		return []FamilyJob{{Project: "owner/app", Number: 7, Status: "merged"}}, nil
	}
	token := stopToken(t, db, true)
	mux := http.NewServeMux()
	s.Register(mux)
	request := func(query string) (int, map[string]any) {
		req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts"+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		var out map[string]any
		if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
			t.Fatal(e)
		}
		return w.Code, out
	}
	code, out := request("?project=owner%2Fapp&status=draft&limit=1")
	if code != 200 || control.fullCalls != 1 || control.fullRows != 1 || control.fullBytes == 0 {
		t.Fatal(code, out, control)
	}
	row := out["items"].([]any)[0].(map[string]any)
	if row["body"] != "old" || row["familyProgress"].(map[string]any)["completed"] != float64(1) {
		t.Fatal(row)
	}
	*control = routeReadControl{}
	code, out = request("?page=999&limit=1")
	if code != 200 || control.fullCalls != 0 || len(out["items"].([]any)) != 0 {
		t.Fatal(code, out, control)
	}
	// Change the database after selection but immediately before the full read.
	for _, mutation := range []string{
		`DELETE FROM drafts WHERE id='parent'`,
		`UPDATE drafts SET json=jsonb_set(json::jsonb, '{status}', '"finished"') WHERE id='parent'`,
		`UPDATE drafts SET json=jsonb_set(json::jsonb, '{project}', '"other/app"') WHERE id='parent'`,
	} {
		*control = routeReadControl{beforeFull: func() {
			if _, err := env.DB.ExecContext(ctx, mutation); err != nil {
				t.Fatal(err)
			}
		}}
		code, out = request("?project=owner%2Fapp&status=draft&limit=1")
		if code != 200 || len(out["items"].([]any)) != 0 || out["total"] != float64(1) || out["hasMore"] != false {
			t.Fatalf("mutation %s: %d %v", mutation, code, out)
		}
		if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES('parent','{"id":"parent","project":"owner/app","status":"draft","body":"old","splitChildren":["child"],"splitIssues":[{}]}') ON CONFLICT(id) DO UPDATE SET json=excluded.json`); err != nil {
			t.Fatal(err)
		}
	}
	// A deletion on a page with a successor retains the selection-time total
	// and hasMore, even though that page now contains no items.
	*control = routeReadControl{beforeFull: func() {
		if _, err := env.DB.ExecContext(ctx, `DELETE FROM drafts WHERE id='parent'`); err != nil {
			t.Fatal(err)
		}
	}}
	code, out = request("?limit=1")
	if code != 200 || len(out["items"].([]any)) != 0 || out["total"] != float64(2) || out["hasMore"] != true {
		t.Fatal(code, out)
	}
	if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES('parent','{"id":"parent","project":"owner/app","status":"draft","body":"old","splitChildren":["child"],"splitIssues":[{}]}')`); err != nil {
		t.Fatal(err)
	}

	plain := "idk_" + strings.Repeat("b", 43)
	if _, err := db.AddPCToken(ctx, store.PCTokenRecord{ID: "plain", Name: "plain", Hash: auth.Hash(plain), IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	*control = routeReadControl{}
	req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || control.fullCalls != 0 {
		t.Fatalf("non-delegated list: %d %s", w.Code, w.Body.String())
	}

	// Non-delegated PC tokens are rejected by the list route itself. Also
	// exercise the full-record access check after an ownership change.
	principal := auth.Principal{Kind: auth.PC, TokenID: "monitor"}
	if _, e = env.DB.ExecContext(ctx, `UPDATE drafts SET json=jsonb_set(json::jsonb, '{owner}', '"token:monitor"', true) WHERE id='parent'`); e != nil {
		t.Fatal(e)
	}
	entries, e := s.options.Records.ListDraftEntries(ctx, DraftReadPage)
	if e != nil {
		t.Fatal(e)
	}
	var selected DraftFields
	for _, entry := range entries {
		if entry.Key == "parent" {
			selected = entry
		}
	}
	if selected.Record == nil || s.draftAccess(selected.Record, principal) != nil {
		t.Fatal("owner projection was not accessible")
	}
	*control = routeReadControl{beforeFull: func() {
		if _, err := env.DB.ExecContext(ctx, `UPDATE drafts SET json=jsonb_set(json::jsonb, '{owner}', '"token:other"') WHERE id='parent'`); err != nil {
			t.Fatal(err)
		}
	}}
	full, e := s.options.Records.GetDrafts(ctx, []string{selected.Key})
	if e != nil || len(full) != 1 || s.draftAccess(full[0], principal) == nil {
		t.Fatalf("changed owner remained accessible: %v", e)
	}

	*control = routeReadControl{fail: true}
	code, _ = request("?limit=1")
	if code < 500 || control.fullCalls != 1 {
		t.Fatal(code, control)
	}
	*control = routeReadControl{}
	if _, e = env.DB.ExecContext(ctx, `UPDATE drafts SET json='{"id":"parent","project":"owner/app","status":"finished","body":"new","splitChildren":["child"],"splitIssues":[{}]}' WHERE id='parent'`); e != nil {
		t.Fatal(e)
	}
	if _, e = env.DB.ExecContext(ctx, `UPDATE drafts SET json='{"id":"child","project":"owner/app","status":"closed","parentSplit":"parent"}' WHERE id='child'`); e != nil {
		t.Fatal(e)
	}
	code, out = request("?project=owner%2Fapp&status=draft")
	if code != 200 || out["total"] != float64(0) {
		t.Fatal(code, out)
	}
	code, out = request("?project=owner%2Fapp&status=finished")
	if code != 200 || out["total"] != float64(1) {
		t.Fatal(code, out)
	}
	row = out["items"].([]any)[0].(map[string]any)
	if row["body"] != "new" || row["familyProgress"].(map[string]any)["closed"] != float64(1) {
		t.Fatal(row)
	}
	if _, e = env.DB.ExecContext(ctx, `DELETE FROM drafts WHERE id='parent'`); e != nil {
		t.Fatal(e)
	}
	code, out = request("?project=owner%2Fapp&status=finished")
	if code != 200 || out["total"] != float64(0) {
		t.Fatal(code, out)
	}
}

// The route must reapply the current access predicate to the full row, even if
// that predicate later gains an owner restriction for delegated principals.
func TestDraftRouteBatchKeysAndRevalidation(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	if _, err := OpenRecords(ctx, env.DB); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	control := &routeReadControl{}
	readDB := sql.OpenDB(routeReadConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), control})
	defer readDB.Close()
	s, db, _ := fixture(t)
	s.options.Records = &Records{DB: readDB}
	s.options.FamilyJobs = func(context.Context, []FamilyIssue) ([]FamilyJob, error) {
		return []FamilyJob{{Project: "owner/app", Number: 7, Status: "merged"}}, nil
	}
	token := stopToken(t, db, true)
	mux := http.NewServeMux()
	s.Register(mux)
	insert := func(key, raw string) {
		t.Helper()
		if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, key, raw); err != nil {
			t.Fatal(err)
		}
	}
	insert("z", `{"id":"dup","project":"owner/app","status":"draft","body":"z","splitChildren":["child"],"splitIssues":[{}]}`)
	insert("y", `{"id":"dup","project":"owner/app","status":"draft","body":"y","familyProgress":{"old":true}}`)
	insert("x", `{"id":42,"project":"owner/app","status":"draft","body":"x"}`)
	insert("w", `{"project":"owner/app","status":"draft","body":"w"}`)
	insert("child", `{"id":"child","project":"owner/app","status":"submitted","parentSplit":"dup","issue":{"number":7}}`)
	request := func() map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts?status=draft&limit=10", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
			t.Fatalf("%d %s %v", w.Code, w.Body.String(), err)
		}
		return out
	}
	out := request()
	items := out["items"].([]any)
	if control.fullCalls != 1 || control.fullRows != 4 || len(items) != 4 || out["total"] != float64(4) || out["hasMore"] != false {
		t.Fatal(out, control)
	}
	for i, body := range []string{"w", "x", "y", "z"} {
		row := items[i].(map[string]any)
		if row["body"] != body {
			t.Fatal(items)
		}
		if i == 3 {
			if row["familyProgress"].(map[string]any)["completed"] != float64(1) {
				t.Fatal(row)
			}
		} else if _, ok := row["familyProgress"]; ok {
			t.Fatal(row)
		}
	}
	*control = routeReadControl{beforeFull: func() {
		if _, err := env.DB.ExecContext(ctx, `DELETE FROM drafts WHERE id='y'`); err != nil {
			t.Fatal(err)
		}
	}}
	out = request()
	items = out["items"].([]any)
	if control.fullCalls != 1 || control.fullRows != 3 || len(items) != 3 || items[0].(map[string]any)["body"] != "w" || items[1].(map[string]any)["body"] != "x" || items[2].(map[string]any)["body"] != "z" || out["total"] != float64(4) {
		t.Fatal(out, control)
	}
	// A selected unassigned row becomes inaccessible when unassigned is removed.
	if _, err := env.DB.ExecContext(ctx, `DELETE FROM drafts`); err != nil {
		t.Fatal(err)
	}
	insert("z", `{"id":"z","unassigned":true,"status":"draft","owner":"token:before"}`)
	*control = routeReadControl{beforeFull: func() {
		if _, err := env.DB.ExecContext(ctx, `UPDATE drafts SET json=jsonb_set(json::jsonb,'{unassigned}','false') WHERE id='z'`); err != nil {
			t.Fatal(err)
		}
	}}
	out = request()
	if len(out["items"].([]any)) != 0 || out["total"] != float64(1) {
		t.Fatal(out)
	}
	// Delegated PCs currently retain access after an owner change. Compare the
	// route with the actual principal's access predicate so future changes fail
	// if the route stops rechecking it.
	if _, err := env.DB.ExecContext(ctx, `UPDATE drafts SET json='{"id":"z","project":"owner/app","status":"draft","owner":"token:monitor"}' WHERE id='z'`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	principal, authErr := s.options.Auth.Authenticate(req)
	if authErr != nil {
		t.Fatal(authErr)
	}
	before, found, err := s.options.Records.Get(ctx, "drafts", "z")
	if err != nil || !found || s.draftAccess(before, principal) != nil {
		t.Fatalf("initial owner must be accessible to the list principal: %v", err)
	}
	updated := false
	*control = routeReadControl{beforeFull: func() {
		result, err := env.DB.ExecContext(ctx, `UPDATE drafts SET json=jsonb_set(json::jsonb,'{owner}','"token:after"') WHERE id='z'`)
		if err != nil {
			t.Fatal(err)
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			t.Fatalf("owner update affected %d rows: %v", n, err)
		}
		updated = true
	}}
	out = request()
	changed, found, err := s.options.Records.Get(ctx, "drafts", "z")
	if err != nil || !found || changed["owner"] != "token:after" || !updated || out["total"] != float64(1) || control.fullCalls != 1 || control.fullRows != 1 {
		t.Fatalf("selection, full read, or owner update missing: out=%v control=%+v changed=%v updated=%v err=%v", out, control, changed, updated, err)
	}
	allowed := s.draftAccess(changed, principal) == nil
	if (len(out["items"].([]any)) == 1) != allowed {
		t.Fatal(out, allowed)
	}
}

func TestDraftDetailFamilySelectedRead(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	if _, err := OpenRecords(ctx, env.DB); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	control := &routeReadControl{}
	readDB := sql.OpenDB(routeReadConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), control})
	defer readDB.Close()
	s, db, _ := fixture(t)
	s.options.Records = &Records{DB: readDB}
	s.options.FamilyJobs = func(context.Context, []FamilyIssue) ([]FamilyJob, error) {
		return []FamilyJob{{Project: "owner/app", Number: 7, Status: "merged"}}, nil
	}
	for _, row := range []struct{ id, raw string }{{"11111111-1111-1111-1111-111111111111", `{"id":"11111111-1111-1111-1111-111111111111","project":"owner/app","splitChildren":["child","missing"],"splitIssues":[{},{},{}]}`}, {"child", `{"id":"child","project":"owner/app","parentSplit":"11111111-1111-1111-1111-111111111111","issue":{"number":7}}`}, {"other", `{"id":"other","body":"` + strings.Repeat("x", 65536) + `"}`}} {
		if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, row.id, row.raw); err != nil {
			t.Fatal(err)
		}
	}
	token := stopToken(t, db, true)
	mux := http.NewServeMux()
	s.Register(mux)
	request := func() map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts/11111111-1111-1111-1111-111111111111", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
			t.Fatalf("%d %s %v", w.Code, w.Body.String(), err)
		}
		return out
	}
	records, err := s.options.Records.List(ctx, "drafts")
	if err != nil {
		t.Fatal(err)
	}
	legacy := s.FamilyProgress(ctx, records)
	var progress any
	for _, r := range legacy {
		if r["id"] == "11111111-1111-1111-1111-111111111111" {
			progress = r["familyProgress"]
		}
	}
	*control = routeReadControl{}
	out := request()
	expected, _ := json.Marshal(progress)
	actual, _ := json.Marshal(out["familyProgress"])
	if string(actual) != string(expected) || control.fullRows != 2 {
		t.Fatalf("out=%v legacy=%s counts=%+v", out, expected, control)
	}
	bytes := control.fullBytes
	if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, "other2", `{"id":"other2","body":"`+strings.Repeat("z", 131072)+`"}`); err != nil {
		t.Fatal(err)
	}
	*control = routeReadControl{}
	request()
	if control.fullRows != 2 || control.fullBytes != bytes {
		t.Fatalf("full read grew: %+v, before=%d", control, bytes)
	}
}

func TestDraftSummaryHTTPAndSelection(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	if _, err := OpenRecords(ctx, env.DB); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("long-message-", 770)[:10000]
	rows := []Record{
		{"id": "one", "project": "owner/app", "status": "draft", "title": "first", "shownTitle": "shown", "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-02T00:00:00Z", "owner": "secret", "messages": []any{Record{"text": long}}},
		{"id": "two", "project": "owner/app", "status": "submitted", "title": "second", "createdAt": "2026-02-01T00:00:00Z"},
		{"id": "three", "unassigned": true, "status": "draft", "title": "third", "updatedAt": "2026-03-01T00:00:00Z"},
		{"id": "hidden", "project": "other/app", "status": "draft", "owner": "secret"},
	}
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, row["id"], string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	control := &routeReadControl{}
	readDB := sql.OpenDB(routeReadConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), control})
	defer readDB.Close()
	s, db, _ := fixture(t)
	s.options.Records = &Records{DB: readDB}
	token := stopToken(t, db, true)
	plain := "idk_" + strings.Repeat("b", 43)
	if _, err := db.AddPCToken(ctx, store.PCTokenRecord{ID: "plain", Name: "plain", Hash: auth.Hash(plain), IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	request := func(query, bearer string) (int, map[string]any, string) {
		t.Helper()
		req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts"+query, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return w.Code, out, w.Body.String()
	}
	code, out, body := request("?view=summary", token)
	if code != 200 || len(out["items"].([]any)) != 3 || strings.Contains(body, long) || control.fullCalls != 0 || control.fullRows != 0 || control.familyCalls != 0 || control.singleCalls != 0 {
		t.Fatalf("%d %s counts=%+v", code, body, control)
	}
	allowed := map[string]bool{"id": true, "project": true, "unassigned": true, "status": true, "title": true, "shownTitle": true, "createdAt": true, "updatedAt": true}
	expected := map[string]Record{}
	for _, row := range rows[:3] {
		expected[row["id"].(string)] = row
	}
	for _, item := range out["items"].([]any) {
		row := item.(map[string]any)
		original := expected[row["id"].(string)]
		for key := range row {
			if !allowed[key] {
				t.Fatalf("unexpected %s: %v", key, row)
			}
		}
		for key := range allowed {
			want, present := original[key]
			got, exists := row[key]
			if present != exists || present && !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: got %v (%v), want %v (%v)", key, got, exists, want, present)
			}
		}
	}
	for _, query := range []string{"", "project=owner%2Fapp", "status=draft", "excludeStatus=submitted", "page=2", "limit=1", "project=owner%2Fapp&status=draft,submitted&excludeStatus=submitted&page=1&limit=1", "page=999&limit=1"} {
		sep := "?"
		if query != "" {
			sep += query + "&"
		}
		a, full, _ := request("?"+query, token)
		b, summary, _ := request(sep+"view=summary", token)
		if a != 200 || b != 200 {
			t.Fatalf("%s: %d %d", query, a, b)
		}
		for _, key := range []string{"total", "hasMore", "page", "limit"} {
			if !reflect.DeepEqual(full[key], summary[key]) {
				t.Fatalf("%s %s: %v %v", query, key, full, summary)
			}
		}
		ids := func(page map[string]any) []any {
			result := []any{}
			for _, v := range page["items"].([]any) {
				result = append(result, v.(map[string]any)["id"])
			}
			return result
		}
		if !reflect.DeepEqual(ids(full), ids(summary)) {
			t.Fatalf("%s: %v %v", query, full, summary)
		}
	}
	for _, tc := range []struct {
		query, token string
		status       int
	}{{"?view=unknown", token, 400}, {"?view=summary", "", 401}, {"?view=summary", plain, 403}} {
		code, _, body := request(tc.query, tc.token)
		if code != tc.status {
			t.Fatalf("%s: %d %s", tc.query, code, body)
		}
	}
}
