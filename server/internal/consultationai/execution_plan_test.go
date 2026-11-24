package consultationai

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/workplan"
	"strings"
	"testing"
)

func TestExternalSyntheticFixedSuiteClassification(t *testing.T) {
	verify := Document{Project: "owner/app", Path: "scripts/verify.sh", Request: ReadRequest{Mode: "full"}, Text: "root_test() { \"$NODE\" --test --test-concurrency=4 test/*.test.ts; }\nstage root-test root_test"}
	testDoc := Document{Project: "owner/app", Path: "test/example.test.ts", Request: ReadRequest{Mode: "full"}, Text: "synthetic test"}
	m := &materials{state: contextState{Projects: map[string]*projectState{"owner/app": {Reads: []Document{verify, testDoc}}}, Delivered: map[string]bool{documentKey(verify): true, documentKey(testDoc): true}}}
	for _, tc := range []struct {
		name, extra            string
		split, delivered, want bool
	}{
		{"通常案", "", false, true, true}, {"分割子", "", true, true, true},
		{"本番", "本番でも確認", false, true, false}, {"別環境", "別環境で確認", true, true, false},
		{"未取得資料", "未取得資料と照合", false, true, false}, {"独立レビュー", "独立レビューを行う", false, true, false},
		{"個別承認", "個別承認を得る", true, true, false}, {"未配信", "", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m.state.Delivered[documentKey(verify)] = tc.delivered
			criterion := "確認: `node --test test/example.test.ts` → 期待: 合成データのテスト成功 " + tc.extra
			r := workplan.Requirement{ID: "synthetic", Criterion: criterion, Owner: "controller", Phase: "verification", Method: "node --test test/example.test.ts", Expected: "合成データのテスト成功 " + tc.extra, EvidenceKind: "external_check"}
			fixed := workplan.Requirement{ID: "implementation", Criterion: "実装の検証", Owner: "implementer", Phase: "verification", Method: "go test", Expected: "成功", EvidenceKind: "fixed_test"}
			plan := &workplan.Plan{Version: 1, Requirements: []workplan.Requirement{r, fixed}}
			a := Record{"ready": true, "project": "owner/app", "body": "## 受入条件\n- " + criterion + "\n- 実装の検証", "executionPlan": plan.Record()}
			if tc.split {
				a["splitIssues"] = []any{Record{"project": "owner/app", "acceptance": []any{criterion, "実装の検証"}}}
			}
			got := executionPlanWithMaterialsProblem(a, m)
			if (strings.Contains(got, "条件synthetic") && strings.Contains(got, "fixed_test")) != tc.want {
				t.Fatalf("problem=%q", got)
			}
			if !tc.want && got != "" {
				t.Fatalf("unexpected problem=%q", got)
			}
		})
	}
}

func TestExecutionInputsRequireDeliveredRange(t *testing.T) {
	doc := Document{Project: "owner/repo", Path: "docs/input.md", Request: ReadRequest{Mode: "lines", Start: 10, End: 20}, Text: "10: 根拠", Size: 12}
	delivered := map[string]bool{documentKey(doc): true}
	for _, tc := range []struct {
		input string
		want  bool
	}{{"docs/input.md:10-20", true}, {"owner/repo:docs/input.md:12", true}, {"docs/input.md:21-30", false}, {"docs/input.md", false}, {"other/repo:docs/input.md:12", false}} {
		if got := executionInputAvailable(tc.input, doc.Project, doc, delivered); got != tc.want {
			t.Fatal(tc, got)
		}
	}
	if executionInputAvailable("docs/input.md:12", doc.Project, doc, nil) {
		t.Fatal("未配信資料を採用")
	}
}

func TestExecutionPlanMaterialInputs(t *testing.T) {
	doc := Document{Project: "owner/app", Path: "worker.go", Request: ReadRequest{Mode: "lines", Start: 10, End: 20}, Text: "既存実装", Size: 30}
	m := &materials{state: contextState{Projects: map[string]*projectState{"owner/app": {Reads: []Document{doc}}}, Delivered: map[string]bool{documentKey(doc): true}}}
	answer := gateReply()
	answer["project"] = "owner/app"
	r := fleet.Object(fleet.Elements(fleet.Object(answer["executionPlan"])["requirements"])[0])
	r["method"] = "実装後の差分とテスト結果を確認"
	r["expected"] = "規則が廃止されテスト成功"
	for _, tc := range []struct {
		input string
		ok    bool
	}{
		{"", true}, {"worker.go:10-20", true}, {"worker.go:21-30", false}, {"変更後のWorkerとテスト", false},
	} {
		inputs := []any{}
		if tc.input != "" {
			inputs = append(inputs, tc.input)
		}
		r["inputs"] = inputs
		if problem := executionPlanWithMaterialsProblem(answer, m); (problem == "") != tc.ok {
			t.Fatalf("input=%q problem=%q", tc.input, problem)
		}
	}
	m.state.Delivered = nil
	r["inputs"] = []any{"worker.go:10-20"}
	if problem := executionPlanWithMaterialsProblem(answer, m); problem == "" {
		t.Fatal("未配信の既存資料を受理")
	}
}

func TestExecutionInputMultipleDeliveredRanges(t *testing.T) {
	makeDoc := func(project, mode string, start, end int) Document {
		body := "本文"
		if mode == "lines" {
			lines := []string{}
			for n := start; n <= end; n++ {
				lines = append(lines, fmt.Sprintf("%d: 本文", n))
			}
			body = strings.Join(lines, "\n")
		}
		return Document{Project: project, Path: "worker.go", Request: ReadRequest{Mode: mode, Start: start, End: end}, Text: body, Size: len(body)}
	}
	full := makeDoc("owner/app", "full", 0, 0)
	first := makeDoc("owner/app", "lines", 22, 69)
	second := makeDoc("owner/app", "lines", 113, 180)
	third := makeDoc("owner/app", "lines", 181, 247)
	other := makeDoc("other/app", "lines", 181, 247)
	answer := gateReply()
	answer["project"] = "owner/app"
	r := fleet.Object(fleet.Elements(fleet.Object(answer["executionPlan"])["requirements"])[0])
	r["inputs"] = []any{"worker.go:22-69,113-247"}
	for _, tc := range []struct {
		name        string
		docs        []Document
		undelivered *Document
		want        bool
	}{
		{"full", []Document{full}, nil, true},
		{"split and contiguous", []Document{first, second, third}, nil, true},
		{"missing part", []Document{first, second}, nil, false},
		{"undelivered part", []Document{first, second, third}, &third, false},
		{"different project", []Document{first, second, other}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &materials{state: contextState{Projects: map[string]*projectState{}, Delivered: map[string]bool{}}}
			for _, doc := range tc.docs {
				if m.state.Projects[doc.Project] == nil {
					m.state.Projects[doc.Project] = &projectState{}
				}
				m.state.Projects[doc.Project].Reads = append(m.state.Projects[doc.Project].Reads, doc)
				m.state.Delivered[documentKey(doc)] = true
			}
			if tc.undelivered != nil {
				delete(m.state.Delivered, documentKey(*tc.undelivered))
			}
			if got := executionPlanWithMaterialsProblem(answer, m); (got == "") != tc.want {
				t.Fatalf("problem=%q want available=%v", got, tc.want)
			}
		})
	}
}

func TestExecutionPlanRequiresOneCompatibleCapableHostBeforeFiling(t *testing.T) {
	plan := &workplan.Plan{Version: 1, Requirements: []workplan.Requirement{{ID: "one", Criterion: "内容", Owner: "implementer", Phase: "verification", Method: "検証", Expected: "成功", EvidenceKind: "fixed_test", Capabilities: []string{"docker", "browser"}}}}
	answer := Record{"ready": true, "executionPlan": plan.Record()}
	for _, tc := range []struct {
		name  string
		hosts []EnvironmentHost
		want  bool
	}{
		{"unknown", []EnvironmentHost{{Name: "one", Values: map[string]any{"executionPlanVersion": 1}}}, false},
		{"old worker", []EnvironmentHost{{Name: "one", Values: map[string]any{"docker": true, "browser": true}}}, false},
		{"different hosts", []EnvironmentHost{{Name: "one", Values: map[string]any{"executionPlanVersion": 1, "docker": true}}, {Name: "two", Values: map[string]any{"executionPlanVersion": 1, "browser": true}}}, false},
		{"same host", []EnvironmentHost{{Name: "one", Values: map[string]any{"executionPlanVersion": float64(1), "docker": true, "browser": true}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{options: Options{Environment: func(context.Context, string) ([]EnvironmentHost, error) { return tc.hosts, nil }}}
			if got := s.executionPlanCapabilityProblem(t.Context(), Record{"project": "owner/app"}, answer); (got == "") != tc.want {
				t.Fatal(got)
			}
		})
	}
	plan.Requirements[0].Owner = "controller"
	plan.Requirements[0].EvidenceKind = "external_check"
	answer["executionPlan"] = plan.Record()
	if got := (&Service{}).executionPlanCapabilityProblem(t.Context(), Record{"project": "owner/app"}, answer); got != "" {
		t.Fatal(got)
	}
}

func TestExecutionPlanCapabilityFailureIsRepairedBeforeReady(t *testing.T) {
	repaired := false
	s, d := testService(t, func(_ context.Context, request ai.Request) (ai.Answer, error) {
		if strings.Contains(request.Input, "同じ担当PCで確認できません") {
			repaired = true
		}
		value := gateReply()
		requirement := fleet.Object(fleet.Elements(fleet.Object(value["executionPlan"])["requirements"])[0])
		if !repaired {
			requirement["capabilities"] = []any{"docker"}
		}
		return measuredAnswer(value), nil
	})
	s.options.Environment = func(context.Context, string) ([]EnvironmentHost, error) {
		return []EnvironmentHost{{Name: "worker", Values: map[string]any{"executionPlanVersion": 1, "docker": false}}}, nil
	}
	result, err := s.Turn(t.Context(), d)
	if err != nil || result["ready"] != true || !repaired {
		t.Fatal(result, err, repaired)
	}
}

func TestExecutionPlanCapabilityAggregatesAllConditionsPerIssue(t *testing.T) {
	first := workplan.Requirement{ID: "docker", Criterion: "Docker", Owner: "implementer", Phase: "verification", Method: "検証", Expected: "成功", EvidenceKind: "fixed_test", Capabilities: []string{"docker"}}
	second := first
	second.ID, second.Criterion, second.Capabilities = "browser", "Browser", []string{"browser"}
	plan := &workplan.Plan{Version: 1, Requirements: []workplan.Requirement{first, second}}
	s := &Service{options: Options{Environment: func(context.Context, string) ([]EnvironmentHost, error) {
		return []EnvironmentHost{{Name: "one", Values: map[string]any{"executionPlanVersion": 1, "docker": true}}, {Name: "two", Values: map[string]any{"executionPlanVersion": 1, "browser": true}}}, nil
	}}}
	answer := Record{"ready": true, "executionPlan": plan.Record()}
	if got := s.executionPlanCapabilityProblem(t.Context(), Record{"project": "owner/app"}, answer); got == "" {
		t.Fatal("別条件を別PCへ分散して起票できた")
	}
	plan.Requirements[0].Children = []int{1}
	plan.Requirements[1].Children = []int{1}
	answer["executionPlan"] = plan.Record()
	answer["splitIssues"] = []any{Record{"project": "owner/app"}}
	if got := s.executionPlanCapabilityProblem(t.Context(), Record{}, answer); got == "" {
		t.Fatal("同じ子の条件を別PCへ分散して起票できた")
	}
	plan.Requirements[1].Children = []int{2}
	answer["executionPlan"] = plan.Record()
	answer["splitIssues"] = []any{Record{"project": "owner/app"}, Record{"project": "owner/app"}}
	if got := s.executionPlanCapabilityProblem(t.Context(), Record{}, answer); got != "" {
		t.Fatal(got)
	}
}

func TestExecutionPlanMaterialsPerChild(t *testing.T) {
	doc := func(repo, path string) Document {
		return Document{Project: repo, Path: path, Request: ReadRequest{Mode: "lines", Start: 10, End: 11}, Text: "10: 本文\n11: 本文", Size: 20}
	}
	a, b := doc("owner/a", "docs/spec.md"), doc("owner/b", "docs/spec.md")
	sharedA, sharedB := doc("owner/a", "shared.md"), doc("owner/b", "shared.md")
	req := func(id, input string, children []int) workplan.Requirement {
		return workplan.Requirement{ID: id, Criterion: id, Owner: "implementer", Phase: "verification", Method: "検証", Expected: "成功", EvidenceKind: "fixed_test", Inputs: []string{input}, Children: children}
	}
	plan := &workplan.Plan{Version: 1, Requirements: []workplan.Requirement{req("A", "docs/spec.md:10-11", []int{1}), req("B", "docs/spec.md:10-11", []int{2}), req("shared", "shared.md:10-11", nil)}}
	answer := Record{"ready": true, "project": "owner/a", "body": "## 受入条件\n- A\n- B\n- shared", "executionPlan": plan.Record(), "splitIssues": []any{Record{"project": "owner/a", "acceptance": []any{"A", "shared"}}, Record{"project": "owner/b", "acceptance": []any{"B", "shared"}}}}
	m := &materials{state: contextState{Projects: map[string]*projectState{}, Delivered: map[string]bool{}}}
	add := func(d Document) {
		if m.state.Projects[d.Project] == nil {
			m.state.Projects[d.Project] = &projectState{}
		}
		m.state.Projects[d.Project].Reads = append(m.state.Projects[d.Project].Reads, d)
		m.state.Delivered[documentKey(d)] = true
	}
	add(b)
	check := func(parts ...string) {
		t.Helper()
		got := executionPlanWithMaterialsProblem(answer, m)
		for _, part := range parts {
			if !strings.Contains(got, part) {
				t.Fatalf("problem=%q missing %q", got, part)
			}
		}
	}
	check("分割案1", "owner/a", "docs/spec.md")
	add(a)
	check("分割案1", "shared.md")
	add(sharedA)
	check("分割案2", "owner/b", "shared.md")
	add(sharedB)
	if got := executionPlanWithMaterialsProblem(answer, m); got != "" {
		t.Fatal(got)
	}
	delete(m.state.Delivered, documentKey(b))
	check("分割案2", "docs/spec.md")
	m.state.Delivered[documentKey(b)] = true
	plan.Requirements[0].Inputs = []string{"owner/b:docs/spec.md:10-11"}
	answer["executionPlan"] = plan.Record()
	delete(m.state.Delivered, documentKey(a))
	if got := executionPlanWithMaterialsProblem(answer, m); got != "" {
		t.Fatal(got)
	}
	delete(m.state.Delivered, documentKey(b))
	check("分割案1", "owner/b", "docs/spec.md")
	answer["splitIssues"] = []any{}
	plan.Requirements = []workplan.Requirement{req("A", "docs/spec.md:10-11", nil)}
	answer["body"] = "## 受入条件\n- A"
	answer["executionPlan"] = plan.Record()
	m.state.Delivered[documentKey(b)] = true
	check("owner/a", "docs/spec.md")
	m.state.Delivered[documentKey(a)] = true
	if got := executionPlanWithMaterialsProblem(answer, m); got != "" {
		t.Fatal(got)
	}
	plan.Requirements[0].Inputs = []string{"docs/spec.md:10-12"}
	answer["executionPlan"] = plan.Record()
	check("owner/a", "docs/spec.md:10-12")
}
