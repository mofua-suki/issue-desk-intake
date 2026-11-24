package issueapi

import (
	"encoding/json"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/workplan"
	"strings"
	"sync"
	"testing"
	"time"
)

func executionCheckFixture(t *testing.T) (*fixture, Object) {
	t.Helper()
	f := newFixture(t)
	plan := &workplan.Plan{Version: 1, Requirements: []workplan.Requirement{{ID: "report", Criterion: "訂正を投稿", Owner: "reporter", Phase: "report", Method: "資料を照合", Expected: "投稿済み", EvidenceKind: "report_comment", Capabilities: []string{}, Inputs: []string{}, Children: []int{}}}}
	raw, _ := json.Marshal(Object{"executionPlan": plan.Record()})
	head := strings.Repeat("a", 40)
	if _, err := f.pg.ExecContext(f.ctx, `UPDATE jobs SET status='held',hold_reason='review_held',head_sha=$2,view=$3::jsonb WHERE id=$1`, f.job, head, raw); err != nil {
		t.Fatal(err)
	}
	check := jobs.ExecutionCheck{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Requirement: "report", PlanSHA: plan.Fingerprint(), Head: head, Generation: 0, Status: "passed", Source: "README.md:1", ObservedAt: f.options.Now().Format(time.RFC3339), Detail: "訂正: 実装と資料を照合した結果"}
	return f, Object{"project": "owner/repo", "number": 1, "id": check.ID, "action": "record-check", "executionCheck": check}
}
func TestExecutionCheckReconcilesPostedReportAfterSaveFailure(t *testing.T) {
	f, data := executionCheckFixture(t)
	if _, err := f.pg.Exec(`CREATE FUNCTION reject_execution_check() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='execution_check' THEN RAISE EXCEPTION 'injected save failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_execution_check BEFORE INSERT ON job_events FOR EACH ROW EXECUTE FUNCTION reject_execution_check()`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.comment(f.ctx, data, auth.Principal{Kind: auth.PC, Name: "delegate", Delegated: true}); err == nil {
		t.Fatal("保存失敗を見落としました")
	}
	if f.api.commentCalls != 1 {
		t.Fatal(f.api.commentCalls)
	}
	if _, err := f.pg.Exec(`DROP TRIGGER reject_execution_check ON job_events; DROP FUNCTION reject_execution_check()`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.comment(f.ctx, data, auth.Principal{Kind: auth.PC, Name: "delegate", Delegated: true}); err != nil {
		t.Fatal(err)
	}
	if f.api.commentCalls != 1 {
		t.Fatal("同じ報告を重複投稿", f.api.commentCalls)
	}
}
func TestExecutionCheckSerializesConcurrentReplayAndRejectsUndelegated(t *testing.T) {
	f, data := executionCheckFixture(t)
	if _, err := f.s.comment(f.ctx, data, auth.Principal{Kind: auth.PC, Name: "nas"}); err == nil {
		t.Fatal("運用代行のないPCから記録")
	}
	if f.api.commentCalls != 0 {
		t.Fatal("拒否前に投稿")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.s.comment(f.ctx, data, auth.Principal{Kind: auth.PC, Name: "delegate", Delegated: true})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.api.commentCalls != 1 {
		t.Fatal("並行再送が二重投稿", f.api.commentCalls)
	}
}
