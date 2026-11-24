package startup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/operations"
	"issue-desk-intake/server/internal/statistics"
)

type slowTestAPI struct {
	posts         []map[string]any
	beforeHistory func()
	postError     error
	attempts      int
	loseResponse  bool
}

type interruptedSlowTestSender struct{}

func (interruptedSlowTestSender) FileSlowIncident(context.Context, *operations.SlowIncidentFiling, func(context.Context, *operations.SlowIncidentFiling) error) (map[string]any, error) {
	return nil, errors.New("interrupted before intent")
}
func (interruptedSlowTestSender) RecoverSlowIncident(context.Context, *operations.SlowIncidentFiling) (map[string]any, error) {
	return nil, errors.New("no intent")
}

func (a *slowTestAPI) Issues(context.Context, string, string) ([]map[string]any, error) {
	if a.beforeHistory != nil {
		a.beforeHistory()
		a.beforeHistory = nil
	}
	return a.posts, nil
}
func (a *slowTestAPI) Issue(context.Context, string, int64) (map[string]any, error) { return nil, nil }
func (a *slowTestAPI) CreateIssue(_ context.Context, _ string, payload any) (map[string]any, error) {
	a.attempts++
	if a.postError != nil && !a.loseResponse {
		return nil, a.postError
	}
	p := payload.(map[string]any)
	row := map[string]any{"number": float64(len(a.posts) + 1), "body": p["body"], "title": p["title"], "user": map[string]any{"id": float64(1)}}
	a.posts = append(a.posts, row)
	if a.postError != nil {
		return nil, a.postError
	}
	return row, nil
}

func TestSlowIncidentStartupIntegration(t *testing.T) {
	t.Run("other_incidents", testOtherIncidentSources)
	for _, mode := range []string{"problem", "no_problem", "stopped", "uncertain", "invalid", "resume", "reserved", "continue", "missing_number", "skipped", "ended_during_judgement", "ended_before_send", "restart", "failed_send", "lost_response", "direct_pr", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			pg := appPostgres(t)
			appDB := newAppStore(t).DB()
			now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
			source := &operations.Service{DB: appDB, PostgreSQL: pg, Now: func() time.Time { return now }, RuntimeDir: t.TempDir()}
			source.Statistics = &statistics.Service{DB: pg, AppDB: appDB, Now: source.Now, RuntimeDir: source.RuntimeDir, Assignments: &fleet.Assignments{Output: map[string]any{}}}
			for i := 0; i < 10; i++ {
				var id int64
				err := pg.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,title,tier,status,view) VALUES('owner/repo',$1,'sample','light','merged', '{"tierHistory":[{"from":"light","to":"light"}]}') RETURNING id`, i+1).Scan(&id)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = pg.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at) VALUES($1,'leased',$2),($1,'pr_created',$3)`, id, now.Add(-3*time.Hour), now.Add(-2*time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			var jobID int64
			if err := pg.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,title,tier,status,view) VALUES('owner/repo',42,'active','light','running','{"tierHistory":[{"from":"light","to":"light"}]}') RETURNING id`).Scan(&jobID); err != nil {
				t.Fatal(err)
			}
			if mode == "missing_number" {
				if _, e := pg.ExecContext(ctx, `UPDATE jobs SET status='merged' WHERE id=$1`, jobID); e != nil {
					t.Fatal(e)
				}
				for i := 0; i < 10; i++ {
					if _, e := pg.ExecContext(ctx, `INSERT INTO requests(id,kind,repo,payload,sender,status,claimed_by,claimed_at,finished_at,result) VALUES($1,'pr_create','owner/repo','{}','pc','done','pc',$2,$3,'{"status":"created"}')`, fmt.Sprintf("sample-%d", i), now.Add(-3*time.Hour), now.Add(-2*time.Hour)); e != nil {
						t.Fatal(e)
					}
				}
				if _, e := pg.ExecContext(ctx, `INSERT INTO requests(id,kind,repo,payload,sender,status,claimed_by,claimed_at) VALUES('active','pr_create','owner/repo','{}','pc','claimed','pc',$1)`, now.Add(-2*time.Hour+time.Minute)); e != nil {
					t.Fatal(e)
				}
			}
			if _, err := pg.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at) VALUES($1,'leased',$2)`, jobID, now.Add(-2*time.Hour+time.Minute)); err != nil {
				t.Fatal(err)
			}
			var secondID int64
			if mode == "continue" || mode == "shutdown" {
				if e := pg.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,title,tier,status,view) VALUES('owner/repo',43,'second','light','running','{"tierHistory":[{"from":"light","to":"light"}]}') RETURNING id`).Scan(&secondID); e != nil {
					t.Fatal(e)
				}
				if _, e := pg.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at) VALUES($1,'leased',$2)`, secondID, now.Add(-2*time.Hour+time.Minute)); e != nil {
					t.Fatal(e)
				}
			}
			records, err := consultations.OpenRecords(ctx, appDB)
			if err != nil {
				t.Fatal(err)
			}
			queue, err := jobs.New(pg, jobs.Options{})
			if err != nil {
				t.Fatal(err)
			}
			api := &slowTestAPI{}
			if mode == "failed_send" {
				api.postError = &gitea.Error{Status: 400}
			}
			if mode == "lost_response" {
				api.postError = &gitea.Error{UnknownOutcome: true}
				api.loseResponse = true
			}
			if mode == "ended_before_send" {
				api.beforeHistory = func() {
					if _, e := pg.ExecContext(ctx, `UPDATE jobs SET status='merged' WHERE id=$1`, jobID); e != nil {
						t.Fatal(e)
					}
				}
			}
			sender, err := filing.New(ctx, filing.Options{Records: records, API: api, Jobs: queue, Project: func(string) error { return nil }, Own: func(map[string]any) bool { return true }, Build: func(context.Context, filing.Record) (filing.Prepared, error) { return filing.Prepared{}, nil }, Check: func(context.Context, filing.Record) error { return nil }, Link: func(string, int64) string { return "" }, IncidentLedger: func(ctx context.Context) (any, error) { return source.OperationalSetting(ctx, "incident-deadline") }})
			if err != nil {
				t.Fatal(err)
			}
			var run func(context.Context) error
			var interval time.Duration
			judgeCalls, transfers := 0, 0
			tickCtx, cancelTick := context.WithCancel(ctx)
			defer cancelTick()
			backgroundCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			app := &App{ctx: backgroundCtx, Log: func(string) {}}
			first := make(chan error, 1)
			deps := operationDependencies{
				operations: func() (*operations.Service, error) { return source, nil }, sender: sender,
				read: func(_ context.Context, repo string, n int64) (map[string]any, error) {
					return map[string]any{"project": repo, "number": n, "stopped": mode == "stopped"}, nil
				},
				send: func(context.Context, string, int64, string, string, string) error { transfers++; return nil },
				answer: func(_ context.Context, request map[string]any) (map[string]any, error) {
					judgeCalls++
					if request["kind"] != "slow-incident" || request["schema"] == "" {
						t.Errorf("専用の判定要求ではない: %v", request)
					}
					switch mode {
					case "shutdown":
						cancelTick()
						return nil, context.Canceled
					case "ended_during_judgement":
						if _, e := pg.ExecContext(ctx, `UPDATE jobs SET status='merged' WHERE id=$1`, jobID); e != nil {
							t.Fatal(e)
						}
						return map[string]any{"judgement": "problem", "cause": "defect", "directPR": false}, nil
					case "continue":
						data := fleet.Object(request["data"])
						target := fleet.Object(fleet.Object(data["incident"])["target"])
						if fleet.Text(target["workKey"]) == fmt.Sprintf("job-%d", jobID) {
							return map[string]any{"judgement": "uncertain", "cause": "", "directPR": false}, nil
						}
						return map[string]any{"judgement": "problem", "cause": "defect", "directPR": false}, nil
					case "direct_pr":
						return map[string]any{"judgement": "problem", "cause": "defect", "directPR": true}, nil
					case "problem", "stopped", "missing_number", "ended_before_send", "restart", "failed_send", "lost_response":
						return map[string]any{"judgement": "problem", "cause": "defect", "directPR": false}, nil
					case "no_problem":
						return map[string]any{"judgement": "no_problem", "cause": "", "directPR": false}, nil
					case "uncertain":
						return map[string]any{"judgement": "uncertain", "cause": "", "directPR": false}, nil
					default:
						return map[string]any{"judgement": "problem"}, nil
					}
				},
				register: func(name string, d time.Duration, fn func(context.Context) error) {
					if name == "operations-monitor" {
						run = fn
						interval = d
						app.periodically(name, d, func(ctx context.Context) error { e := fn(ctx); first <- e; return e })
					}
				},
			}
			if err = app.startOperationsWith(deps); err != nil {
				t.Fatal(err)
			}
			if err = <-first; err != nil {
				t.Fatal(err)
			}
			cancel()
			app.wg.Wait()
			if interval != time.Minute {
				t.Fatalf("interval %v", interval)
			}
			rows := func() []any {
				v, e := source.OperationalSetting(ctx, "incident-deadline")
				if e != nil {
					t.Fatal(e)
				}
				return fleet.List(v)
			}
			if len(rows()) != 0 {
				t.Fatalf("2倍未満で登録: %v", rows())
			}
			now = now.Add(time.Minute)
			if err = run(ctx); err != nil {
				t.Fatal(err)
			}
			wantRows := 1
			if mode == "continue" || mode == "shutdown" {
				wantRows = 2
			}
			if len(rows()) != wantRows || fleet.Object(rows()[0])["kind"] != "slow" {
				t.Fatalf("2倍到達: %v", rows())
			}
			workKey := fmt.Sprintf("job-%d", jobID)
			if mode == "continue" {
				workKey = fmt.Sprintf("job-%d", secondID)
			}
			now = now.Add(15 * time.Minute)
			if mode == "resume" || mode == "reserved" || mode == "skipped" {
				// Persist a decision through the real action, as if the process
				// stopped between saving it and the next startup cycle.
				monitor := &operations.IncidentMonitor{Service: source, Sources: source.SlowWorkSources}
				if e := monitor.Reconcile(ctx); e != nil {
					t.Fatal(e)
				}
				actions := &operations.IncidentActions{Service: source, ReadTarget: deps.read}
				id := fleet.Text(fleet.Object(rows()[0])["id"])
				f, e := actions.JudgeSlowIncident(ctx, id, func(context.Context, map[string]any) (operations.SlowIncidentDecision, error) {
					return operations.SlowIncidentDecision{Problem: true, Cause: "defect"}, nil
				})
				if e != nil {
					t.Fatal(e)
				}
				if mode == "reserved" {
					if _, e = actions.FileJudgedSlowIncident(ctx, f, interruptedSlowTestSender{}); e == nil {
						t.Fatal("中断が記録されなかった")
					}
				}
				if mode == "skipped" {
					canceled, cancelTick := context.WithCancel(ctx)
					cancelTick()
					if e := run(canceled); e == nil {
						t.Fatal("取消しが検出されなかった")
					}
					if judgeCalls != 0 {
						t.Fatal("取消し後にAIを呼びました")
					}
				}
			}
			if mode == "shutdown" {
				err = run(tickCtx)
				if !errors.Is(err, context.Canceled) || judgeCalls != 1 || api.attempts != 0 {
					t.Fatalf("cancel err=%v calls=%d posts=%d", err, judgeCalls, api.attempts)
				}
				_ = run(tickCtx)
				if judgeCalls != 1 || api.attempts != 0 {
					t.Fatalf("after cancel calls=%d posts=%d", judgeCalls, api.attempts)
				}
				return
			}
			err = run(ctx)
			if mode == "problem" || mode == "no_problem" || mode == "resume" || mode == "skipped" || mode == "restart" || mode == "direct_pr" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("判定不能または停止を受理した")
			}
			if mode == "problem" || mode == "resume" || mode == "continue" || mode == "restart" || mode == "direct_pr" {
				if len(api.posts) != 1 {
					t.Fatalf("posts %d", len(api.posts))
				}
				body := api.posts[0]["body"].(string)
				marker := "<!-- issue-desk:settings "
				index := strings.LastIndex(body, marker)
				if index < 0 {
					t.Fatal(body)
				}
				var fields map[string]any
				if e := json.Unmarshal([]byte(strings.TrimSuffix(body[index+len(marker):], " -->")), &fields); e != nil {
					t.Fatal(e, body)
				}
				reason, _ := fields["priorityReason"].(string)
				if fields["priority"] != "high" || !strings.Contains(reason, workKey) || !strings.Contains(reason, "7200秒") {
					t.Fatalf("priority fields %v", fields)
				}
			} else if mode != "lost_response" && len(api.posts) != 0 {
				t.Fatalf("unexpected posts %d", len(api.posts))
			}
			if mode == "no_problem" && fleet.Object(rows()[0])["slowJudgement"] != "no_problem" {
				t.Fatal(rows())
			}
			if mode == "ended_before_send" && fleet.Object(fleet.Object(rows()[0])["slowFiling"])["result"] != "failed" {
				t.Fatalf("終了後の送信拒否が失敗として記録されていません: %v", rows())
			}
			if mode == "failed_send" || mode == "lost_response" {
				result := fleet.Object(fleet.Object(rows()[0])["slowFiling"])
				want := "failed"
				if mode == "lost_response" {
					want = "unknown"
				}
				if result["result"] != want || result["reported"] != true || api.attempts != 1 {
					t.Fatalf("filing=%v attempts=%d", result, api.attempts)
				}
				count := 0
				for _, item := range fleet.List(fleet.Object(rows()[0])["history"]) {
					if fleet.Object(item)["event"] == "起票結果" {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("起票結果の記録=%d", count)
				}
			}
			if mode == "direct_pr" {
				if !strings.Contains(api.posts[0]["body"].(string), "直接PRが要る") {
					t.Fatal(api.posts[0])
				}
				if _, e := pg.ExecContext(ctx, `UPDATE jobs SET status='merged' WHERE id=$1`, jobID); e != nil {
					t.Fatal(e)
				}
				now = now.Add(time.Minute)
				if e := run(ctx); e != nil {
					t.Fatal(e)
				}
				alerts, e := source.Attention(ctx)
				if e != nil {
					t.Fatal(e)
				}
				count := 0
				for _, raw := range alerts {
					if fleet.Object(raw)["sourceKey"] == "slow-"+workKey {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("direct PR attention=%d: %v", count, alerts)
				}
			}
			if mode == "restart" || mode == "reserved" || mode == "lost_response" {
				// Re-register the real startup callback with the same persistent stores.
				deps.register = func(name string, _ time.Duration, fn func(context.Context) error) {
					if name == "operations-monitor" {
						run = fn
					}
				}
				if e := (&App{ctx: ctx, Log: func(string) {}}).startOperationsWith(deps); e != nil {
					t.Fatal(e)
				}
			}
			posts := len(api.posts)
			attempts := api.attempts
			if mode == "skipped" {
				if judgeCalls != 0 {
					t.Fatal("照合を省略した周期でAIを呼びました")
				}
				return
			}
			now = now.Add(time.Minute)
			_ = run(ctx)
			if len(api.posts) != posts || api.attempts != attempts || transfers != 0 {
				t.Fatalf("posts=%d transfers=%d", len(api.posts), transfers)
			}
			if mode == "lost_response" && fleet.Object(fleet.Object(rows()[0])["slowFiling"])["result"] != "success" {
				t.Fatalf("再起動後に送信結果を照合できません: %v", rows())
			}
			if mode == "problem" && judgeCalls != 1 {
				t.Fatalf("judge calls=%d", judgeCalls)
			}
			if (mode == "stopped" || mode == "resume" || mode == "reserved") && judgeCalls != 0 {
				t.Fatalf("unexpected judge calls=%d", judgeCalls)
			}
		})
	}
}
