package consultationai

import (
	"strings"
	"testing"

	"issue-desk-intake/server/internal/workplan"
)

// 変更するファイルを宣言しながら固定テストの受入条件が無い計画は、Workerが実装しないため起票前に直させる。
func TestChangeScopeRequiresImplementationEvidence(t *testing.T) {
	external := workplan.Requirement{ID: "deploy", Criterion: "配置後に画面で確かめる", Owner: "controller", Phase: "deployment", Method: "画面を開く", Expected: "表示される", EvidenceKind: "external_check"}
	report := workplan.Requirement{ID: "report", Criterion: "結果を報告する", Owner: "reporter", Phase: "report", Method: "報告", Expected: "投稿される", EvidenceKind: "report_comment"}
	fixed := workplan.Requirement{ID: "test", Criterion: "固定テストが通る", Owner: "implementer", Phase: "verification", Method: "go test", Expected: "成功", EvidenceKind: "fixed_test"}
	answer := func(scope any, requirements ...workplan.Requirement) Record {
		criteria := []string{}
		for _, r := range requirements {
			criteria = append(criteria, "- "+r.Criterion)
		}
		plan := &workplan.Plan{Version: 1, Requirements: requirements}
		return Record{"ready": true, "body": "## 受入条件\n" + strings.Join(criteria, "\n") + "\n", "executionPlan": plan.Record(), "changeScope": scope}
	}
	scope := Record{"known": true, "paths": []any{"server/a.go", "server/b.go"}}
	for name, a := range map[string]Record{"external only": answer(scope, external), "report only": answer(scope, external, report)} {
		if got := executionPlanProblem(a); !strings.Contains(got, "変更範囲に2件のファイル") || !strings.Contains(got, `"known":false`) {
			t.Fatalf("%s with files: %q", name, got)
		}
	}
	for name, a := range map[string]Record{
		"no files":        answer(Record{"known": true, "paths": []any{}}, external),
		"no scope":        answer(nil, external),
		"with fixed test": answer(scope, external, fixed),
		"fixed test only": answer(scope, fixed),
		"unknown scope":   answer(Record{"known": false}, external),
	} {
		if got := executionPlanProblem(a); got != "" {
			t.Fatalf("%s: %q", name, got)
		}
	}
}
