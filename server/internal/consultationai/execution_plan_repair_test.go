package consultationai

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
)

const repairCondition = "確認: go test ./... → 期待: 終了コード0"
const repairCondition2 = "確認: TestOther → 期待: 件数が2件"

func runInputRepair(t *testing.T, initialInputs []any) {
	t.Helper()
	repairs := 0
	s, d := testService(t, func(_ context.Context, request ai.Request) (ai.Answer, error) {
		v := gateReply()
		v["body"] = "## 受入条件\n- 確認: go test ./... → 期待: 規則の廃止後にテストが成功"
		r := fleet.Object(fleet.Elements(fleet.Object(v["executionPlan"])["requirements"])[0])
		r["criterion"] = "確認: go test ./... → 期待: 規則の廃止後にテストが成功"
		r["method"] = "go test ./... で変更後のWorkerとテストを確認"
		r["expected"] = "規則の廃止後にテストが成功"
		if strings.Contains(request.Input, "必要資料をまだ配信していません") {
			repairs++
		}
		if repairs > 0 {
			r["inputs"] = []any{"README.md"}
		} else {
			r["inputs"] = initialInputs
		}
		return measuredAnswer(v), nil
	})
	result, err := s.Turn(t.Context(), d)
	if err != nil || result["ready"] != true || repairs != 1 {
		t.Fatalf("result=%v err=%v repairs=%d", result, err, repairs)
	}
	p := fleet.Object(result["executionPlan"])
	r := fleet.Object(fleet.Elements(p["requirements"])[0])
	if inputs := fleet.Elements(r["inputs"]); len(inputs) != 1 || inputs[0] != "README.md" {
		t.Fatalf("inputs=%v", inputs)
	}
}

func TestExecutionPlanInputRepair(t *testing.T) {
	runInputRepair(t, []any{"変更後のWorkerとテスト"})
}

func TestExecutionPlanInputGuidance(t *testing.T) {
	repaired := false
	s, d := testService(t, func(_ context.Context, request ai.Request) (ai.Answer, error) {
		v := gateReply()
		r := fleet.Object(fleet.Elements(fleet.Object(v["executionPlan"])["requirements"])[0])
		if strings.Contains(request.Input, "必要資料をまだ配信していません") {
			repaired = true
		}
		if repaired {
			r["inputs"] = []any{"missing.go:1-10"}
		} else {
			r["inputs"] = []any{"実装後のテスト結果"}
		}
		return measuredAnswer(v), nil
	})
	_, err := s.Turn(t.Context(), d)
	var failure *ai.Failure
	if !repaired || !errors.As(err, &failure) || failure.Kind != "reply_check" || !strings.Contains(failure.Message, "missing.go:1-10") {
		t.Fatalf("repaired=%v err=%v", repaired, err)
	}
}

func repairPlanReply(fixed bool) map[string]any {
	v := gateReply()
	v["body"] = "## 受入条件\n- " + repairCondition + "\n- " + repairCondition2
	requirements := fleet.Elements(fleet.Object(v["executionPlan"])["requirements"])
	first := fleet.Object(requirements[0])
	first["criterion"] = "言い換え"
	if !fixed {
		// Neither the criterion nor the method and expected result name the body condition.
		first["method"] = "別の確認"
	}
	if fixed {
		first["criterion"] = repairCondition
	}
	second := copyRecord(first)
	second["id"], second["criterion"] = "second", repairCondition2
	fleet.Object(v["executionPlan"])["requirements"] = append(requirements, second)
	return v
}

func runExecutionRepair(t *testing.T, fixed bool, totalLimit bool) (Record, []string, error) {
	t.Helper()
	t.Setenv("CODEX_HOME", t.TempDir())
	var prompts []string
	repairs := 0
	s, d := testService(t, func(_ context.Context, request ai.Request) (ai.Answer, error) {
		if request.OnSession != nil && request.Session == "" {
			if err := request.OnSession("11111111-1111-4111-8111-111111111112"); err != nil {
				t.Fatal(err)
			}
		}
		if strings.Contains(request.Input, "全受入条件の実行分担を直してください") {
			if !request.Resume {
				t.Fatal("修復で会話を再開していない")
			}
			repairs++
			prompts = append(prompts, request.Input)
		}
		return measuredAnswer(repairPlanReply(fixed && repairs > 0)), nil
	})
	if totalLimit {
		d["formatRepairs"] = Record{"total": 8}
	}
	result, err := s.Turn(t.Context(), d)
	return result, prompts, err
}

func TestExecutionPlanRepairShowsAcceptanceList(t *testing.T) {
	_, prompts, err := runExecutionRepair(t, true, false)
	if err != nil || len(prompts) != 1 {
		t.Fatalf("err=%v prompts=%d", err, len(prompts))
	}
	for _, condition := range []string{repairCondition, repairCondition2} {
		if !strings.Contains(prompts[0], condition) {
			t.Fatalf("条件がありません: %q", condition)
		}
	}
}

func TestExecutionPlanRepairRecovers(t *testing.T) {
	result, prompts, err := runExecutionRepair(t, true, false)
	if err != nil || result["ready"] != true || len(prompts) != 1 {
		t.Fatalf("result=%v err=%v repairs=%d", result, err, len(prompts))
	}
}

func TestExecutionPlanRepairStopsOnRepeatedProblem(t *testing.T) {
	_, prompts, err := runExecutionRepair(t, false, false)
	var failure *ai.Failure
	if !errors.As(err, &failure) || failure.Kind != "reply_check" || !strings.Contains(failure.Message, "実行計画を確定できません: 実行分担がない受入条件: "+repairCondition) || len(prompts) != 1 {
		t.Fatalf("err=%v repairs=%d", err, len(prompts))
	}
}

func TestExecutionPlanRepairIgnoresDraftTotalLimit(t *testing.T) {
	result, prompts, err := runExecutionRepair(t, true, true)
	if err != nil || result["ready"] != true || len(prompts) != 1 {
		t.Fatalf("result=%v err=%v repairs=%d", result, err, len(prompts))
	}
	s, d := testService(t, nil)
	d["formatRepairs"] = Record{"total": 8}
	ok, err := s.repair(t.Context(), d, map[string]any{}, map[string]int{}, "completion_plan")
	if err != nil || ok {
		t.Fatalf("other repair: ok=%v err=%v", ok, err)
	}
}

func TestExecutionRepairsDoNotExhaustOtherRepairs(t *testing.T) {
	s, d := testService(t, nil)
	config := map[string]any{}
	used := map[string]int{}
	for i := 0; i < 8; i++ {
		ok, err := s.repair(t.Context(), d, config, used, "execution_plan")
		if err != nil || !ok {
			t.Fatalf("execution repair %d: ok=%v err=%v", i, ok, err)
		}
	}
	saved, found, err := s.options.Records.Get(t.Context(), "drafts", fleet.Text(d["id"]))
	if err != nil || !found {
		t.Fatalf("reload: found=%v err=%v", found, err)
	}
	counts := fleet.Object(saved["formatRepairs"])
	if fleet.Number(counts["total"]) != 8 || fleet.Number(fleet.Object(counts["kinds"])["execution_plan"]) != 8 {
		t.Fatalf("recorded repairs: %v", counts)
	}
	for i := 0; i < 8; i++ {
		ok, err := s.repair(t.Context(), saved, config, map[string]int{}, "completion_plan")
		if err != nil || !ok {
			t.Fatalf("later repair %d: ok=%v err=%v", i, ok, err)
		}
	}
	ok, err := s.repair(t.Context(), saved, config, map[string]int{}, "draft_format")
	if err != nil || ok {
		t.Fatalf("other repair limit: ok=%v err=%v", ok, err)
	}
	counts = fleet.Object(saved["formatRepairs"])
	if fleet.Number(counts["total"]) != 16 || fleet.Number(fleet.Object(counts["kinds"])["completion_plan"]) != 8 {
		t.Fatalf("recorded repairs: %v", counts)
	}
}

func TestExecutionPlanAcceptsBodyBuiltFromMethodAndExpected(t *testing.T) {
	v := gateReply()
	v["body"] = "## 受入条件\n- " + repairCondition
	first := fleet.Object(fleet.Elements(fleet.Object(v["executionPlan"])["requirements"])[0])
	if "確認: "+fleet.Text(first["method"])+" → 期待: "+fleet.Text(first["expected"]) != repairCondition {
		t.Fatal("the fixture must state the body condition through method and expected")
	}
	first["criterion"] = "言い換えた条件"
	if problem := executionPlanProblem(v); problem != "" {
		t.Fatal(problem)
	}
}

func TestRewrittenAcceptanceBodyPassesCoverage(t *testing.T) {
	v := gateReply()
	fleet.Object(fleet.Elements(fleet.Object(v["executionPlan"])["requirements"])[0])["criterion"] = "言い換えた条件"
	rewritten := executableAcceptanceAnswer(v)
	if !strings.Contains(fleet.Text(rewritten["body"]), repairCondition) {
		t.Fatalf("body was not rewritten from method and expected: %q", rewritten["body"])
	}
	if problem := executionPlanProblem(rewritten); problem != "" {
		t.Fatal(problem)
	}
}

// The consultation for draft 5495f4ff stopped on "本文 2行：具体的な確認手段が不足"
// although each criterion named its go test command: the plan's method only said
// how to run that command. A concrete criterion of the same requirement passes,
// and a requirement without any concrete method still stops.
func TestAcceptanceReadsConcreteCriterionWhenMethodIsProse(t *testing.T) {
	v := gateReply()
	first := fleet.Object(fleet.Elements(fleet.Object(v["executionPlan"])["requirements"])[0])
	first["criterion"] = "確認: `go test ./internal/consultations -run MeasurementRelease` → 期待: 解除記録が1件残り、テストが終了コード0で完了する。"
	first["method"] = "serverディレクトリで指定コマンドを実行し、MeasurementReleaseに一致する追加テストの実行を確認する。"
	first["expected"] = "解除記録が1件残り、テストが終了コード0で完了する。"
	v["body"] = "## 受入条件\n- " + fleet.Text(first["criterion"])
	if problems := acceptanceProblems(v); len(problems) != 0 {
		t.Fatalf("a concrete criterion was rejected: %v", problems)
	}
	if problem := executionPlanProblem(executableAcceptanceAnswer(v)); problem != "" {
		t.Fatal(problem)
	}
	first["criterion"] = "解除されることを確認する"
	if problems := acceptanceProblems(v); len(problems) != 1 || !strings.Contains(problems[0], "具体的な確認手段") {
		t.Fatalf("a requirement without a concrete method passed: %v", problems)
	}
}

func TestIndependentPlanProblemsAreRepairedInOneCall(t *testing.T) {
	for _, repairsBoth := range []bool{true, false} {
		name := "completion problem remains"
		if repairsBoth {
			name = "both fixed"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			s, d := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
				calls++
				v := repairPlanReply(calls > 1)
				if calls == 2 {
					// 両方の直しを1回で送るときは、実行計画の種類で記録する。
					if req.CallTrigger != "format_repair" || req.RepairKind != "execution_plan" {
						t.Errorf("repair usage: trigger=%q kind=%q", req.CallTrigger, req.RepairKind)
					}
					for _, note := range []string{"全受入条件の実行分担を直してください", "起票前に完了までの担当・環境・検証・マージ・配置と承認を整理してください"} {
						if !strings.Contains(req.Input, note) {
							t.Errorf("repair request is missing %q", note)
						}
					}
				}
				if calls == 1 || !repairsBoth {
					delete(v, "completionPlan")
				}
				return measuredAnswer(v), nil
			})
			// This fixture isolates format repair from a separate overlap read.
			s.options.Reference = nil
			d["completionPlanVersion"] = 1
			result, err := s.Turn(t.Context(), d)
			if calls != 2 || (err == nil && result["ready"] == true) != repairsBoth {
				t.Fatalf("calls=%d ready=%v error=%v", calls, result["ready"], err)
			}
		})
	}
}

func TestExecutionPlanRepairChainsWhileProblemChanges(t *testing.T) {
	repairs := 0
	s, d := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
		if req.RepairKind == "execution_plan" {
			repairs++
		}
		v := repairPlanReply(repairs > 0)
		if repairs == 1 {
			r := fleet.Object(fleet.Elements(fleet.Object(v["executionPlan"])["requirements"])[0])
			r["inputs"] = []any{"missing.go:1-10"}
		}
		return measuredAnswer(v), nil
	})
	result, err := s.Turn(t.Context(), d)
	if err != nil || result["ready"] != true || repairs < 2 {
		t.Fatalf("result=%v err=%v repairs=%d", result, err, repairs)
	}
}

func TestExecutionPlanRepairHonorsFormatRepairLimit(t *testing.T) {
	repairs := 0
	s, d := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
		if req.RepairKind == "execution_plan" {
			repairs++
		}
		v := repairPlanReply(repairs > 0)
		if repairs > 0 {
			r := fleet.Object(fleet.Elements(fleet.Object(v["executionPlan"])["requirements"])[0])
			r["inputs"] = []any{fmt.Sprintf("missing%d.go:1-10", repairs)}
		}
		return measuredAnswer(v), nil
	})
	originalConfig := s.options.Config
	s.options.Config = func(ctx context.Context) (map[string]any, error) {
		config, err := originalConfig(ctx)
		fleet.Object(config["intake"])["formatRepairLimit"] = 2
		return config, err
	}
	_, err := s.Turn(t.Context(), d)
	var failure *ai.Failure
	if !errors.As(err, &failure) || failure.Kind != "reply_check" || repairs != 2 {
		t.Fatalf("err=%v repairs=%d", err, repairs)
	}
}
