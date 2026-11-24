package filingcheck

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/completionplan"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/workplan"
	"reflect"
	"strings"
	"testing"
)

func TestSplitFilingCheckIssueSections(t *testing.T) {
	p := completionplan.Plan{Version: 1, Basis: "親の共通条件", Approvals: []completionplan.Approval{}}
	for _, phase := range []string{"implementation", "verification", "merge", "deployment"} {
		p.Steps = append(p.Steps, completionplan.Step{Phase: phase, Owner: "担当", Prerequisite: "前提", Evidence: "確認"})
	}
	d := Record{"id": "11111111-1111-4111-8111-111111111111", "project": "owner/repo", "provider": "codex", "title": "子", "body": "## 目的\n共通条件を引き継ぐ", "ready": true, "questions": []any{}, "changeScope": Record{"known": true, "paths": []any{"child.go"}}, "parentSplit": "parent", "completionPlan": p.Record(), "splitBackground": "## 分割背景\n親本文\n全分割案\n兄弟専用 sibling.go <!-- marker -->"}
	target := Record{"body": d["body"]}
	if err := addSplitIssueSections(target, d); err != nil {
		t.Fatal(err)
	}
	prepared, err := filing.Prepare(context.Background(), filing.BuildConfig{Config: map[string]any{"model": "gpt-6.1"}, Project: func(string) error { return nil }}, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"completionPlanSection", "splitBackgroundSection"} {
		section := fleet.Text(target[key])
		if section == "" || !strings.Contains(prepared.Body, section) {
			t.Fatalf("%s: %q not in %s", key, section, prepared.Body)
		}
	}
	d["completionPlan"] = Record{"invalid": true}
	if err := addSplitIssueSections(Record{}, d); err == nil {
		t.Fatal("invalid approval plan accepted")
	}
	t.Run("判定資料生成入口と起票本文", func(t *testing.T) {
		f := newSiblingCheckFixture(t)
		f.d["completionPlan"] = p.Record()
		material, err := f.s.material(t.Context(), f.d)
		if err != nil {
			t.Fatal(err)
		}
		child := clone(f.d)
		child["id"] = "11111111-1111-4111-8111-111111111111"
		prepared, err := filing.Prepare(t.Context(), filing.BuildConfig{Config: map[string]any{"model": "gpt-6.1"}, Project: func(string) error { return nil }}, child)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"completionPlanSection", "splitBackgroundSection"} {
			section := fleet.Text(obj(material["target"])[key])
			if section == "" || !strings.Contains(prepared.Body, section) {
				t.Fatalf("%s not in prepared issue: %q", key, section)
			}
		}
	})
	t.Run("通常相談の判定資料と起票本文", func(t *testing.T) {
		f := newSiblingCheckFixture(t)
		delete(f.d, "parentSplit")
		f.d["completionPlan"] = p.Record()
		f.d["splitBackground"] = "通常相談には付けない背景"
		material, err := f.s.material(t.Context(), f.d)
		if err != nil {
			t.Fatal(err)
		}
		ordinary := clone(f.d)
		ordinary["id"] = "11111111-1111-4111-8111-111111111111"
		prepared, err := filing.Prepare(t.Context(), filing.BuildConfig{Config: map[string]any{"model": "gpt-6.1"}, Project: func(string) error { return nil }}, ordinary)
		if err != nil {
			t.Fatal(err)
		}
		target := obj(material["target"])
		section := fleet.Text(target["completionPlanSection"])
		if section == "" || !strings.Contains(prepared.Body, section) || target["splitBackgroundSection"] != nil {
			t.Fatalf("plan=%q background=%v body=%q", section, target["splitBackgroundSection"], prepared.Body)
		}
	})
	t.Run("通常相談の旧計画と拒否", func(t *testing.T) {
		f := newSiblingCheckFixture(t)
		delete(f.d, "parentSplit")
		delete(f.d, "completionPlan")
		material, err := f.s.material(t.Context(), f.d)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := obj(material["target"])["completionPlanSection"]; ok {
			t.Fatal("旧相談に空の計画節がある")
		}
		for name, plan := range map[string]any{
			"無効": Record{"invalid": true},
			"未承認": func() Record {
				q := p
				q.Approvals = []completionplan.Approval{{Category: "permission", ID: "deploy", Phase: "before_implementation", Target: "owner/repo", Operation: "権限変更", Condition: "確認"}}
				return q.Record()
			}(),
		} {
			t.Run(name, func(t *testing.T) {
				f.d["completionPlan"] = plan
				if _, err := f.s.material(t.Context(), f.d); err == nil {
					t.Fatal("計画が受理された")
				}
			})
		}
	})
}
func TestSplitFilingCheckExecutionPlan(t *testing.T) {
	f := newSiblingCheckFixture(t)
	plan := &workplan.Plan{Version: 1, Requirements: []workplan.Requirement{
		{ID: "first", Criterion: "第一の条件", Owner: "implementer", Phase: "verification", EvidenceKind: "fixed_test", Method: "非複製の手順", Expected: "成功", Inputs: []string{"private/input.go"}, Children: []int{1}},
		{ID: "second", Criterion: "第二の条件", Owner: "controller", Phase: "deployment", EvidenceKind: "external_check", Method: "別の手順", Expected: "稼働", Children: []int{2}},
	}}
	for _, tc := range []struct {
		name     string
		selected *workplan.Plan
		child    bool
	}{
		{"通常", plan, false}, {"分割子1", plan.ForChild(1), true}, {"分割子2", plan.ForChild(2), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := clone(f.d)
			if !tc.child {
				delete(d, "parentSplit")
			}
			d["id"] = "11111111-1111-4111-8111-111111111111"
			var body strings.Builder
			body.WriteString("## 受入条件\n")
			for _, r := range tc.selected.Requirements {
				body.WriteString("- " + r.Criterion + "\n")
			}
			d["body"] = body.String()
			d["executionPlan"] = tc.selected.Record()
			material, err := f.s.material(t.Context(), d)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := filing.Prepare(t.Context(), filing.BuildConfig{Config: map[string]any{"model": "gpt-6.1"}, Project: func(string) error { return nil }}, d)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := workplan.Parse(prepared.Job.View["executionPlan"])
			if err != nil {
				t.Fatal(err)
			}
			got := obj(material["target"])["executionPlanRequirements"]
			want := make([]Record, 0, len(saved.Requirements))
			for _, r := range saved.Requirements {
				want = append(want, Record{"id": r.ID, "criterion": r.Criterion, "owner": r.Owner, "phase": r.Phase, "evidenceKind": r.EvidenceKind})
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("資料=%v 保存=%v", got, want)
			}
		})
	}
	t.Run("未保存と不正", func(t *testing.T) {
		d := clone(f.d)
		delete(d, "executionPlan")
		material, err := f.s.material(t.Context(), d)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := obj(material["target"])["executionPlanRequirements"]; ok {
			t.Fatal("未保存の計画がある")
		}
		d["executionPlan"] = Record{"version": 999}
		if _, err := f.s.material(t.Context(), d); err == nil {
			t.Fatal("不正な計画を受理")
		}
		d["executionPlan"] = plan.ForChild(1).Record()
		d["body"] = "## 受入条件\n- 第二の条件"
		if _, err := f.s.material(t.Context(), d); err == nil {
			t.Fatal("別の子の条件を受理")
		}
	})
}
func TestSplitFilingCheckInputBudget(t *testing.T) {
	s := &Service{}
	body := "protected body"
	material := Record{"target": Record{"body": body, "completionPlanSection": "plan", "splitBackgroundSection": strings.Repeat("背景", 50000)}}
	result := s.fitInput(material, nil)
	if s.inputSize(result) > inputLimit || obj(result["target"])["body"] != body || obj(result["target"])["completionPlanSection"] != "plan" || obj(result["target"])["splitBackgroundSection"] != nil {
		t.Fatal("background not reduced safely")
	}
	omissions := inputOmissions(result)
	if len(omissions) != 1 || obj(omissions[0])["kind"] != "分割背景" || refusalCategory(Record{"proceed": false, "refusalCategory": "input_limit_only"}, omissions) != "input_limit_only" {
		t.Fatal(omissions)
	}
	huge := Record{"target": Record{"body": strings.Repeat("x", inputLimit)}}
	if s.inputSize(s.fitInput(huge, nil)) <= inputLimit {
		t.Fatal("protected target body was truncated")
	}
	t.Run("判定入口で省略を記録", func(t *testing.T) {
		var sent Record
		s, d, m := fixture(t, func(_ context.Context, call map[string]any) (map[string]any, error) {
			parts := strings.SplitN(fleet.Text(call["input"]), materialSeparator, 2)
			if len(parts) != 2 {
				t.Fatal("判定資料なし")
			}
			if err := json.Unmarshal([]byte(parts[1]), &sent); err != nil {
				t.Fatal(err)
			}
			return allowed(), nil
		})
		target := obj((*m)["target"])
		target["splitBackgroundSection"] = strings.Repeat("背景", 50000)
		if err := s.Check(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		if obj(sent["target"])["splitBackgroundSection"] != nil || len(fleet.Elements(sent[inputLimitOmittedKey])) == 0 {
			t.Fatal("省略が送信されていない", sent)
		}
		saved, _, err := s.options.Records.Get(t.Context(), "drafts", fleet.Text(d["id"]))
		if err != nil {
			t.Fatal(err)
		}
		checks := fleet.Elements(obj(saved["filingCheckCalls"])["checks"])
		if len(checks) == 0 || len(fleet.Elements(obj(checks[len(checks)-1])["inputOmissions"])) == 0 {
			t.Fatal("省略が記録されていない", checks)
		}
	})
	t.Run("判定入口で対象本文超過を停止", func(t *testing.T) {
		calls := 0
		s, d, m := fixture(t, func(context.Context, map[string]any) (map[string]any, error) { calls++; return allowed(), nil })
		obj((*m)["target"])["body"] = strings.Repeat("x", inputLimit)
		obj((*m)["target"])["splitBackgroundSection"] = strings.Repeat("背景", 50000)
		err := s.Check(t.Context(), d)
		if err == nil || !strings.Contains(err.Error(), "入力上限") || calls != 0 {
			t.Fatalf("error=%v calls=%d", err, calls)
		}
	})
}
