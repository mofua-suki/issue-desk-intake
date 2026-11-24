package startup

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/store"
	"log"
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
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/screenapi"
)

type screenCounts struct {
	calls, rows, bytes int
	external           time.Duration
	omitFinished       bool
	mutated            int
}
type screenConnector struct {
	base   driver.Connector
	counts *screenCounts
}

func (c screenConnector) Driver() driver.Driver { return c.base.Driver() }
func (c screenConnector) Connect(ctx context.Context) (driver.Conn, error) {
	v, e := c.base.Connect(ctx)
	if e != nil {
		return nil, e
	}
	return &screenConn{PostgresConn: v.(appsql.PostgresConn), counts: c.counts}, nil
}

type screenConn struct {
	appsql.PostgresConn
	counts *screenCounts
}

func (c *screenConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.counts.calls++
	if c.counts.omitFinished && strings.Contains(q, "'finishedWithoutIssue'") {
		c.counts.mutated++
		q = strings.Replace(q, "'finishedWithoutIssue',", "", 1)
	}
	r, e := c.PostgresConn.QueryContext(ctx, q, args)
	if e != nil {
		return nil, e
	}
	return &screenRows{Rows: r, counts: c.counts}, nil
}

type screenRows struct {
	driver.Rows
	counts *screenCounts
}

func (r *screenRows) Next(v []driver.Value) error {
	e := r.Rows.Next(v)
	if e == nil {
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
	return e
}

func screenFixture(t *testing.T) (pgtest.Env, *App, *screenCounts, map[string]screenapi.ListReader, map[string]screenapi.EntryReader) {
	t.Helper()
	env := pgtest.NewIsolated(t)
	ctx := context.Background()
	if _, e := consultations.OpenRecords(ctx, env.DB); e != nil {
		t.Fatal(e)
	}
	cfg, e := pgx.ParseConfig(env.DSN)
	if e != nil {
		t.Fatal(e)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	counts := &screenCounts{}
	db := sql.OpenDB(screenConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), counts})
	t.Cleanup(func() { db.Close() })
	records := &consultations.Records{DB: db}
	service, e := consultations.New(consultations.Options{Records: records, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }, FamilyJobs: func(context.Context, []consultations.FamilyIssue) ([]consultations.FamilyJob, error) {
		start := time.Now()
		defer func() { counts.external += time.Since(start) }()
		return []consultations.FamilyJob{{Project: "owner/app", Number: 7, Status: "merged"}}, nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	app := &App{Records: records, Consultations: service}
	lists := map[string]screenapi.ListReader{}
	entries := map[string]screenapi.EntryReader{}
	app.registerConsultationReaders(lists, entries)
	return env, app, counts, lists, entries
}
func summaries(items []map[string]any) []screenapi.Object {
	out := make([]screenapi.Object, len(items))
	for i, v := range items {
		out[i] = screenapi.Summarize("draft", screenapi.Object(v))
	}
	return out
}
func fullScreen(ctx context.Context, app *App) ([]map[string]any, error) {
	records, e := app.Records.List(ctx, "drafts")
	if e != nil {
		return nil, e
	}
	records = app.Consultations.FamilyProgress(ctx, records)
	out := make([]map[string]any, 0, len(records))
	for _, r := range records {
		out = append(out, map[string]any(app.Consultations.DraftView(r, auth.Principal{Kind: auth.Screen}, "")))
	}
	return out, nil
}

func TestScreenDraftReaders(t *testing.T) {
	env, app, counts, lists, entries := screenFixture(t)
	ctx := context.Background()
	data := []string{`{"id":"00000000-0000-4000-8000-000000000001","project":"owner/app","status":"submit_error","ready":true,"splitChildren":["00000000-0000-4000-8000-000000000002","00000000-0000-4000-8000-000000000005","00000000-0000-4000-8000-000000000006","00000000-0000-4000-8000-000000000007"],"splitIssues":[{"project":"owner/app"},{},{},{}],"body":"<!-- issue-desk:followup -->","messages":[{"role":"proxy"},{"role":"user","text":"hello","images":[{"id":"00000000-0000-0000-0000-000000000000"}]}]}`, `{"id":"00000000-0000-4000-8000-000000000002","project":"owner/app","status":"submitted","parentSplit":"00000000-0000-4000-8000-000000000001","issue":{"number":7}}`, `{"id":"00000000-0000-4000-8000-000000000003"}`, `{"id":"00000000-0000-4000-8000-000000000005","status":"submitted","parentSplit":"00000000-0000-4000-8000-000000000001","finishedWithoutIssue":true}`, `{"id":"00000000-0000-4000-8000-000000000006","status":"draft","parentSplit":"00000000-0000-4000-8000-000000000001","attempted":true}`, `{"id":"00000000-0000-4000-8000-000000000007","status":"draft","parentSplit":"00000000-0000-4000-8000-000000000001","investigations":[{"status":"done"}]}`, `{"id":"00000000-0000-4000-8000-000000000008","parentSplit":"00000000-0000-4000-8000-000000000099"}`, `{"id":"00000000-0000-4000-8000-000000000009","splitChildren":["00000000-0000-4000-8000-000000000098"]}`, `{"id":"00000000-0000-4000-8000-000000000004","project":17,"status":null,"messages":false,"proposal":5,"questions":null,"consultationBudget":{"stopped":true}}`}
	data = append(data,
		`{"id":"family-parent","status":"submit_error","splitChildren":["family-filed","family-finished"],"splitIssues":[{},{}]}`,
		`{"id":"family-filed","status":"submitted","parentSplit":"family-parent","issue":{"number":7}}`,
		`{"id":"family-finished","status":"submitted","parentSplit":"family-parent","finishedWithoutIssue":true}`,
	)
	for _, raw := range data {
		var v map[string]any
		if e := json.Unmarshal([]byte(raw), &v); e != nil {
			t.Fatal(e)
		}
		if _, e := env.DB.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES (?,?)", v["id"], raw); e != nil {
			t.Fatal(e)
		}
	}
	compare := func() {
		t.Helper()
		full, e := fullScreen(ctx, app)
		if e != nil {
			t.Fatal(e)
		}
		got, e := lists["draft"](ctx, "")
		if e != nil {
			t.Fatal(e)
		}
		if !reflect.DeepEqual(summaries(full), summaries(got)) {
			t.Fatalf("full=%#v projected=%#v", summaries(full), summaries(got))
		}
		for i, row := range got {
			entry, e := entries["draft"](ctx, map[string]any{"id": row["id"]}, "")
			if e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(summaries([]map[string]any{entry})[0], summaries(got)[i]) {
				t.Fatalf("entry %v differs", row["id"])
			}
		}
	}
	for _, purpose := range []consultations.DraftReadPurpose{consultations.DraftReadPage, consultations.DraftReadAlerts, consultations.DraftReadScreen, consultations.DraftReadRecovery, consultations.DraftReadFiling} {
		fields, err := app.Records.ListDraftFields(ctx, purpose)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range fields {
			if _, ok := field["_screenDatabaseKey"]; ok {
				t.Fatalf("internal key leaked for %s", purpose)
			}
		}
	}
	compare()
	// Mutate the actual screen projection query while keeping the full read
	// unchanged: the family-action summary must detect the missing field.
	fullBefore, e := fullScreen(ctx, app)
	if e != nil {
		t.Fatal(e)
	}
	app.Records = &consultations.Records{DB: app.Records.DB} // Start with an empty projection cache.
	counts.omitFinished = true
	missing, e := lists["draft"](ctx, "")
	counts.omitFinished = false
	if counts.mutated == 0 {
		t.Fatal("projection query not mutated")
	}
	app.Records = &consultations.Records{DB: app.Records.DB}
	if e != nil {
		t.Fatal(e)
	}
	if reflect.DeepEqual(summaries(fullBefore), summaries(missing)) {
		t.Fatal("screen comparison did not detect missing finishedWithoutIssue")
	}
	compare() // Repopulate the cache using the unmodified production projection.
	*counts = screenCounts{}
	if _, e := lists["draft"](ctx, ""); e != nil {
		t.Fatal(e)
	}
	if counts.calls != 1 || counts.rows != len(data) || counts.bytes > 60*len(data) {
		t.Fatalf("warm transfer %+v", counts)
	}
	if _, e := env.DB.ExecContext(ctx, `UPDATE drafts SET json='{"id":"00000000-0000-4000-8000-000000000002","status":"closed"}' WHERE id='00000000-0000-4000-8000-000000000002'`); e != nil {
		t.Fatal(e)
	}
	if _, e := env.DB.ExecContext(ctx, `DELETE FROM drafts WHERE id='00000000-0000-4000-8000-000000000004'`); e != nil {
		t.Fatal(e)
	}
	compare()
	if _, e := env.DB.ExecContext(ctx, `INSERT INTO todos(id,json) VALUES ('todo','{"id":"todo","status":"draft"}')`); e != nil {
		t.Fatal(e)
	}
	todos, e := lists["todo"](ctx, "")
	if e != nil || len(todos) != 1 {
		t.Fatalf("todos=%v error=%v", todos, e)
	}
	// A legacy row whose stored key and JSON id differ must use the original
	// database-backed family checks instead of guessing the relationship.
	if _, e := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES ('legacy-key','{"id":"legacy-id","splitChildren":["00000000-0000-4000-8000-000000000002"]}')`); e != nil {
		t.Fatal(e)
	}
	if _, e := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES ('legacy-key-2','{"id":"legacy-id-2"}')`); e != nil {
		t.Fatal(e)
	}
	for i := 3; i <= 7; i++ {
		key := fmt.Sprintf("legacy-key-%d", i)
		if _, e := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES (?,?)`, key, fmt.Sprintf(`{"id":"legacy-id-%d"}`, i)); e != nil {
			t.Fatal(e)
		}
	}
	full, e := fullScreen(ctx, app)
	if e != nil {
		t.Fatal(e)
	}
	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	projected, e := lists["draft"](ctx, "")
	_, repeatErr := lists["draft"](ctx, "")
	log.SetOutput(oldWriter)
	if repeatErr != nil {
		t.Fatal(repeatErr)
	}
	if strings.Count(logs.String(), "draft screen snapshot identifiers invalid") != 1 || !strings.Contains(logs.String(), "invalid rows=7") {
		t.Fatal("fallback was not logged")
	}
	// Rows are read newest first; only the first five database keys are logged.
	for _, key := range []string{"legacy-key-7", "legacy-key-6", "legacy-key-5", "legacy-key-4", "legacy-key-3"} {
		if !strings.Contains(logs.String(), `"`+key+`"`) {
			t.Fatalf("missing DB key %s: %s", key, logs.String())
		}
	}
	for _, key := range []string{"legacy-key-2", "legacy-key"} {
		if strings.Contains(logs.String(), `"`+key+`"`) {
			t.Fatalf("too many DB keys: %s", logs.String())
		}
	}
	if e != nil || !reflect.DeepEqual(summaries(full), summaries(projected)) {
		t.Fatalf("legacy key mismatch: %v", e)
	}
	if e := app.Records.DB.Close(); e != nil {
		t.Fatal(e)
	}
	if value, e := lists["draft"](ctx, ""); e == nil {
		t.Fatalf("failed read returned success: %v", value)
	}
}

// screenListHandler exercises the actual HTTP list handler with the startup draft reader.
func screenListHandler(t *testing.T, env pgtest.Env, lists map[string]screenapi.ListReader, aux ...map[string]func(context.Context) (any, error)) http.Handler {
	t.Helper()
	return screenListHandlerFor(t, env, []string{"owner/app"}, lists, aux...)
}

// screenListHandlerFor serves the screen list with the given candidate repositories.
func screenListHandlerFor(t *testing.T, env pgtest.Env, repos []string, lists map[string]screenapi.ListReader, aux ...map[string]func(context.Context) (any, error)) http.Handler {
	t.Helper()
	db, a := screenSession(t, env)
	return screenListMux(t, db, a, repos, lists, aux...)
}

// screenSession opens the application store and signs in the screen session
// (cookie desk_session=screen) that the HTTP fixtures send.
func screenSession(t *testing.T, env pgtest.Env) (*store.SQLite, *auth.Authenticator) {
	t.Helper()
	db, err := store.OpenApplicationPostgres(context.Background(), env.DSN, env.Schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now()
	if err = db.CreateSession(context.Background(), auth.Hash("screen"), auth.Session{CSRF: "csrf", ExpiresAt: now.Add(time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(auth.Config{PublicURL: "http://127.0.0.1:8787"}, &store.AuthStore{SQLite: db})
	if err != nil {
		t.Fatal(err)
	}
	return db, a
}

// screenListMux serves the screen list for an already signed-in session.
func screenListMux(t *testing.T, db *store.SQLite, a *auth.Authenticator, repos []string, lists map[string]screenapi.ListReader, aux ...map[string]func(context.Context) (any, error)) http.Handler {
	t.Helper()
	for _, kind := range []string{"issue", "closeRequest"} {
		if lists[kind] == nil {
			lists[kind] = func(context.Context, string) ([]map[string]any, error) { return nil, nil }
		}
	}
	mux := http.NewServeMux()
	var auxiliary map[string]func(context.Context) (any, error)
	if len(aux) > 0 {
		auxiliary = aux[0]
	}
	_, err := screenapi.Register(mux, screenapi.Options{Aux: auxiliary, Auth: a, AppDB: db.DB(), Config: func() (screenapi.Object, error) {
		configured := []any{}
		for _, repo := range repos {
			configured = append(configured, screenapi.Object{"name": repo})
		}
		return screenapi.Object{"repositories": configured}, nil
	}, Meta: func(context.Context) (screenapi.Object, error) { return screenapi.Object{}, nil }, Lists: lists})
	if err != nil {
		t.Fatal(err)
	}
	return mux
}
func measureScreenList(t *testing.T, handler http.Handler) (time.Duration, int) {
	t.Helper()
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/list", nil)
	r.AddCookie(&http.Cookie{Name: "desk_session", Value: "screen"})
	r.Header.Set("Origin", "http://127.0.0.1:8787")
	w := httptest.NewRecorder()
	start := time.Now()
	handler.ServeHTTP(w, r)
	elapsed := time.Since(start)
	if w.Code != http.StatusOK {
		t.Fatalf("list: status %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Rows []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Rows) == 0 {
		t.Fatalf("list response rows=%d error=%v", len(body.Rows), err)
	}
	return elapsed, w.Body.Len()
}
func TestScreenListHTTPMeasurement(t *testing.T) {
	env, _, _, lists, _ := screenFixture(t)
	if _, err := env.DB.ExecContext(context.Background(), `INSERT INTO drafts(id,json) VALUES ('00000000-0000-4000-8000-000000000001','{"id":"00000000-0000-4000-8000-000000000001","project":"owner/app"}')`); err != nil {
		t.Fatal(err)
	}
	elapsed, size := measureScreenList(t, screenListHandler(t, env, lists))
	if size <= 0 {
		t.Fatal("empty HTTP response")
	}
	t.Logf("api_list_elapsed=%s response_bytes=%d", elapsed, size)
}
