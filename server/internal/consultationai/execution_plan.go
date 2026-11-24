package consultationai

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/workplan"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func executionPlanAcceptanceList(answer Record) string {
	conditions := workplan.Conditions(fleet.Text(answer["body"]))
	if len(conditions) == 0 {
		return ""
	}
	return "\n本文の受入条件（そのまま写してください）:\n- " + strings.Join(conditions, "\n- ")
}

func executionPlanProblem(answer Record) string {
	if answer["ready"] != true {
		return ""
	}
	value, present := answer["executionPlan"]
	if !present {
		return ""
	}
	plan, err := workplan.Parse(value)
	if err != nil {
		return err.Error()
	}
	if plan == nil {
		return "受入条件ごとの実行主体・工程・確認手段・期待結果をexecutionPlanへ記録してください"
	}
	children := fleet.Elements(answer["splitIssues"])
	if len(children) == 0 {
		if err := plan.Covers(workplan.Conditions(fleet.Text(answer["body"]))); err != nil {
			return err.Error()
		}
		if problem := implementationEvidenceProblem(plan, answer["changeScope"]); problem != "" {
			return problem
		}
	} else {
		if err := plan.Covers(workplan.Conditions(fleet.Text(answer["body"]))); err != nil {
			return err.Error()
		}
		if err := plan.ValidateChildren(len(children)); err != nil {
			return err.Error()
		}
		for i, raw := range children {
			conditions := []string{}
			for _, condition := range fleet.Elements(fleet.Object(raw)["acceptance"]) {
				conditions = append(conditions, fleet.Text(condition))
			}
			if err := plan.ForChild(i + 1).Covers(conditions); err != nil {
				return fmt.Sprintf("分割案%d: %s", i+1, err)
			}
		}
	}
	return ""
}

// implementationEvidenceProblem は、変更するファイルを宣言しながら固定テストの受入条件が無い計画を見つける。
// Workerは fixed_test が無い計画では実装AIを呼ばない（外部確認だけなら保留、報告があれば報告だけで終える）ため、
// そのまま起票するとファイルが変わらない。
func implementationEvidenceProblem(plan *workplan.Plan, scope any) string {
	value := fleet.Object(scope)
	paths := len(fleet.Elements(value["paths"]))
	if value["known"] != true || paths == 0 {
		return ""
	}
	for _, r := range plan.Requirements {
		if r.EvidenceKind == "fixed_test" {
			return ""
		}
	}
	return fmt.Sprintf("変更範囲に%d件のファイルがありますが、固定テスト（fixed_test）の受入条件がありません。この計画ではWorkerが実装AIを呼ばず、ファイルは変わりません。変更を確かめる受入条件を固定テストにするか、ファイルを変えない依頼ならchangeScopeを{\"known\":false,\"paths\":[]}にしてください", paths)
}

func executionPlanWithMaterialsProblem(answer Record, m *materials) string {
	if problem := executionPlanProblem(answer); problem != "" {
		return problem
	}
	plan, err := workplan.Parse(answer["executionPlan"])
	if err != nil || plan == nil || answer["ready"] != true {
		return ""
	}
	root := fleet.Text(answer["project"])
	check := func(repo, label string, p *workplan.Plan) string {
		if problem := fixedSuiteClassificationProblem(repo, label, p, m); problem != "" {
			return problem
		}
		for _, r := range p.Requirements {
			if r.EvidenceKind == "external_check" {
				continue
			}
			for _, input := range r.Inputs {
				target := repo
				file := executionMultipleRanges.ReplaceAllString(input, "")
				file = evidenceRangeSuffix.ReplaceAllString(file, "")
				if prefix, _, ok := strings.Cut(file, ":"); ok && strings.Contains(prefix, "/") {
					target = prefix
				}
				docs := []Document{}
				if project := m.state.Projects[target]; project != nil {
					docs = append(docs, project.Reads...)
				}
				for _, doc := range m.evidence.documents {
					if doc.Project == target {
						docs = append(docs, doc)
					}
				}
				found := executionInputRangesAvailable(input, target, docs, m.state.Delivered)
				for _, doc := range docs {
					found = found || (doc.Project == target && executionInputAvailable(input, target, doc, m.state.Delivered))
				}
				if !found {
					return fmt.Sprintf("%s条件%sの必要資料をまだ配信していません: リポジトリ%s、%s。inputsは起票前に必要な既存資料のパス・範囲です。既存資料ならreadFilesで該当範囲を読み、または取得するコントローラーの条件へ分けてください。変更後の実装・差分やテスト結果など実装後の確認対象ならinputsから外してmethodとexpectedに記し、起票前に必要な既存資料は残してください", label, r.ID, target, input)
				}
			}
		}
		return ""
	}
	children := fleet.Elements(answer["splitIssues"])
	if len(children) == 0 {
		return check(root, "", plan)
	}
	for i, raw := range children {
		repo := fleet.Text(fleet.Object(raw)["project"])
		if repo == "" {
			repo = root
		}
		if problem := check(repo, fmt.Sprintf("分割案%dの", i+1), plan.ForChild(i+1)); problem != "" {
			return problem
		}
	}
	return ""
}

var executionMultipleRanges = regexp.MustCompile(`:([0-9]+(?:-[0-9]+)?(?:,[0-9]+(?:-[0-9]+)?)+)\s*$`)

type executionLineRange struct{ start, end int }

// executionInputRangesAvailable combines delivered intervals only within one project and path.
func executionInputRangesAvailable(input, repo string, docs []Document, delivered map[string]bool) bool {
	match := executionMultipleRanges.FindStringSubmatchIndex(input)
	if match == nil {
		return false
	}
	file := strings.TrimSpace(input[:match[0]])
	var available []executionLineRange
	for _, doc := range docs {
		if doc.Project != repo || (file != doc.Path && file != repo+":"+doc.Path) || !bodyDocument(doc) || !delivered[documentKey(doc)] {
			continue
		}
		switch doc.Request.Mode {
		case "full":
			available = append(available, executionLineRange{1, int(^uint(0) >> 1)})
		case "lines", "keyword":
			for _, line := range strings.Split(doc.Text, "\n") {
				if prefix, _, ok := strings.Cut(line, ": "); ok {
					if n, err := strconv.Atoi(prefix); err == nil && n > 0 {
						available = append(available, executionLineRange{n, n})
					}
				}
			}
		}
	}
	sort.Slice(available, func(i, j int) bool { return available[i].start < available[j].start })
	for _, part := range strings.Split(input[match[2]:match[3]], ",") {
		a, b, hasEnd := strings.Cut(part, "-")
		start, err := strconv.Atoi(a)
		if err != nil || start < 1 {
			return false
		}
		end := start
		if hasEnd {
			end, err = strconv.Atoi(b)
			if err != nil || end < start {
				return false
			}
		}
		covered := false
		for _, span := range available {
			if span.start > start {
				break
			}
			if span.end >= end {
				covered = true
				break
			}
			if span.end >= start {
				start = span.end + 1
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

// Validation and generated prose read the same assigned methods. External and
// report work must not be reinterpreted as implementation-sandbox requirements.
func executableAcceptanceAnswer(answer Record) Record {
	plan, err := workplan.Parse(answer["executionPlan"])
	if err != nil || plan == nil {
		return answer
	}
	out := copyRecord(answer)
	conditions := func(p *workplan.Plan) []any {
		rows := []any{}
		for _, r := range p.Requirements {
			if r.EvidenceKind == "fixed_test" {
				rows = append(rows, executableCondition(r))
			}
		}
		return rows
	}
	lines := []string{"## 受入条件"}
	for _, v := range conditions(plan) {
		lines = append(lines, "- "+fleet.Text(v))
	}
	out["body"] = strings.Join(lines, "\n")
	for i, child := range fleet.Elements(out["splitIssues"]) {
		row := fleet.Object(child)
		row["acceptance"] = conditions(plan.ForChild(i + 1))
		row["purpose"] = ""
	}
	return out
}

// executableCondition states a fixed-test requirement through its method and
// expected result. When the method only describes how to run the command that
// the criterion already names in the same 確認 → 期待 form, the criterion is the
// concrete statement of the same requirement (Covers accepts both), so the check
// reads it instead of stopping the consultation on the prose method.
func executableCondition(r workplan.Requirement) string {
	condition := r.Condition()
	if acceptanceItemProblem("", condition) != "" && acceptanceItemProblem("", r.Criterion) == "" {
		return r.Criterion
	}
	return condition
}

func executionInputAvailable(input, repo string, doc Document, delivered map[string]bool) bool {
	if !bodyDocument(doc) || !delivered[documentKey(doc)] {
		return false
	}
	match := evidenceRangeSuffix.FindStringSubmatch(input)
	whole := match == nil
	file := input
	start, end := 0, 0
	if match != nil {
		file = strings.TrimSpace(input[:len(input)-len(match[0])])
		a, b := match[1], match[2]
		if a == "" {
			a, b = match[3], match[4]
		}
		start, _ = strconv.Atoi(a)
		end = start
		if b != "" {
			end, _ = strconv.Atoi(b)
		}
	}
	if file != doc.Path && file != repo+":"+doc.Path {
		return false
	}
	if whole && doc.Next > 0 {
		return false
	}
	return evidenceOrdinaryCovers(doc, start, end, whole)
}

// Explicit capability requirements are checked before filing. A declaration or
// a combination of different hosts never proves one compatible execution host.
func (s *Service) executionPlanCapabilityProblem(ctx context.Context, draft, answer Record) string {
	p, err := workplan.Parse(answer["executionPlan"])
	if answer["ready"] != true || err != nil || p == nil {
		return ""
	}
	root := fleet.Text(answer["project"])
	if root == "" {
		root = fleet.Text(draft["project"])
	}
	hosts := map[string][]EnvironmentHost{}
	loaded := map[string]bool{}
	check := func(repo string, plan *workplan.Plan) string {
		capabilities := []string{}
		ids := []string{}
		for _, r := range plan.Requirements {
			if r.EvidenceKind == "fixed_test" && len(r.Capabilities) > 0 {
				capabilities = append(capabilities, r.Capabilities...)
				ids = append(ids, r.ID)
			}
		}
		if len(capabilities) == 0 {
			return ""
		}
		if !loaded[repo] {
			loaded[repo] = true
			if s.options.Environment != nil {
				values, e := s.options.Environment(ctx, repo)
				if e == nil {
					hosts[repo] = values
				}
			}
		}
		for _, host := range hosts[repo] {
			version := host.Values["executionPlanVersion"]
			available := version == 1 || version == float64(1)
			for _, capability := range capabilities {
				available = available && host.Values[capability] == true
			}
			if available {
				return ""
			}
		}
		return fmt.Sprintf("条件%sの必要能力（%s）と対応版を同じ担当PCで確認できません: %s。実行できる確認方法へ直すか、コントローラーの外部確認へ分けてください", strings.Join(ids, "、"), strings.Join(capabilities, "、"), repo)
	}
	children := fleet.Elements(answer["splitIssues"])
	if len(children) == 0 {
		return check(root, p)
	}
	for i, raw := range children {
		repo := fleet.Text(fleet.Object(raw)["project"])
		if repo == "" {
			repo = root
		}
		if problem := check(repo, p.ForChild(i+1)); problem != "" {
			return problem
		}
	}
	return ""
}
