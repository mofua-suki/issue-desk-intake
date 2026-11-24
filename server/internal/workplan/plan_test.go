package workplan

import (
	"strings"
	"testing"
)

func example() map[string]any {
	return map[string]any{"version": 1, "requirements": []any{map[string]any{"id": "behavior", "criterion": "入力の拒否", "owner": "implementer", "evidenceKind": "fixed_test", "phase": "verification", "method": "go test ./...", "expected": "拒否時に保存しない", "capabilities": []any{}, "inputs": []any{}, "children": []any{1}}, map[string]any{"id": "live", "criterion": "配備版の確認", "owner": "controller", "evidenceKind": "external_check", "phase": "deployment", "method": "稼働版を照合", "expected": "対象SHAと一致", "capabilities": []any{"realDevice"}, "inputs": []any{"配置記録"}, "children": []any{2}}}}
}
func TestChecksRequireMatchingExecutionEvidence(t *testing.T) {
	p, err := Parse(example())
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	base := Evidence{Requirement: "behavior", PlanSHA: p.Fingerprint(), Head: head, Source: "implementer", Status: "passed", Detail: "test run 7"}
	for _, tc := range []struct {
		name   string
		change func(*Evidence)
		want   string
	}{{"match", func(*Evidence) {}, "passed"}, {"old head", func(e *Evidence) { e.Head = strings.Repeat("b", 40) }, "pending"}, {"old plan", func(e *Evidence) { e.PlanSHA = "old" }, "pending"}, {"wrong owner", func(e *Evidence) { e.Source = "controller" }, "pending"}, {"no proof", func(e *Evidence) { e.Detail = "" }, "pending"}} {
		t.Run(tc.name, func(t *testing.T) {
			e := base
			tc.change(&e)
			checks := p.Checks(head, []Evidence{e})
			if checks[0].Status != tc.want || checks[1].Status != "pending" {
				t.Fatal(checks)
			}
		})
	}
}
func TestPartitionKeepsControllerConditions(t *testing.T) {
	p, err := Parse(example())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ForChild(1).Requirements) != 1 || p.ForChild(1).Requirements[0].Owner != "implementer" || p.ForChild(2).Requirements[0].Owner != "controller" {
		t.Fatal(p)
	}
}
func TestCannotAssignExternalCapabilityToImplementation(t *testing.T) {
	v := example()
	v["requirements"].([]any)[0].(map[string]any)["capabilities"] = []any{"otherRepository"}
	if _, err := Parse(v); err == nil {
		t.Fatal("外部資料を持たない実装担当へ配布できた")
	}
}
func TestLegacyAndMalformedRemainDifferent(t *testing.T) {
	if p, e := Parse(nil); p != nil || e != nil {
		t.Fatal(p, e)
	}
	if _, e := Parse(map[string]any{"version": 1}); e == nil {
		t.Fatal("不正な計画を旧形式扱いした")
	}
}
func TestEveryAcceptanceConditionAndChildMustBeCovered(t *testing.T) {
	p, _ := Parse(example())
	if err := p.Covers([]string{"入力の拒否", "配備版の確認"}); err != nil {
		t.Fatal(err)
	}
	if p.Covers([]string{"追加の未割当条件"}) == nil {
		t.Fatal("条件の欠落を見逃した")
	}
	if p.ValidateChildren(1) == nil {
		t.Fatal("存在しない子を見逃した")
	}
	if p.ValidateChildren(3) == nil {
		t.Fatal("未割当の子を見逃した")
	}
	if p.ValidateChildren(2) != nil {
		t.Fatal("正しい分割を拒否した")
	}
}

func TestMalformedEvidenceDoesNotKeepPassedPrefix(t *testing.T) {
	evidence, err := DecodeEvidence([]any{map[string]any{"requirement": "ok", "status": "passed"}, map[string]any{"head": 42}})
	if err == nil || len(evidence) != 0 {
		t.Fatalf("部分的な証拠を採用: %+v %v", evidence, err)
	}
}

func TestCoversBodyConditionsWrittenFromMethodAndExpected(t *testing.T) {
	p, err := Parse(example())
	if err != nil {
		t.Fatal(err)
	}
	// A reworded criterion still covers the body line built from its method and expected result.
	if err := p.Covers([]string{"確認: go test ./... → 期待: 拒否時に保存しない", "配備版の確認"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Covers([]string{"確認:  go test ./...  → 期待: 拒否時に保存しない"}); err != nil {
		t.Fatal("whitespace differences must not matter:", err)
	}
	err = p.Covers([]string{"確認: go test ./... → 期待: 別の結果"})
	if err == nil || !strings.Contains(err.Error(), "実行分担がない受入条件") {
		t.Fatal("an unassigned condition must stay refused:", err)
	}
}
