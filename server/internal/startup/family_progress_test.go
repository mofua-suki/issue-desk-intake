package startup

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/screenapi"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/numbers"
)

func TestPostgresFamilyJobsProjectsStoredJobs(t *testing.T) {
	db := appPostgres(t)
	ctx := context.Background()
	store, err := jobs.New(db, jobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ns, err := numbers.OpenLedger(ctx, db, filepath.Join(t.TempDir(), "state-desk-numbers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO desk_numbers(number,project,gitea,assigned_at) VALUES(42,'owner/repo',1,'2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO jobs(repo,issue,status,result) VALUES('owner/repo',1,'failed','{"error":"failure detail"}')`); err != nil {
		t.Fatal(err)
	}
	a := &App{Jobs: store, Numbers: ns}
	got, err := a.familyJobs(ctx, []consultations.FamilyIssue{{Project: "owner/repo", Number: 1}})
	if err != nil || len(got) != 1 {
		t.Fatalf("jobs=%v err=%v", got, err)
	}
	if got[0].Project != "owner/repo" || got[0].Number != 1 || got[0].Desk != int64(42) || got[0].Status != "failed" || got[0].Reason != "failure detail" {
		t.Fatalf("job=%+v", got[0])
	}
}

func TestFamilyIssueHistoryScreenListAndDetailPostgres(t *testing.T) {
	env, app, _, _, _ := screenFixture(t)
	ctx := context.Background()
	calls := 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v1/repos/mofua-suki/issue-desk/issues" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"number":965,"state":"closed"},{"number":966,"state":"open"}]`))
	}))
	defer fake.Close()
	token := filepath.Join(t.TempDir(), "fake-token")
	if err := os.WriteFile(token, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New(fake.URL, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	app.Gitea = client
	app.Consultations, err = consultations.New(consultations.Options{Records: app.Records, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }, FamilyJobs: func(context.Context, []consultations.FamilyIssue) ([]consultations.FamilyJob, error) { return nil, nil }, FamilyIssues: app.familyIssues})
	if err != nil {
		t.Fatal(err)
	}
	parent := "00000000-0000-4000-8000-000000000001"
	data := []string{`{"id":"00000000-0000-4000-8000-000000000001","status":"submitted","splitChildren":["00000000-0000-4000-8000-000000000002","00000000-0000-4000-8000-000000000003","missing"],"splitIssues":[{},{},{}]}`, `{"id":"00000000-0000-4000-8000-000000000002","status":"submitted","parentSplit":"` + parent + `","project":"mofua-suki/issue-desk","issue":{"number":965}}`, `{"id":"00000000-0000-4000-8000-000000000003","status":"submitted","parentSplit":"` + parent + `","project":"mofua-suki/issue-desk","issue":{"number":966}}`}
	for _, raw := range data {
		var v map[string]any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		if _, err := env.DB.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES (?,?)", v["id"], raw); err != nil {
			t.Fatal(err)
		}
	}
	lists := map[string]screenapi.ListReader{}
	entries := map[string]screenapi.EntryReader{}
	app.registerConsultationReaders(lists, entries)
	rows, err := lists["draft"](ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	detail, err := entries["draft"](ctx, map[string]any{"id": parent}, "")
	if err != nil {
		t.Fatal(err)
	}
	var list map[string]any
	for _, row := range rows {
		if row["id"] == parent {
			list = row
		}
	}
	if list == nil || !reflect.DeepEqual(list["familyProgress"], detail["familyProgress"]) {
		t.Fatalf("list=%v detail=%v", list, detail)
	}
	progress := detail["familyProgress"].(consultations.Record)
	if progress["closed"] != 1 || progress["remaining"] != 1 || progress["unknown"] != 1 {
		t.Fatal(progress)
	}
	if calls != 2 {
		t.Fatalf("repository requests=%d", calls)
	}
}

func TestFamilyIssueHistoryNotFoundDoesNotRetry(t *testing.T) {
	calls := 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.NotFound(w, r) }))
	defer fake.Close()
	token := filepath.Join(t.TempDir(), "fake-token")
	if err := os.WriteFile(token, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New(fake.URL, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Gitea: client}
	if _, err = app.familyIssues(context.Background(), "mofua-suki/issue-desk"); err == nil || calls != 1 {
		t.Fatalf("error=%v requests=%d", err, calls)
	}
}
