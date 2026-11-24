package startup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/numbers"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"issue-desk-intake/server/internal/jobs"
	"testing"
	"time"

	"issue-desk-intake/server/internal/operations"
)

func TestRecoveryCycleContinuesIndependentJobsAndStopsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := errors.New("最初の依頼を読み取れません")
	calls := 0
	err := runJobRecoveryCandidates(ctx, make([]operations.RecoveryCandidate, 3), func(context.Context, operations.RecoveryCandidate) (operations.RecoveryResult, error) {
		calls++
		if calls == 1 {
			return operations.Skipped, first
		}
		cancel()
		return operations.Acted, nil
	})
	if calls != 2 || !errors.Is(err, first) || !errors.Is(err, context.Canceled) {
		t.Fatal(calls, err)
	}
}

func TestRecoveryPolicyUsesValidatedAssignmentSnapshotWithoutFallback(t *testing.T) {
	source := &operations.Service{}
	for _, values := range []map[string]any{nil, {"assignments": map[string]any{}}, {"assignments": map[string]any{"implementationRunLimit": float64(1)}}} {
		if _, err := jobRecoveryPolicy(source, values); err == nil {
			t.Fatal("上限の欠測を既定値で補いました", values)
		}
	}
	values := map[string]any{"assignments": map[string]any{"implementationRunLimit": float64(1), "reviewRoundLimit": float64(1), "implementationUsageLimit": float64(123)}, "usageWeights": map[string]any{"inputTokens": float64(1)}}
	policy, err := jobRecoveryPolicy(source, values)
	if err != nil || policy.RunLimit != 1 || policy.ReviewLimit != 1 || policy.UsageLimit != 123 || policy.UsageWeights["inputTokens"] != float64(1) {
		t.Fatal(policy, err)
	}
}

func TestRecoveryCycleIsRegisteredAndCloseCancelsItPostgres(t *testing.T) {
	a := reviewNotesApp(t)
	started, stopped := make(chan struct{}), make(chan struct{})
	a.deferBackground = true
	calls := 0
	a.Operations = func() (*operations.Service, error) {
		calls++
		if calls == 1 {
			return &operations.Service{}, nil
		}
		close(started)
		<-a.backgroundCtx.Done()
		return nil, a.backgroundCtx.Err()
	}
	if err := a.startOperations(); err != nil {
		t.Fatal(err)
	}
	if len(a.backgroundPending) != 4 {
		t.Fatal("有事、自動復旧、前提保留観測の周期が登録されていません", len(a.backgroundPending))
	}
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	a.backgroundCtx = ctx
	// Invoke the actual recovery callback registered by startOperations.
	go func() { a.backgroundPending[1](ctx); close(stopped) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("自動復旧の周期が起動しません")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("終了時に自動復旧の周期を回収していません")
	}
}

func TestRecoveryActDeadlinePrecedesReservationExpiry(t *testing.T) {
	r := &operations.JobRecovery{ReservationTimeout: time.Hour}
	started := time.Now()
	act := boundedRecoveryAct(r, func(ctx context.Context, _ operations.RecoveryCandidate) (operations.RecoveryResult, error) {
		deadline, ok := ctx.Deadline()
		if !ok || deadline.After(started.Add(r.ReservationTTL())) || deadline.Before(started.Add(29*time.Minute)) {
			t.Fatal(deadline, ok)
		}
		return operations.Skipped, nil
	})
	if _, err := act(context.Background(), operations.RecoveryCandidate{}); err != nil {
		t.Fatal(err)
	}
}

func waitReaderForTest(items []*jobs.Job) userWaitReader {
	return userWaitReader{
		list: func(_ context.Context, _ string, _ []string, limit, offset int) ([]*jobs.Job, int, error) {
			if offset >= len(items) {
				return nil, len(items), nil
			}
			end := min(offset+limit, len(items))
			return items[offset:end], len(items), nil
		},
		number: func(_ context.Context, _ string, issue int64) (int64, bool, error) { return issue + 100, true, nil },
		usage:  func(_ context.Context, j *jobs.Job, _ jobs.ControlOptions) bool { return j.Title == "usage" },
	}
}
func waitJobForTest(issue int64, reason string) *jobs.Job {
	return &jobs.Job{Repo: "project/repo", Issue: issue, Title: fmt.Sprint(issue), Status: "held", HoldReason: &reason, View: map[string]any{}, Result: map[string]any{}}
}

func TestUserWaitNoticeSourcesKinds(t *testing.T) {
	permission := waitJobForTest(1, "review_held")
	permission.View["permissionHold"] = map[string]any{"files": true}
	deletion := waitJobForTest(2, "deletions")
	deletion.View["deletionApproval"] = map[string]any{}
	final := waitJobForTest(3, "review_held")
	var plan any
	if err := json.Unmarshal([]byte(`{"version":1,"basis":"根拠","steps":[{"phase":"implementation","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"verification","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"merge","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"deployment","owner":"担当","prerequisite":"前提","evidence":"確認"}],"approvals":[{"category":"irreversible","id":"deploy","phase":"after_diff","target":"配置","operation":"配置","condition":"差分確認"}]}`), &plan); err != nil {
		t.Fatal(err)
	}
	final.View["completionPlan"] = plan
	running := waitJobForTest(4, "question")
	running.Status = "running"
	running.Generation = 3
	stopped := waitJobForTest(5, "question")
	stopped.Generation = 3
	stopped.View["stopRequested"] = true
	prerequisite := waitJobForTest(6, "question")
	prerequisite.Generation = 4
	prerequisite.Result = map[string]any{"status": "failed", "prerequisiteMissing": map[string]any{"artifact": "a"}}
	prerequisite.View["prerequisiteMissingGenerations"] = []any{float64(4)}
	investigation := waitJobForTest(7, "question")
	investigation.Generation = 3
	investigation.Kind = "investigation"
	got, err := userWaitNoticeSources(t.Context(), waitReaderForTest([]*jobs.Job{permission, deletion, final, running, stopped, prerequisite, investigation}), jobs.ControlOptions{RunLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	want := []operations.UserWaitNoticeItem{{Project: "project/repo", IssueNumber: 1, CommonNumber: 101, Title: "1", Kind: jobs.UserWaitPermission}, {Project: "project/repo", IssueNumber: 2, CommonNumber: 102, Title: "2", Kind: jobs.UserWaitDeletion}, {Project: "project/repo", IssueNumber: 3, CommonNumber: 103, Title: "3", Kind: jobs.UserWaitFinalApproval}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}
func TestUserWaitNoticeSourcesLimits(t *testing.T) {
	run := waitJobForTest(1, "question")
	run.Generation = 3
	usage := waitJobForTest(2, "question")
	usage.Title = "usage"
	got, err := userWaitNoticeSources(t.Context(), waitReaderForTest([]*jobs.Job{run, usage}), jobs.ControlOptions{RunLimit: 4})
	if err != nil || len(got) != 2 || got[0].Kind != jobs.UserWaitRunLimit || got[1].Kind != jobs.UserWaitUsageLimit {
		t.Fatal(got, err)
	}
}
func TestUserWaitNoticeSourcesPagination(t *testing.T) {
	all := make([]*jobs.Job, 1001)
	for i := range all {
		all[i] = waitJobForTest(int64(i+1), "question")
	}
	all[1000].Generation = 3
	got, err := userWaitNoticeSources(t.Context(), waitReaderForTest(all), jobs.ControlOptions{RunLimit: 4})
	if err != nil || len(got) != 1 || got[0].IssueNumber != 1001 {
		t.Fatal(got, err)
	}
}
func TestUserWaitNoticeSourcesFailure(t *testing.T) {
	a := reviewNotesApp(t)
	service := &operations.Service{PostgreSQL: a.DB}
	calls := 0
	notice := operations.UserWaitNotice{Service: service, Notify: func(context.Context, map[string]any) (map[string]any, error) { calls++; return nil, nil }, Sources: func(context.Context) ([]operations.UserWaitNoticeItem, error) {
		return []operations.UserWaitNoticeItem{{Project: "owner/repo", IssueNumber: 99, CommonNumber: 100, Kind: jobs.UserWaitPermission}}, nil
	}}
	if err := notice.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := service.OperationalSetting(t.Context(), "user-wait-notice")
	if err != nil {
		t.Fatal(err)
	}
	calls = 0
	job := waitJobForTest(1, "question")
	job.Generation = 3
	for _, kind := range []string{"page", "parse", "number", "missing", "nonpositive"} {
		t.Run(kind, func(t *testing.T) {
			j := *job
			j.View = map[string]any{}
			reader := waitReaderForTest([]*jobs.Job{&j})
			switch kind {
			case "page":
				reader.list = func(_ context.Context, _ string, _ []string, _ int, offset int) ([]*jobs.Job, int, error) {
					if offset == 0 {
						return []*jobs.Job{&j}, 2, nil
					}
					return nil, 0, errors.New("page")
				}
			case "parse":
				j.View["completionPlan"] = "invalid"
			case "number":
				reader.number = func(context.Context, string, int64) (int64, bool, error) { return 0, false, errors.New("number") }
			case "missing":
				reader.number = func(context.Context, string, int64) (int64, bool, error) { return 0, false, nil }
			case "nonpositive":
				reader.number = func(context.Context, string, int64) (int64, bool, error) { return 0, true, nil }
			}
			got, err := userWaitNoticeSources(t.Context(), reader, jobs.ControlOptions{RunLimit: 4})
			if err == nil || got != nil {
				t.Fatal(got, err)
			}
			notice.Sources = func(ctx context.Context) ([]operations.UserWaitNoticeItem, error) {
				return userWaitNoticeSources(ctx, reader, jobs.ControlOptions{RunLimit: 4})
			}
			if err := notice.Reconcile(t.Context()); err == nil {
				t.Fatal("収集エラーが失われました")
			}
			after, err := service.OperationalSetting(t.Context(), "user-wait-notice")
			if err != nil || calls != 0 || !reflect.DeepEqual(before, after) {
				t.Fatal(before, after, calls, err)
			}
		})
	}
}
func TestUserWaitNoticePolicyNoFallback(t *testing.T) {
	for _, values := range []map[string]any{nil, {"assignments": map[string]any{}}, {"assignments": map[string]any{"implementationRunLimit": 0, "implementationUsageLimit": 1}}, {"assignments": map[string]any{"implementationRunLimit": 1, "implementationUsageLimit": 0}}} {
		if _, err := userWaitNoticeOptions(values); err == nil {
			t.Fatal(values)
		}
	}
	options, err := userWaitNoticeOptions(map[string]any{"assignments": map[string]any{"implementationRunLimit": float64(5), "implementationUsageLimit": float64(10)}, "usageWeights": map[string]any{"inputTokens": float64(2)}})
	if err != nil || options.RunLimit != 5 || options.UsageLimit != 10 || options.UsageWeights["inputTokens"] != float64(2) {
		t.Fatal(options, err)
	}
}

func TestUserWaitNoticeCycleRegistered(t *testing.T) {
	a := reviewNotesApp(t)
	var err error
	a.Jobs, err = jobs.New(a.DB, jobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	a.Numbers, err = numbers.OpenLedger(t.Context(), a.SQLite.DB(), filepath.Join(t.TempDir(), "numbers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SQLite.DB().Exec(`INSERT INTO desk_numbers(number,project,gitea,assigned_at) VALUES(123,'owner/repo',7,'now')`); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DB.Exec(`INSERT INTO jobs(repo,issue,title,status,hold_reason,generation,view,result) VALUES('owner/repo',7,'確認','held','review_held',0,'{"permissionHold":{"files":true}}','{}')`); err != nil {
		t.Fatal(err)
	}
	service := &operations.Service{PostgreSQL: a.DB}
	type cycle struct {
		name     string
		interval time.Duration
		run      func(context.Context) error
	}
	var cycles []cycle
	calls, reads := 0, 0
	deps := operationDependencies{operations: func() (*operations.Service, error) { reads++; return service, nil }, notify: func(_ context.Context, msg map[string]any) (map[string]any, error) {
		calls++
		if msg["url"] != "/#/list/attention" || !strings.Contains(fmt.Sprint(msg["body"]), "123") {
			t.Fatal(msg)
		}
		return nil, nil
	}, register: func(name string, d time.Duration, f func(context.Context) error) {
		cycles = append(cycles, cycle{name, d, f})
	}}
	if err = a.startOperationsWith(deps); err != nil {
		t.Fatal(err)
	}
	expected := []string{"operations-monitor", "recover-jobs", "prerequisite-hold-observation", "user-wait-notice"}
	if len(cycles) != len(expected) || reads != 1 {
		t.Fatal(cycles, reads)
	}
	for i, c := range cycles {
		if c.name != expected[i] || c.interval != time.Minute {
			t.Fatal(c)
		}
	}
	path := a.Fleet.AssignmentsPath
	a.Fleet.AssignmentsPath = filepath.Join(t.TempDir(), "missing.json")
	if err = cycles[3].run(t.Context()); err == nil {
		t.Fatal("設定取得失敗が無視されました")
	}
	var count int
	if err = a.DB.QueryRow(`SELECT count(*) FROM settings WHERE key='user-wait-notice'`).Scan(&count); err != nil || count != 0 || calls != 0 {
		t.Fatal(count, calls, err)
	}
	a.Fleet.AssignmentsPath = path
	if err = cycles[3].run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = cycles[3].run(t.Context()); err != nil || calls != 1 || reads != 4 {
		t.Fatal(err, calls, reads)
	}
	// Change the actual assignment snapshot between cycles; a lower limit must be used.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var assignment map[string]any
	if err = json.Unmarshal(raw, &assignment); err != nil {
		t.Fatal(err)
	}
	assignment["implementationRunLimit"] = 2
	raw, err = json.Marshal(assignment)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DB.Exec(`UPDATE jobs SET hold_reason='question',generation=1,view='{}' WHERE repo='owner/repo' AND issue=7`); err != nil {
		t.Fatal(err)
	}
	if err = cycles[3].run(t.Context()); err != nil || calls != 2 {
		t.Fatal(err, calls)
	}
}
