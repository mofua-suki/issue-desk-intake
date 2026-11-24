package startup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/issueapi"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/numbers"
	"issue-desk-intake/server/internal/operations"
)

func TestIssueTrackingLifecycle(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	progress := now.Add(-time.Hour).Format(time.RFC3339Nano)
	hold := "question"
	branch := "work"
	head := strings.Repeat("a", 40)
	pr := int64(9)
	job := &jobs.Job{ID: 1, Repo: "owner/repo", Issue: 7, Title: "作業", Status: "running", ProgressAt: &progress, View: map[string]any{}, Result: map[string]any{}, UpdatedAt: "1"}
	ledger := map[string]string{}
	var notices []string
	var readErr, errorPull error
	var targetErr error
	stopPending := false
	var detailReads, pullReads int
	detail := map[string]any{}
	pull := map[string]any{"number": float64(9), "merged": true, "merge_commit_sha": strings.Repeat("b", 40), "head": map[string]any{"sha": head, "ref": branch, "repo": map[string]any{"full_name": "owner/repo"}}, "base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "owner/repo", "default_branch": "main"}}}
	tracker := func() *operations.IssueTracking {
		return &operations.IssueTracking{
			Service: &operations.Service{Now: func() time.Time { return now }},
			List: func(context.Context, string, []string, int, int) ([]*jobs.Job, int, error) {
				return []*jobs.Job{job}, 1, nil
			},
			ReadJob: func(context.Context, string, int64) (*jobs.Job, error) {
				if readErr != nil {
					return nil, readErr
				}
				j := *job
				return &j, nil
			},
			Detail: func(context.Context, string, int64) (map[string]any, error) { detailReads++; return detail, nil },
			ReadTarget: func(context.Context, string, int64) (map[string]any, error) {
				return map[string]any{"stopped": stopPending}, targetErr
			},
			PendingStop: func(context.Context, *jobs.Job) (bool, error) { return stopPending, nil },
			Pull:        func(context.Context, string, int64) (map[string]any, error) { pullReads++; return pull, errorPull },
			Reserve: func(_ context.Context, j *jobs.Job, b []byte) (bool, error) {
				k := fmt.Sprintf("%s#%d", j.Repo, j.Issue)
				v := string(b)
				if ledger[k] == v {
					return false, nil
				}
				ledger[k] = v
				return true, nil
			},
			Notify: func(_ context.Context, n map[string]any) (map[string]any, error) {
				notices = append(notices, fmt.Sprint(n["title"], " ", n["body"]))
				return nil, nil
			},
		}
	}
	run := func() {
		t.Helper()
		if err := tracker().Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	state := func() string {
		t.Helper()
		var v map[string]any
		if err := json.Unmarshal([]byte(ledger["owner/repo#7"]), &v); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(v["state"])
	}
	run()
	if state() != "stalled" || len(notices) != 1 || !strings.Contains(notices[0], "60分") || !strings.Contains(notices[0], "受付") || detailReads != 0 || pullReads != 0 {
		t.Fatal(ledger, notices, detailReads, pullReads)
	}
	run()
	if len(notices) != 1 {
		t.Fatal(notices)
	}
	job.Status = "held"
	job.HoldReason = &hold
	job.Generation++
	job.UpdatedAt = "2"
	job.Result = map[string]any{"reason": "質問があります"}
	detail = map[string]any{"requiresUser": true, "nextAction": "質問に回答する"}
	run()
	if state() != "waiting" || len(notices) != 2 || !strings.Contains(notices[1], "利用者") || !strings.Contains(notices[1], "回答") {
		t.Fatal(ledger, notices)
	}
	job.Status = "running"
	job.HoldReason = nil
	job.ProgressAt = nil
	job.UpdatedAt = "3"
	run()
	if state() != "running" {
		t.Fatal(ledger)
	}
	job.Status = "stopped"
	job.UpdatedAt = "4"
	detail = map[string]any{"error": "利用者が停止", "requiresUser": true}
	run()
	if state() != "stopped" || !strings.Contains(notices[len(notices)-1], "利用者が停止") {
		t.Fatal(ledger, notices)
	}
	before := ledger["owner/repo#7"]
	readErr = errors.New("読取失敗")
	if err := tracker().Reconcile(ctx); err == nil || ledger["owner/repo#7"] != before {
		t.Fatal(err, ledger)
	}
	readErr = nil
	host := "worker"
	job.Status = "running"
	job.LeaseHost = &host
	job.UpdatedAt = "4a"
	stopPending = true
	run()
	if state() != "stopped" {
		t.Fatal(ledger)
	}
	stopPending = false
	job.LeaseHost = nil
	job.Status = "merged"
	job.UpdatedAt = "5"
	run()
	if state() != "unconfirmed" {
		t.Fatal(ledger)
	}
	job.PR = &pr
	job.HeadSHA = &head
	job.Branch = &branch
	job.LeaseHost = &host
	job.UpdatedAt = "6"
	stopPending = true
	run()
	if state() != "stopped" || pullReads != 0 {
		t.Fatal(ledger, pullReads)
	}
	stopPending = false
	targetErr = errors.New("停止指示を読めません")
	before = ledger["owner/repo#7"]
	if err := tracker().Reconcile(ctx); err == nil || ledger["owner/repo#7"] != before {
		t.Fatal(err, ledger)
	}
	targetErr = nil
	errorPull = errors.New("取得失敗")
	before = ledger["owner/repo#7"]
	if err := tracker().Reconcile(ctx); err == nil || ledger["owner/repo#7"] != before {
		t.Fatal(err, ledger)
	}
	errorPull = nil
	pull["number"] = float64(10)
	run()
	if state() != "unconfirmed" {
		t.Fatal(ledger)
	}
	pull["number"] = float64(9)
	h := pull["head"].(map[string]any)
	h["sha"] = strings.Repeat("c", 40)
	run()
	if state() != "unconfirmed" {
		t.Fatal(ledger)
	}
	h["sha"] = head
	run()
	if state() != "merged" {
		t.Fatal(ledger)
	}
	job.Status = "closed"
	job.UpdatedAt = "7"
	detail = map[string]any{"state": "closed", "closeReason": map[string]any{"reason": "unknown"}}
	run()
	if state() != "unconfirmed" {
		t.Fatal(ledger)
	}
	detail = map[string]any{"state": "closed", "closeReason": map[string]any{"reason": "voluntary", "source": "manual"}}
	run()
	if state() != "withdrawn" {
		t.Fatal(ledger)
	}
	t.Run("persistent_stop_and_restart", trackingPersistentStopAndRestart)
	t.Run("wait_limit_and_recovery", trackingWaitLimitAndRecovery)
}

func trackingWaitLimitAndRecovery(t *testing.T) {
	ctx := t.Context()
	a := reviewNotesApp(t)
	now := time.Now().UTC()
	store, err := jobs.New(a.DB, jobs.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	var permissionID, limitID int64
	err = a.DB.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,title,status,hold_reason,view,result,updated_at) VALUES('owner/repo',81,'権限','held','review_held','{"permissionHold":{"v":1},"reviewHolds":0}','{"summary":"review"}',$1) RETURNING id`, now.Add(-time.Hour)).Scan(&permissionID)
	if err != nil {
		t.Fatal(err)
	}
	err = a.DB.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,title,status,hold_reason,generation,view,result,updated_at) VALUES('owner/repo',82,'上限','held','question',3,'{"workerExecution":{"implementationRuns":4}}','{"questions":["why"]}',$1) RETURNING id`, now.Add(-time.Hour)).Scan(&limitID)
	if err != nil {
		t.Fatal(err)
	}
	service := &operations.Service{PostgreSQL: a.DB, DB: a.SQLite.DB(), Now: func() time.Time { return now }, Repositories: func() ([]string, error) { return []string{"owner/repo"}, nil }}
	recovery := &operations.JobRecovery{Service: service, MinAge: time.Minute, Limit: 3, RunLimit: 4, ReviewLimit: 4, UsageLimit: 8000000}
	notices := 0
	tracker := &operations.IssueTracking{Service: service, List: store.List, ReadJob: store.ByIssue,
		Detail: func(ctx context.Context, repo string, issue int64) (map[string]any, error) {
			j, e := store.ByIssue(ctx, repo, issue)
			if e != nil {
				return nil, e
			}
			waits, e := jobs.UserWaits(j, 4, false)
			question := j.Status == "held" && j.HoldReason != nil && *j.HoldReason == "question"
			return map[string]any{"requiresUser": len(waits) > 0 || question, "nextAction": "利用者の判断を確認する"}, e
		}, Pull: func(context.Context, string, int64) (map[string]any, error) {
			return nil, errors.New("unexpected PR read")
		},
		Notify: func(context.Context, map[string]any) (map[string]any, error) { notices++; return nil, nil }}
	if err = tracker.Reconcile(ctx); err != nil || notices != 2 {
		t.Fatal(err, notices)
	}
	for _, id := range []int64{permissionID, limitID} {
		job, e := store.Get(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		waits, e := jobs.UserWaits(job, 4, false)
		if e != nil || len(waits) == 0 {
			t.Fatal(id, waits, e)
		}
	}
	acts := 0
	err = runRecoveryCycle(ctx, func(context.Context) error { return nil }, recovery.Candidates, func(context.Context, operations.RecoveryCandidate) (operations.RecoveryResult, error) {
		acts++
		return operations.Acted, nil
	})
	if err != nil || acts != 0 {
		t.Fatal(err, acts)
	}
	retry := jobs.ControlInput{ID: "22222222-2222-4222-8222-222222222222", Action: "retry", By: "token:worker", Text: "原因を確認して再試行"}
	if _, err = store.Control(ctx, limitID, retry, jobs.ControlOptions{RunLimit: 4, UsageLimit: 8000000}); err == nil {
		t.Fatal("上限の承認なしで再試行しました")
	}
	retry.OverLimitStatement = "共通番号82の原因を確認し、この再試行を承認します"
	job, err := store.Control(ctx, limitID, retry, jobs.ControlOptions{RunLimit: 4, UsageLimit: 8000000})
	if err != nil || job.Status != "queued" {
		t.Fatal(job, err)
	}
	if err = tracker.Reconcile(ctx); err != nil || notices != 2 {
		t.Fatal(err, notices)
	}
	var resumed int
	if err = a.DB.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key LIKE 'issue-tracking:%' AND value->>'state'='queued' AND value->>'generation'='4'`).Scan(&resumed); err != nil || resumed != 1 {
		t.Fatal(err, resumed)
	}
	permission, err := store.Get(ctx, permissionID)
	if err != nil || permission.Status != "held" {
		t.Fatal(permission, err)
	}
	var questionID int64
	err = a.DB.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,title,status,hold_reason,result,updated_at) VALUES('owner/repo',83,'質問','held','question','{"questions":["確認事項"]}',$1) RETURNING id`, now.Add(-time.Hour)).Scan(&questionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = tracker.Reconcile(ctx); err != nil || notices != 3 {
		t.Fatal(err, notices)
	}
	answer := jobs.ControlInput{ID: "33333333-3333-4333-8333-333333333333", Action: "instruction", By: "issue-screen", Text: "確認事項への回答です"}
	question, err := store.Control(ctx, questionID, answer, jobs.ControlOptions{RunLimit: 4, UsageLimit: 8000000})
	if err != nil || question.Status != "queued" {
		t.Fatal(question, err)
	}
	if err = tracker.Reconcile(ctx); err != nil || notices != 3 {
		t.Fatal(err, notices)
	}
}

func trackingPersistentStopAndRestart(t *testing.T) {
	ctx := t.Context()
	a := reviewNotesApp(t)
	db := a.DB
	now := time.Now().UTC()
	store, err := jobs.New(db, jobs.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	err = db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,title,status,lease_host,lease_epoch,lease_expires_at,progress_at) VALUES('owner/repo',1,'追跡','running','worker',1,$1,$2) RETURNING id`, now.Add(time.Hour), now.Add(-2*time.Hour)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	nums, err := numbers.OpenLedger(ctx, a.SQLite.DB(), filepath.Join(t.TempDir(), "numbers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	issueAPI := &recoveryIssueAPI{entered: make(chan struct{}), release: make(chan struct{})}
	close(issueAPI.release)
	issues, err := issueapi.Register(http.NewServeMux(), issueapi.Options{AppDB: a.SQLite.DB(), Postgres: db, Auth: &auth.Authenticator{}, Gitea: issueAPI, Numbers: nums, Jobs: store, Config: map[string]any{"workerId": "nas"}, Project: func(string) error { return nil }, Own: func(user map[string]any) bool { return user["login"] == "intake" }})
	if err != nil {
		t.Fatal(err)
	}
	reads, sends := 0, 0
	stop := jobs.ControlInput{ID: "11111111-1111-4111-8111-111111111111", Action: "stop", By: "issue-screen"}
	service := &operations.Service{PostgreSQL: db, Now: func() time.Time { return now }, Repositories: func() ([]string, error) { return []string{"owner/repo"}, nil }}
	tracker := func() *operations.IssueTracking {
		return &operations.IssueTracking{Service: service, List: store.List, ReadJob: func(ctx context.Context, repo string, n int64) (*jobs.Job, error) {
			job, e := store.ByIssue(ctx, repo, n)
			reads++
			if e == nil && reads == 2 {
				_, e = store.Control(ctx, id, stop, jobs.ControlOptions{})
			}
			return job, e
		}, Detail: func(context.Context, string, int64) (map[string]any, error) { return map[string]any{}, nil },
			PendingStop: func(ctx context.Context, j *jobs.Job) (bool, error) {
				var pending bool
				e := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_commands WHERE repo=$1 AND issue=$2 AND target_host=$3 AND kind='stop' AND status IN ('queued','acked'))`, j.Repo, j.Issue, *j.LeaseHost).Scan(&pending)
				return pending, e
			},
			ReadTarget: issues.ReadIncidentTarget,
			Pull: func(context.Context, string, int64) (map[string]any, error) {
				return nil, errors.New("unexpected PR read")
			},
			Notify: func(context.Context, map[string]any) (map[string]any, error) {
				sends++
				if sends == 1 {
					return map[string]any{"sent": 0, "failed": 1, "subscriptions": 1}, nil
				}
				return map[string]any{"sent": 1, "failed": 0, "subscriptions": 1}, nil
			},
		}
	}
	if err = tracker().Reconcile(ctx); err == nil || sends != 0 {
		t.Fatal(err, sends)
	}
	var reservations int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key LIKE 'issue-tracking:%'`).Scan(&reservations); err != nil || reservations != 0 {
		t.Fatal(err, reservations)
	}
	if err = tracker().Reconcile(ctx); err == nil || sends != 1 {
		t.Fatal(err, sends)
	}
	if err = tracker().Reconcile(ctx); err != nil || sends != 2 {
		t.Fatal(err, sends)
	}
	if err = tracker().Reconcile(ctx); err != nil || sends != 2 {
		t.Fatal(err, sends)
	}
}
