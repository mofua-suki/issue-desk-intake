package consultationai

import (
	"fmt"
	"regexp"
	"strings"

	"issue-desk-intake/server/internal/workplan"
)

var rootTestCommand = regexp.MustCompile(`(?:node|\$NODE)\s+--test\s+(test/[A-Za-z0-9_.-]+\.test\.ts)\b`)

// Only flag an explicitly local synthetic test when the delivered files establish
// both the test and the fixed-suite entry point. Never rewrite the saved plan.
func fixedSuiteClassificationProblem(repo, label string, plan *workplan.Plan, m *materials) string {
	project := m.state.Projects[repo]
	if project == nil {
		return ""
	}
	docs := append([]Document{}, project.Reads...)
	for _, doc := range m.evidence.documents {
		if doc.Project == repo {
			docs = append(docs, doc)
		}
	}
	available := func(path string) bool {
		for _, doc := range docs {
			if doc.Project == repo && doc.Path == path && executionInputAvailable(path, repo, doc, m.state.Delivered) {
				return true
			}
		}
		return false
	}
	suite := false
	for _, doc := range docs {
		if doc.Project == repo && doc.Path == "scripts/verify.sh" && bodyDocument(doc) && m.state.Delivered[documentKey(doc)] && strings.Contains(doc.Text, `root_test() { "$NODE" --test --test-concurrency=4 test/*.test.ts; }`) && strings.Contains(doc.Text, "stage root-test root_test") {
			suite = true
		}
	}
	if !suite {
		return ""
	}
	for _, r := range plan.Requirements {
		if r.EvidenceKind != "external_check" || r.Phase != "verification" || len(r.Capabilities) != 0 {
			continue
		}
		text := r.Criterion + " " + r.Method + " " + r.Expected
		match := rootTestCommand.FindStringSubmatch(text)
		if match == nil || !available(match[1]) || !(strings.Contains(text, "合成") || strings.Contains(text, "フィクスチャ")) {
			continue
		}
		external := false
		for _, word := range []string{"本番", "実機", "配置", "別環境", "外部", "未取得", "独立レビュー", "承認", "production", "staging", "deploy", "review", "approval"} {
			external = external || strings.Contains(strings.ToLower(text), word)
		}
		for _, input := range r.Inputs {
			external = external || !available(input)
		}
		if !external {
			return fmt.Sprintf("%s条件%sは配信済みのscripts/verify.shで固定検証に含まれる%sの合成テストです。外部確認の根拠を確認し、作業ツリー内だけで完結するなら実装担当のfixed_testへ修正してください。実機・外部資料・独立レビュー・個別承認が必要なら条件を分けて維持してください", label, r.ID, match[1])
		}
	}
	return ""
}
