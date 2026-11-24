package filing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"sort"
	"strings"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/workplan"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func target(p Record, parent string) string {
	if t := strings.TrimSpace(fleet.Text(p["project"])); t != "" {
		return t
	}
	return parent
}
func evidence(context Record, project, parent string) Record {
	if context == nil {
		return nil
	}
	if project != parent {
		return row(row(context["projects"])[project])
	}
	if fleet.Text(context["revision"]) == "" {
		return nil
	}
	out := clone(context)
	delete(out, "projects")
	return out
}
func validateProposals(d Record) error {
	plans := fleet.Elements(d["splitIssues"])
	if len(plans) > 25 {
		return fail(400, "分割案は25件以内で指定してください。")
	}
	for index, raw := range plans {
		p := row(raw)
		if e := assertManual(p); e != nil {
			return e
		}
		if strings.TrimSpace(fleet.Text(p["title"])) == "" || utfSize(fleet.Text(p["title"])) > 200 || strings.TrimSpace(fleet.Text(p["purpose"])) == "" || utfSize(fleet.Text(p["purpose"])) > 8000 {
			return fail(400, "分割案のタイトルと目的が不正です。")
		}
		accept := fleet.Elements(p["acceptance"])
		if len(accept) == 0 || len(accept) > 30 {
			return fail(400, "分割案の受入条件は1〜30項目で指定してください。")
		}
		for _, v := range accept {
			s, ok := v.(string)
			if !ok || strings.TrimSpace(s) == "" || utfSize(s) > 2000 {
				return fail(400, "分割案の受入条件が不正です。")
			}
		}
		deps := fleet.Elements(p["dependsOn"])
		if deps == nil {
			return fail(400, "分割案の依存先の配列が必要です。")
		}
		seen := map[int64]bool{}
		for _, v := range deps {
			n, ok := integer(v)
			if !ok || n < 1 || n > int64(index) || seen[n] {
				return fail(400, "分割案の依存先は前の番号だけ指定してください。")
			}
			seen[n] = true
		}
		if p["project"] != nil && !repositoryPattern.MatchString(target(p, fleet.Text(d["project"]))) {
			return fail(400, "分割案の対象リポジトリが不正です。")
		}
		if e := validatePreceding(p["dependsOnIssues"]); e != nil {
			return e
		}
	}
	return nil
}
func validatePreceding(v any) error {
	if v == nil {
		return nil
	}
	items := fleet.Elements(v)
	if items == nil || len(items) > 30 {
		return fail(400, "先行Issueは30件以内の配列で指定してください。")
	}
	seen := map[string]bool{}
	for _, raw := range items {
		r := row(raw)
		n, ok := integer(r["number"])
		if !ok || n < 1 || n > 9007199254740991 || r["source"] != "desk" && r["source"] != "gitea" {
			return fail(400, "先行Issueの種類と番号が不正です。")
		}
		key := fmt.Sprintf("%s:%d", fleet.Text(r["source"]), n)
		if seen[key] {
			return fail(400, "先行Issueが重複しています。")
		}
		seen[key] = true
	}
	return nil
}

// ValidateSplit validates every target before a child can be posted.
func ValidateSplit(ctx context.Context, c BuildConfig, d Record) error {
	if c.Records == nil {
		return validateSplitWithRecords(ctx, c, d, nil)
	}
	return validateSplitWithRecords(ctx, c, d, c.Records)
}

func validateSplitWithRecords(ctx context.Context, c BuildConfig, d Record, records interface {
	Get(context.Context, string, string) (consultations.Record, bool, error)
}) error {
	if len(repositoryGroups(row(c.Config["assignments"])["repositoryGroups"])) > 0 {
		seen := map[string]bool{}
		current := d
		var parents []Record
		for {
			id := fleet.Text(current["id"])
			if id != "" && seen[id] {
				return fail(409, "分割元の相談が循環しています。")
			}
			seen[id] = true
			parentID := fleet.Text(current["parentSplit"])
			if parentID == "" {
				break
			}
			if seen[parentID] {
				return fail(409, "分割元の相談が循環しています。")
			}
			if records == nil {
				return fail(503, "分割元の相談を確認できません。")
			}
			parent, found, err := records.Get(ctx, "drafts", parentID)
			if err != nil {
				return err
			}
			if !found || fleet.Text(parent["id"]) != parentID {
				return fail(409, "分割元の相談がありません。")
			}
			index, ok := integer(current["splitIndex"])
			children, plans := fleet.Elements(parent["splitChildren"]), fleet.Elements(parent["splitIssues"])
			if !ok || index < 1 || index > int64(len(children)) || index > int64(len(plans)) || id == "" || fleet.Text(children[index-1]) != id || !strings.EqualFold(target(row(plans[index-1]), fleet.Text(parent["project"])), fleet.Text(current["project"])) {
				return fail(409, "分割元と起票先が一致しません。")
			}
			parents = append(parents, parent)
			current = parent
		}
		for _, parent := range parents {
			if err := validateSplitPlan(ctx, c, parent, fleet.Text(parent["parentSplit"]) == ""); err != nil {
				return err
			}
		}
	}
	return validateSplitPlan(ctx, c, d, fleet.Text(d["parentSplit"]) == "")
}

func validateSplitPlan(ctx context.Context, c BuildConfig, d Record, checkGroups bool) error {
	if checkGroups {
		if err := validateRepositoryGroups(c, d); err != nil {
			return err
		}
	}
	plans := fleet.Elements(d["splitIssues"])
	if len(plans) == 0 {
		if !truth(d["attempted"]) && d["parentSplit"] == nil {
			if e := interruptScope(d); e != nil {
				return e
			}
		}
		return nil
	}
	if e := validateProposals(d); e != nil {
		return e
	}
	parent := fleet.Text(d["project"])
	targets := map[string]bool{}
	cross := false
	for _, raw := range plans {
		p := row(raw)
		t := target(p, parent)
		if !repositoryPattern.MatchString(t) {
			return fail(409, "分割案ごとの対象プロジェクトを指定してください。")
		}
		if c.Project == nil {
			return fail(503, "起票先の許可設定を確認できません。")
		}
		if e := c.Project(t); e != nil {
			return e
		}
		targets[t] = true
		cross = cross || t != parent
		if !truth(d["attempted"]) && len(fleet.Elements(d["splitChildren"])) == 0 {
			if e := interruptScope(p); e != nil {
				return e
			}
		}
		for _, v := range fleet.Elements(p["dependsOn"]) {
			n, _ := integer(v)
			source := target(row(plans[n-1]), parent)
			if source != t && (c.DependencyCheckable == nil || !c.DependencyCheckable(t, source)) {
				return fail(409, "案どうしの依存を担当PCが確認できません。")
			}
		}
		if c.ResolvePreceding != nil && len(fleet.Elements(p["dependsOnIssues"])) > 0 {
			check := clone(d)
			check["project"] = t
			check["dependsOnIssues"] = p["dependsOnIssues"]
			if _, e := c.ResolvePreceding(ctx, check); e != nil {
				return e
			}
		} else if len(fleet.Elements(p["dependsOnIssues"])) > 0 {
			return fail(503, "先行Issueの確認を利用できません。")
		}
	}
	if !cross {
		return nil
	}
	return validateCrossApproval(c, d, targets, parent)
}

// validateCrossApproval checks that the saved approval covers exactly the targets of the
// split, matches its fingerprint and that every target's materials were read.
func validateCrossApproval(c BuildConfig, d Record, targets map[string]bool, parent string) error {
	proposal := row(d["crossProposal"])
	approval := Record(nil)
	for _, raw := range fleet.Elements(d["crossApprovals"]) {
		a := row(raw)
		if a["fingerprint"] == proposal["fingerprint"] && fleet.Text(a["fingerprint"]) != "" {
			approval = a
			break
		}
	}
	if approval == nil {
		return fail(409, "複数プロジェクトへの変更の了承が確認できません。")
	}
	type declared struct {
		Project string `json:"project"`
		Scope   string `json:"scope"`
	}
	scopes := []declared{}
	approvedTargets := map[string]bool{}
	for _, raw := range fleet.Elements(approval["scopes"]) {
		p := row(raw)
		t := fleet.Text(p["project"])
		if approvedTargets[t] || (!targets[t] && !isGroupExclusion(c, d, t, fleet.Text(p["scope"]))) || strings.TrimSpace(fleet.Text(p["scope"])) == "" {
			return fail(409, "分割案が了承した対象範囲と一致しません。")
		}
		approvedTargets[t] = true
		scopes = append(scopes, declared{t, strings.Join(strings.Fields(fleet.Text(p["scope"])), " ")})
	}
	for target := range targets {
		if !approvedTargets[target] {
			return fail(409, "了承した対象に分割案がありません。")
		}
	}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].Project < scopes[j].Project })
	sum := sha256.Sum256([]byte(encoded(scopes)))
	if hex.EncodeToString(sum[:]) != fleet.Text(approval["fingerprint"]) {
		return fail(409, "保存済みの了承と対象範囲の指紋が一致しません。")
	}
	for t := range targets {
		if evidence(row(d["context"]), t, parent) == nil {
			return fail(409, "資料を確認していない対象を含むため起票しません。")
		}
	}
	return nil
}

// splitPlanOverview lists every planned issue with its target and dependencies.
func splitPlanOverview(plans []any, parent string, cross bool) []string {
	planLines := []string{}
	for i, raw := range plans {
		p := row(raw)
		deps := []string{}
		for _, v := range fleet.Elements(p["dependsOn"]) {
			n, _ := integer(v)
			deps = append(deps, fmt.Sprint(n))
		}
		depText := strings.Join(deps, ", ")
		if depText == "" {
			depText = "なし"
		}
		label := "依存: " + depText
		if cross {
			label = "対象: " + target(p, parent) + " / " + label
		}
		planLines = append(planLines, fmt.Sprintf("%d. %s（%s）\n%s", i+1, fleet.Text(p["title"]), label, proposalBody(p)))
	}
	return planLines
}

// splitChildDependencies names the planned issues the child depends on.
func splitChildDependencies(p Record, plans []any, parent string, cross bool) string {
	own := []string{}
	for _, v := range fleet.Elements(p["dependsOn"]) {
		n, _ := integer(v)
		before := row(plans[n-1])
		at := ""
		if cross {
			at = "・対象 " + target(before, parent)
		}
		own = append(own, fmt.Sprintf("分割案%d（%s%s）", n, fleet.Text(before["title"]), at))
	}
	deps := strings.Join(own, "、")
	if deps == "" {
		deps = "なし"
	}
	return deps
}

// crossReasonSection explains why the split spans projects, with the approved scopes.
func crossReasonSection(d Record) string {
	proposal := row(d["crossProposal"])
	reason := "\n\n## 複数プロジェクトにまたがる理由（依頼者が会話で了承）\n" + fleet.Text(proposal["reason"])
	for _, v := range fleet.Elements(proposal["scopes"]) {
		s := row(v)
		reason += "\n- " + fleet.Text(s["project"]) + ": " + fleet.Text(s["scope"])
	}
	return reason
}

// interruptScope は優先度によらず、起票の前に変更範囲の宣言を確かめる（割り込みだけの条件ではない）。
func interruptScope(d Record) error {
	s := scopeDeclaration(d["changeScope"])
	if s["known"] != true {
		return fail(409, "起票には変更範囲の宣言が必要です（優先度によらず）。")
	}
	for _, p := range fleet.Elements(s["paths"]) {
		if strings.HasSuffix(fleet.Text(p), "/") && strings.TrimSpace(fleet.Text(s["reason"])) == "" {
			return fail(409, "ディレクトリを含む宣言には理由が必要です。")
		}
	}
	return nil
}
func PlanSplit(d Record) ([]Record, error) {
	if execution, err := workplan.Parse(d["executionPlan"]); err != nil {
		return nil, fail(409, err.Error())
	} else if execution != nil {
		if err := execution.ValidateChildren(len(fleet.Elements(d["splitIssues"]))); err != nil {
			return nil, fail(409, err.Error())
		}
	}
	if _, err := prepareCompletionPlan(d); err != nil {
		return nil, err
	}
	if e := validateProposals(d); e != nil {
		return nil, e
	}
	plans := fleet.Elements(d["splitIssues"])
	parent := fleet.Text(d["project"])
	cross := false
	for _, raw := range plans {
		cross = cross || target(row(raw), parent) != parent
	}
	planLines := splitPlanOverview(plans, parent, cross)
	out := []Record{}
	for index, raw := range plans {
		p := row(raw)
		t := target(p, parent)
		deps := splitChildDependencies(p, plans, parent, cross)
		reason := ""
		if cross {
			reason = crossReasonSection(d)
		}
		scope := scopeDeclaration(p["changeScope"])
		body := splitChildBody(d, p, scope, index, len(plans), deps, reason)
		child := Record{"_index": index + 1, "_dependsOn": p["dependsOn"], "project": t, "parentSplit": d["id"], "imageSourceId": d["id"], "messages": d["messages"], "imageReviews": d["imageReviews"], "ready": true, "status": "auto_ready", "questions": []any{}, "title": p["title"], "body": body, "splitBackground": "## 分割案の全体（受付AIが決定）\n実施順・依存関係:\n" + strings.Join(planLines, "\n") + "\n\n## 元の相談（背景資料）\n" + fleet.Text(d["body"]), "changeScope": scope, "workType": normalizedWork(p["workType"]), "dependsOnIssues": p["dependsOnIssues"], "dependencies": []any{}, "revision": 0}
		for _, key := range []string{"model", "effort", "provider", "consultationProvider", "priority", "prioritySource", "requestedImplementationTier", "replaceIssues"} {
			if d[key] != nil {
				child[key] = d[key]
			}
		}
		if execution, err := workplan.Parse(d["executionPlan"]); err != nil {
			return nil, fail(409, err.Error())
		} else if execution != nil {
			selected := execution.ForChild(index + 1)
			if len(selected.Requirements) > 0 {
				child["executionPlan"] = selected.Record()
				child["executionPlanParentSHA"] = execution.Fingerprint()
			}
		}
		if d["completionPlan"] != nil {
			child["completionPlan"] = d["completionPlan"]
			child["completionApproval"] = d["completionApproval"]
			child["completionPlanVersion"] = 1
		}
		for _, key := range []string{"completionReconfirmation", "completionReconfirmationAfter"} {
			if d[key] != nil {
				child[key] = d[key]
			}
		}
		if cross {
			child["crossProject"] = true
			child["context"] = evidence(row(d["context"]), t, parent)
		} else {
			child["context"] = d["context"]
		}
		if strings.TrimSpace(fleet.Text(p["heavyReason"])) != "" {
			child["heavyReason"] = strings.TrimSpace(fleet.Text(p["heavyReason"]))
		}
		if _, _, ok := estimate(p["estimate"]); ok {
			child["estimate"] = p["estimate"]
		}
		if p["replaceIssues"] != nil {
			child["replaceIssues"] = p["replaceIssues"]
		}
		out = append(out, child)
	}
	return out, nil
}

// splitChildBody is the issue body of one planned child: its purpose and acceptance, its
// place in the split, the files it owns and the user's original statements.
func splitChildBody(d, p, scope Record, index, total int, deps, reason string) string {
	body := fmt.Sprintf("%s\n\n## 確認済み事項\n受付AIが決定した分割案の %d/%d。今回の実装範囲・受入条件は上記のみです。\nこの案の依存: %s%s%s", proposalBody(p), index+1, total, deps, splitCorrectionNotes(d, index), reason)
	if scope["known"] == true {
		missing := []string{}
		for _, v := range fleet.Elements(scope["paths"]) {
			path := fleet.Text(v)
			if !strings.Contains(body, path) {
				missing = append(missing, "- `"+path+"`")
			}
		}
		if len(missing) > 0 {
			body += "\n\n## 担当ファイル\n" + strings.Join(missing, "\n")
		}
	}
	if reference := splitUserReferences(d["messages"]); reference != "" {
		body += "\n\n## 参照（経緯・根拠）\n" + reference
	}
	return body
}
func proposalBody(p Record) string {
	lines := []string{}
	for _, v := range fleet.Elements(p["acceptance"]) {
		lines = append(lines, "- "+fleet.Text(v))
	}
	return "## 目的\n" + fleet.Text(p["purpose"]) + "\n\n## 受入条件\n" + strings.Join(lines, "\n")
}

// splitUserReferences retains the original user messages as background, not instructions.
func splitUserReferences(messages any) string {
	var quotes []string
	for _, raw := range fleet.Elements(messages) {
		message := row(raw)
		text, ok := message["text"].(string)
		if message["role"] != "user" || !ok || strings.TrimSpace(text) == "" {
			continue
		}
		var source []string
		if id := fleet.Text(message["id"]); strings.TrimSpace(id) != "" {
			source = append(source, "発言ID: "+id)
		}
		if at := fleet.Text(message["at"]); strings.TrimSpace(at) != "" {
			source = append(source, "日時: "+at)
		}
		header := "依頼者の発言"
		if len(source) > 0 {
			header += "（" + strings.Join(source, " / ") + "）"
		}
		lines := strings.Split(header+"\n"+text, "\n")
		for i := range lines {
			lines[i] = "> " + lines[i]
		}
		quotes = append(quotes, strings.Join(lines, "\n"))
	}
	return strings.Join(quotes, "\n\n")
}
