package filingcheck

import (
	"context"
	"database/sql"
	"fmt"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
)

var repairCauses = []string{"ai_failed", "invalid_answer", "fetch_failed", "refused", "evidence_requested", "evidence_missing", "evidence_input_size"}

func repairBaseline(calls Record) bool {
	recovery := obj(calls["recovery"])
	if recovery == nil {
		recovery = Record{}
		calls["recovery"] = recovery
	}
	baseline := obj(recovery["baseline"])
	if baseline == nil {
		baseline = Record{}
		recovery["baseline"] = baseline
	}
	used := obj(recovery["used"])
	if used == nil {
		used = Record{}
		recovery["used"] = used
	}
	cause := fleet.Text(calls["outcome"])
	if calls["stopKind"] == "evidence_missing" {
		cause = "evidence_missing"
		if obj(calls["evidenceHold"])["cause"] == "input_size" {
			cause = "evidence_input_size"
		}
	}
	available := false
	for _, reason := range repairCauses {
		list := fleet.Elements(used[reason])
		has0, has1 := false, false
		for _, g := range list {
			has0 = has0 || g == "0"
			has1 = has1 || g == "1"
		}
		if baseline[reason] == "0" && has0 && !has1 && reason == cause {
			available = true
		}
		if baseline[reason] == nil || len(list) == 0 {
			baseline[reason] = "1"
			used[reason] = []any{"1"}
		}
	}
	return available
}
func consumeRepair(calls Record) {
	recovery := obj(calls["recovery"])
	for _, cause := range repairCauses {
		used := obj(recovery["used"])
		has := false
		for _, v := range fleet.Elements(used[cause]) {
			has = has || v == "1"
		}
		if !has {
			used[cause] = append(fleet.Elements(used[cause]), "1")
		}
	}
}

var commonNumber = regexp.MustCompile(`共通番号\s*([1-9][0-9]*)`)

func (s *Service) numberSystem(ctx context.Context, d Record, dependencies []any, material Record) error {
	db := s.options.AppDB
	if db == nil {
		return nil
	}
	explicit := map[int64]bool{}
	for _, entry := range fleet.Elements(obj(material["target"])["dependsOnIssues"]) {
		explicit[fleet.WholeNumber(obj(entry)["number"])] = true
	}
	referenced := map[int64]bool{}
	for _, match := range commonNumber.FindAllStringSubmatch(fleet.Text(d["body"]), -1) {
		referenced[numString(match[1])] = true
	}
	for _, dep := range dependencies {
		n := fleet.WholeNumber(obj(dep)["number"])
		if explicit[n] || !referenced[n] {
			continue
		}
		var repo string
		var issue int64
		err := db.QueryRowContext(ctx, bindSQL("SELECT project,gitea FROM desk_numbers WHERE number=?"), n).Scan(&repo, &issue)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return err
		}
		if !strings.EqualFold(repo, fleet.Text(d["project"])) || issue != n {
			return fail(fmt.Sprintf("依存のGitea番号 %d は本文の共通番号との取り違えの可能性があります。共通番号%dの対応を確認して再送してください。", n, n))
		}
	}
	return nil
}
func numString(s string) int64 { var n int64; _, _ = fmt.Sscan(s, &n); return n }
func (s *Service) FilingCheckLimitAlerts(ctx context.Context) ([]any, error) {
	drafts, err := s.options.Records.ListDraftFields(ctx, consultations.DraftReadFilingAlerts)
	if err != nil {
		return nil, err
	}
	out := []any{}
	missing := map[string]int{}
	for _, d := range drafts {
		calls := obj(d["filingCheckCalls"])
		if d["issue"] != nil || d["status"] == "submitting" || d["status"] == "submitted" || d["status"] == "discarded" || fleet.WholeNumber(calls["revision"]) != fleet.WholeNumber(d["revision"]) {
			continue
		}
		if calls["stopKind"] == "evidence_missing" {
			missing[strings.ToLower(fleet.Text(d["project"]))]++
		}
		budget, stopped := filingBudgetStopped(calls, d["revision"])
		if !stopped {
			continue
		}
		last := Record{}
		if checks := fleet.Elements(calls["checks"]); len(checks) > 0 {
			last = obj(checks[len(checks)-1])
		}
		detail := fmt.Sprintf("判定が上限%d回に達しました（読み足し%d回/上限%d回・判定%d回/上限%d回）。相談を更新して再送してください。", filingDecisionLimit, budget.Evidence, filingEvidenceLimit, budget.Decisions, filingDecisionLimit)
		out = append(out, Record{"id": "filing-check-limit-" + fleet.Text(d["id"]), "kind": "intake_error", "status": "filing_check_stopped", "project": d["project"], "draftId": d["id"], "gist": d["title"], "outcome": calls["outcome"], "reason": detail, "requiresUser": true, "nextAction": "相談を更新して再送する", "failures": budget.Decisions, "evidenceCalls": budget.Evidence, "decisionCalls": budget.Decisions, "title": fmt.Sprintf("起票前の確認を止めました: %v（読み足し%d回・判定%d回）", d["title"], budget.Evidence, budget.Decisions), "detectedAt": last["at"], "lastCheckAt": last["at"], "codeRevision": last["codeRevision"], "codeRevisionChanged": fleet.Text(last["codeRevision"]) != "" && fleet.Text(last["codeRevision"]) != s.options.Revision, "detail": detail})
	}
	for repo, count := range missing {
		if count >= 3 {
			out = append(out, Record{"id": "filing-evidence-missing-" + repo, "kind": "intake_error", "status": "filing_check_stopped", "project": repo, "outcome": "evidence_missing", "reason": "起票前の確認が資料不足で止まった相談が同じプロジェクトに3件以上あります。", "requiresUser": true, "nextAction": "各相談へ必要な資料を追加して改訂するか、明示的に再試行する", "failures": count, "title": fmt.Sprintf("起票前の資料不足で停止中: %s（%d件）", repo, count), "detail": "各相談へ必要な資料を追加して改訂するか明示的に再試行してください。"})
		}
	}
	return out, nil
}
