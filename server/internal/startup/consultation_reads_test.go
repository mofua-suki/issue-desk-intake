package startup

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/screenapi"
	"issue-desk-intake/server/internal/store"
)

func entryTestApp(t *testing.T, db *sql.DB, jobs consultations.FamilyJobs, issues consultations.FamilyIssues) (*App, map[string]screenapi.EntryReader) {
	t.Helper()
	r := &consultations.Records{DB: db}
	s, err := consultations.New(consultations.Options{Records: r, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }, FamilyJobs: jobs, FamilyIssues: issues})
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Records: r, Consultations: s}
	e := map[string]screenapi.EntryReader{}
	a.registerConsultationReaders(map[string]screenapi.ListReader{}, e)
	return a, e
}

// legacyDraftEntry retains the original ordered full-read entry behavior.
func legacyDraftEntry(t *testing.T, app *App, id string) map[string]any {
	t.Helper()
	ctx := context.Background()
	record, found, err := app.Records.Get(ctx, "drafts", id)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return nil
	}
	if record["parentSplit"] == nil && record["splitChildren"] != nil {
		if records, err := app.Records.List(ctx, "drafts"); err == nil {
			for _, candidate := range app.Consultations.FamilyProgress(ctx, records) {
				if candidate["id"] == record["id"] {
					record = candidate
					break
				}
			}
		}
	}
	return map[string]any(app.Consultations.DraftView(record, auth.Principal{Kind: auth.Screen}, ""))
}

func TestDraftEntryLegacyAndTransfer(t *testing.T) {
	env, app, counts, _, entries := screenFixture(t)
	ctx := context.Background()
	parent := `{"id":"parent","project":"owner/app","splitChildren":["child","missing"],"body":"<!-- issue-desk:followup -->","messages":[{"role":"proxy"},{"role":"user","text":"hello","images":[{"id":"00000000-0000-0000-0000-000000000000"}]}]}`
	child := `{"id":"child","project":"owner/app","parentSplit":"parent","issue":{"number":7},"status":"submitted"}`
	for _, row := range [][2]string{{"parent", parent}, {"child", child}} {
		if _, err := env.DB.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES (?,?)", row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	compare := func(id string) {
		t.Helper()
		want := legacyDraftEntry(t, app, id)
		got, err := entries["draft"](ctx, map[string]any{"id": id}, "")
		if err != nil || !reflect.DeepEqual(want, got) {
			t.Fatalf("id=%s legacy=%v got=%v err=%v", id, want, got, err)
		}
	}
	for _, id := range []string{"parent", "child", "missing"} {
		compare(id)
	}
	for _, n := range []int{0, 100, 1000} {
		for _, size := range []int{1024, 65536} {
			t.Run(fmt.Sprintf("unrelated_%d_body_%d", n, size), func(t *testing.T) {
				if _, err := env.DB.ExecContext(ctx, "DELETE FROM drafts WHERE id LIKE 'other-%'"); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < n; i++ {
					id := fmt.Sprintf("other-%04d", i)
					raw, _ := json.Marshal(map[string]any{"id": id, "body": strings.Repeat("x", size), "messages": []any{}, "attachments": []any{}})
					if _, err := env.DB.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES (?,?)", id, string(raw)); err != nil {
						t.Fatal(err)
					}
				}
				*counts = screenCounts{}
				want := legacyDraftEntry(t, app, "parent")
				legacy := *counts
				coldApp, coldEntries := entryTestApp(t, app.Records.DB, func(context.Context, []consultations.FamilyIssue) ([]consultations.FamilyJob, error) {
					return []consultations.FamilyJob{{Project: "owner/app", Number: 7, Status: "merged"}}, nil
				}, nil)
				_ = coldApp
				for _, pass := range []string{"cold", "warm"} {
					*counts = screenCounts{}
					got, err := coldEntries["draft"](ctx, map[string]any{"id": "parent"}, "")
					if err != nil || !reflect.DeepEqual(want, got) {
						t.Fatalf("%s differs: %v", pass, err)
					}
					t.Logf("unrelated=%d body=%d %s: queries=%d rows=%d bytes=%d legacy: queries=%d rows=%d bytes=%d", n, size, pass, counts.calls, counts.rows, counts.bytes, legacy.calls, legacy.rows, legacy.bytes)
					if counts.bytes > 10000+n*200 {
						t.Fatalf("unexpected transfer: %+v", counts)
					}
				}
			})
		}
	}
	// Duplicate body IDs must retain the first full record in legacy ordering.
	if _, err := env.DB.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES (?,?)", "duplicate", `{"id":"parent","title":"duplicate","splitChildren":[]}`); err != nil {
		t.Fatal(err)
	}
	compare("parent")
	if _, err := env.DB.ExecContext(ctx, "DELETE FROM drafts WHERE id = ?", "duplicate"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.DB.ExecContext(ctx, "UPDATE drafts SET json = ? WHERE id = ?", `{"id":"child","status":"closed"}`, "child"); err != nil {
		t.Fatal(err)
	}
	compare("parent")
	if _, err := env.DB.ExecContext(ctx, "DELETE FROM drafts WHERE id = ?", "child"); err != nil {
		t.Fatal(err)
	}
	compare("parent")
}

// A connector fault affects the same database operation in the legacy and new paths.
type entryFault struct {
	mode string
	gets int
}
type entryFaultConnector struct {
	base  driver.Connector
	fault *entryFault
}

func (c entryFaultConnector) Driver() driver.Driver { return c.base.Driver() }
func (c entryFaultConnector) Connect(ctx context.Context) (driver.Conn, error) {
	v, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &entryFaultConn{PostgresConn: v.(appsql.PostgresConn), fault: c.fault}, nil
}

type entryFaultConn struct {
	appsql.PostgresConn
	fault *entryFault
}

func (c *entryFaultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if c.fault.mode == "22P05" && strings.Contains(q, "json_typeof(json::json)") {
		return nil, &pgconn.PgError{Code: "22P05", Message: "injected JSON conversion error"}
	}
	if strings.HasPrefix(q, "SELECT json FROM drafts WHERE id=") {
		c.fault.gets++
		if c.fault.mode == "get" && c.fault.gets == 1 {
			return nil, errors.New("injected get failure")
		}
		if c.fault.mode == "second-get" && c.fault.gets == 2 {
			return nil, errors.New("injected reread failure")
		}
	}
	if c.fault.mode == "list" && (strings.Contains(q, "xmin::text FROM drafts") || strings.HasPrefix(q, "SELECT json FROM drafts ORDER BY")) {
		return nil, errors.New("injected list failure")
	}
	if c.fault.mode == "no-candidate" && strings.Contains(q, "xmin::text FROM drafts") {
		q = strings.Replace(q, " FROM drafts ORDER BY ", " FROM drafts WHERE false ORDER BY ", 1)
	}
	if c.fault.mode == "no-candidate" && strings.HasPrefix(q, "SELECT json FROM drafts ORDER BY") {
		q = strings.Replace(q, " FROM drafts ORDER BY ", " FROM drafts WHERE false ORDER BY ", 1)
	}
	return c.PostgresConn.QueryContext(ctx, q, args)
}

func TestDraftEntryJSONCompatibility(t *testing.T) {
	env, app, counts, _, entries := screenFixture(t)
	ctx := context.Background()
	parent := `{"id":"parent","project":"owner/app","status":"draft","splitChildren":["child"],"body":"parent body"}`
	child := `{"id":"child","project":"owner/app","parentSplit":"parent","issue":{"number":7},"status":"submitted"}`
	for _, row := range [][2]string{{"parent", parent}, {"child", child}} {
		if _, err := env.DB.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES (?,?)", row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	sentinel, _ := json.Marshal(map[string]any{"id": "sentinel", "body": strings.Repeat("s", 8192)})
	if _, err := env.DB.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES (?,?)", "sentinel", string(sentinel)); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, key, raw string }{
		{"missing-id", "extra", `{"body":"missing"}`},
		{"null-id", "extra", `{"id":null,"status":null}`},
		{"numeric-id", "extra", `{"id":12,"project":1e3}`},
		{"wrong-types", "extra", `{"id":"extra","parentSplit":false,"splitChildren":42,"issue":false}`},
		{"numeric-values", "extra", `{"id":"extra","project":1e3,"issue":{"number":9007199254740993}}`},
		{"key-mismatch", "extra", `{"id":"other","body":"different"}`},
		{"duplicate-id", "extra", `{"id":"parent","title":"duplicate","splitChildren":[]}`},
		{"duplicate-json-key", "extra", `{"id":"wrong","id":"extra","status":"draft"}`},
		{"nul-row", "extra", `{"id":"extra","body":"` + strings.Repeat("n", 8192) + `\u0000body"}`},
		{"conversion-fallback", "extra", `{"id":"extra","body":"` + strings.Repeat("f", 8192) + `","title":"\ud800"}`},
		{"invalid-json", "extra", `{"id":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := env.DB.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES (?,?)", tc.key, tc.raw); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := env.DB.ExecContext(ctx, "DELETE FROM drafts WHERE id=?", tc.key); err != nil {
					t.Fatal(err)
				}
			}()
			if tc.name == "conversion-fallback" || tc.name == "invalid-json" {
				var ignored string
				err := env.DB.QueryRowContext(ctx, "SELECT json::jsonb FROM drafts WHERE id=?", tc.key).Scan(&ignored)
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || (pgErr.Code != "22P02" && pgErr.Code != "22P05") {
					t.Fatalf("unexpected PostgreSQL JSON conversion error: %v", err)
				}
				t.Logf("conversion SQLSTATE=%s", pgErr.Code)
			}
			want := legacyDraftEntry(t, app, "parent")
			*counts = screenCounts{}
			got, err := entries["draft"](ctx, map[string]any{"id": "parent"}, "")
			if err != nil || !reflect.DeepEqual(want, got) {
				t.Fatalf("legacy=%#v got=%#v err=%v", want, got, err)
			}
			raw, _ := json.Marshal(got)
			if strings.Contains(string(raw), "_screenDatabaseKey") {
				t.Fatal("internal key leaked")
			}
			t.Logf("%s queries=%d rows=%d bytes=%d", tc.name, counts.calls, counts.rows, counts.bytes)
			if tc.name == "nul-row" && counts.bytes < 8192 {
				t.Fatal("NUL row was not read in full")
			}
			if tc.name == "conversion-fallback" && counts.bytes < 16384 {
				t.Fatal("conversion fallback did not read full unrelated rows")
			}
		})
	}
}

func TestDraftEntryFailures(t *testing.T) {
	env, _, _, _, _ := screenFixture(t)
	ctx := context.Background()
	for _, row := range [][2]string{{"parent", `{"id":"parent","project":"owner/app","splitChildren":["child"]}`}, {"child", `{"id":"child","parentSplit":"parent","project":"owner/app","issue":{"number":7},"status":"submitted"}`}} {
		if _, err := env.DB.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES (?,?)", row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	fault := &entryFault{}
	db := sql.OpenDB(entryFaultConnector{base: appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), fault: fault})
	defer db.Close()
	for _, mode := range []string{"list", "no-candidate", "jobs", "issues", "get", "second-get", "missing", "22P05"} {
		t.Run(mode, func(t *testing.T) {
			fault.mode = mode
			fault.gets = 0
			jobs := func(context.Context, []consultations.FamilyIssue) ([]consultations.FamilyJob, error) {
				if mode == "jobs" {
					return nil, errors.New("jobs unavailable")
				}
				return nil, nil
			}
			issues := func(context.Context, string) (map[string]string, error) {
				if mode == "issues" {
					return nil, errors.New("issues unavailable")
				}
				return map[string]string{"owner/app#7": "open"}, nil
			}
			app, entries := entryTestApp(t, db, jobs, issues)
			id := "parent"
			if mode == "missing" {
				id = "absent"
			}
			if mode == "get" {
				if got, err := entries["draft"](ctx, map[string]any{"id": id}, ""); err == nil || got != nil {
					t.Fatalf("get failure: %v %v", got, err)
				}
				return
			}
			want := legacyDraftEntry(t, app, id)
			fault.gets = 0
			got, err := entries["draft"](ctx, map[string]any{"id": id}, "")
			if err != nil || !reflect.DeepEqual(want, got) {
				t.Fatalf("mode=%s legacy=%#v got=%#v err=%v", mode, want, got, err)
			}
			if mode == "list" || mode == "no-candidate" || mode == "jobs" {
				if _, present := got["familyProgress"]; present {
					t.Fatalf("%s fabricated progress: %#v", mode, got["familyProgress"])
				}
			}
			if mode == "issues" {
				progress, ok := got["familyProgress"].(consultations.Record)
				if !ok || progress["unknown"] != 1 {
					t.Fatalf("issue failure did not remain unknown: %#v", got["familyProgress"])
				}
			}
		})
	}
}

func TestDraftEntryAuthorizationAndTodo(t *testing.T) {
	env, app, _, _, entries := screenFixture(t)
	ctx := context.Background()
	const parent = "00000000-0000-4000-8000-000000000001"
	const denied = "00000000-0000-4000-8000-000000000002"
	const todo = "00000000-0000-4000-8000-000000000003"
	const missing = "00000000-0000-4000-8000-000000000004"
	for _, row := range []struct{ table, id, raw string }{
		{"drafts", parent, `{"id":"` + parent + `","project":"owner/app","splitChildren":[]}`},
		{"drafts", denied, `{"id":"` + denied + `","project":"other/app","splitChildren":[]}`},
		{"todos", todo, `{"id":"` + todo + `","project":"owner/app","status":"draft"}`},
	} {
		if _, err := env.DB.ExecContext(ctx, "INSERT INTO "+row.table+"(id,json) VALUES (?,?)", row.id, row.raw); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.OpenApplicationPostgres(ctx, env.DSN, env.Schema)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	if err := db.CreateSession(ctx, auth.Hash("screen"), auth.Session{CSRF: "csrf", ExpiresAt: now.Add(time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	authenticator, err := auth.New(auth.Config{PublicURL: "http://127.0.0.1:8787"}, &store.AuthStore{SQLite: db})
	if err != nil {
		t.Fatal(err)
	}
	handler := func(readers map[string]screenapi.EntryReader) http.Handler {
		mux := http.NewServeMux()
		_, err := screenapi.Register(mux, screenapi.Options{Auth: authenticator, AppDB: db.DB(), Config: func() (screenapi.Object, error) {
			return screenapi.Object{"repositories": []any{screenapi.Object{"name": "owner/app"}}}, nil
		}, Entries: readers})
		if err != nil {
			t.Fatal(err)
		}
		return mux
	}
	old := map[string]screenapi.EntryReader{"draft": func(_ context.Context, id map[string]any, _ string) (map[string]any, error) {
		return legacyDraftEntry(t, app, id["id"].(string)), nil
	}, "todo": entries["todo"]}
	newer, older := handler(entries), handler(old)
	for _, tc := range []struct {
		name, kind, id string
		cookie         bool
		status         int
	}{{"parent", "draft", parent, true, 200}, {"denied", "draft", denied, true, 403}, {"missing", "draft", missing, true, 404}, {"todo", "todo", todo, true, 200}, {"unauthenticated", "draft", parent, false, 401}} {
		t.Run(tc.name, func(t *testing.T) {
			call := func(h http.Handler) *httptest.ResponseRecorder {
				r := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/list/entry?type="+tc.kind+"&id="+tc.id, nil)
				if tc.cookie {
					r.AddCookie(&http.Cookie{Name: "desk_session", Value: "screen"})
					r.Header.Set("Origin", "http://127.0.0.1:8787")
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			a, b := call(older), call(newer)
			if a.Code != tc.status || b.Code != tc.status || a.Body.String() != b.Body.String() {
				t.Fatalf("legacy=%d %s new=%d %s", a.Code, a.Body, b.Code, b.Body)
			}
		})
	}
}

type entryReadTotals struct{ queries, rows, bytes int }
type entryReadBreakdown struct{ totals map[string]*entryReadTotals }
type entryMeasureConnector struct {
	base  driver.Connector
	stats *entryReadBreakdown
}

func (c entryMeasureConnector) Driver() driver.Driver { return c.base.Driver() }
func (c entryMeasureConnector) Connect(ctx context.Context) (driver.Conn, error) {
	v, e := c.base.Connect(ctx)
	if e != nil {
		return nil, e
	}
	return &entryMeasureConn{PostgresConn: v.(appsql.PostgresConn), stats: c.stats}, nil
}

type entryMeasureConn struct {
	appsql.PostgresConn
	stats *entryReadBreakdown
}

func (c *entryMeasureConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	kind := "other"
	if strings.Contains(q, "xmin::text FROM drafts") {
		kind = "versions"
	} else if strings.Contains(q, "json_typeof(json::json)") {
		kind = "projection"
	} else if strings.HasPrefix(q, "SELECT json FROM drafts WHERE id=") {
		kind = "family-or-action"
		if len(args) > 0 && args[0].Value == "parent" {
			kind = "parent"
		}
	}
	stat := c.stats.totals[kind]
	if stat == nil {
		stat = &entryReadTotals{}
		c.stats.totals[kind] = stat
	}
	stat.queries++
	rows, err := c.PostgresConn.QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	return &entryMeasureRows{Rows: rows, stat: stat}, nil
}

type entryMeasureRows struct {
	driver.Rows
	stat *entryReadTotals
}

func (r *entryMeasureRows) Next(values []driver.Value) error {
	err := r.Rows.Next(values)
	if err == nil {
		r.stat.rows++
		for _, v := range values {
			switch x := v.(type) {
			case string:
				r.stat.bytes += len(x)
			case []byte:
				r.stat.bytes += len(x)
			}
		}
	}
	return err
}

func TestDraftEntryReadBreakdown(t *testing.T) {
	env, _, _, _, _ := screenFixture(t)
	ctx := context.Background()
	for _, row := range [][2]string{{"parent", `{"id":"parent","project":"owner/app","splitChildren":["child"]}`}, {"child", `{"id":"child","project":"owner/app","parentSplit":"parent","issue":{"number":7}}`}, {"unrelated", `{"id":"unrelated","body":"unrelated body","messages":[{"text":"private"}],"attachments":[{"data":"private"}]}`}} {
		if _, err := env.DB.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES (?,?)", row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	stats := &entryReadBreakdown{totals: map[string]*entryReadTotals{}}
	db := sql.OpenDB(entryMeasureConnector{base: appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), stats: stats})
	defer db.Close()
	_, entries := entryTestApp(t, db, func(context.Context, []consultations.FamilyIssue) ([]consultations.FamilyJob, error) { return nil, nil }, nil)
	for _, pass := range []string{"cold", "warm"} {
		stats.totals = map[string]*entryReadTotals{}
		if _, err := entries["draft"](ctx, map[string]any{"id": "parent"}, ""); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"parent", "family-or-action", "versions", "projection", "other"} {
			if v := stats.totals[kind]; v != nil {
				t.Logf("%s %s queries=%d rows=%d bytes=%d", pass, kind, v.queries, v.rows, v.bytes)
			}
		}
		if stats.totals["parent"] == nil || stats.totals["versions"] == nil {
			t.Fatal("missing target or version scan")
		}
	}
}
