package filing

import (
	"context"
	"issue-desk-intake/server/internal/workplan"
	"testing"
)

func executionPlanFixture() Record {
	return Record{"version": 1, "requirements": []any{Record{"id": "a", "criterion": "動作を確認する", "owner": "implementer", "evidenceKind": "fixed_test", "phase": "verification", "method": "go test ./...", "expected": "終了コード0", "capabilities": []any{}, "inputs": []any{"fixture"}, "children": []any{1}}, Record{"id": "b", "criterion": "配置を確認する", "owner": "controller", "evidenceKind": "external_check", "phase": "deployment", "method": "稼働版の照合", "expected": "対象版と一致", "capabilities": []any{"realDevice"}, "inputs": []any{"配置記録"}, "children": []any{2}}}}
}
func TestPrepareAttachesExactExecutionContract(t *testing.T) {
	d := draftFixture()
	d["body"] = "## 受入条件\n- 動作を確認する\n- 配置を確認する"
	d["executionPlan"] = executionPlanFixture()
	prepared, err := Prepare(context.Background(), configFixture(), d)
	if err != nil {
		t.Fatal(err)
	}
	p, err := workplan.Parse(prepared.Job.View["executionPlan"])
	if err != nil || len(p.Requirements) != 2 || p.Requirements[1].Owner != "controller" || prepared.Job.View["executionPlanSHA"] != p.Fingerprint() {
		t.Fatal(prepared.Job.View, err)
	}
	if d["executionPlanSHA"] != nil {
		t.Fatal("保存元を書き換えた")
	}
}
