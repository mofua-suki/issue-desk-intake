package issueapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/numbers"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	t       *testing.T
	ctx     context.Context
	app     *store.SQLite
	pg      *sql.DB
	s       *Service
	options Options
	api     *fakeAPI
	job     int64
	mux     *http.ServeMux
	tokens  map[string]string
}

// testDatabase opens the shared (jobs) PostgreSQL schema and returns it with
// the isolated application schema made by pgtest (the application store).
func testDatabase(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN is not configured")
	}
	u, e := url.Parse(dsn)
	if e != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("explicit test database required")
	}
	ctx := context.Background()
	admin, e := store.OpenPostgres(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	var random [8]byte
	_, _ = rand.Read(random[:])
	schema := "issue_api_" + hex.EncodeToString(random[:])
	if _, e = admin.Exec("CREATE SCHEMA " + schema); e != nil {
		t.Fatal(e)
	}
	params := u.Query()
	params.Set("search_path", schema)
	u.RawQuery = params.Encode()
	db, e := store.OpenPostgres(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		db.Close()
		_, e := admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if e != nil {
			t.Error(e)
		}
		admin.Close()
	})
	if _, e = store.Migrate(ctx, db); e != nil {
		t.Fatal(e)
	}
	return db
}

// testApplication opens the application store on its own isolated PostgreSQL
// schema (the production application schema, made by internal/pgtest).
func testApplication(t *testing.T) *store.SQLite {
	t.Helper()
	env := pgtest.NewIsolated(t)
	app, e := store.OpenApplicationPostgres(context.Background(), env.DSN, env.Schema)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { app.Close() })
	return app
}
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	directory := t.TempDir()
	var e error
	app := testApplication(t)
	pg := testDatabase(t)
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	j, e := jobs.New(pg, jobs.Options{AppDB: app.DB(), Now: func() time.Time { return now }})
	if e != nil {
		t.Fatal(e)
	}
	created, e := j.Create(ctx, jobs.NewJob{Repo: "owner/repo", Issue: 1, Title: "利用者向けの実装", Scope: []string{"src/"}, View: map[string]any{}})
	if e != nil {
		t.Fatal(e)
	}
	nums, e := numbers.OpenLedger(ctx, app.DB(), filepath.Join(directory, "state-desk-numbers.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = nums.Assign(ctx, []numbers.List{{Project: "owner/repo", Issues: []numbers.Candidate{{Number: 1, CreatedAt: "2026-11-01T00:00:00Z"}}}}, []string{"owner/repo"}, true); e != nil {
		t.Fatal(e)
	}
	if e = app.CreateSession(ctx, auth.Hash("screen"), auth.Session{CSRF: "csrf", ExpiresAt: now.Add(time.Hour)}, now); e != nil {
		t.Fatal(e)
	}
	a, e := auth.New(auth.Config{PublicURL: "http://127.0.0.1:8787", Now: func() time.Time { return now }}, &store.AuthStore{SQLite: app})
	if e != nil {
		t.Fatal(e)
	}
	f := &fixture{t: t, ctx: ctx, app: app, pg: pg, api: &fakeAPI{issue: Object{"id": float64(100), "number": float64(1), "title": "利用者向けの実装", "body": "## 依頼\n実装する", "user": Object{"login": "intake"}, "state": "open", "created_at": "2026-11-01T00:00:00Z", "updated_at": "2026-11-01T00:00:00Z"}, comments: []Object{}, timeline: []Object{}}, job: created.Job.ID, tokens: map[string]string{}}
	for index, name := range []string{"nas", "other", "delegate"} {
		id := fmt.Sprintf("%08d-1111-4111-8111-111111111111", index+1)
		token := "idk_" + strings.Repeat(string(rune('a'+index)), 43)
		f.tokens[name] = token
		if _, e = app.AddPCToken(ctx, store.PCTokenRecord{ID: id, Name: name, Hash: auth.Hash(token), IssuedAt: now}); e != nil {
			t.Fatal(e)
		}
		if name == "delegate" {
			if _, e = app.SetDelegated(ctx, id, true, now); e != nil {
				t.Fatal(e)
			}
		}
	}
	f.options = Options{AppDB: app.DB(), Postgres: pg, Auth: a, Gitea: f.api, Numbers: nums, Jobs: j, Now: func() time.Time { return now }, Config: map[string]any{"workerId": "nas", "giteaUrl": "https://gitea.example", "repositories": []any{Object{"name": "owner/repo"}}, "assignments": fixtureAssignments(t)}, Project: func(repo string) error {
		if repo != "owner/repo" {
			return fail(403, "対象外")
		}
		return nil
	}, Own: func(user Object) bool { return user["login"] == "intake" }, RunLimit: 1, UsageLimit: 8000000}
	f.restart()
	t.Cleanup(func() { f.s.Close() })
	return f
}
func (f *fixture) restart() {
	f.t.Helper()
	if f.s != nil {
		f.s.Close()
	}
	f.mux = http.NewServeMux()
	var e error
	f.s, e = Register(f.mux, f.options)
	if e != nil {
		f.t.Fatal(e)
	}
}
func (f *fixture) call(method, path, actor string, data any) (int, any) {
	f.t.Helper()
	raw, _ := json.Marshal(data)
	r := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	if actor == "screen" {
		r.AddCookie(&http.Cookie{Name: "desk_session", Value: "screen"})
		r.Header.Set("Origin", "http://127.0.0.1:8787")
		r.Header.Set("X-CSRF-Token", "csrf")
	} else {
		r.Header.Set("Authorization", "Bearer "+f.tokens[actor])
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	var value any
	if e := json.Unmarshal(w.Body.Bytes(), &value); e != nil {
		f.t.Fatalf("response %d %s", w.Code, w.Body.String())
	}
	return w.Code, value
}
func TestIssueReadCommonNumberAndCommentUnknownResultPostgres(t *testing.T) {
	f := newFixture(t)
	code, value := f.call("GET", "/api/issues", "screen", nil)
	if code != 200 || len(fleet.Items(value)) != 1 || fleet.ObjectOrEmpty(fleet.Items(value)[0])["desk"] == nil {
		t.Fatal(code, value)
	}
	desk := fleet.ObjectOrEmpty(fleet.Items(value)[0])["desk"]
	code, value = f.call("GET", fmt.Sprintf("/api/issue?desk=%v", desk), "screen", nil)
	if code != 200 || fleet.ObjectOrEmpty(value)["project"] != "owner/repo" || len(fleet.Items(fleet.ObjectOrEmpty(value)["actions"])) == 0 {
		t.Fatal(code, value)
	}
	code, _ = f.call("GET", "/api/issue?desk=1&project=owner%2Frepo", "screen", nil)
	if code != 400 {
		t.Fatal("mixed numbers accepted", code)
	}
	input := Object{"id": "11111111-1111-4111-8111-111111111111", "project": "owner/repo", "number": 1, "action": "reply", "text": "追加の指示"}
	f.api.unknownComment = true
	code, _ = f.call("POST", "/api/comment", "screen", input)
	if code != 503 {
		t.Fatal("unknown reply", code)
	}
	f.restart()
	code, value = f.call("POST", "/api/comment", "screen", input)
	if code != 200 || f.api.commentCalls != 1 {
		t.Fatal("duplicate reply", code, value, f.api.commentCalls)
	}
	job, e := f.options.Jobs.Get(f.ctx, f.job)
	if e != nil || len(fleet.Items(job.View["instructions"])) != 1 {
		t.Fatal("instruction not applied once", job, e)
	}
	input["text"] = "別の内容"
	code, _ = f.call("POST", "/api/comment", "screen", input)
	if code != 409 {
		t.Fatal("changed payload accepted", code)
	}
	input["id"] = "22222222-2222-4222-8222-222222222222"
	code, _ = f.call("POST", "/api/comment", "other", input)
	if code != 403 {
		t.Fatal("undelegated control", code)
	}
}
func TestIssueRetryLimitAndScreenDeletionBoundaryPostgres(t *testing.T) {
	f := newFixture(t)
	if _, e := f.pg.Exec(`UPDATE jobs SET status='failed' WHERE id=$1`, f.job); e != nil {
		t.Fatal(e)
	}
	input := Object{"id": "33333333-3333-4333-8333-333333333333", "project": "owner/repo", "number": 1, "action": "retry", "text": "原因と修正方針を確認した"}
	code, _ := f.call("POST", "/api/comment", "delegate", input)
	if code != 409 {
		t.Fatal("run limit bypassed", code)
	}
	input["overLimitStatement"] = "共通番号1の原因を確認して、この再試行を承認します"
	// 承認発言には利用者がチャットで指示した日時が要る。
	if code, _ := f.call("POST", "/api/comment", "delegate", input); code != 400 {
		t.Fatal("over-limit approval without instructionAt", code)
	}
	input["instructionAt"] = "2026-11-01T08:58:00+09:00"
	code, value := f.call("POST", "/api/comment", "delegate", input)
	if code != 200 {
		t.Fatal(code, value)
	}
	job, _ := f.options.Jobs.Get(f.ctx, f.job)
	if job.Generation != 1 {
		t.Fatal(job)
	}
	// 運用代行トークンの削除の承認は発言が必須で、保留でなければ409。
	input = Object{"id": "44444444-4444-4444-8444-444444444444", "project": "owner/repo", "number": 1, "action": "approve-deletions", "head": strings.Repeat("a", 40)}
	code, _ = f.call("POST", "/api/comment", "delegate", input)
	if code != 400 {
		t.Fatal("token deletion approval without statement", code)
	}
	input["statement"] = "承認します"
	code, _ = f.call("POST", "/api/comment", "delegate", input)
	if code != 400 {
		t.Fatal("token deletion approval with boilerplate", code)
	}
	input["statement"] = "共通番号1の削除を承認してマージして"
	input["instructionAt"] = "2026-11-01T08:58:00+09:00"
	code, _ = f.call("POST", "/api/comment", "delegate", input)
	if code != 409 {
		t.Fatal("token deletion approval without hold", code)
	}
}
func TestTokenCloseOwnerAndManualDecisionPostgres(t *testing.T) {
	f := newFixture(t)
	f.options.Answer = func(context.Context, Object) (Object, error) {
		return Object{"decision": "confirm", "contentMatch": false, "reasonConfirmed": false, "reason": "確認不足", "evidence": []any{}}, nil
	}
	f.restart()
	input := Object{"id": "55555555-5555-4555-8555-555555555555", "project": "owner/repo", "number": 1, "reason": "resolved", "description": "利用者向けの実装は対応済み", "comment": "終了を記録する"}
	code, value := f.call("POST", "/api/issues/close", "nas", input)
	if code != 200 {
		t.Fatal(code, value)
	}
	deadline := time.Now().Add(3 * time.Second)
	var saved Object
	for time.Now().Before(deadline) {
		saved, _ = f.s.read(f.ctx, "close_requests", fleet.Text(input["id"]))
		if saved["status"] == "confirm" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if saved["status"] != "confirm" {
		t.Fatal(saved)
	}
	code, _ = f.call("GET", "/api/close-requests/"+fleet.Text(input["id"]), "other", nil)
	if code != 404 {
		t.Fatal("other token read close request", code)
	}
	code, _ = f.call("POST", "/api/close-requests/"+fleet.Text(input["id"])+"/close", "nas", Object{"fingerprint": saved["fingerprint"]})
	if code != 403 {
		t.Fatal("undelegated decision", code)
	}
	code, value = f.call("POST", "/api/close-requests/"+fleet.Text(input["id"])+"/keep", "screen", Object{})
	if code != 200 || fleet.ObjectOrEmpty(value)["status"] != "declined" || f.api.closed != 0 {
		t.Fatal(code, value, f.api.closed)
	}
}
func TestScreenCloseStopsJobAndRecordsReasonPostgres(t *testing.T) {
	f := newFixture(t)
	input := Object{"project": "owner/repo", "number": 1, "reason": "voluntary", "comment": "利用者の判断で終了"}
	code, _ := f.call("POST", "/api/issue/close", "delegate", input)
	if code != 403 {
		t.Fatal("token used screen close", code)
	}
	code, value := f.call("POST", "/api/issue/close", "screen", input)
	if code != 200 || fleet.ObjectOrEmpty(value)["state"] != "closed" || f.api.closed != 1 {
		t.Fatal(code, value)
	}
	job, _ := f.options.Jobs.Get(f.ctx, f.job)
	if job.Status != "closed" {
		t.Fatal(job)
	}
	code, value = f.call("POST", "/api/close-reason", "screen", Object{"project": "owner/repo", "number": 1, "reason": "resolved", "closedAt": "old"})
	if code != 409 {
		t.Fatal("stale close reason accepted", code, value)
	}
}

func TestIssueDetailWorkerExecutionPostgres(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name, view string
		present    bool
	}{
		{"absent", `{}`, false},
		{"checkpoint", `{"workerExecution":{"stage":"test","implementationRuns":2,"generation":3,"startedGenerations":[1,3]}}`, true},
		{"finish_with_unknown_timing", `{"workerExecution":{"stage":"deploy","implementationRuns":2,"reportCompletionRuns":1,"weightedUsage":123,"generation":3,"startedGenerations":[1,3],"continuationExcludedGenerations":[],"otherExcludedGenerations":[],"timings":{"testMs":1200,"repairMs":350,"fixedTests":[100,200]}}}`, true},
		{"legacy_without_timing", `{"workerExecution":{"stage":"implement","implementationRuns":1}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.pg.Exec(`UPDATE jobs SET view=$2::jsonb WHERE id=$1`, f.job, tc.view); err != nil {
				t.Fatal(err)
			}
			f.restart()
			code, response := f.call("GET", "/api/issue?project=owner%2Frepo&number=1", "nas", nil)
			if code != http.StatusOK {
				t.Fatalf("status %d: %v", code, response)
			}
			got, present := fleet.ObjectOrEmpty(response)["workerExecution"]
			if present != tc.present {
				t.Fatalf("presence = %v, response = %v", present, response)
			}
			if tc.present {
				var view map[string]any
				if err := json.Unmarshal([]byte(tc.view), &view); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, view["workerExecution"]) {
					t.Fatalf("got %#v, want %#v", got, view["workerExecution"])
				}
			}
		})
	}
}

func TestWorkerExecutionExcludedFromListsPostgres(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pg.Exec(`UPDATE jobs SET view=$2::jsonb WHERE id=$1`, f.job, `{"workerExecution":{"stage":"test","timings":{"testMs":1200}}}`); err != nil {
		t.Fatal(err)
	}
	f.restart()
	for _, path := range []string{"/api/issues", "/api/issues/search"} {
		code, value := f.call("GET", path, "nas", nil)
		rows := fleet.Items(value)
		if path == "/api/issues/search" {
			rows = fleet.Items(fleet.ObjectOrEmpty(value)["issues"])
		}
		if code != http.StatusOK || len(rows) != 1 {
			t.Fatalf("%s: %d %v", path, code, value)
		}
		if _, present := fleet.ObjectOrEmpty(rows[0])["workerExecution"]; present {
			t.Fatalf("%s: workerExecution leaked: %v", path, rows[0])
		}
	}
	entry, err := f.s.ListEntry(f.ctx, "owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := entry["workerExecution"]; present {
		t.Fatalf("ListEntry: workerExecution leaked: %v", entry)
	}
}

func TestSearchPaginationAndDetailProjectionPostgres(t *testing.T) {
	f := newFixture(t)
	f.api.issues = []Object{clone(f.api.issue)}
	for i := 2; i <= 105; i++ {
		issue := clone(f.api.issue)
		issue["number"] = i
		issue["id"] = i + 100
		issue["title"] = fmt.Sprintf("Issue %03d", i)
		f.api.issues = append(f.api.issues, issue)
	}
	if _, err := f.pg.Exec(`UPDATE jobs SET view=$2::jsonb WHERE id=$1`, f.job, `{"dispatchRecord":{"note":"heavy"},"executionSettings":{"provider":"codex"}}`); err != nil {
		t.Fatal(err)
	}
	seen := map[float64]bool{}
	for page := 1; page <= 2; page++ {
		route := "/api/issues/search?sort=created&order=asc"
		if page > 1 {
			route += fmt.Sprintf("&page=%d", page)
		}
		code, value := f.call("GET", route, "nas", nil)
		result := fleet.ObjectOrEmpty(value)
		rows := fleet.Items(result["issues"])
		if code != 200 || result["total"] != float64(105) || result["hasMore"] != (page == 1) || len(rows) != map[int]int{1: 100, 2: 5}[page] {
			t.Fatal(code, result)
		}
		for _, raw := range rows {
			row := fleet.ObjectOrEmpty(raw)
			n := row["number"].(float64)
			if seen[n] {
				t.Fatal("duplicate", n)
			}
			seen[n] = true
			if row["dispatchRecord"] != nil || row["executionSettings"] != nil {
				t.Fatal("heavy search row", row)
			}
		}
	}
	if len(seen) != 105 {
		t.Fatal(len(seen))
	}
	code, count := f.call("GET", "/api/issues/search?countOnly=1", "nas", nil)
	if code != 200 || len(fleet.ObjectOrEmpty(count)) != 1 || fleet.ObjectOrEmpty(count)["total"] != float64(105) {
		t.Fatal(code, count)
	}
	for _, q := range []string{"limit=0", "limit=101", "page=0"} {
		code, _ := f.call("GET", "/api/issues/search?"+q, "nas", nil)
		if code != 400 {
			t.Fatal(q, code)
		}
	}
	code, detail := f.call("GET", "/api/issue?project=owner%2Frepo&number=1", "nas", nil)
	if code != 200 || fleet.ObjectOrEmpty(detail)["body"] != f.api.issue["body"] || fleet.ObjectOrEmpty(detail)["shownBody"] != nil || fleet.ObjectOrEmpty(detail)["dispatchRecord"] == nil || fleet.ObjectOrEmpty(detail)["executionSettings"] == nil {
		t.Fatal(code, detail)
	}
	code, detail = f.call("GET", "/api/issue?project=owner%2Frepo&number=1&view=summary", "nas", nil)
	if code != 200 || fleet.ObjectOrEmpty(detail)["view"] != "summary" || fleet.ObjectOrEmpty(detail)["actions"] != nil {
		t.Fatal(code, detail)
	}
}

func TestSearchPageAndDetailBodyWithoutDatabase(t *testing.T) {
	limit, page, err := searchPagination(Object{})
	if err != nil || limit != 100 || page != 1 {
		t.Fatalf("default: %d %d %v", limit, page, err)
	}
	rows := make([]Object, 105)
	for i := range rows {
		rows[i] = Object{"number": i + 1, "dispatchRecord": "heavy", "executionSettings": "heavy"}
	}
	first := searchPage(rows, limit, page)
	got := first["issues"].([]Object)
	if first["total"] != 105 || first["hasMore"] != true || len(got) != 100 {
		t.Fatalf("first: %#v", first)
	}
	for i, row := range got {
		if row["number"] != i+1 {
			t.Fatal("order", row)
		}
		if _, ok := row["dispatchRecord"]; ok {
			t.Fatal("dispatchRecord present")
		}
		if _, ok := row["executionSettings"]; ok {
			t.Fatal("executionSettings present")
		}
	}
	last := searchPage(rows, limit, 2)
	if last["total"] != 105 || last["hasMore"] != false || len(last["issues"].([]Object)) != 5 || last["issues"].([]Object)[0]["number"] != 101 {
		t.Fatalf("last: %#v", last)
	}
	body := Object{}
	setDetailBody(body, "same", "same")
	if body["body"] != "same" {
		t.Fatal(body)
	}
	if _, ok := body["shownBody"]; ok {
		t.Fatal(body)
	}
	setDetailBody(body, "original", "shown")
	if body["body"] != "original" || body["shownBody"] != "shown" {
		t.Fatal(body)
	}
}
