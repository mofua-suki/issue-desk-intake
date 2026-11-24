package jobs

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/store"
)

// All tests use store.Migrate's shipped schema and the public Store operations.
func notificationStore(t *testing.T, db *sql.DB, ctx context.Context) (*Store, func(...int64)) {
	t.Helper()
	notices := []int64{}
	s, err := New(db, Options{Now: func() time.Time { return now }, Publish: func(event map[string]any) {
		project, ok := event["project"].(string)
		number, numeric := event["number"].(int64)
		if len(event) != 3 || event["type"] != "issue" || !ok || !numeric {
			t.Fatalf("invalid live issue identifier: %+v", event)
		}
		// A different connection must already see the committed row.
		job, err := (&Store{db: db}).ByIssue(ctx, project, number)
		if err != nil || job == nil {
			t.Fatalf("notice before commit: %+v %v", event, err)
		}
		notices = append(notices, number)
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s, func(want ...int64) {
		t.Helper()
		if !reflect.DeepEqual(notices, append([]int64{}, want...)) {
			t.Fatalf("notices=%v want=%v", notices, want)
		}
		notices = []int64{}
	}
}

func TestPostgresIssueNotificationsReportFieldsAndNoChanges(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "nas", 41, 7)
	if _, err := db.ExecContext(ctx, "UPDATE jobs SET tier='standard' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	s, check := notificationStore(t, db, ctx)
	report := Report{Epoch: 7, Status: "running", View: map[string]any{"reviewHolds": 2}}
	if _, err := s.Report(ctx, id, "nas", report); err != nil {
		t.Fatal(err)
	}
	check()
	branch, sha, pr := "feature/notice", strings.Repeat("a", 40), int64(12)
	report.Branch = Optional[string]{Present: true, Value: &branch}
	report.PR = Optional[int64]{Present: true, Value: &pr}
	report.HeadSHA = Optional[string]{Present: true, Value: &sha}
	report.View["tierHistory"] = []any{map[string]any{"from": "standard", "to": "heavy", "reason": "レビューの自動差し戻し"}}
	if _, err := s.Report(ctx, id, "nas", report); err != nil {
		t.Fatal(err)
	}
	check(41)
	if _, err := s.Report(ctx, id, "nas", report); err != nil {
		t.Fatal(err)
	}
	check()
	report.Result = map[string]any{"summary": "保存した結果"}
	if _, err := s.Report(ctx, id, "nas", report); err != nil {
		t.Fatal(err)
	}
	check(41)
	hold := "review_held"
	report.Status, report.HoldReason = "held", &hold
	report.View = map[string]any{"phase": "review", "review": map[string]any{"summary": "要確認"}}
	if _, err := s.Report(ctx, id, "nas", report); err != nil {
		t.Fatal(err)
	}
	check(41)
	if _, err := s.Report(ctx, id, "nas", report); err != nil {
		t.Fatal(err)
	}
	check()
	report.Epoch--
	if _, err := s.Report(ctx, id, "nas", report); err == nil {
		t.Fatal("stale report accepted")
	}
	check()
	job, err := s.Get(ctx, id)
	if err != nil || job.Status != "held" || job.Tier == nil || *job.Tier != "heavy" || job.PR == nil || *job.PR != pr || job.Result["summary"] != "保存した結果" {
		t.Fatalf("committed projection: %+v %v", job, err)
	}
}

func TestPostgresIssueNotificationsCreateDispatchLeaseControlScopeDependencies(t *testing.T) {
	db, ctx := testPostgres(t)
	s, check := notificationStore(t, db, ctx)
	repo := "mofua-suki/issue-desk-intake"
	input := NewJob{Repo: repo, Issue: 101, Kind: "normal", Scope: []string{"server/"}}
	created, err := s.Create(ctx, input)
	if err != nil || !created.Created {
		t.Fatal(created, err)
	}
	check(101)
	if _, err = s.Create(ctx, input); err != nil {
		t.Fatal(err)
	}
	check()
	if _, err = db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4)`, now); err != nil {
		t.Fatal(err)
	}
	job, _, err := s.Next(ctx, "gt12", 4, balanceAssignments(t))
	if err != nil || job == nil || job.ID != created.Job.ID {
		t.Fatal(job, err)
	}
	check(101)
	for _, progress := range []map[string]any{nil, {"stage": "editing"}, {"stage": "testing"}} {
		if _, _, err = s.Renew(ctx, job.ID, "gt12", job.LeaseEpoch, progress); err != nil {
			t.Fatal(err)
		}
		check()
	}
	if _, err = s.Narrow(ctx, job.ID, "gt12", job.LeaseEpoch, []string{"server/one.go"}); err != nil {
		t.Fatal(err)
	}
	check(101)
	if _, err = s.Narrow(ctx, job.ID, "gt12", job.LeaseEpoch, []string{"server/one.go"}); err != nil {
		t.Fatal(err)
	}
	check()
	if _, err = s.Widen(ctx, job.ID, "gt12", job.LeaseEpoch, []string{"server/two.go"}, nil); err != nil {
		t.Fatal(err)
	}
	check(101)
	control := ControlInput{ID: "11111111-1111-4111-8111-111111111111", Action: "instruction", By: "screen", Text: "結果も確認する"}
	if _, err = s.Control(ctx, job.ID, control, ControlOptions{}); err != nil {
		t.Fatal(err)
	}
	check(101)
	if _, err = s.Control(ctx, job.ID, control, ControlOptions{}); err != nil {
		t.Fatal(err)
	}
	check()
	child, err := s.Create(ctx, NewJob{Repo: repo, Issue: 102, Kind: "normal", Scope: []string{"client/"}})
	if err != nil {
		t.Fatal(err)
	}
	check(102)
	if n, err := s.AddDependencies(ctx, child.Job.ID, []int64{job.ID, job.ID}); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	check(102)
	if n, err := s.AddDependencies(ctx, child.Job.ID, []int64{job.ID}); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	check()
	control = ControlInput{ID: "22222222-2222-4222-8222-222222222222", Action: "priority", Value: "high", Reason: "急ぎ", By: "screen"}
	if _, err = s.Control(ctx, child.Job.ID, control, ControlOptions{}); err != nil {
		t.Fatal(err)
	}
	check(102)
	// A dispatch skip changes the list's dispatchRecord even without a jobs UPDATE.
	if _, _, err = s.Next(ctx, "gt12", 4, balanceAssignments(t)); err != nil {
		t.Fatal(err)
	}
	check(102)
}

func TestPostgresIssueNotificationsSweepMultipleIssues(t *testing.T) {
	db, ctx := testPostgres(t)
	first := seedJob(t, ctx, db, "owner/repo", "nas", 1, 1)
	second := seedJob(t, ctx, db, "other/repo", "nas", 2, 1)
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET lease_expires_at=$1 WHERE id IN ($2,$3)`, now.Add(-time.Minute), first, second); err != nil {
		t.Fatal(err)
	}
	s, check := notificationStore(t, db, ctx)
	hold := "scope_wait"
	if _, err := s.Create(ctx, NewJob{Repo: "owner/repo", Issue: 3, Kind: "normal", Status: "held", HoldReason: &hold, Scope: []string{"isolated.go"}}); err != nil {
		t.Fatal(err)
	}
	check(3)
	if err := s.Sweep(ctx, nil); err != nil {
		t.Fatal(err)
	}
	check(2, 1, 3)
	if err := s.Sweep(ctx, nil); err != nil {
		t.Fatal(err)
	}
	check()
}

func TestPostgresIssueNotificationsRollbackCommitFailureAndSavepoint(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
	s, check := notificationStore(t, db, ctx)
	if _, err := s.Create(ctx, NewJob{Repo: "owner/repo", Issue: 2, Kind: "normal", DependsOn: []int64{999999}}); err == nil {
		t.Fatal("missing predecessor accepted")
	}
	check()
	if job, err := s.ByIssue(ctx, "owner/repo", 2); err != nil || job != nil {
		t.Fatal("failed creation was not rolled back", job, err)
	}
	sentinel := errors.New("merged hook rejected")
	s.options.OnMerged = func(ctx context.Context, tx *sql.Tx, job *Job, _ map[string]any) error {
		check() // Work has updated the row but has not committed.
		var status string
		if err := db.QueryRowContext(ctx, "SELECT status FROM jobs WHERE id=$1", job.ID).Scan(&status); err != nil || status != "running" {
			t.Fatal(status, err)
		}
		return sentinel
	}
	if _, err := s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "merged"}); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	check()
	// A deferred database failure happens at Commit, after target collection.
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION reject_job_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'rejected at commit'; END $$;
 CREATE CONSTRAINT TRIGGER reject_job_commit AFTER UPDATE ON jobs DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_job_commit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "running", Result: map[string]any{"summary": "unsaved"}}); err == nil {
		t.Fatal("commit failure lost")
	}
	check()
	if _, err := db.ExecContext(ctx, "DROP TRIGGER reject_job_commit ON jobs"); err != nil {
		t.Fatal(err)
	}
	if err := s.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "SAVEPOINT before_pr"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE jobs SET pr=42 WHERE id=$1", id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT before_pr")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	check()
	if _, err := s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "running", Result: map[string]any{"summary": "saved"}}); err != nil {
		t.Fatal(err)
	}
	check(1)
	job, err := s.Get(ctx, id)
	if err != nil || job.PR != nil || job.Result["summary"] != "saved" || job.Status != "running" {
		t.Fatal(job, err)
	}
}

func TestPostgresIssueNotificationsResolveFilingReferences(t *testing.T) {
	db, ctx := testPostgres(t)
	s, check := notificationStore(t, db, ctx)
	if _, err := s.Create(ctx, NewJob{Repo: "owner/repo", Issue: 1, Kind: "normal"}); err != nil {
		t.Fatal(err)
	}
	check(1)
	if _, err := s.Create(ctx, NewJob{Repo: "owner/repo", Issue: 2, Kind: "normal", View: map[string]any{
		"unresolvedDependencies": []any{map[string]any{"repo": "owner/repo", "issue": 1}},
	}}); err != nil {
		t.Fatal(err)
	}
	check(2)
	if err := s.ResolveFilingReferences(ctx); err != nil {
		t.Fatal(err)
	}
	check(2)
	if err := s.ResolveFilingReferences(ctx); err != nil {
		t.Fatal(err)
	}
	check()
}

func TestPostgresIssueNotificationsOptionalPublisherAndReopen(t *testing.T) {
	db, ctx := testPostgres(t)
	db.SetMaxOpenConns(1) // Exercise transaction-local state on a reused connection.
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(ctx, NewJob{Repo: "owner/repo", Issue: 1, Kind: "normal"})
	if err != nil {
		t.Fatal(err)
	}
	// Reapplying migrations and reopening preserves existing jobs and needs no data conversion.
	if _, err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	count := 0
	s, err = New(db, Options{Publish: func(map[string]any) { count++ }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Control(ctx, created.Job.ID, ControlInput{ID: "11111111-1111-4111-8111-111111111111", Action: "stop"}, ControlOptions{}); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal(count)
	}
	s, err = New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Control(ctx, created.Job.ID, ControlInput{ID: "22222222-2222-4222-8222-222222222222", Action: "resume"}, ControlOptions{}); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("publisher leaked across transactions")
	}
	job, err := s.Get(ctx, created.Job.ID)
	if err != nil || job.Status != "queued" {
		t.Fatal(job, err)
	}
}
