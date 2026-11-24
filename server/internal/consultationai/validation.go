package consultationai

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"sort"
	"strings"
	"time"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/usage"
)

func validateScope(scope map[string]any) error {
	if scope == nil {
		return errors.New("変更範囲の宣言がありません")
	}
	known, valid := scope["known"].(bool)
	if !valid {
		return errors.New("変更範囲のknownは真偽値です")
	}
	if !known {
		scope["paths"] = []any{}
		if strings.TrimSpace(fleet.Text(scope["reason"])) == "" {
			scope["reason"] = "固定版資料から範囲を特定できません"
		}
		return nil
	}
	paths := fleet.Elements(scope["paths"])
	if len(paths) < 1 || len(paths) > 40 {
		return errors.New("変更範囲のパスは1〜40件です")
	}
	input := []string{}
	for _, value := range paths {
		if fleet.Text(value) == "*" {
			return errors.New("全体を特定済みの変更範囲にできません")
		}
		input = append(input, fleet.Text(value))
	}
	normalized, err := jobs.NormalizeScope(input)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	out := []any{}
	directory := false
	for _, p := range normalized {
		key := strings.ToUpper(p)
		if !seen[key] {
			out = append(out, p)
			seen[key] = true
		}
		directory = directory || strings.HasSuffix(p, "/")
	}
	if directory && strings.TrimSpace(fleet.Text(scope["reason"])) == "" {
		return errors.New("ディレクトリの変更範囲には具体的な理由が必要です")
	}
	if !directory {
		scope["reason"] = ""
	}
	scope["paths"] = out
	return nil
}
func controllerQuestion(answer Record, question, reason string) {
	questions := append([]any{}, fleet.Elements(answer["questions"])...)
	for _, item := range questions {
		if fleet.Text(item) == question {
			return
		}
	}
	answer["questions"] = append(questions, question)
	answer["ready"] = false
	reasons := append([]any{}, fleet.Elements(answer["questionReasons"])...)
	answer["questionReasons"] = append(reasons, Record{"question": question, "reason": reason, "controller": true})
}
func (s *Service) crossApproval(ctx context.Context, d, answer Record, m *materials) error {
	parent := fleet.Text(d["project"])
	targets := map[string]bool{}
	for _, item := range fleet.Elements(answer["splitIssues"]) {
		project := fleet.Text(fleet.Object(item)["project"])
		if project == "" {
			project = parent
		}
		targets[project] = true
	}
	if len(m.group) > 0 && len(fleet.Elements(answer["splitIssues"])) == 0 {
		targets[parent] = true
	}
	cross := fleet.Object(answer["crossProject"])
	scopes, seen, exception, err := crossScopes(cross, m)
	if err != nil {
		return err
	}
	for repo := range seen {
		targets[repo] = true
	}
	if err := m.checkRepositoryGroup(answer, cross, scopes); err != nil {
		return err
	}
	outside := false
	for target := range targets {
		if !m.allowed[target] {
			return errors.New("許可対象外のプロジェクトを提案しています")
		}
		outside = outside || target != parent
		if target != parent && !seen[target] {
			return errors.New("分割先が明示した変更対象の集合にありません")
		}
	}
	if !outside {
		return nil
	}
	if len(m.group) > 0 {
		for target := range targets {
			if !seen[target] {
				return errors.New("組の変更対象すべてに対象範囲が必要です")
			}
		}
	}
	if strings.TrimSpace(fleet.Text(cross["reason"])) == "" || len(scopes) == 0 || len(scopes) > 25 {
		return errors.New("複数プロジェクトの理由と対象範囲が必要です")
	}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i]["project"] < scopes[j]["project"] })
	canonical, err := usage.Compact(marshal(scopes))
	if err != nil {
		return err
	}
	fingerprint := digest(canonical)
	scopeRecords := []any{}
	for _, scope := range scopes {
		scopeRecords = append(scopeRecords, Record{"project": scope["project"], "scope": scope["scope"]})
	}
	proposal := Record{"fingerprint": fingerprint, "reason": cross["reason"], "scopes": scopeRecords, "approvalMessageId": cross["approvalMessageId"]}
	if len(m.group) > 0 {
		proposal["repositoryGroup"] = true
	}
	answer["crossProposal"] = proposal
	approvals := append([]any{}, fleet.Elements(d["crossApprovals"])...)
	if len(m.group) > 0 && fleet.Text(fleet.Object(d["crossProposal"])["fingerprint"]) != "" && fleet.Text(fleet.Object(d["crossProposal"])["fingerprint"]) != fingerprint {
		approvals = []any{}
	}
	approved := savedCrossApproval(d, approvals, m, fingerprint, exception)
	if !approved {
		if approval := s.crossApprovalFromReply(d, cross, m, fingerprint, exception, scopeRecords); approval != nil {
			approved = true
			approvals = append(approvals, approval)
		}
	}
	proposal["approved"] = approved
	fields := Record{"crossProposal": proposal, "crossApprovals": approvals}
	if err := s.save(ctx, d, fields); err != nil {
		return err
	}
	answer["crossApprovals"] = approvals
	if !approved {
		askCrossApproval(answer, cross, scopes, len(m.group) > 0, exception)
	}
	return nil
}

// savedCrossApproval reports whether a saved approval covers this proposal fingerprint.
func savedCrossApproval(d Record, approvals []any, m *materials, fingerprint string, exception bool) bool {
	approved := false
	for _, item := range approvals {
		if fleet.Text(fleet.Object(item)["fingerprint"]) == fingerprint && (len(m.group) == 0 || fleet.Text(fleet.Object(d["crossProposal"])["fingerprint"]) == fingerprint) && (!exception || fleet.Object(item)["exceptionExplicit"] == true) && (len(m.group) == 0 && !exception || validGroupApprovalReceipt(fleet.Object(item)["exceptionApproval"], fingerprint)) {
			approved = true
		}
	}
	return approved
}

// askCrossApproval shows the reason and scopes and asks the user to approve them.
func askCrossApproval(answer, cross Record, scopes []map[string]string, grouped, exception bool) {
	description := []string{fleet.Text(cross["reason"])}
	for _, scope := range scopes {
		description = append(description, scope["project"]+": "+scope["scope"])
	}
	question := "上の理由・対象リポジトリ・変更範囲で複数プロジェクトへの変更を進めてよいか、返信で了承してください。修正は了承として扱いません。"
	if grouped || exception {
		question += "了承する場合は『了承します』または『承認します』と返信してください。除外対象がある場合はその理由と条件も了承の対象です。"
		if grouped || exception {
			question += "運用代行PCからは提示の指紋をapprovalFingerprint、利用者の承認発言をstatement、その日時をinstructionAtとして添えてください。"
		}
	}
	answer["reply"] = fleet.Text(answer["reply"]) + "\n\n" + strings.Join(description, "\n")
	controllerQuestion(answer, question, "複数プロジェクトの対象集合に対する利用者の了承が必要です")
}

// crossScopes validates the declared change scopes of a cross-project answer.
func crossScopes(cross Record, m *materials) ([]map[string]string, map[string]bool, bool, error) {
	scopes := []map[string]string{}
	seen := map[string]bool{}
	exception := false
	for _, item := range fleet.Elements(cross["scopes"]) {
		entry := fleet.Object(item)
		repo, scope := fleet.Text(entry["project"]), strings.Join(strings.Fields(fleet.Text(entry["scope"])), " ")
		if !m.allowed[repo] || scope == "" || contract.UTF16Length(scope) > 2000 || seen[repo] {
			return nil, nil, false, errors.New("複数プロジェクトの対象範囲が不正です")
		}
		seen[repo] = true
		if strings.HasPrefix(scope, "除外理由: ") || strings.HasPrefix(scope, "除外:") || strings.HasPrefix(scope, "除外：") {
			exception = true
		}
		scopes = append(scopes, map[string]string{"project": repo, "scope": scope})
	}
	return scopes, seen, exception, nil
}

// crossApprovalFromReply returns the approval given by the user's latest reply to the
// proposal with this fingerprint, or nil when that reply does not approve it.
func (s *Service) crossApprovalFromReply(d, cross Record, m *materials, fingerprint string, exception bool, scopeRecords []any) Record {
	messages := fleet.Elements(d["messages"])
	lastUser := -1
	for index, item := range messages {
		if fleet.Object(item)["role"] == "user" {
			lastUser = index
		}
	}
	if lastUser >= 0 && fleet.Text(fleet.Object(messages[lastUser])["id"]) != "" && fleet.Text(fleet.Object(messages[lastUser])["id"]) == fleet.Text(cross["approvalMessageId"]) && (len(m.group) == 0 || explicitGroupConsent(fleet.Text(fleet.Object(messages[lastUser])["text"]))) && (len(m.group) == 0 && !exception || explicitGroupExceptionReply(fleet.Text(fleet.Object(messages[lastUser])["text"])) && validGroupApprovalReceipt(fleet.Object(messages[lastUser])["groupExceptionApproval"], fingerprint)) {
		approved := false
		for index := lastUser - 1; index >= 0; index-- {
			shown := fleet.Object(messages[index])
			if shown["role"] == "assistant" {
				approved = fleet.Text(shown["crossProposal"]) == fingerprint
				break
			}
		}
		if approved {
			approval := Record{"fingerprint": fingerprint, "reason": cross["reason"], "scopes": scopeRecords, "messageId": cross["approvalMessageId"], "approvedAt": s.options.Now().UTC().Format(time.RFC3339Nano), "exceptionExplicit": exception}
			if exception || len(m.group) > 0 {
				approval["exceptionApproval"] = fleet.Object(messages[lastUser])["groupExceptionApproval"]
			}
			return approval
		}
	}
	return nil
}

func explicitGroupExceptionReply(reply string) bool {
	switch strings.TrimSpace(reply) {
	case "了承します", "承認します":
		return true
	}
	return false
}

func validGroupApprovalReceipt(raw any, fingerprint string) bool {
	receipt := fleet.Object(raw)
	if fleet.Text(receipt["fingerprint"]) != fingerprint {
		return false
	}
	return receipt["via"] == "screen" || receipt["via"] == "chat" && fleet.Text(receipt["pc"]) != "" && fleet.Text(receipt["statement"]) != "" && fleet.Text(receipt["instructionAt"]) != ""
}
