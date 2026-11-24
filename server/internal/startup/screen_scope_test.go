package startup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/issueapi"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/listscope"
	"issue-desk-intake/server/internal/numbers"
	"issue-desk-intake/server/internal/operations"
	"issue-desk-intake/server/internal/screenapi"
	"issue-desk-intake/server/internal/statistics"
)

type scopeAPI struct {
	issueapi.API
	items []map[string]any
	calls int
	// other holds the Issues of candidates besides owner/app; unread names
	// candidates whose read fails.
	other  map[string][]map[string]any
	unread map[string]bool
}

func (s *scopeAPI) Issues(_ context.Context, repo, state string) ([]map[string]any, error) {
	if state != "all" {
		return nil, fmt.Errorf("expected all issues, got %s", state)
	}
	if s.unread[repo] {
		return nil, fmt.Errorf("%s を読めません", repo)
	}
	if repo != "owner/app" {
		return s.other[repo], nil
	}
	s.calls++
	return s.items, nil
}

type scopeCase struct {
	Name         string                      `json:"name"`
	Issues       []map[string]any            `json:"issues"`
	Drafts       []consultations.Record      `json:"drafts"`
	Closes       []map[string]any            `json:"closes"`
	ActiveIssues []int                       `json:"activeIssues"`
	ActiveDrafts []string                    `json:"activeDrafts"`
	Wire         map[string][]map[string]any `json:"wire"`
	// Other lists additional candidate repositories (not in the shared fixtures).
	Other []string `json:"-"`
}

func scopeCases(t *testing.T) []scopeCase {
	t.Helper()
	raw, e := os.ReadFile("../../../test/fixtures/list-scope-cases.json")
	if e != nil {
		t.Fatal(e)
	}
	var data struct {
		Cases []scopeCase `json:"cases"`
	}
	if e = json.Unmarshal(raw, &data); e != nil {
		t.Fatal(e)
	}
	return data.Cases
}
func setupScopeCase(t *testing.T, c scopeCase) (*App, *scopeAPI, *numbers.Store, map[string]screenapi.ListReader, http.Handler) {
	t.Helper()
	ctx := context.Background()
	env, app, _, lists, _ := screenFixture(t)
	app.Records = &consultations.Records{DB: env.DB}
	pg := appPostgres(t)
	js, e := jobs.New(pg, jobs.Options{AppDB: env.DB})
	if e != nil {
		t.Fatal(e)
	}
	ns, e := numbers.OpenLedger(ctx, env.DB, filepath.Join(t.TempDir(), "state-desk-numbers.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	api := &scopeAPI{other: map[string][]map[string]any{}, unread: map[string]bool{}}
	repos := append([]string{"owner/app"}, c.Other...)
	configRepos := []any{}
	for _, repo := range repos {
		configRepos = append(configRepos, map[string]any{"name": repo})
	}
	for _, input := range c.Issues {
		item := map[string]any{"title": "request", "user": map[string]any{"login": "intake"}, "created_at": "2026-11-01T00:00:00Z", "updated_at": "2026-11-01T00:00:00Z", "closed_at": "2026-11-02T00:00:00Z", "body": ""}
		for k, v := range input {
			item[k] = v
		}
		if id, ok := input["draft"]; ok {
			item["body"] = fmt.Sprintf("<!-- issue-desk:00000000-0000-4000-8000-%012.0f -->", id)
		}
		api.items = append(api.items, item)
		if status, ok := input["jobStatus"]; ok {
			view, _ := json.Marshal(input["jobView"])
			result, _ := json.Marshal(input["jobResult"])
			if string(view) == "null" {
				view = []byte("{}")
			}
			if string(result) == "null" {
				result = []byte("{}")
			}
			if _, e = pg.Exec(`INSERT INTO jobs(repo,issue,status,view,hold_reason,result) VALUES('owner/app',$1,$2,$3,$4,$5)`, input["number"], status, string(view), input["holdReason"], string(result)); e != nil {
				t.Fatal(e)
			}
			if input["jobDependsOn"] != nil {
				if _, e = pg.Exec(`INSERT INTO jobs(repo,issue,status) VALUES('owner/app',$1,'queued')`, input["jobDependsOn"]); e != nil {
					t.Fatal(e)
				}
				if _, e = pg.Exec(`INSERT INTO job_deps(job_id,depends_on) SELECT j.id,p.id FROM jobs j JOIN jobs p ON p.repo=j.repo WHERE j.repo='owner/app' AND j.issue=$1 AND p.issue=$2`, input["number"], input["jobDependsOn"]); e != nil {
					t.Fatal(e)
				}
			}
		}
	}
	app.Jobs, app.Numbers = js, ns
	app.Consultations, e = consultations.New(consultations.Options{Records: app.Records, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }, FamilyJobs: app.familyJobs, FamilyIssues: func(context.Context, string) (map[string]string, error) {
		states := map[string]string{}
		for _, i := range api.items {
			states[listscope.IssueKey("owner/app", i["number"])] = i["state"].(string)
		}
		return states, nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	for _, d := range c.Drafts {
		raw, _ := json.Marshal(d)
		if _, e = env.DB.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES (?,?)", d["id"], string(raw)); e != nil {
			t.Fatal(e)
		}
	}
	for _, close := range c.Closes {
		raw, err := json.Marshal(close)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = env.DB.ExecContext(ctx, "INSERT INTO close_requests(id,json) VALUES (?,?)", close["id"], string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	// The Issue API shares the screen session, so app.Mux answers GET /api/issues.
	session, signed := screenSession(t, env)
	app.Mux = http.NewServeMux()
	app.Issues, e = issueapi.Register(app.Mux, issueapi.Options{ActiveList: app.activeIssues, AppDB: env.DB, Postgres: pg, Auth: signed, Gitea: api, Numbers: ns, Jobs: js, Drafts: app.Records, Project: func(string) error { return nil }, Own: func(v map[string]any) bool { return v["login"] == "intake" }, Config: map[string]any{"repositories": configRepos, "assignments": map[string]any{}}, Repositories: func() []string { return repos }})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(app.Issues.Close)
	app.registerConsultationReaders(lists, map[string]screenapi.EntryReader{})
	lists["closeRequest"] = func(ctx context.Context, _ string) ([]map[string]any, error) {
		return app.Issues.CloseRequests(ctx)
	}
	lists["issue"] = func(ctx context.Context, _ string) ([]map[string]any, error) {
		result, e := app.screenIssues(ctx)
		if e == nil {
			screenapi.ReportUnreadProjects(ctx, result.unread)
		}
		return result.rows, e
	}
	assignments := &fleet.Assignments{Output: map[string]any{}}
	runtimeDir := t.TempDir()
	stats := &statistics.Service{DB: pg, AppDB: app.Records.DB, Assignments: assignments, RuntimeDir: runtimeDir, Now: time.Now}
	ops := &operations.Service{DB: app.Records.DB, PostgreSQL: pg, RuntimeDir: runtimeDir, Assignments: assignments, Statistics: stats, Repositories: func() ([]string, error) { return []string{"owner/app"}, nil }, Now: time.Now}
	handler := screenListMux(t, session, signed, repos, lists, map[string]func(context.Context) (any, error){"attentionAlerts": func(ctx context.Context) (any, error) {
		stage := ""
		rows, err := ops.AttentionWithTiming(ctx, func(name string, d time.Duration) {
			stage = name
			screenapi.ReportAttentionTiming(ctx, name, d)
		})
		if err != nil {
			t.Errorf("Attention stage %q failed: %v", stage, err)
		}
		return rows, err
	}})
	return app, api, ns, lists, handler
}
func scopeRequest(t *testing.T, h http.Handler, path string) (map[string]any, int, string) {
	t.Helper()
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787"+path, nil)
	r.AddCookie(&http.Cookie{Name: "desk_session", Value: "screen"})
	r.Header.Set("Origin", "http://127.0.0.1:8787")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d %s", w.Code, w.Body.String())
	}
	var result map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &result); e != nil {
		t.Fatal(e)
	}
	return result, w.Body.Len(), w.Header().Get("Server-Timing")
}
func scopeRows(body map[string]any) map[string]map[string]any {
	rows := map[string]map[string]any{}
	for _, v := range body["rows"].([]any) {
		r := v.(map[string]any)
		rows[r["key"].(string)] = r["item"].(map[string]any)
	}
	return rows
}

// The wire contract binds TypeScript classification fixtures to actual HTTP output.
func TestListScopeFixtures(t *testing.T) {
	corpus := []map[string]any{}
	for _, c := range scopeCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			app, api, ns, _, h := setupScopeCase(t, c)
			// Exercise a cold active read first: omitted rows must still be numbered and
			// included in the saved close baseline, before any full list is requested.
			_, _, _ = scopeRequest(t, h, "/api/list?tabs=active")
			assigned, e := ns.All(context.Background())
			if e != nil || len(assigned) != len(c.Issues) {
				t.Fatalf("numbers=%v err=%v", assigned, e)
			}
			var baseline string
			if e = app.Records.DB.QueryRow("SELECT json FROM settings WHERE id='closeReasonBaseline'").Scan(&baseline); e != nil {
				t.Fatal(e)
			}
			var saved struct {
				Issues map[string]any `json:"issues"`
			}
			if e = json.Unmarshal([]byte(baseline), &saved); e != nil {
				t.Fatal(e)
			}
			for _, i := range c.Issues {
				if i["state"] == "closed" && saved.Issues[listscope.IssueKey("owner/app", i["number"])] == nil {
					t.Fatalf("closed baseline missing %v", i)
				}
			}
			full, _, _ := scopeRequest(t, h, "/api/list")
			active, _, _ := scopeRequest(t, h, "/api/list?tabs=active")
			fullRowsForWire := scopeRows(full)
			for kind, rows := range c.Wire {
				for _, wire := range rows {
					key := "draft:" + fmt.Sprint(wire["id"])
					if kind == "issues" {
						key = fmt.Sprintf("issue:%v#%v", wire["project"], wire["number"])
					}
					if kind == "closes" {
						key = "closeRequest:" + fmt.Sprint(wire["id"])
					}
					row := fullRowsForWire[key]
					for field, value := range wire {
						if !reflect.DeepEqual(row[field], value) {
							t.Fatalf("wire %s.%s actual=%v expected=%v", key, field, row[field], value)
						}
					}
				}
			}
			want := []string{}
			for _, n := range c.ActiveIssues {
				want = append(want, fmt.Sprintf("issue:owner/app#%d", n))
			}
			for _, id := range c.ActiveDrafts {
				want = append(want, "draft:"+id)
			}
			for _, close := range c.Closes {
				if status := close["status"]; status == "reviewing" || status == "confirm" || status == "closing" {
					want = append(want, "closeRequest:"+fmt.Sprint(close["id"]))
				}
			}
			got := []string{}
			fullRows := scopeRows(full)
			for k, v := range scopeRows(active) {
				got = append(got, k)
				if !reflect.DeepEqual(v, fullRows[k]) {
					t.Fatalf("retained row changed %s: active=%v full=%v", k, v, fullRows[k])
				}
			}
			sort.Strings(got)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("active keys=%v want=%v", got, want)
			}
			if api.calls != 3 {
				t.Fatalf("Gitea all reads=%d want 3", api.calls)
			}
			after, e := ns.All(context.Background())
			if e != nil || !reflect.DeepEqual(assigned, after) {
				t.Fatalf("ledger changed %v %v", after, e)
			}
			var savedAfter string
			if e = app.Records.DB.QueryRow("SELECT json FROM settings WHERE id='closeReasonBaseline'").Scan(&savedAfter); e != nil || baseline != savedAfter {
				t.Fatalf("baseline changed err=%v", e)
			}
			corpus = append(corpus, map[string]any{"name": c.Name, "full": full, "active": active})
		})
	}
	t.Run("work decisions", func(t *testing.T) {
		c := scopeCase{Issues: []map[string]any{
			{"number": 901, "state": "open", "jobStatus": "held", "holdReason": "question", "jobResult": map[string]any{"status": "held", "questions": []any{}, "error": "実装が未完了です。原因を確認して再開してください"}},
			{"number": 902, "state": "open", "jobStatus": "held", "holdReason": "question", "jobResult": map[string]any{"status": "question", "questions": []any{"どちらにしますか"}}},
			{"number": 903, "state": "open", "jobStatus": "queued", "jobDependsOn": 904, "jobView": map[string]any{"dependsOnIssues": []any{map[string]any{"repo": "owner/app", "issue": 904}}}},
			{"number": 905, "state": "open", "jobStatus": "held", "holdReason": "question", "jobResult": map[string]any{"status": "question"}},
		}}
		_, _, _, _, h := setupScopeCase(t, c)
		full, _, _ := scopeRequest(t, h, "/api/list")
		active, _, _ := scopeRequest(t, h, "/api/list?tabs=active")
		work, _, _ := scopeRequest(t, h, "/api/list?view=work")
		corpus = append(corpus, map[string]any{"name": "work decisions", "full": full, "active": active, "work": work})
	})
	if len(corpus) == 0 {
		t.Skip("HTTP cases require isolated PostgreSQL")
	}
	raw, e := json.Marshal(corpus)
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.CommandContext(t.Context(), "node", "../../../client/test/list-scope-classification.test.ts", "--http-input")
	cmd.Stdin = bytes.NewReader(raw)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Client rowGroup disagrees with actual HTTP responses: %v\n%s", err, output)
	}
}

func TestListScopeHTTPResponseReduction(t *testing.T) {
	// Fixed representative history: 200 closed posted requests and 10 open ones.
	c := scopeCase{Name: "representative"}
	for n := 1; n <= 210; n++ {
		state := "closed"
		if n > 200 {
			state = "open"
		}
		c.Issues = append(c.Issues, map[string]any{"number": float64(n), "state": state})
		c.Drafts = append(c.Drafts, consultations.Record{"id": fmt.Sprintf("00000000-0000-4000-8000-%012d", n), "project": "owner/app", "status": "submitted", "issue": map[string]any{"number": n}})
	}
	app, _, _, lists, h := setupScopeCase(t, c)
	// Include a real saved alert so an empty or omitted auxiliary result cannot
	// satisfy the before/after comparison.
	if _, err := app.Records.DB.ExecContext(context.Background(), `INSERT INTO attention_cases(key,json) VALUES (?,?)`, "owner/app#1", `{"key":"owner/app#1","kind":"failed","state":"notify","project":"owner/app","number":1,"title":"保存済みの要対応","reason":"確認が必要"}`); err != nil {
		t.Fatal(err)
	}
	originals := map[string]screenapi.ListReader{"issue": lists["issue"], "draft": lists["draft"]}
	var baselineSize int
	var baselineAux any
	measure := func(label string, wantRows int) {
		for run := 0; run < 3; run++ {
			start := time.Now()
			body, size, timing := scopeRequest(t, h, "/api/list?tabs=active")
			if len(scopeRows(body)) != wantRows {
				t.Fatalf("%s rows=%d want=%d", label, len(scopeRows(body)), wantRows)
			}
			aux := body["aux"].(map[string]any)
			if _, ok := aux["attentionAlerts"]; !ok {
				t.Fatal("Attention pipeline unavailable")
			}
			alerts, ok := aux["attentionAlerts"].([]any)
			if !ok || len(alerts) != 1 || fleet.Object(alerts[0])["key"] != "owner/app#1" {
				t.Fatalf("saved Attention alert missing: %v", aux["attentionAlerts"])
			}
			if label == "before" {
				baselineSize = size
				baselineAux = aux
			} else if size >= baselineSize || !reflect.DeepEqual(aux, baselineAux) {
				t.Fatalf("active response: bytes=%d before=%d aux changed=%v", size, baselineSize, !reflect.DeepEqual(aux, baselineAux))
			}
			for _, stage := range []string{"issue_read;dur=", "draft_read;dur=", "attentionAlerts_read;dur=", "attentionAlerts_saved;dur=", "attentionAlerts_rework;dur=", "attentionAlerts_visible;dur="} {
				if !strings.Contains(timing, stage) {
					t.Fatalf("missing timing %s", stage)
				}
			}
			t.Logf("%s run=%d rows=%d decodedBytes=%d elapsedMs=%.3f timing=%s", label, run, len(scopeRows(body)), size, float64(time.Since(start))/float64(time.Millisecond), timing)
		}
	}
	for kind, read := range originals {
		lists[kind] = func(ctx context.Context, origin string) ([]map[string]any, error) {
			return read(listscope.WithScope(ctx, "full"), origin)
		}
	}
	measure("before", 420)
	for kind, read := range originals {
		lists[kind] = read
	}
	measure("after", 10)
}
