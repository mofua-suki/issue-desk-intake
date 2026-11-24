package issueapi

import (
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/listscope"
	"issue-desk-intake/server/internal/workplan"
	"strings"
	"testing"
)

func TestExecutionProgressDoesNotCompleteOnMergeAlone(t *testing.T) {
	p := &workplan.Plan{Version: 1, Requirements: []workplan.Requirement{{ID: "deploy", Criterion: "配備版", Owner: "controller", Phase: "deployment", Method: "稼働版", EvidenceKind: "external_check", Expected: "一致", Capabilities: []string{}, Inputs: []string{}, Children: []int{}}}}
	head := strings.Repeat("a", 40)
	j := &jobs.Job{Status: "merged", HeadSHA: &head, View: Object{"executionPlan": p.Record()}, Result: Object{"status": "merged"}}
	out := executionPlanProjection(j)
	if out["pending"] != float64(1) {
		t.Fatal(out)
	}
	j.View["executionReceipts"] = []workplan.Evidence{{Requirement: "deploy", PlanSHA: p.Fingerprint(), Head: head, Source: "controller", Status: "passed", Detail: "record 1"}}
	out = executionPlanProjection(j)
	if out["pending"] != float64(0) {
		t.Fatal(out)
	}
	raw, _ := json.Marshal(out)
	if len(raw) == 0 {
		t.Fatal("表示不能")
	}
}
func TestMalformedExecutionPlanRemainsUnavailable(t *testing.T) {
	out := executionPlanProjection(&jobs.Job{View: Object{"executionPlan": Object{"version": 7}}})
	if out["unavailable"] == nil || out["pending"] != nil {
		t.Fatal(out)
	}
}

func TestClosedIssueRetainsUnfinishedReportInActiveList(t *testing.T) {
	f := newFixture(t)
	p := &workplan.Plan{Version: 1, Requirements: []workplan.Requirement{{ID: "report", Criterion: "訂正", Owner: "reporter", Phase: "report", Method: "投稿", Expected: "訂正済み", EvidenceKind: "report_comment"}}}
	f.api.issue["state"] = "closed"
	f.api.issues = []Object{clone(f.api.issue)}
	view, _ := json.Marshal(Object{"executionPlan": p.Record(), "codeMerged": true, "executionWait": "reporter"})
	if _, err := f.pg.Exec(`UPDATE jobs SET status='held',hold_reason='review_held',view=$1::jsonb WHERE id=$2`, string(view), f.job); err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.list(listscope.WithScope(f.ctx, "active"))
	if err != nil || len(rows) != 1 || rows[0]["status"] != "review_held" || fleet.ObjectOrEmpty(rows[0]["executionProgress"])["pending"] != float64(1) {
		t.Fatalf("rows=%v error=%v", rows, err)
	}
	if _, err = f.pg.Exec(`UPDATE jobs SET status='closed',hold_reason=NULL WHERE id=$1`, f.job); err != nil {
		t.Fatal(err)
	}
	rows, err = f.s.list(listscope.WithScope(f.ctx, "active"))
	if err != nil || len(rows) != 0 {
		t.Fatalf("明示的に閉じた仕事を復活: rows=%v error=%v", rows, err)
	}
}
