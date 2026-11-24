package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

func workerFixture(t *testing.T) (http.Handler, *sql.DB, string) {
	return workerFixtureOptions(t, false, nil)
}
func workerFixtureOptions(t testing.TB, delegated bool, change func(*WorkerRouteOptions)) (http.Handler, *sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN is not configured")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("dedicated test database required")
	}
	ctx := context.Background()
	admin, err := store.OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	schema := "worker_api_" + hex.EncodeToString(nonce[:])
	if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := store.OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		if _, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	if _, err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	env := pgtest.NewIsolated(t)
	sqlite, err := store.OpenApplicationPostgres(ctx, env.DSN, env.Schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlite.Close() })
	token := "idk_" + strings.Repeat("a", 43)
	if _, err = sqlite.AddPCToken(ctx, store.PCTokenRecord{ID: "test-token", Name: "nas", Hash: auth.Hash(token), IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if delegated {
		if _, err = sqlite.DB().ExecContext(ctx, "INSERT INTO api_token_grants(token_id,granted_at)VALUES('test-token',?)", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	authenticator, err := auth.New(auth.Config{Port: 8787}, &store.AuthStore{SQLite: sqlite, Postgres: db})
	if err != nil {
		t.Fatal(err)
	}
	fleetPath, assignmentPath := filepath.Join(dir, "fleet.json"), filepath.Join(dir, "assignments.json")
	if err = os.WriteFile(fleetPath, []byte(`{"version":1,"defaults":{},"hosts":{"nas":{"giteaUrl":"http://gitea.example","repositories":[{"name":"owner/repo"}]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(assignmentPath, []byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":2,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	options := WorkerRouteOptions{DB: db, AppDB: sqlite.DB(), RuntimeDir: dir, Auth: authenticator, FleetPath: fleetPath, AssignmentsPath: assignmentPath}
	if change != nil {
		change(&options)
	}
	if err = RegisterWorkerRoutes(mux, options); err != nil {
		t.Fatal(err)
	}
	return mux, db, token
}

func workerRequest(t *testing.T, handler http.Handler, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, "http://localhost:8787"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(contract.Header, contract.Version)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var value map[string]any
	if w.Code != 204 {
		if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
			t.Fatalf("invalid response HTTP %d: %s", w.Code, w.Body.String())
		}
	}
	return w.Code, value
}

func TestWorkerConfigurationHeartbeatDispatchAndFencedReport(t *testing.T) {
	handler, db, token := workerFixture(t)
	status, config := workerRequest(t, handler, token, "GET", "/api/v1/client-config", "")
	if status != 200 || config["configurationVersion"] == nil {
		t.Fatalf("config: %d %v", status, config)
	}
	status, _ = workerRequest(t, handler, token, "GET", "/api/v1/client-config?since="+config["version"].(string), "")
	if status != 200 {
		t.Fatalf("config unchanged: %d", status)
	}
	status, heartbeat := workerRequest(t, handler, token, "PUT", "/api/v1/hosts/nas", `{"ready":true,"capacity":2,"capabilities":{"repositories":["owner/repo"]}}`)
	if status != 200 || heartbeat["ready"] != true {
		t.Fatalf("heartbeat: %d %v", status, heartbeat)
	}
	if _, err := db.Exec(`INSERT INTO jobs(repo,issue,title,scope,view) VALUES('owner/repo',1,'test',ARRAY['server/a.go'],'{"reviewHolds":0,"reviewHoldsLowerBound":0,"reviewHoldsUnknown":false}')`); err != nil {
		t.Fatal(err)
	}
	status, taken := workerRequest(t, handler, token, "POST", "/api/v1/jobs/next", `{"host":"nas","capacity":2}`)
	if status != 200 {
		t.Fatalf("next: %d %v", status, taken)
	}
	job := taken["job"].(map[string]any)
	id := int64(job["id"].(float64))
	epoch := int64(taken["lease"].(map[string]any)["epoch"].(float64))
	base := "/api/v1/jobs/" + fmtInt(id)
	status, renewed := workerRequest(t, handler, token, "POST", base+"/lease", `{"epoch":`+fmtInt(epoch)+`,"progress":{"phase":"test"}}`)
	if status != 200 || renewed["lease"] == nil {
		t.Fatalf("renew: %d %v", status, renewed)
	}
	status, turn := workerRequest(t, handler, token, "POST", "/api/v1/merge-turns/owner/repo/acquire", `{"jobId":`+fmtInt(id)+`,"epoch":`+fmtInt(epoch)+`}`)
	if status != 200 || turn["granted"] != true {
		t.Fatalf("turn: %d %v", status, turn)
	}
	status, reported := workerRequest(t, handler, token, "POST", base+"/state", `{"epoch":`+fmtInt(epoch)+`,"status":"merged","view":{"reviewHolds":0},"result":{"status":"completed"}}`)
	if status != 200 || reported["job"].(map[string]any)["status"] != "merged" {
		t.Fatalf("report: %d %v", status, reported)
	}
	status, refused := workerRequest(t, handler, token, "POST", base+"/lease", `{"epoch":`+fmtInt(epoch)+`}`)
	if status != 409 || refused["code"] != "lease_lost" {
		t.Fatalf("old lease not fenced: %d %v", status, refused)
	}
}

func TestWorkerIdentityAndScopeConflictStayServerOwned(t *testing.T) {
	handler, db, token := workerFixture(t)
	status, _ := workerRequest(t, handler, token, "PUT", "/api/v1/hosts/other", `{"ready":true}`)
	if status != 403 {
		t.Fatal("another host was modified")
	}
	status, _ = workerRequest(t, handler, token, "PUT", "/api/v1/hosts/nas", `{"ready":true,"capacity":2}`)
	if status != 200 {
		t.Fatal("own heartbeat failed")
	}
	if _, err := db.Exec(`INSERT INTO jobs(repo,issue,status,lease_host,lease_epoch,lease_expires_at,scope,view) VALUES('owner/repo',1,'running','nas',1,now()+interval '1 hour',ARRAY['server/a.go'],'{}'),('owner/repo',2,'running','nas',1,now()+interval '1 hour',ARRAY['client/a.ts'],'{}')`); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.QueryRow("SELECT id FROM jobs WHERE issue=2").Scan(&id); err != nil {
		t.Fatal(err)
	}
	status, scope := workerRequest(t, handler, token, "POST", "/api/v1/jobs/"+fmtInt(id)+"/scope", `{"epoch":1,"add":["server/"]}`)
	if status != 200 || scope["result"] != "wait" || scope["job"].(map[string]any)["status"] != "held" {
		t.Fatalf("overlapping scope granted: %d %v", status, scope)
	}
	status, _ = workerRequest(t, handler, token, "PUT", "/api/v1/hosts/nas/drain", `{"reason":"test"}`)
	if status != 403 {
		t.Fatal("delegation requirement widened")
	}
}

func fmtInt(value int64) string { raw, _ := json.Marshal(value); return string(raw) }

func TestWorkerNextRestrictsEligibleRepositoriesWithoutBroadeningAssignments(t *testing.T) {
	handler, db, token := workerFixture(t)
	status, view := workerRequest(t, handler, token, "PUT", "/api/v1/hosts/nas", `{"ready":true,"capacity":2,"capabilities":{"repositories":["owner/repo"]}}`)
	if status != 200 {
		t.Fatal(status, view)
	}
	if _, err := db.Exec(`INSERT INTO jobs(repo,issue,title,scope,view) VALUES('owner/repo',1,'test',ARRAY['server/a.go'],'{"reviewHolds":0,"reviewHoldsLowerBound":0,"reviewHoldsUnknown":false}')`); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"capacity":2,"repos":null}`, `{"capacity":2,"repos":[1]}`, `{"capacity":2,"repos":["invalid"]}`} {
		status, view = workerRequest(t, handler, token, "POST", "/api/v1/jobs/next", body)
		if status != 400 {
			t.Fatal(body, status, view)
		}
	}
	for _, body := range []string{`{"capacity":2,"repos":[]}`, `{"capacity":2,"repos":["owner/other"]}`} {
		status, view = workerRequest(t, handler, token, "POST", "/api/v1/jobs/next", body)
		if status != 204 {
			t.Fatal(body, status, view)
		}
	}
	status, view = workerRequest(t, handler, token, "POST", "/api/v1/jobs/next", `{"host":"nas","capacity":2,"repos":["OWNER/REPO"]}`)
	if status != 200 || view["job"] == nil || view["lease"] == nil {
		t.Fatal(status, view)
	}
}

func TestWorkerJobListPagination(t *testing.T) {
	handler, db, token := workerFixture(t)
	_, err := db.Exec(`INSERT INTO jobs(repo,issue,status,priority,filed_at) SELECT CASE WHEN n<=205 THEN 'owner/repo' ELSE 'other/repo' END,n,CASE WHEN n%2=0 THEN 'queued' ELSE 'running' END,(n/2)%2,'2025-01-01'::timestamptz + n * interval '1 second' FROM generate_series(1,210) n`)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) map[string]any {
		t.Helper()
		status, body := workerRequest(t, handler, token, "GET", path, "")
		if status != 200 {
			t.Fatalf("%s: %d %v", path, status, body)
		}
		return body
	}
	first := get("/api/v1/jobs")
	if len(first["jobs"].([]any)) != 200 || first["total"] != float64(210) || first["page"] != float64(1) || first["limit"] != float64(200) || first["hasMore"] != true {
		t.Fatalf("default: %v", first)
	}
	var expected []int64
	rows, err := db.Query(`SELECT id FROM jobs ORDER BY priority DESC,filed_at,id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for i, item := range first["jobs"].([]any) {
		if int64(item.(map[string]any)["id"].(float64)) != expected[i] {
			t.Fatalf("default order at %d", i)
		}
	}
	var all []int64
	for page := 1; page <= 6; page++ {
		body := get("/api/v1/jobs?limit=37&page=" + fmtInt(int64(page)))
		for _, item := range body["jobs"].([]any) {
			all = append(all, int64(item.(map[string]any)["id"].(float64)))
		}
		if body["hasMore"] != (page < 6) {
			t.Fatalf("hasMore page %d", page)
		}
	}
	if len(all) != len(expected) {
		t.Fatalf("pages: %d", len(all))
	}
	for i := range all {
		if all[i] != expected[i] {
			t.Fatalf("pages order at %d", i)
		}
	}
	filtered := get("/api/v1/jobs?repo=OWNER%2Frepo&status=queued&limit=1&page=103")
	if filtered["total"] != float64(102) || len(filtered["jobs"].([]any)) != 0 || filtered["hasMore"] != false {
		t.Fatalf("filter/outside: %v", filtered)
	}
	for _, query := range []string{"limit=0", "limit=201", "limit=x", "limit=", "limit=1&limit=2", "page=0", "page=-1", "page=x", "page=9007199254740992&limit=2", "page=999999999999999999999999", "status=invalid"} {
		status, _ := workerRequest(t, handler, token, "GET", "/api/v1/jobs?"+query, "")
		if status != 400 {
			t.Errorf("%s: %d", query, status)
		}
	}
}

func TestAttentionFilterRoutes(t *testing.T) {
	handler, _, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		o.AttentionDraftAlerts = func(context.Context) ([]any, error) {
			return []any{
				map[string]any{"project": "igs", "kind": "failed"},
				map[string]any{"project": "other", "kind": "failed"},
				map[string]any{"project": "other", "kind": "review_held"},
				map[string]any{"kind": "failed"},
			}, nil
		}
	})
	cases := []struct {
		query         string
		status, count int
	}{
		{"", 200, 4}, {"?project=igs&project=other&kind=failed", 200, 2},
		{"?excludeProject=igs&excludeKind=review_held", 200, 2},
		{"?kind=ops_watch", 200, 0}, {"?project=igs&excludeProject=igs", 200, 0},
		{"?project=", 400, 0}, {"?kind=%20failed", 400, 0}, {"?unknown=x", 400, 0}, {"?kind=%zz", 400, 0},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "http://localhost:8787/api/attention/alerts", nil)
		r.URL.RawQuery = strings.TrimPrefix(tc.query, "?")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s: HTTP%d %s", tc.query, w.Code, w.Body.String())
		}
		if tc.status == 200 {
			timing := w.Header().Get("Server-Timing")
			for _, name := range []string{"drafts", "saved", "notices", "stage", "incidents", "jobs", "checkout", "rework", "rc", "visible"} {
				if !strings.Contains(timing, name+";dur=") {
					t.Fatalf("missing timing %s: %s", name, timing)
				}
			}
			var rows []any
			if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != tc.count {
				t.Fatalf("%s: %s", tc.query, w.Body.String())
			}
			original := []any{map[string]any{"project": "igs", "kind": "failed"}, map[string]any{"project": "other", "kind": "failed"}, map[string]any{"project": "other", "kind": "review_held"}, map[string]any{"kind": "failed"}}
			expected := []any{}
			switch tc.query {
			case "":
				expected = original
			case "?project=igs&project=other&kind=failed":
				expected = []any{original[0], original[1]}
			case "?excludeProject=igs&excludeKind=review_held":
				expected = []any{original[1], original[3]}
			}
			if !reflect.DeepEqual(rows, expected) {
				t.Fatalf("%s: 内容・順序不一致: %v", tc.query, rows)
			}
		}
	}
}

func TestAttentionFilterRoutesWithArchivedRecords(t *testing.T) {
	rows := []any{
		map[string]any{"key": "first", "project": "igs", "kind": "failed"},
		map[string]any{"key": "hidden", "project": "igs", "kind": "failed"},
		map[string]any{"key": "excluded-project", "project": "other", "kind": "failed"},
		map[string]any{"key": "excluded-kind", "project": "igs", "kind": "review_held"},
		map[string]any{"key": "last", "project": "igs", "kind": "failed"},
	}
	handler, _, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		o.AttentionDraftAlerts = func(context.Context) ([]any, error) { return rows, nil }
		raw, err := json.Marshal([]any{map[string]any{"target": "attention:attention:hidden", "snapshot": rows[1]}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = o.AppDB.Exec(`INSERT INTO settings(id,json) VALUES('desk-cleanup-archive',?)`, string(raw)); err != nil {
			t.Fatal(err)
		}
	})
	for _, tc := range []struct {
		query string
		want  []any
	}{
		{"", []any{rows[0], rows[2], rows[3], rows[4]}},
		{"?project=igs&project=other&kind=failed&kind=review_held&excludeProject=other&excludeKind=review_held", []any{rows[0], rows[4]}},
		{"?project=igs&kind=failed&excludeProject=igs", []any{}},
	} {
		r := httptest.NewRequest("GET", "http://localhost:8787/api/attention/alerts"+tc.query, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var got []any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: HTTP%d 内容・順序不一致: %s (err=%v)", tc.query, w.Code, w.Body.String(), err)
		}
	}
}

func TestAttentionServerTiming(t *testing.T) {
	handler, _, token := workerFixtureOptions(t, true, nil)
	r := httptest.NewRequest("GET", "http://localhost:8787/api/attention/alerts", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("Server-Timing") == "" {
		t.Fatalf("HTTP%d: timing=%q body=%s", w.Code, w.Header().Get("Server-Timing"), w.Body.String())
	}
	want := []string{"drafts", "saved", "notices", "stage", "incidents", "jobs", "jobRecovery", "checkout", "rework", "rc", "visible"}
	var stages []string
	for _, metric := range strings.Split(w.Header().Get("Server-Timing"), ",") {
		stage, duration, ok := strings.Cut(strings.TrimSpace(metric), ";dur=")
		value, err := strconv.ParseFloat(duration, 64)
		if !ok || err != nil || value < 0 {
			t.Fatalf("invalid duration in Server-Timing metric %q", metric)
		}
		stages = append(stages, stage)
	}
	if !reflect.DeepEqual(stages, want) {
		t.Fatalf("Server-Timing stages=%v, want %v", stages, want)
	}
}

func TestAttentionServerTimingOnStageError(t *testing.T) {
	handler, _, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		o.AttentionDraftAlerts = func(context.Context) ([]any, error) { return nil, errors.New("draft failure") }
	})
	r := httptest.NewRequest("GET", "http://localhost:8787/api/attention/alerts", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code < 500 || !strings.HasPrefix(w.Header().Get("Server-Timing"), "drafts;dur=") || strings.Contains(w.Header().Get("Server-Timing"), ",") {
		t.Fatalf("HTTP%d timing=%q body=%s", w.Code, w.Header().Get("Server-Timing"), w.Body.String())
	}
}

func TestAttentionFilterValidationAndMatching(t *testing.T) {
	rows := []any{map[string]any{"project": "igs", "kind": "failed"}, map[string]any{"project": "other", "kind": "failed"}, map[string]any{"kind": "failed"}, map[string]any{"project": "other", "kind": "review_held"}}
	for _, tc := range []struct {
		query   string
		count   int
		invalid bool
	}{
		{"", 4, false}, {"project=igs&project=other&kind=failed", 2, false}, {"excludeProject=igs&excludeKind=review_held", 2, false}, {"kind=ops_watch", 0, false}, {"project=igs&excludeProject=igs", 0, false}, {"project=", 0, true}, {"kind=%20failed", 0, true}, {"unknown=x", 0, true}, {"kind=%zz", 0, true},
	} {
		f, err := parseAttentionFilters(tc.query)
		if (err != nil) != tc.invalid {
			t.Fatalf("%q: %v", tc.query, err)
		}
		if err == nil && len(f.apply(rows)) != tc.count {
			t.Fatalf("%q: %v", tc.query, f.apply(rows))
		}
	}
}

func TestStatsTimingLogsSelectedTables(t *testing.T) {
	handler, _, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) { return nil, nil }
	})
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(old)
	status, response := workerRequest(t, handler, token, "GET", "/api/stats?tables=usage,waits&details=true", "")
	if status != 200 {
		t.Fatalf("%d %v", status, response)
	}
	for _, name := range []string{"work", "usage", "waits"} {
		if !strings.Contains(output.String(), "stats calculation table="+name+" duration_ms=") {
			t.Fatalf("時間がありません: %s", name)
		}
	}
	if strings.Contains(output.String(), "table=rework ") {
		t.Fatal("未選択の表を計算しました")
	}
	if _, ok := response["usage"]; !ok {
		t.Fatal("usage がありません")
	}
	if _, ok := response["waits"]; !ok {
		t.Fatal("waits がありません")
	}
	if _, ok := response["rework"]; ok {
		t.Fatal("未選択の表があります")
	}
}

func TestWorkerNextCanRetryUndeliveredLeaseImmediately(t *testing.T) {
	handler, db, token := workerFixture(t)
	id := prepareHandoffJob(t, handler, db, token)
	barrier, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback()
	if _, err := barrier.Exec("LOCK TABLE usage_rows IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	failure := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	handler.ServeHTTP(failure, handoffNextRequest(ctx, token, `{"capacity":2}`))
	code, response := failure.Code, failure.Body.String()
	if ctx.Err() != nil {
		t.Fatal("delivery wait was limited only by request cancellation")
	}
	if code < 500 {
		t.Fatalf("delivery read error: HTTP %d: %v", code, response)
	}
	var status string
	var epoch, expirations int
	if err := db.QueryRow("SELECT status,lease_epoch FROM jobs WHERE id=$1", id).Scan(&status, &epoch); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || epoch != 0 {
		t.Fatalf("undelivered job was changed: %s epoch=%d", status, epoch)
	}
	if err := barrier.Rollback(); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, handoffNextRequest(t.Context(), token, `{"capacity":2}`))
	assertHandoffJob(t, w, db, id)
	if err := db.QueryRow("SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='lease_expired'", id).Scan(&expirations); err != nil {
		t.Fatal(err)
	}
	if expirations != 0 {
		t.Fatalf("undelivered attempt counted as a lease expiration: %d", expirations)
	}
}
