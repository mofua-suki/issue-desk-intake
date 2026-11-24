package startup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/notices"
	"issue-desk-intake/server/internal/operations"
	"issue-desk-intake/server/internal/statistics"
)

func testOtherIncidentSources(t *testing.T) {
	ctx := context.Background()
	pg, db := appPostgres(t), newAppStore(t).DB()
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	repos := func() ([]string, error) { return []string{"owner/repo"}, nil }
	source := &operations.Service{DB: db, PostgreSQL: pg, Now: func() time.Time { return now }, RuntimeDir: t.TempDir(), Repositories: repos, Assignments: &fleet.Assignments{MonitorHost: "nas"}}
	if _, err := pg.ExecContext(ctx, `INSERT INTO hosts(host,heartbeat_at,ready,capacity) VALUES('nas',$1,true,1)`, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO main_break_cases(repo,json) VALUES('owner/repo',$1)`, `{"repo":"owner/repo","state":"checking","tests":["test"],"fix":{"issue":{"project":"owner/repo","number":42}}}`); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{}
	if err := json.Unmarshal([]byte(`{"kind":"stage-watch","event":"result","id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","operationId":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","name":"ship","at":"2026-11-11T00:00:00.000Z","startedAt":"2026-11-11T00:00:00.000Z","deadline":null,"target":{"project":"owner/repo","number":42},"observation":"process","lastSuccessAt":null,"holder":"watcher","exitCode":1,"outputs":0,"anomalies":["exit"]}`), &event); err != nil {
		t.Fatal(err)
	}
	if _, err := (&notices.Service{DB: db, Now: source.Now, Repositories: repos}).Receive(ctx, "nas", event); err != nil {
		t.Fatal(err)
	}
	// Legacy full-test records are not a live source in the shipped service.
	if _, err := db.ExecContext(ctx, `INSERT INTO full_test_intake(repo,json) VALUES('owner/repo',$1)`, `{"mainBreakResult":{"status":"failed"}}`); err != nil {
		t.Fatal(err)
	}
	api := &slowTestAPI{}
	records, err := consultations.OpenRecords(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(pg, jobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sender, err := filing.New(ctx, filing.Options{Records: records, API: api, Jobs: queue, Project: func(string) error { return nil }, Own: func(map[string]any) bool { return true }, Build: func(context.Context, filing.Record) (filing.Prepared, error) { return filing.Prepared{}, nil }, Check: func(context.Context, filing.Record) error { return nil }, Link: func(string, int64) string { return "" }, IncidentLedger: func(ctx context.Context) (any, error) { return source.OperationalSetting(ctx, "incident-deadline") }})
	if err != nil {
		t.Fatal(err)
	}
	var run func(context.Context) error
	generic := map[string]int{}
	slow := 0
	sends := 0
	deps := operationDependencies{operations: func() (*operations.Service, error) { return source, nil }, sender: sender,
		read: func(_ context.Context, repo string, n int64) (map[string]any, error) {
			return map[string]any{"project": repo, "number": n}, nil
		},
		send: func(context.Context, string, int64, string, string, string) error { sends++; return nil },
		answer: func(_ context.Context, r map[string]any) (map[string]any, error) {
			if r["kind"] == "slow-incident" {
				slow++
				return map[string]any{"judgement": "problem", "cause": "defect", "directPR": false}, nil
			}
			items := fleet.List(r["items"])
			if len(items) != 1 {
				t.Fatalf("items=%v", items)
			}
			item := fleet.Object(items[0])
			kind := fleet.Text(item["kind"])
			generic[kind]++
			return map[string]any{"actions": []any{map[string]any{"action": "file", "item": item["key"], "reason": "defect", "text": "file", "evidence": "evidence", "resolved": false, "requiresUser": false}}}, nil
		}, register: func(name string, _ time.Duration, fn func(context.Context) error) {
			if name == "operations-monitor" {
				run = fn
			}
		}}
	if err = (&App{ctx: ctx, Log: func(string) {}}).startOperationsWith(deps); err != nil {
		t.Fatal(err)
	}
	if err = run(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Minute)
	if err = run(ctx); err != nil {
		t.Fatal(err)
	}
	value, err := source.OperationalSetting(ctx, "incident-deadline")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, raw := range fleet.List(value) {
		row := fleet.Object(raw)
		kind := fleet.Text(row["kind"])
		seen[kind]++
		if row["endedAt"] != nil || row["slowFiling"] != nil || row["state"] != "acted" {
			t.Fatalf("%s: %v", kind, row)
		}
	}
	for _, kind := range []string{"monitor_stopped", "main_broken", "stage_watch"} {
		if seen[kind] != 1 || generic[kind] != 1 {
			t.Fatalf("%s seen=%v generic=%v", kind, seen, generic)
		}
	}
	if seen["full_test"] != 0 || slow != 0 || api.attempts != 0 || sends != 0 {
		t.Fatalf("seen=%v slow=%d posts=%d actions=%d", seen, slow, api.attempts, sends)
	}
}

// TestSlowIncidentOperationsDocumentation exercises the runbook's unresolved
// reservation path with actual persisted work, intent lookup and attention output.
func TestSlowIncidentOperationsDocumentation(t *testing.T) {
	ctx := context.Background()
	pg, db := appPostgres(t), newAppStore(t).DB()
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	source := &operations.Service{DB: db, PostgreSQL: pg, Now: func() time.Time { return now }, RuntimeDir: t.TempDir()}
	source.Statistics = &statistics.Service{DB: pg, AppDB: db, Now: source.Now, RuntimeDir: source.RuntimeDir, Assignments: &fleet.Assignments{Output: map[string]any{}}}
	for i := 0; i < 10; i++ {
		if _, err := pg.ExecContext(ctx, `INSERT INTO requests(id,kind,repo,payload,sender,status,claimed_by,claimed_at,finished_at,result) VALUES($1,'pr_create','owner/repo','{}','pc','done','pc',$2,$3,'{"status":"created"}')`, fmt.Sprintf("completed-%d", i), now.Add(-3*time.Hour), now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pg.ExecContext(ctx, `INSERT INTO requests(id,kind,repo,payload,sender,status,claimed_by,claimed_at) VALUES('active','pr_create','owner/repo','{"issue":42}','pc','claimed','pc',$1)`, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	api := &slowTestAPI{}
	records, err := consultations.OpenRecords(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(pg, jobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sender, err := filing.New(ctx, filing.Options{Records: records, API: api, Jobs: queue, Project: func(string) error { return nil }, Own: func(map[string]any) bool { return true }, Build: func(context.Context, filing.Record) (filing.Prepared, error) { return filing.Prepared{}, nil }, Check: func(context.Context, filing.Record) error { return nil }, Link: func(string, int64) string { return "" }, IncidentLedger: func(ctx context.Context) (any, error) { return source.OperationalSetting(ctx, "incident-deadline") }})
	if err != nil {
		t.Fatal(err)
	}
	var run func(context.Context) error
	calls := 0
	deps := operationDependencies{operations: func() (*operations.Service, error) { return source, nil }, sender: sender, read: func(_ context.Context, repo string, n int64) (map[string]any, error) {
		return map[string]any{"project": repo, "number": n}, nil
	}, answer: func(context.Context, map[string]any) (map[string]any, error) {
		calls++
		return map[string]any{"judgement": "problem", "cause": "defect", "directPR": false}, nil
	}, register: func(name string, _ time.Duration, fn func(context.Context) error) {
		if name == "operations-monitor" {
			run = fn
		}
	}}
	if err = (&App{ctx: ctx, Log: func(string) {}}).startOperationsWith(deps); err != nil {
		t.Fatal(err)
	}
	if err = run(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Minute)
	monitor := operations.IncidentMonitor{Service: source, Sources: source.SlowWorkSources}
	if err = monitor.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	value, err := source.OperationalSetting(ctx, "incident-deadline")
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet.List(value)) != 1 {
		t.Fatalf("incidents=%v", value)
	}
	id := fleet.Text(fleet.Object(fleet.List(value)[0])["id"])
	actions := operations.IncidentActions{Service: source, ReadTarget: deps.read}
	decision, err := actions.JudgeSlowIncident(ctx, id, func(context.Context, map[string]any) (operations.SlowIncidentDecision, error) {
		return operations.SlowIncidentDecision{Problem: true, Cause: "defect"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = actions.FileJudgedSlowIncident(ctx, decision, interruptedSlowTestSender{}); err == nil {
		t.Fatal("expected interrupted send")
	}
	if _, err = pg.ExecContext(ctx, `UPDATE requests SET status='done',finished_at=$1,result='{"status":"created"}' WHERE id='active'`, now); err != nil {
		t.Fatal(err)
	}
	if err = run(ctx); err == nil {
		t.Fatal("missing intent must remain unresolved")
	}
	alerts, err := source.Attention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, v := range alerts {
		r := fleet.Object(v)
		if r["id"] == id {
			count++
			if !strings.Contains(fleet.Text(r["reason"]), "再送しません") || fleet.Object(r["slowFiling"])["result"] != "unknown" {
				t.Fatalf("alert=%v", r)
			}
		}
	}
	if count != 1 || api.attempts != 0 || calls != 0 {
		t.Fatalf("attention=%d posts=%d calls=%d", count, api.attempts, calls)
	}
}
