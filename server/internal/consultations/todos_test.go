package consultations

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
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

const todoID = "11111111-1111-4111-8111-111111111111"
const sendID = "22222222-2222-4222-8222-222222222222"
const photoID = "33333333-3333-4333-8333-333333333333"

func fixture(t *testing.T) (*Service, *store.SQLite, *atomic.Int32) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	sqlite := openPostgresStore(t)
	records, err := OpenRecords(ctx, sqlite.DB())
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(auth.Config{Port: 8787, Now: func() time.Time { return now }}, &store.AuthStore{SQLite: sqlite})
	if err != nil {
		t.Fatal(err)
	}
	count := &atomic.Int32{}
	s, err := New(Options{Records: records, Auth: a, Now: func() time.Time { return now }, Project: func(project string) error {
		if project != "owner/app" {
			return refuse(400, "許可されていないプロジェクトです。")
		}
		return nil
	}, Start: func(ctx context.Context, id string, draft Record, p auth.Principal) (Record, error) {
		count.Add(1)
		return draft, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s, sqlite, count
}

func TestTodoProjectionMatchesFullRead(t *testing.T) {
	s, db, _ := fixture(t)
	ctx := context.Background()
	inputs := []string{
		`{"id":12,"project":"owner/app","owner":"token:a","extra":12345678901234567890}`,
		`{"project":null,"owner":12,"text":"large body"}`,
		`{"project":"owner/app","owner":"\u0000","extra":"\ud800"}`,
		`{"project":"owner/app","extra":1}`,
	}
	for i, raw := range inputs {
		if _, err := db.DB().ExecContext(ctx, bindSQL("INSERT INTO todos(id,json) VALUES(?,?)"), fmt.Sprint(i), raw); err != nil {
			t.Fatal(err)
		}
	}
	full, err := s.options.Records.List(ctx, "todos")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := s.options.Records.ListTodoEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(full) {
		t.Fatalf("entries %d full %d", len(entries), len(full))
	}
	for i, entry := range entries {
		for _, key := range []string{"project", "owner"} {
			if !reflect.DeepEqual(entry.Record[key], full[i][key]) {
				t.Fatalf("%s: %v != %v", key, entry.Record, full[i])
			}
		}
	}
	selected, err := s.options.Records.GetTodos(ctx, []string{entries[0].Key, entries[1].Key})
	if err != nil || !reflect.DeepEqual(selected, full[:2]) {
		t.Fatalf("selected=%v err=%v", selected, err)
	}
	if _, err := db.DB().ExecContext(ctx, bindSQL("DELETE FROM todos WHERE id=?"), entries[0].Key); err != nil {
		t.Fatal(err)
	}
	selected, err = s.options.Records.GetTodos(ctx, []string{entries[0].Key, entries[1].Key})
	if err != nil || !reflect.DeepEqual(selected, full[1:2]) {
		t.Fatalf("after deletion=%v err=%v", selected, err)
	}
	if _, err := db.DB().ExecContext(ctx, bindSQL("INSERT INTO todos(id,json) VALUES(?,?)"), "trailing", `{"project":"owner/app"} trailing`); err != nil {
		t.Fatal(err)
	}
	full, err = s.options.Records.List(ctx, "todos")
	if err != nil {
		t.Fatal(err)
	}
	entries, err = s.options.Records.ListTodoEntries(ctx)
	if err != nil || len(entries) != len(full) {
		t.Fatalf("fallback=%v err=%v", entries, err)
	}
	for i := range entries {
		if !reflect.DeepEqual(entries[i].Record, full[i]) {
			t.Fatalf("fallback mismatch: %v != %v", entries[i].Record, full[i])
		}
	}
}

func TestTodoAccessOwner(t *testing.T) {
	s, _, _ := fixture(t)
	p := auth.Principal{Kind: auth.PC, TokenID: "one"}
	row := Record{"project": "owner/app", "owner": "token:one"}
	if err := s.todoAccess(row, p); err != nil {
		t.Fatal(err)
	}
	row["owner"] = "token:other"
	if err := s.todoAccess(row, p); err == nil {
		t.Fatal("another owner allowed")
	}
	p.Delegated = true
	if err := s.todoAccess(row, p); err != nil {
		t.Fatal(err)
	}
	row["project"] = "other/app"
	if err := s.todoAccess(row, p); err == nil {
		t.Fatal("disallowed project allowed")
	}
}

func TestTodoSaveRevisionAndSendIDAndUnknownFields(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	todo, err := s.CreateTodo(ctx, Record{"id": todoID, "project": "owner/app"}, p)
	if err != nil {
		t.Fatal(err)
	}
	if revision, _ := integer(todo["revision"]); revision != 0 {
		t.Fatal("initial revision")
	}
	_, err = s.options.Records.Change(ctx, "todos", todoID, func(_ *sql.Tx, r Record) (Record, error) { r["futureField"] = Record{"keep": true}; return r, nil })
	if err != nil {
		t.Fatal(err)
	}
	input := Record{"id": sendID, "revision": 0, "text": "保存する内容"}
	saved, err := s.SaveTodo(ctx, todoID, input, p)
	if err != nil {
		t.Fatal(err)
	}
	if revision, _ := integer(saved["revision"]); revision != 1 || saved["futureField"] == nil {
		t.Fatalf("saved record: %v", saved)
	}
	if saved["dueAt"] != "2026-11-08T03:00:00.000Z" {
		t.Fatalf("default due date: %v", saved["dueAt"])
	}
	replayed, err := s.SaveTodo(ctx, todoID, input, p)
	if err != nil {
		t.Fatal(err)
	}
	if revision, _ := integer(replayed["revision"]); revision != 1 {
		t.Fatal("replay advanced revision")
	}
	input["text"] = "別の内容"
	if _, err := s.SaveTodo(ctx, todoID, input, p); problem(err).Status != 409 {
		t.Fatalf("send ID conflict: %v", err)
	}
	input["id"] = "44444444-4444-4444-8444-444444444444"
	if _, err := s.SaveTodo(ctx, todoID, input, p); problem(err).Status != 409 {
		t.Fatalf("revision conflict: %v", err)
	}
}

func TestPCTodoOwnershipAndSavedByDoNotTransferOwner(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	ownerPC := auth.Principal{Kind: auth.PC, TokenID: "owner-token", Name: "pc-one"}
	other := auth.Principal{Kind: auth.PC, TokenID: "other-token", Name: "pc-two"}
	if _, err := s.CreateTodo(ctx, Record{"id": todoID, "project": "owner/app"}, ownerPC); err != nil {
		t.Fatal(err)
	}
	input := Record{"id": sendID, "revision": 0, "text": "内容"}
	if _, err := s.SaveTodo(ctx, todoID, input, other); problem(err).Status != 404 {
		t.Fatalf("other owner exposed: %v", err)
	}
	other.Delegated = true
	saved, err := s.SaveTodo(ctx, todoID, input, other)
	if err != nil {
		t.Fatal(err)
	}
	if saved["owner"] != "token:owner-token" || saved["savedBy"] != "pc-two" {
		t.Fatalf("owner changed: %v", saved)
	}
}

func TestTodoStartMovesImagesAndBodyAtomically(t *testing.T) {
	s, sqlite, count := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateTodo(ctx, Record{"id": todoID, "project": "owner/app"}, p); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"` + photoID + `","messageId":"before","mime":"image/png","width":1,"height":1}`
	if _, err := sqlite.DB().ExecContext(ctx, "INSERT INTO images(id,scope,message,json,bytes) VALUES(?,?,?,?,?)", photoID, "todo:"+todoID, "before", meta, []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveTodo(ctx, todoID, Record{"revision": 0, "text": "画像付きの相談", "images": []string{photoID}}, p); err != nil {
		t.Fatal(err)
	}
	draft, err := s.StartTodo(ctx, todoID, Record{"tier": "light", "revision": 1, "id": sendID}, p)
	if err != nil {
		t.Fatal(err)
	}
	if count.Load() != 1 || draft["status"] != "thinking" {
		t.Fatalf("start: %v", draft)
	}
	if _, found, err := s.options.Records.Get(ctx, "todos", todoID); err != nil || found {
		t.Fatalf("TODO survived promotion: %v", err)
	}
	stored, found, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil || !found {
		t.Fatalf("draft missing: %v", err)
	}
	raw, _ := json.Marshal(stored["messages"])
	if !strings.Contains(string(raw), "画像付きの相談") {
		t.Fatal("body lost")
	}
	var scope, message string
	var bound int
	if err := sqlite.DB().QueryRowContext(ctx, "SELECT scope,message,bound FROM images WHERE id=?", photoID).Scan(&scope, &message, &bound); err != nil {
		t.Fatal(err)
	}
	if scope != "draft:"+todoID || message != todoID || bound != 1 {
		t.Fatalf("image move: %s %s %d", scope, message, bound)
	}
}

func TestTodoRemovalChecksInstructionAndRevision(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.PC, TokenID: "pc", Name: "pc-one", Delegated: true}
	if _, err := s.CreateTodo(ctx, Record{"id": todoID, "project": "owner/app"}, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveTodo(ctx, todoID, Record{"revision": 0}, p); problem(err).Status != 400 {
		t.Fatalf("missing instruction: %v", err)
	}
	if _, err := s.RemoveTodo(ctx, todoID, Record{"revision": 0, "instructionAt": "2026-10-31T00:00:00Z"}, p); problem(err).Status != 409 {
		t.Fatalf("newer target: %v", err)
	}
	input := Record{"revision": 0, "instructionAt": "2026-11-01T03:00:00Z"}
	if _, err := s.RemoveTodo(ctx, todoID, input, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveTodo(ctx, todoID, input, p); err != nil {
		t.Fatalf("removal replay: %v", err)
	}
}

func TestTodoListFiltersBeforePagination(t *testing.T) {
	s, db, _ := fixture(t)
	token := stopToken(t, db, true)
	for i, project := range []string{"owner/app", "owner/app", "other/app"} {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		row := Record{"id": id, "project": project}
		if _, err := s.options.Records.Change(context.Background(), "todos", id, func(*sql.Tx, Record) (Record, error) { return row, nil }); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	s.Register(mux)
	request := func(query string) (int, map[string]any) {
		req := httptest.NewRequest("GET", "http://localhost:8787/api/todos"+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return w.Code, result
	}
	for _, tc := range []struct {
		query string
		total float64
		more  bool
		count int
	}{
		{"?project=owner%2Fapp&limit=1&page=1", 2, true, 1},
		{"?project=owner%2Fapp&limit=1&page=2", 2, false, 1},
		{"?project=other%2Fapp", 0, false, 0},
		{"?limit=1&page=3", 2, false, 0},
	} {
		code, out := request(tc.query)
		if code != 200 || out["total"] != tc.total || out["hasMore"] != tc.more || len(out["items"].([]any)) != tc.count {
			t.Fatal(code, out)
		}
	}
	for _, q := range []string{"?limit=0", "?limit=201", "?page=0", "?page=x"} {
		if code, _ := request(q); code != 400 {
			t.Fatal(q, code)
		}
	}
}

// The wrapper measures values delivered by the actual PostgreSQL driver, and
// can run a concurrent mutation precisely between selection and full fetch.
type todoReadStats struct {
	calls, projectedCalls, fullCalls, fallbackCalls                                 int
	projectedRows, projectedBytes, fullRows, fullBytes, fallbackRows, fallbackBytes int
	beforeFull                                                                      func()
	fail                                                                            string
}
type todoConnector struct {
	base  driver.Connector
	stats *todoReadStats
}

func (c todoConnector) Driver() driver.Driver { return c.base.Driver() }
func (c todoConnector) Connect(ctx context.Context) (driver.Conn, error) {
	v, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &todoConn{PostgresConn: v.(appsql.PostgresConn), stats: c.stats}, nil
}

type todoConn struct {
	appsql.PostgresConn
	stats *todoReadStats
}

func (c *todoConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	kind := "other"
	switch {
	case strings.HasPrefix(query, "SELECT id, CASE WHEN json_typeof(json::json)") && strings.Contains(query, "FROM todos ORDER BY"):
		kind = "projection"
		c.stats.projectedCalls++
	case strings.HasPrefix(query, "SELECT json FROM todos WHERE id IN"):
		kind = "full"
		c.stats.fullCalls++
		if c.stats.beforeFull != nil {
			f := c.stats.beforeFull
			c.stats.beforeFull = nil
			f()
		}
	case strings.HasPrefix(query, "SELECT id, json FROM todos ORDER BY"):
		kind = "fallback"
		c.stats.fallbackCalls++
	case strings.HasPrefix(query, "SELECT json FROM todos ORDER BY"):
		kind = "full"
		c.stats.fullCalls++
	}
	c.stats.calls++
	if c.stats.fail == kind {
		return nil, errors.New("injected todo read error")
	}
	rows, err := c.PostgresConn.QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return &todoRows{Rows: rows, stats: c.stats, kind: kind}, nil
}

type todoRows struct {
	driver.Rows
	stats *todoReadStats
	kind  string
}

func (r *todoRows) Next(values []driver.Value) error {
	if r.stats.fail == "scan" {
		return errors.New("injected todo scan error")
	}
	err := r.Rows.Next(values)
	if err != nil {
		return err
	}
	n := 0
	for _, value := range values {
		switch v := value.(type) {
		case string:
			n += len(v)
		case []byte:
			n += len(v)
		}
	}
	switch r.kind {
	case "projection":
		r.stats.projectedRows++
		r.stats.projectedBytes += n
	case "full":
		r.stats.fullRows++
		r.stats.fullBytes += n
	case "fallback":
		r.stats.fallbackRows++
		r.stats.fallbackBytes += n
	}
	return nil
}

func todoInstrumented(t *testing.T) (*Service, *store.SQLite, *todoReadStats, *sql.DB) {
	t.Helper()
	env := pgtest.NewIsolated(t)
	if _, err := OpenRecords(context.Background(), env.DB); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	stats := &todoReadStats{}
	readDB := sql.OpenDB(todoConnector{base: appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), stats: stats})
	t.Cleanup(func() { readDB.Close() })
	s, authDB, _ := fixture(t)
	s.options.Records = &Records{DB: readDB}
	return s, authDB, stats, env.DB
}

func todoRequest(s *Service, token, query string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://localhost:8787/api/todos"+query, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux := http.NewServeMux()
	s.Register(mux)
	mux.ServeHTTP(w, r)
	return w
}

// This reproduces the fixed version's List -> filter -> view -> Paginate path.
func todoReference(t *testing.T, s *Service, p auth.Principal, query string) (*httptest.ResponseRecorder, int) {
	t.Helper()
	r := httptest.NewRequest("GET", "http://localhost:8787/api/todos"+query, nil)
	page, failure := contract.ParsePagination(r.URL.Query())
	w := httptest.NewRecorder()
	if failure != nil {
		contract.WriteProblem(w, failure)
		return w, 0
	}
	rows, err := s.options.Records.List(r.Context(), "todos")
	if err != nil {
		contract.WriteProblem(w, problem(err))
		return w, 0
	}
	values := make([]Record, 0, len(rows))
	project := r.URL.Query().Get("project")
	for _, row := range rows {
		if s.todoAccess(row, p) == nil && (project == "" || text(row["project"]) == project) {
			values = append(values, s.todoView(row, p, "http://localhost:8787"))
		}
	}
	respond(w, 200, contract.Paginate(values, page))
	return w, len(values)
}

func TestTodoPageVolumeAndReference(t *testing.T) {
	s, authDB, stats, writeDB := todoInstrumented(t)
	ctx := context.Background()
	token := stopToken(t, authDB, true)
	p := auth.Principal{Kind: auth.PC, Name: "fixture", TokenID: "monitor", Delegated: true}
	// A single INSERT produces 2,000 real persisted rows with 8,000-byte bodies.
	_, err := writeDB.ExecContext(ctx, `INSERT INTO todos(id,json) SELECT i::text, json_build_object('id',CASE WHEN i%3=0 THEN NULL ELSE to_json(i::text) END,'project',CASE WHEN i%4=0 THEN 'other/app' ELSE 'owner/app' END,'owner','token:monitor','body',repeat('x',8000),'future',json_build_object('n',i),'images',json_build_array(json_build_object('id','33333333-3333-4333-8333-333333333333')))::text FROM generate_series(1,2000) AS i`)
	if err != nil {
		t.Fatal(err)
	}
	query := "?project=owner%2Fapp&limit=7&page=2"
	var m1, m2 runtime.MemStats
	runtime.ReadMemStats(&m1)
	started := time.Now()
	old, views := todoReference(t, s, p, query)
	oldTime := time.Since(started)
	runtime.ReadMemStats(&m2)
	oldAlloc := m2.TotalAlloc - m1.TotalAlloc
	oldStats := *stats
	*stats = todoReadStats{}
	projectChecks := 0
	s.options.Project = func(project string) error {
		projectChecks++
		if project != "owner/app" {
			return refuse(400, "denied")
		}
		return nil
	}
	runtime.ReadMemStats(&m1)
	started = time.Now()
	got := todoRequest(s, token, query)
	newTime := time.Since(started)
	runtime.ReadMemStats(&m2)
	newAlloc := m2.TotalAlloc - m1.TotalAlloc
	current := *stats
	if old.Code != got.Code || old.Body.String() != got.Body.String() {
		t.Fatalf("reference response mismatch: old=%d new=%d", old.Code, got.Code)
	}
	// The route invokes todoView exactly once after each successful second
	// todoAccess; all accepted projects here match the request project.
	newViews := projectChecks - current.projectedRows
	if oldStats.fullCalls != 1 || oldStats.fullRows != 2000 || views != 1500 || current.projectedRows != 2000 || current.fullCalls != 1 || current.fullRows != 7 || newViews != 7 || current.fallbackCalls != 0 {
		t.Fatalf("old=%+v views=%d new=%+v views=%d", oldStats, views, current, newViews)
	}
	if current.fullBytes >= oldStats.fullBytes || current.projectedBytes >= oldStats.fullBytes {
		t.Fatalf("bytes old=%+v new=%+v", oldStats, current)
	}
	t.Logf("reference duration=%s allocations=%d SQL=%d full_rows=%d full_bytes=%d views=%d; optimized duration=%s allocations=%d SQL=%d projected_rows=%d projected_bytes=%d full_rows=%d full_bytes=%d views=%d fallback_calls=%d", oldTime, oldAlloc, oldStats.calls, oldStats.fullRows, oldStats.fullBytes, views, newTime, newAlloc, current.calls, current.projectedRows, current.projectedBytes, current.fullRows, current.fullBytes, newViews, current.fallbackCalls)
	// Grow only records beyond page 2 and verify selected transfers and views remain fixed.
	if _, err := writeDB.ExecContext(ctx, `UPDATE todos SET json=json::jsonb || jsonb_build_object('body',repeat('z',16000)) WHERE id::int < 1000`); err != nil {
		t.Fatal(err)
	}
	*stats = todoReadStats{}
	projectChecks = 0
	again := todoRequest(s, token, query)
	if again.Code != 200 || again.Body.String() != got.Body.String() || stats.fullRows != current.fullRows || stats.fullBytes != current.fullBytes || projectChecks-stats.projectedRows != newViews {
		t.Fatalf("off-page growth: %+v", *stats)
	}
	*stats = todoReadStats{}
	empty := todoRequest(s, token, "?limit=1&page=999999999")
	if empty.Code != 200 || stats.fullCalls != 0 {
		t.Fatalf("empty page: %s %+v", empty.Body, *stats)
	}
}

func TestTodoPageConcurrentChangesAndFailures(t *testing.T) {
	for _, action := range []string{"delete", "start", "project", "project-denied", "owner", "bad-full-error", "full-error", "projection-error", "scan-error"} {
		t.Run(action, func(t *testing.T) {
			s, authDB, stats, db := todoInstrumented(t)
			ctx := context.Background()
			token := stopToken(t, authDB, true)
			p := auth.Principal{Kind: auth.Screen}
			for _, id := range []string{sendID, todoID} {
				if _, err := s.CreateTodo(ctx, Record{"id": id, "project": "owner/app"}, p); err != nil {
					t.Fatal(err)
				}
				if _, err := s.SaveTodo(ctx, id, Record{"text": "saved body", "revision": 0}, p); err != nil {
					t.Fatal(err)
				}
			}
			*stats = todoReadStats{}
			switch action {
			case "delete":
				stats.beforeFull = func() {
					if _, err := db.ExecContext(ctx, "DELETE FROM todos WHERE id=$1", todoID); err != nil {
						t.Error(err)
					}
				}
			case "start":
				stats.beforeFull = func() {
					if _, err := s.StartTodo(ctx, todoID, Record{"tier": "light"}, p); err != nil {
						t.Error(err)
					}
				}
			case "project", "project-denied", "owner":
				if action == "project" {
					// The changed project remains allowed; the request filter must
					// independently reject it after the second read.
					s.options.Project = func(string) error { return nil }
				}
				stats.beforeFull = func() {
					key, value := "project", "other/app"
					if action == "owner" {
						key, value = "owner", "token:someone-else"
					}
					if _, err := db.ExecContext(ctx, `UPDATE todos SET json=jsonb_set(json::jsonb,ARRAY[$2],to_jsonb($3::text))::text WHERE id=$1`, todoID, key, value); err != nil {
						t.Error(err)
					}
				}
			case "full-error":
				stats.fail = "full"
			case "bad-full-error":
				stats.beforeFull = func() {
					if _, err := db.ExecContext(ctx, "UPDATE todos SET json=$2 WHERE id=$1", todoID, `{"project":`); err != nil {
						t.Error(err)
					}
				}
			case "projection-error":
				stats.fail = "projection"
			case "scan-error":
				stats.fail = "scan"
			}
			query := "?project=owner%2Fapp&limit=1"
			if action == "project-denied" {
				query = "?limit=1"
			}
			w := todoRequest(s, token, query)
			if strings.HasSuffix(action, "error") {
				if w.Code == 200 {
					t.Fatalf("failure became success: %s", w.Body)
				}
				return
			}
			if w.Code != 200 {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			var out struct {
				Items   []Record `json:"items"`
				Total   int      `json:"total"`
				HasMore bool     `json:"hasMore"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			want := 0
			if action == "owner" {
				want = 1
			} // Delegated PC is allowed irrespective of owner.
			if out.Total != 2 || !out.HasMore || len(out.Items) != want {
				t.Fatalf("page after %s: %+v", action, out)
			}
			if action == "start" {
				var n int
				if err := db.QueryRowContext(ctx, "SELECT count(*) FROM drafts WHERE id=$1", todoID).Scan(&n); err != nil || n != 1 {
					t.Fatalf("draft count %d: %v", n, err)
				}
			}
		})
	}
}

func TestTodoRouteReferenceCredentialsAndBoundaries(t *testing.T) {
	s, db, _ := fixture(t)
	ctx := context.Background()
	delegated := stopToken(t, db, true)
	plain := "idk_" + strings.Repeat("b", 43)
	if _, err := db.AddPCToken(ctx, store.PCTokenRecord{ID: "plain", Name: "plain", Hash: auth.Hash(plain), IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(ctx, auth.Hash("todo-session"), auth.Session{ExpiresAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	for i, raw := range []string{
		`{"project":"owner/app","id":"body-id","unknown":{"n":9007199254740993},"images":[{"id":"33333333-3333-4333-8333-333333333333"}]}`,
		`{"project":"owner/app","id":123,"body":"text"}`,
		`{"project":"owner/app","body":"missing id"}`,
		`{"project":null,"owner":false}`, `{"project":42}`, `{"owner":"token:plain"}`,
	} {
		if _, err := db.DB().ExecContext(ctx, bindSQL("INSERT INTO todos(id,json) VALUES(?,?)"), fmt.Sprintf("key-%d", i), raw); err != nil {
			t.Fatal(err)
		}
	}
	queries := []string{"", "?limit=2&page=1", "?limit=2&page=2", "?limit=2&page=3", "?limit=2&page=9223372036854775807", "?project=owner%2Fapp&limit=1", "?project=none", "?page=0", "?limit=201", "?page=1&page=2", "?page=abc", "?page=999999999999999999999999999"}
	creds := []struct {
		name      string
		principal auth.Principal
	}{
		{"delegated", auth.Principal{Kind: auth.PC, Name: "fixture", TokenID: "monitor", Delegated: true}},
		{"plain", auth.Principal{Kind: auth.PC, Name: "plain", TokenID: "plain"}},
		{"cli", auth.Principal{Kind: auth.CLI}}, {"screen", auth.Principal{Kind: auth.Screen}},
	}
	for _, cred := range creds {
		for _, query := range queries {
			t.Run(cred.name+"/"+query, func(t *testing.T) {
				r := httptest.NewRequest("GET", "http://localhost:8787/api/todos"+query, nil)
				switch cred.name {
				case "delegated":
					r.Header.Set("Authorization", "Bearer "+delegated)
				case "plain":
					r.Header.Set("Authorization", "Bearer "+plain)
				case "cli":
					if err := db.AddCLIKey(ctx, auth.Hash("todo-cli"), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
						t.Fatal(err)
					}
					r.RemoteAddr = "127.0.0.1:1234"
					r.Header.Set("X-Desk-Key", "todo-cli")
				case "screen":
					r.AddCookie(&http.Cookie{Name: "desk_session", Value: "todo-session"})
				}
				actual := httptest.NewRecorder()
				mux := http.NewServeMux()
				s.Register(mux)
				mux.ServeHTTP(actual, r)
				if cred.name == "plain" {
					if actual.Code != 403 {
						t.Fatalf("plain PC status=%d %s", actual.Code, actual.Body)
					}
					return
				}
				expected, _ := todoReference(t, s, cred.principal, query)
				if actual.Code != expected.Code || actual.Body.String() != expected.Body.String() {
					t.Fatalf("actual %d %s expected %d %s", actual.Code, actual.Body, expected.Code, expected.Body)
				}
			})
		}
	}
}

func TestTodoPageInvalidJSONOutsidePage(t *testing.T) {
	s, authDB, stats, db := todoInstrumented(t)
	ctx := context.Background()
	token := stopToken(t, authDB, true)
	for i, raw := range []string{`{"project":"owner/app","body":`, `{"project":"owner/app","id":"valid"}`} {
		if _, err := db.ExecContext(ctx, "INSERT INTO todos(id,json) VALUES($1,$2)", fmt.Sprint(i), raw); err != nil {
			t.Fatal(err)
		}
	}
	*stats = todoReadStats{}
	got := todoRequest(s, token, "?limit=1")
	ref, _ := todoReference(t, s, auth.Principal{Kind: auth.PC, Name: "fixture", TokenID: "monitor", Delegated: true}, "?limit=1")
	if got.Code == 200 || got.Code != ref.Code || got.Body.String() != ref.Body.String() || stats.fallbackCalls != 1 {
		t.Fatalf("invalid page exterior got=%d %s reference=%d %s stats=%+v", got.Code, got.Body, ref.Code, ref.Body, *stats)
	}
}

func TestTodoPageJSONCompatibility(t *testing.T) {
	s, authDB, stats, db := todoInstrumented(t)
	ctx := context.Background()
	token := stopToken(t, authDB, true)
	principal := auth.Principal{Kind: auth.PC, Name: "fixture", TokenID: "monitor", Delegated: true}
	cases := []struct{ name, raw string }{
		{"missing", `{"id":"x","body":"body"}`},
		{"null", `{"id":null,"project":null,"owner":null}`},
		{"types", `{"id":42,"project":[1],"owner":{"x":1}}`},
		{"numbers", `{"id":"x","project":"owner/app","large":900719925474099312345,"decimal":1.2500e+21}`},
		{"nul", `{"id":"x","project":"owner/app","text":"\u0000"}`},
		{"surrogate", `{"id":"x","project":"owner/app","text":"\ud800"}`},
		{"trailing", `{"id":"x","project":"owner/app"} trailing`},
		{"bad-json", `{"project":"owner/app",`},
		{"non-object", `[]`},
		{"json-null", `null`},
		{"duplicate", `{"project":"other/app","project":"owner/app","id":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, "DELETE FROM todos"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, "INSERT INTO todos(id,json) VALUES($1,$2)", "different-db-key", tc.raw); err != nil {
				t.Fatal(err)
			}
			*stats = todoReadStats{}
			got := todoRequest(s, token, "")
			observed := *stats
			ref, _ := todoReference(t, s, principal, "")
			if got.Code != ref.Code || got.Body.String() != ref.Body.String() {
				t.Fatalf("actual=%d %s reference=%d %s", got.Code, got.Body, ref.Code, ref.Body)
			}
			t.Logf("%s projection: calls=%d rows=%d bytes=%d; full: calls=%d rows=%d bytes=%d; compatibility: calls=%d rows=%d bytes=%d", tc.name, observed.projectedCalls, observed.projectedRows, observed.projectedBytes, observed.fullCalls, observed.fullRows, observed.fullBytes, observed.fallbackCalls, observed.fallbackRows, observed.fallbackBytes)
		})
	}
}
