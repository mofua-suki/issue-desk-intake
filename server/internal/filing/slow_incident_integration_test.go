package filing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/operations"
	"issue-desk-intake/server/internal/statistics"
	"issue-desk-intake/server/internal/store"
)

func slowIntegration(t *testing.T) (*Service, *memoryAPI, *operations.Service, *operations.IncidentActions, *bool) {
	t.Helper()
	sender, api := fixture(t)
	ctx := context.Background()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := store.OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "slow_ops_" + strings.ReplaceAll(id, "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pg, err := store.OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Migrate(ctx, pg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pg.Close()
		admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	ops := &operations.Service{DB: sender.options.Records.DB, PostgreSQL: pg, IncludeArchived: true, RuntimeDir: t.TempDir()}
	ops.Assignments = &fleet.Assignments{Output: map[string]any{}}
	ops.Statistics = &statistics.Service{DB: pg, AppDB: ops.DB, Assignments: ops.Assignments, RuntimeDir: ops.RuntimeDir}
	row := map[string]any{"id": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "key": "slow-work-1", "kind": "slow", "state": "expired", "title": "遅延", "target": map[string]any{"project": "owner/app", "number": 42, "workKey": "work-1", "elapsedSeconds": 120}, "history": []any{}}
	raw, err := json.Marshal([]any{row})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pg.ExecContext(ctx, "INSERT INTO settings(key,value,updated_by) VALUES('incident-deadline',$1::jsonb,'test')", string(raw)); err != nil {
		t.Fatal(err)
	}
	stopped := false
	a := &operations.IncidentActions{Service: ops, ReadTarget: func(context.Context, string, int64) (map[string]any, error) {
		return map[string]any{"project": "owner/app", "number": 42, "stopped": stopped}, nil
	}}
	sender.options.IncidentLedger = func(ctx context.Context) (any, error) { return ops.OperationalSetting(ctx, "incident-deadline") }
	return sender, api, ops, a, &stopped
}

func slowJudge(t *testing.T, a *operations.IncidentActions, direct bool) *operations.SlowIncidentFiling {
	t.Helper()
	f, err := a.JudgeSlowIncident(context.Background(), "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", func(context.Context, map[string]any) (operations.SlowIncidentDecision, error) {
		return operations.SlowIncidentDecision{Problem: true, Cause: "defect", DirectPR: direct}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func slowRecord(t *testing.T, s *operations.Service) map[string]any {
	t.Helper()
	value, err := s.OperationalSetting(context.Background(), "incident-deadline")
	if err != nil {
		t.Fatal(err)
	}
	return fleet.Object(fleet.List(value)[0])
}

func TestSlowIncidentJudgementAndRecovery(t *testing.T) {
	for _, mode := range []string{"success", "failed", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			sender, api, ops, actions, _ := slowIntegration(t)
			var rejection *countedSlowRejection
			if mode == "failed" {
				rejection = &countedSlowRejection{memoryAPI: api}
				sender.options.API = rejection
			}
			if mode == "unknown" {
				api.uncertain = true
				api.lose = true
			}
			f := slowJudge(t, actions, true)
			issue, err := actions.FileJudgedSlowIncident(ctx, f, sender)
			if (err == nil) != (mode == "success") {
				t.Fatalf("issue=%v err=%v", issue, err)
			}
			before := api.posts
			_, _ = actions.FileJudgedSlowIncident(ctx, f, sender)
			if api.posts != before {
				t.Fatalf("再送 posts=%d before=%d", api.posts, before)
			}
			if rejection != nil && rejection.attempts != 1 {
				t.Fatalf("失敗した送信の試行回数=%d", rejection.attempts)
			}
			record := slowRecord(t, ops)
			filing := fleet.Object(record["slowFiling"])
			want := map[string]string{"success": "success", "failed": "failed", "unknown": "unknown"}[mode]
			if filing["result"] != want || filing["id"] != operations.SlowIncidentSubmissionID(f.IncidentID, f.Cause) {
				t.Fatal(filing)
			}
			count := 0
			for _, h := range fleet.List(record["history"]) {
				if fleet.Object(h)["event"] == "起票結果" {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("history=%v", record["history"])
			}
			if mode == "success" {
				if api.posts != 1 {
					t.Fatal(api.posts)
				}
				body := fleet.Text(issue["body"])
				if !strings.Contains(body, "直接PRが要る") || !strings.Contains(body, "work-1") || !strings.Contains(body, "120秒") {
					t.Fatal(body)
				}
				job, e := JobFromIssue("owner/app", issue, "")
				if e != nil || job.Priority != 1 {
					t.Fatalf("job=%+v error=%v body=%s", job, e, body)
				}
				marker := DraftID(body)
				tail := strings.Split(body, "<!-- issue-desk:"+marker+" -->")
				if len(tail) != 2 {
					t.Fatal(body)
				}
				settings := settingsTrailer.FindStringSubmatch(tail[1])
				if len(settings) != 2 {
					t.Fatal(body)
				}
				var meta map[string]any
				if e = json.Unmarshal([]byte(settings[1]), &meta); e != nil || meta["priority"] != "high" || !strings.Contains(fmt.Sprint(meta["priorityReason"]), "work-1") || !strings.Contains(fmt.Sprint(meta["priorityReason"]), "120秒") {
					t.Fatal(meta, e)
				}
			}
			alerts, e := ops.Attention(ctx)
			if e != nil {
				t.Fatal(e)
			}
			count = 0
			for _, v := range alerts {
				r := fleet.Object(v)
				if r["id"] == f.IncidentID {
					count++
					if !strings.Contains(fmt.Sprint(r["reason"]), "直接PR") {
						t.Fatal(r)
					}
				}
			}
			if count != 1 {
				t.Fatalf("alerts=%v", alerts)
			}
			monitor := operations.IncidentMonitor{Service: ops, Sources: func(context.Context) ([]operations.IncidentSource, error) { return nil, nil }}
			if e := monitor.Reconcile(ctx); e != nil {
				t.Fatal(e)
			}
			after := slowRecord(t, ops)
			if after["endedAt"] == nil || after["acknowledgedAt"] != nil {
				t.Fatal(after)
			}
			alerts, e = ops.Attention(ctx)
			if e != nil {
				t.Fatal(e)
			}
			count = 0
			for _, v := range alerts {
				r := fleet.Object(v)
				if r["id"] == f.IncidentID && strings.Contains(fmt.Sprint(r["reason"]), "直接PR") {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("ended alerts=%v", alerts)
			}
		})
	}
}

type countedSlowRejection struct {
	*memoryAPI
	attempts int
}

func (a *countedSlowRejection) CreateIssue(context.Context, string, any) (map[string]any, error) {
	a.attempts++
	return nil, &gitea.Error{Status: 403}
}

func slowIncidentFilingRecoveryIntegrated(t *testing.T) {
	ctx := context.Background()
	sender, api, ops, actions, _ := slowIntegration(t)
	api.uncertain = true
	api.lose = true
	f := slowJudge(t, actions, false)
	if _, err := actions.FileJudgedSlowIncident(ctx, f, sender); err == nil {
		t.Fatal("missing unknown error")
	}
	restarted, err := New(ctx, sender.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = actions.FileJudgedSlowIncident(ctx, f, restarted); err == nil || api.posts != 1 {
		t.Fatalf("err=%v posts=%d", err, api.posts)
	}
	monitor := operations.IncidentMonitor{Service: ops, Sources: func(context.Context) ([]operations.IncidentSource, error) { return nil, nil }}
	if err = monitor.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	record := slowRecord(t, ops)
	if record["endedAt"] == nil || record["acknowledgedAt"] != nil {
		t.Fatal(record)
	}
	alerts, err := ops.Attention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, v := range alerts {
		r := fleet.Object(v)
		if r["id"] == f.IncidentID {
			count++
			if !strings.Contains(fmt.Sprint(r["reason"]), "確認できません") {
				t.Fatal(r)
			}
		}
	}
	if count != 1 {
		t.Fatalf("alerts=%v", alerts)
	}
}

func slowIncidentFilingBoundaryIntegrated(t *testing.T) {
	ctx := context.Background()
	sender, api, _, actions, stopped := slowIntegration(t)
	f := slowJudge(t, actions, false)
	*stopped = true
	if _, err := actions.FileJudgedSlowIncident(ctx, f, sender); err == nil || api.posts != 0 {
		t.Fatalf("err=%v posts=%d", err, api.posts)
	}
}

func TestSlowIncidentPostBoundaryAndReservationFailure(t *testing.T) {
	t.Run("stopped after intent persisted", func(t *testing.T) {
		ctx := context.Background()
		sender, api, ops, actions, _ := slowIntegration(t)
		f := slowJudge(t, actions, false)
		reads := 0
		actions.ReadTarget = func(context.Context, string, int64) (map[string]any, error) {
			reads++
			return map[string]any{"project": "owner/app", "number": 42, "stopped": reads >= 4}, nil
		}
		if _, err := actions.FileJudgedSlowIncident(ctx, f, sender); err == nil || api.posts != 0 {
			t.Fatalf("err=%v reads=%d posts=%d", err, reads, api.posts)
		}
		if fleet.Object(slowRecord(t, ops)["slowFiling"])["result"] != "failed" {
			t.Fatal(slowRecord(t, ops))
		}
		if _, err := actions.RecoverSlowIncidentFiling(ctx, f.IncidentID, sender); err == nil || api.posts != 0 {
			t.Fatalf("err=%v posts=%d", err, api.posts)
		}
	})
	t.Run("reservation save failure", func(t *testing.T) {
		ctx := context.Background()
		sender, api, ops, actions, _ := slowIntegration(t)
		f := slowJudge(t, actions, false)
		if _, err := ops.PostgreSQL.ExecContext(ctx, `ALTER TABLE settings ADD CONSTRAINT reject_slow_filing CHECK (value::text NOT LIKE '%slowFiling%')`); err != nil {
			t.Fatal(err)
		}
		if _, err := actions.FileJudgedSlowIncident(ctx, f, sender); err == nil || api.posts != 0 {
			t.Fatalf("err=%v posts=%d", err, api.posts)
		}
	})
}

func TestSlowIncidentRecoveryAfterLostResponse(t *testing.T) {
	ctx := context.Background()
	sender, api, ops, actions, _ := slowIntegration(t)
	api.uncertain = true
	f := slowJudge(t, actions, false)
	if _, err := actions.FileJudgedSlowIncident(ctx, f, sender); err == nil {
		t.Fatal("expected lost response")
	}
	monitor := operations.IncidentMonitor{Service: ops, Sources: func(context.Context) ([]operations.IncidentSource, error) { return nil, nil }}
	if err := monitor.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(ctx, sender.options)
	if err != nil {
		t.Fatal(err)
	}
	issue, err := actions.RecoverSlowIncidentFiling(ctx, f.IncidentID, restarted)
	if err != nil || issue == nil || api.posts != 1 {
		t.Fatalf("issue=%v err=%v posts=%d", issue, err, api.posts)
	}
	if fleet.Object(slowRecord(t, ops)["slowFiling"])["result"] != "success" {
		t.Fatal(slowRecord(t, ops))
	}
}

type pausedSlowSender struct {
	*Service
	started chan struct{}
	release chan struct{}
}

func (s *pausedSlowSender) FileSlowIncident(ctx context.Context, f *operations.SlowIncidentFiling, validate func(context.Context, *operations.SlowIncidentFiling) error) (map[string]any, error) {
	close(s.started)
	<-s.release
	return s.Service.FileSlowIncident(ctx, f, validate)
}

func TestSlowIncidentConcurrentReservations(t *testing.T) {
	ctx := context.Background()
	sender, api, ops, actions, _ := slowIntegration(t)
	f := slowJudge(t, actions, false)
	paused := &pausedSlowSender{Service: sender, started: make(chan struct{}), release: make(chan struct{})}
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() { defer close(done); _, _ = actions.FileJudgedSlowIncident(ctx, f, paused) }()
	<-paused.started
	for i := 0; i < 7; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = actions.FileJudgedSlowIncident(ctx, f, paused) }()
	}
	wg.Wait()
	_, _ = actions.RecoverSlowIncidentFiling(ctx, f.IncidentID, paused)
	if record := slowRecord(t, ops); fleet.Object(record["slowFiling"])["reported"] != nil {
		t.Fatalf("送信中に結果を記録しました: %v", record)
	}
	close(paused.release)
	<-done
	if api.posts != 1 {
		t.Fatalf("posts=%d", api.posts)
	}
	record := slowRecord(t, ops)
	if fleet.Object(record["slowFiling"])["result"] != "success" {
		t.Fatal(record)
	}
	count := 0
	for _, h := range fleet.List(record["history"]) {
		if fleet.Object(h)["event"] == "起票結果" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("起票結果の履歴件数=%d history=%v", count, record["history"])
	}
}

type interruptedSlowSender struct{ *Service }

func (s interruptedSlowSender) FileSlowIncident(context.Context, *operations.SlowIncidentFiling, func(context.Context, *operations.SlowIncidentFiling) error) (map[string]any, error) {
	return nil, errors.New("送信前に中断")
}

func TestSlowIncidentRecoveryBeforeIntentNeverPosts(t *testing.T) {
	ctx := context.Background()
	sender, api, ops, actions, _ := slowIntegration(t)
	f := slowJudge(t, actions, false)
	if _, err := actions.FileJudgedSlowIncident(ctx, f, interruptedSlowSender{sender}); err == nil {
		t.Fatal("expected interruption")
	}
	restarted, err := New(ctx, sender.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = actions.RecoverSlowIncidentFiling(ctx, f.IncidentID, restarted); err == nil || api.posts != 0 {
		t.Fatalf("err=%v posts=%d", err, api.posts)
	}
	if fleet.Object(slowRecord(t, ops)["slowFiling"])["result"] != "unknown" {
		t.Fatal(slowRecord(t, ops))
	}
}

type failIncidentResultAPI struct {
	*memoryAPI
	after func() error
}

func (a *failIncidentResultAPI) CreateIssue(ctx context.Context, repo string, payload any) (map[string]any, error) {
	issue, err := a.memoryAPI.CreateIssue(ctx, repo, payload)
	if err != nil {
		return nil, err
	}
	if err = a.after(); err != nil {
		return nil, err
	}
	return issue, nil
}
func TestSlowIncidentResultSaveFailureRecovery(t *testing.T) {
	ctx := context.Background()
	sender, api, ops, actions, _ := slowIntegration(t)
	f := slowJudge(t, actions, false)
	sender.options.API = &failIncidentResultAPI{memoryAPI: api, after: func() error {
		_, err := ops.PostgreSQL.ExecContext(ctx, `ALTER TABLE settings ADD CONSTRAINT reject_success CHECK (value::text NOT LIKE '%"result": "success"%')`)
		return err
	}}
	if _, err := actions.FileJudgedSlowIncident(ctx, f, sender); err == nil || api.posts != 1 {
		t.Fatalf("err=%v posts=%d", err, api.posts)
	}
	if fleet.Object(slowRecord(t, ops)["slowFiling"])["result"] != "unknown" {
		t.Fatal(slowRecord(t, ops))
	}
	if _, err := ops.PostgreSQL.ExecContext(ctx, "ALTER TABLE settings DROP CONSTRAINT reject_success"); err != nil {
		t.Fatal(err)
	}
	api.historyErr = errors.New("履歴を取得できません")
	restarted, err := New(ctx, sender.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = actions.RecoverSlowIncidentFiling(ctx, f.IncidentID, restarted); err == nil || api.posts != 1 {
		t.Fatalf("照合失敗 err=%v posts=%d", err, api.posts)
	}
	record := slowRecord(t, ops)
	filing := fleet.Object(record["slowFiling"])
	if filing["result"] != "unknown" || filing["reported"] != nil {
		t.Fatalf("照合失敗時の予約=%v", filing)
	}
	alerts, err := ops.Attention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, alert := range alerts {
		if fleet.Object(alert)["id"] == f.IncidentID {
			found = true
		}
	}
	if !found {
		t.Fatalf("要対応に表示されません: %v", alerts)
	}
	api.historyErr = nil
	if _, err = actions.RecoverSlowIncidentFiling(ctx, f.IncidentID, restarted); err != nil || api.posts != 1 {
		t.Fatalf("err=%v posts=%d", err, api.posts)
	}
	if fleet.Object(slowRecord(t, ops)["slowFiling"])["result"] != "success" {
		t.Fatal(slowRecord(t, ops))
	}
}

func TestSlowIncidentRealWorkSourceToFiling(t *testing.T) {
	ctx := context.Background()
	sender, api, ops, actions, _ := slowIntegration(t)
	if _, err := ops.PostgreSQL.ExecContext(ctx, "DELETE FROM settings WHERE key='incident-deadline'"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	ops.Now = func() time.Time { return now }
	for i := 0; i < 11; i++ {
		status, issue := "merged", 100+i
		if i == 10 {
			status, issue = "running", 42
		}
		var id int64
		if err := ops.PostgreSQL.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,title,tier,status,view) VALUES('owner/app',$1,'work','standard',$2,'{"tierHistory":[{"from":"standard","to":"standard"}]}') RETURNING id`, issue, status).Scan(&id); err != nil {
			t.Fatal(err)
		}
		start := now.Add(-2 * time.Hour)
		if i == 10 {
			start = now.Add(-3 * time.Hour)
		}
		if _, err := ops.PostgreSQL.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at) VALUES($1,'leased',$2)`, id, start); err != nil {
			t.Fatal(err)
		}
		if i < 10 {
			if _, err := ops.PostgreSQL.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at) VALUES($1,'pr_created',$2)`, id, now.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}
	monitor := operations.IncidentMonitor{Service: ops, Sources: ops.SlowWorkSources}
	if err := monitor.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	row := slowRecord(t, ops)
	id := fleet.Text(row["id"])
	if id == "" || row["kind"] != "slow" {
		t.Fatal(row)
	}
	now = now.Add(15 * time.Minute)
	if err := monitor.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := actions.JudgeSlowIncident(ctx, id, func(context.Context, map[string]any) (operations.SlowIncidentDecision, error) {
		return operations.SlowIncidentDecision{Problem: true, Cause: "実行の不具合"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	issue, err := actions.FileJudgedSlowIncident(ctx, f, sender)
	if err != nil || api.posts != 1 {
		t.Fatalf("issue=%v err=%v posts=%d", issue, err, api.posts)
	}
	job, err := JobFromIssue("owner/app", issue, "")
	if err != nil || job.Priority != 1 || !strings.Contains(fleet.Text(issue["body"]), f.WorkKey) || !strings.Contains(fleet.Text(issue["body"]), "10800秒") {
		t.Fatalf("job=%+v issue=%v err=%v", job, issue, err)
	}
}
