package startup

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/httpapi"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/store"
)

func TestOpenAppJobNotificationReachesIssueSSEPostgres(t *testing.T) {
	app := openLifecycleApp(t, lifecycleGitea, nil)
	ctx := t.Context()
	if err := app.SQLite.CreateSession(ctx, auth.Hash("job-notice-session"), auth.Session{CSRF: "fixture", ExpiresAt: time.Now().Add(time.Hour)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Mux)
	defer server.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "localhost:8787"
	request.AddCookie(&http.Cookie{Name: "desk_session", Value: "job-notice-session"})
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("SSE response: %s %+v", response.Status, response.Header)
	}
	// The initial flushed response confirms registration before the job is written.
	created, err := app.Jobs.Create(ctx, jobs.NewJob{Repo: "owner/repo", Issue: 1781, Kind: "normal", Scope: []string{"server/"}})
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(response.Body)
	await := func(status string) {
		t.Helper()
		for scanner.Scan() {
			if !strings.HasPrefix(scanner.Text(), "data: ") {
				continue
			}
			var event struct {
				Type    string `json:"type"`
				Project string `json:"project"`
				Number  int64  `json:"number"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			if event.Type != "issue" || event.Project != "owner/repo" || event.Number != 1781 {
				t.Fatalf("live.event rejected or changed target: %+v", event)
			}
			saved, err := app.Jobs.Get(ctx, created.Job.ID)
			if err != nil || saved.Repo != event.Project || saved.Issue != event.Number || saved.Status != status {
				t.Fatal(saved, err)
			}
			return
		}
		t.Fatalf("SSE ended without the committed issue: %v", scanner.Err())
	}
	await("queued")
	pc := "idk_" + strings.Repeat("n", 43)
	if _, err := app.SQLite.AddPCToken(ctx, store.PCTokenRecord{ID: "job-notice-worker", Name: "nas", Hash: auth.Hash(pc), IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB.ExecContext(ctx, "UPDATE jobs SET status='running',lease_host='nas',lease_epoch=1,lease_expires_at=$2 WHERE id=$1", created.Job.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Worker routes construct their own Store. Exercise that actual HTTP entry.
	report := httptest.NewRequest(http.MethodPost, fmt.Sprintf("http://localhost:8787/api/v1/jobs/%d/state", created.Job.ID), strings.NewReader(`{"epoch":1,"status":"running","pr":27,"result":{"summary":"保存済み"}}`))
	report.Header.Set("Authorization", "Bearer "+pc)
	report.Header.Set("Content-Type", "application/json")
	result := httptest.NewRecorder()
	app.Mux.ServeHTTP(result, report)
	if result.Code != http.StatusOK {
		t.Fatalf("worker report: %d %s", result.Code, result.Body.String())
	}
	await("running")
	// Maintenance constructs a third Store; it must publish expired leases too.
	if _, err := app.DB.ExecContext(ctx, "UPDATE jobs SET lease_expires_at=$2 WHERE id=$1", created.Job.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := httpapi.WorkerMaintenance(ctx, app.workerOptions()); err != nil {
		t.Fatal(err)
	}
	await("queued")
}
