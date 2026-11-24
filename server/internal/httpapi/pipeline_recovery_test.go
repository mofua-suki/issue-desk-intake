package httpapi

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/jobs"
)

// Only the external Gitea boundary is replaced. Receipt recovery, filing,
// dispatch, fencing, merge turns and terminal report receipts use real stores
// and the production HTTP routes. This does not emulate Worker internals.
type pipelineIssueAPI struct {
	mu    sync.Mutex
	issue map[string]any
	posts int
}

func (a *pipelineIssueAPI) Issues(context.Context, string, string) ([]map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.issue == nil {
		return []map[string]any{}, nil
	}
	return []map[string]any{a.issue}, nil
}
func (a *pipelineIssueAPI) Issue(context.Context, string, int64) (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.issue, nil
}
func (a *pipelineIssueAPI) CreateIssue(_ context.Context, _ string, input any) (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.posts++
	body := fleet.Object(input)
	a.issue = map[string]any{"number": int64(1), "state": "open", "title": body["title"], "body": body["body"], "user": map[string]any{"login": "intake"}}
	// The external side committed, but the sender never received its response.
	return nil, &gitea.Error{UnknownOutcome: true}
}

func TestConsultationFilingDispatchAndMergeRecoverAcrossLostResponses(t *testing.T) {
	ctx := t.Context()
	now := time.Now()
	var options WorkerRouteOptions
	var merged atomic.Int32
	_, db, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		o.Now = func() time.Time { return now }
		o.OnMerged = func(context.Context, *sql.Tx, *jobs.Job, map[string]any) error { merged.Add(1); return nil }
		options = *o
	})
	records, err := consultations.OpenRecords(ctx, options.AppDB)
	if err != nil {
		t.Fatal(err)
	}
	external := &pipelineIssueAPI{}
	const id = "11111111-1111-4111-8111-111111111111"
	var aiCalls atomic.Int32
	var engine *consultations.Engine
	// Rebuilding both route sets and the engine drops process-local state while
	// retaining only the same databases, credentials and external Issue.
	rebuild := func(start bool) http.Handler {
		t.Helper()
		queue, e := jobs.New(db, jobs.Options{Now: options.Now})
		if e != nil {
			t.Fatal(e)
		}
		filer, e := filing.New(ctx, filing.Options{Records: records, API: external, Jobs: queue, Now: options.Now,
			Project: func(string) error { return nil }, Own: func(u map[string]any) bool { return u["login"] == "intake" },
			Build: func(_ context.Context, d filing.Record) (filing.Prepared, error) {
				return filing.Prepared{Title: "回復試験", Body: "本文\n<!-- issue-desk:" + fleet.Text(d["id"]) + " -->", Job: jobs.NewJob{Scope: []string{"server/a.go"}}}, nil
			},
			Check: func(context.Context, filing.Record) error { return nil }, Link: func(string, int64) string { return "/fixture" },
		})
		if e != nil {
			t.Fatal(e)
		}
		service, e := consultations.New(consultations.Options{Records: records, Auth: options.Auth, Project: func(string) error { return nil }, Submit: filer.Submit, SubmissionQueued: func(d consultations.Record) {
			if engine != nil {
				engine.SubmitQueued(d)
			}
		}})
		if e != nil {
			t.Fatal(e)
		}
		mux := http.NewServeMux()
		if e = RegisterWorkerRoutes(mux, options); e != nil {
			t.Fatal(e)
		}
		service.RegisterDrafts(mux)
		if start {
			engine, e = consultations.NewEngine(ctx, consultations.EngineOptions{Service: service,
				Principal: func(context.Context, string) (auth.Principal, error) {
					return auth.Principal{Kind: auth.PC, TokenID: "test-token", Name: "nas", Delegated: true}, nil
				},
				Turn: func(context.Context, consultations.Record) (consultations.Record, error) {
					aiCalls.Add(1)
					return consultations.Record{"title": "回復試験", "body": "本文", "ready": true, "questions": []any{}}, nil
				},
				Submit: func(c context.Context, d consultations.Record) (consultations.Record, error) {
					return filer.SubmitAccepted(c, fleet.Text(d["id"]), consultations.Record{"revision": d["revision"]}, auth.Principal{Kind: auth.PC, TokenID: "test-token", Delegated: true})
				},
			})
			if e != nil {
				t.Fatal(e)
			}
			service.BindEngine(engine)
			if e = engine.Recover(ctx); e != nil {
				t.Fatal(e)
			}
		}
		return mux
	}
	handler := rebuild(false)
	body := `{"id":"` + id + `","project":"owner/repo","text":"回復してマージまで進める","tier":"light"}`
	// Lose the acceptance response; no AI owner exists yet.
	if status, _ := workerRequest(t, handler, token, "POST", "/api/issues", body); status != 202 {
		t.Fatal(status)
	}
	handler = rebuild(true)
	t.Cleanup(func() {
		if engine != nil {
			engine.Close()
		}
	})
	if status, _ := workerRequest(t, handler, token, "POST", "/api/issues", body); status != 202 {
		t.Fatal("受付再送", status)
	}
	await := func(status string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			d, ok, e := records.Get(ctx, "drafts", id)
			if e != nil {
				t.Fatal(e)
			}
			if ok && d["status"] == status && !engine.Busy(id) {
				return
			}
			time.Sleep(time.Millisecond)
		}
		d, _, _ := records.Get(ctx, "drafts", id)
		t.Fatalf("相談が%sに到達しない: %v", status, d)
	}
	await("submit_unknown")
	engine.Close()
	handler = rebuild(true)
	draft, _, err := records.Get(ctx, "drafts", id)
	if err != nil {
		t.Fatal(err)
	}
	if status, value := workerRequest(t, handler, token, "POST", "/api/drafts/"+id+"/submit", fmt.Sprintf(`{"revision":%d}`, int64(fleet.Number(draft["revision"])))); status != 202 {
		t.Fatal(status, value)
	}
	await("submitted")
	engine.Shutdown(ctx)
	engine = nil
	external.mu.Lock()
	posts := external.posts
	external.mu.Unlock()
	if posts != 1 || aiCalls.Load() != 1 {
		t.Fatal("復旧時に外部副作用またはAIを重複実行", posts, aiCalls.Load())
	}
	var receipts, count int
	if err = options.AppDB.QueryRow("SELECT count(*) FROM intake_receipts").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT count(*) FROM jobs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || count != 1 {
		t.Fatal(receipts, count)
	}
	request := func(method, path, body string, want int) map[string]any {
		t.Helper()
		status, v := workerRequest(t, handler, token, method, path, body)
		if status != want {
			t.Fatalf("%s %s: %d %v", method, path, status, v)
		}
		return v
	}
	heartbeat := func() { request("PUT", "/api/v1/hosts/nas", `{"ready":true,"capacity":2}`, 200) }
	heartbeat()
	taken := request("POST", "/api/v1/jobs/next", `{"capacity":2}`, 200)
	job := fleet.Object(taken["job"])
	jobID := int64(fleet.Number(job["id"]))
	epoch := int64(fleet.Number(fleet.Object(taken["lease"])["epoch"]))
	path := "/api/v1/jobs/" + fmtInt(jobID)
	request("POST", "/api/v1/merge-turns/owner/repo/acquire", fmt.Sprintf(`{"jobId":%d,"epoch":%d}`, jobID, epoch), 200)
	// A Worker disappears after claiming. Expiry is exercised through the real
	// sweeper, not by rewriting job state or bypassing its fencing rules.
	now = now.Add(time.Hour)
	queue, err := jobs.New(db, jobs.Options{Now: options.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err = queue.Sweep(ctx, nil); err != nil {
		t.Fatal(err)
	}
	handler = rebuild(false)
	heartbeat()
	taken = request("POST", "/api/v1/jobs/next", `{"capacity":2}`, 200)
	nextEpoch := int64(fleet.Number(fleet.Object(taken["lease"])["epoch"]))
	if nextEpoch <= epoch || int64(fleet.Number(fleet.Object(taken["job"])["id"])) != jobID {
		t.Fatal("同じ仕事を新しいleaseで再開しない", taken)
	}
	late := request("POST", path+"/state", fmt.Sprintf(`{"epoch":%d,"status":"merged","result":{"status":"completed"}}`, epoch), 409)
	if late["code"] != "lease_lost" || merged.Load() != 0 {
		t.Fatal("古い担当の結果が反映された", late)
	}
	request("POST", path+"/lease", fmt.Sprintf(`{"epoch":%d}`, epoch), 409)
	request("POST", "/api/v1/merge-turns/owner/repo/acquire", fmt.Sprintf(`{"jobId":%d,"epoch":%d}`, jobID, epoch), 409)
	request("POST", "/api/v1/merge-turns/owner/repo/acquire", fmt.Sprintf(`{"jobId":%d,"epoch":%d}`, jobID, nextEpoch), 200)
	result := fmt.Sprintf(`{"epoch":%d,"status":"merged","headSha":"%s","pr":7,"result":{"status":"completed","mergeCommit":"%s"}}`, nextEpoch, strings.Repeat("a", 40), strings.Repeat("a", 40))
	// Lose the terminal merge report response, rebuild, then deliver it again.
	request("POST", path+"/state", result, 200)
	handler = rebuild(false)
	replay := request("POST", path+"/state", result, 200)
	if fleet.Object(replay["job"])["status"] != "merged" || merged.Load() != 1 {
		t.Fatal("終端結果の再送が副作用を再実行", replay, merged.Load())
	}
	if err = db.QueryRow("SELECT count(*) FROM jobs WHERE status='merged'").Scan(&count); err != nil || count != 1 {
		t.Fatal(err, count)
	}
}
