package filing

import (
	"crypto/sha256"
	"encoding/hex"
	"issue-desk-intake/server/internal/fleet"
	"sort"
	"strings"
)

// validateRepositoryGroups checks the current shared configuration, including on
// saved split children. A child is authorized only by its current parent plan.
func validateRepositoryGroups(c BuildConfig, d Record) error {
	groups := repositoryGroups(row(c.Config["assignments"])["repositoryGroups"])
	if len(groups) == 0 {
		return nil
	}
	parent := fleet.Text(d["project"])
	targets := map[string]bool{strings.ToLower(parent): true}
	plans := fleet.Elements(d["splitIssues"])
	if len(plans) > 0 {
		targets = map[string]bool{}
		for _, p := range plans {
			targets[strings.ToLower(target(row(p), parent))] = true
		}
	}
	missing := map[string]bool{}
	touchedGroup := false
	for _, group := range groups {
		// The selected parent matters even when all plans target another repository.
		touched := false
		for _, name := range group {
			if strings.EqualFold(name, parent) || targets[strings.ToLower(name)] {
				touched = true
			}
		}
		if !touched {
			continue
		}
		touchedGroup = true
		for _, name := range group {
			if c.Project == nil {
				return fail(503, "起票先の許可設定を確認できません。")
			}
			if err := c.Project(name); err != nil {
				return err
			}
			if !targets[strings.ToLower(name)] {
				missing[name] = true
			}
			ev := evidence(row(d["context"]), name, parent)
			if !groupEvidence(ev) {
				return fail(409, "組の全対象の資料本文を確認してください。")
			}
		}
	}
	if touchedGroup {
		for _, raw := range plans {
			name := target(row(raw), parent)
			if !groupEvidence(evidence(row(d["context"]), name, parent)) {
				return fail(409, "全対象の資料本文を確認してください。")
			}
		}
	}
	if len(missing) > 0 {
		return validateGroupException(d, targets, missing)
	}
	if touchedGroup {
		proposal := row(d["crossProposal"])
		for _, raw := range fleet.Elements(d["crossApprovals"]) {
			approval := row(raw)
			if fleet.Text(approval["fingerprint"]) != "" && approval["fingerprint"] == proposal["fingerprint"] && validGroupReceipt(approval) && validGroupReason(approval, proposal) {
				return nil
			}
		}
		return fail(409, "組の変更への了承出典を確認できません。")
	}
	return nil
}

func repositoryGroups(v any) [][]string {
	var groups [][]string
	switch x := v.(type) {
	case [][]string:
		return x
	case []any:
		for _, raw := range x {
			var group []string
			for _, name := range fleet.Elements(raw) {
				group = append(group, fleet.Text(name))
			}
			groups = append(groups, group)
		}
	}
	return groups
}

// groupEvidence accepts only repository file contents or a concrete line/keyword
// excerpt, not a tree outline, refused read, or a revision by itself.
func groupEvidence(ev Record) bool {
	if ev == nil || len(fleet.Text(ev["revision"])) != 40 {
		return false
	}
	for _, key := range []string{"files", "parts"} {
		for _, raw := range fleet.Elements(ev[key]) {
			item := row(raw)
			if item == nil || strings.TrimSpace(fleet.Text(item["path"])) == "" || len(fleet.Text(item["sha256"])) != 64 {
				continue
			}
			size, ok := integer(item["bytes"])
			if !ok || size <= 0 {
				continue
			}
			if key == "files" || fleet.Text(item["kind"]) == "lines" || fleet.Text(item["kind"]) == "keyword" {
				return true
			}
		}
	}
	return false
}

// The exclusion is part of the already displayed, fingerprinted scope. This
// makes a change of target, reason or condition require a new approval.
func groupExclusion(scope string) bool {
	for _, prefix := range []string{"除外:", "除外："} {
		if strings.HasPrefix(scope, prefix) {
			condition := strings.TrimSpace(strings.TrimPrefix(scope, prefix))
			if i := strings.LastIndex(condition, "（理由: "); i >= 0 && strings.HasSuffix(condition, "）") {
				condition = strings.TrimSpace(condition[:i])
			}
			return condition != ""
		}
	}
	const prefix = "除外理由: "
	const separator = " / 条件: "
	if !strings.HasPrefix(scope, prefix) {
		return false
	}
	reason, condition, ok := strings.Cut(strings.TrimPrefix(scope, prefix), separator)
	return ok && strings.TrimSpace(reason) != "" && strings.TrimSpace(condition) != ""
}

func validateGroupException(d Record, targets, missing map[string]bool) error {
	proposal := row(d["crossProposal"])
	if strings.TrimSpace(fleet.Text(proposal["reason"])) == "" {
		return fail(409, "除外の理由と了承が必要です。")
	}
	var approval Record
	for _, raw := range fleet.Elements(d["crossApprovals"]) {
		a := row(raw)
		if fleet.Text(a["fingerprint"]) != "" && a["fingerprint"] == proposal["fingerprint"] {
			approval = a
			break
		}
	}
	if approval == nil || approval["exceptionExplicit"] != true {
		return fail(409, "除外例外の了承が必要です。")
	}
	if !validGroupReceipt(approval) {
		return fail(409, "除外例外の了承出典を確認できません。")
	}
	type scope struct {
		Project string `json:"project"`
		Scope   string `json:"scope"`
	}
	scopes := []scope{}
	seen := map[string]bool{}
	for _, raw := range fleet.Elements(approval["scopes"]) {
		item := row(raw)
		project := fleet.Text(item["project"])
		value := strings.Join(strings.Fields(fleet.Text(item["scope"])), " ")
		key := strings.ToLower(project)
		if seen[key] || value == "" {
			return fail(409, "了承した対象範囲が不正です。")
		}
		seen[key] = true
		if missing[project] {
			if !groupExclusion(value) {
				return fail(409, "除外理由と条件が必要です。")
			}
		} else if !targets[key] || groupExclusion(value) {
			return fail(409, "了承した対象と起票案が一致しません。")
		}
		scopes = append(scopes, scope{project, value})
	}
	for target := range targets {
		if !seen[target] {
			return fail(409, "起票対象の了承がありません。")
		}
	}
	for target := range missing {
		if !seen[strings.ToLower(target)] {
			return fail(409, "除外対象の了承がありません。")
		}
	}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].Project < scopes[j].Project })
	// Group consultations bind the reason into the first sorted scope.
	if len(scopes) > 0 {
		legacy := false
		for _, item := range scopes {
			if missing[item.Project] && strings.HasPrefix(item.Scope, "除外理由: ") {
				legacy = true
			}
		}
		suffix := "（理由: " + strings.Join(strings.Fields(fleet.Text(proposal["reason"])), " ") + "）"
		if !legacy && !strings.HasSuffix(scopes[0].Scope, suffix) {
			return fail(409, "除外理由の了承が一致しません。")
		}
	}
	sum := sha256.Sum256([]byte(encoded(scopes)))
	if hex.EncodeToString(sum[:]) != fleet.Text(approval["fingerprint"]) {
		return fail(409, "除外例外の了承が一致しません。")
	}
	return nil
}

func isGroupExclusion(c BuildConfig, d Record, project, scope string) bool {
	if !groupExclusion(scope) {
		return false
	}
	parent := fleet.Text(d["project"])
	for _, group := range repositoryGroups(row(c.Config["assignments"])["repositoryGroups"]) {
		present, member := false, false
		for _, name := range group {
			if strings.EqualFold(name, project) {
				member = true
			}
			if strings.EqualFold(name, parent) {
				present = true
			}
			for _, plan := range fleet.Elements(d["splitIssues"]) {
				if strings.EqualFold(name, target(row(plan), parent)) {
					present = true
				}
			}
		}
		if present && member {
			return true
		}
	}
	return false
}

func validGroupReceipt(approval Record) bool {
	receipt := row(approval["exceptionApproval"])
	if receipt == nil || receipt["fingerprint"] != approval["fingerprint"] {
		return false
	}
	return receipt["via"] == "screen" || receipt["via"] == "chat" && fleet.Text(receipt["pc"]) != "" && fleet.Text(receipt["statement"]) != "" && fleet.Text(receipt["instructionAt"]) != ""
}

// The consultation binds the reason to the first canonical scope.
func validGroupReason(approval, proposal Record) bool {
	reason := strings.Join(strings.Fields(fleet.Text(proposal["reason"])), " ")
	if reason == "" {
		return false
	}
	firstProject, firstScope := "", ""
	for _, raw := range fleet.Elements(approval["scopes"]) {
		scope := row(raw)
		project := fleet.Text(scope["project"])
		if project == "" {
			return false
		}
		if firstProject == "" || project < firstProject {
			firstProject, firstScope = project, strings.Join(strings.Fields(fleet.Text(scope["scope"])), " ")
		}
	}
	return firstProject != "" && strings.HasSuffix(firstScope, "（理由: "+reason+"）")
}
