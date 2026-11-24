package filing

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/completionplan"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/workplan"
	"regexp"
	"strings"
	"unicode"
)

var whitespace = regexp.MustCompile(`\s+`)
var titleNumbers = regexp.MustCompile(`#\d+`)
var closeDetails = regexp.MustCompile(`(?i)</details`)
var heading = regexp.MustCompile(`^#{1,2}\s`)

func inert(v string) string {
	v = strings.ReplaceAll(v, "<!--", "&lt;!--")
	return closeDetails.ReplaceAllString(v, "&lt;/details")
}
func referencePart(source string) (string, string) {
	body, reference := []string{}, []string{}
	inside := false
	found := false
	for _, row := range markdownLines(source) {
		line, fenced := row.text, row.fenced
		t := strings.TrimSpace(line)
		if !fenced && t == "## 参照（経緯・根拠）" {
			inside = true
			found = true
			continue
		}
		if inside && !fenced && heading.MatchString(t) {
			inside = false
		}
		if inside {
			reference = append(reference, line)
		} else {
			body = append(body, line)
		}
	}
	if !found {
		return source, ""
	}
	return strings.TrimRight(strings.Join(body, "\n"), " \t\r\n"), strings.TrimSpace(strings.Join(reference, "\n"))
}
func title(d Record) string {
	value := strings.Join(strings.Fields(titleNumbers.ReplaceAllString(fleet.Text(d["title"]), "")), " ")
	for _, pair := range [][2]string{{"(", ")"}, {"（", "）"}, {"「", "」"}, {"[", "]"}} {
		value = strings.ReplaceAll(value, pair[0]+" ", pair[0])
		value = strings.ReplaceAll(value, " "+pair[1], pair[1])
		value = strings.ReplaceAll(value, pair[0]+pair[1], "")
	}
	value = strings.TrimSpace(contract.ClipUTF16(value, 200))
	meaningful := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			meaningful = true
			break
		}
	}
	if !meaningful {
		if d["parentSplit"] != nil {
			return "依頼の続き"
		}
		return "依頼"
	}
	return value
}
func scopeDeclaration(v any) Record {
	if v == nil {
		return nil
	}
	r := row(v)
	unknown := func(reason string) Record { return Record{"known": false, "paths": []any{}, "reason": reason} }
	reason := strings.TrimSpace(whitespace.ReplaceAllString(strings.NewReplacer("<", " ", ">", " ").Replace(fleet.Text(r["reason"])), " "))
	reason = contract.ClipUTF16(reason, 200)
	known, ok := r["known"].(bool)
	if !ok {
		return unknown("宣言の形式を読めません")
	}
	if !known {
		if reason == "" {
			reason = "受付AIが範囲を特定できないと判断しました"
		}
		return unknown(reason)
	}
	a := fleet.Elements(r["paths"])
	if len(a) == 0 || len(a) > 40 {
		return unknown("宣言のパスが空か40件を超えています")
	}
	paths := []any{}
	seen := map[string]bool{}
	directory := false
	for _, raw := range a {
		p, ok := raw.(string)
		if !ok || p == "*" {
			return unknown("読めないパスを含みます")
		}
		normalized, e := jobs.NormalizeScope([]string{p})
		if e != nil || len(normalized) != 1 {
			return unknown("読めないパスを含みます")
		}
		p = normalized[0]
		key := strings.ToUpper(p)
		if !seen[key] {
			paths = append(paths, p)
			seen[key] = true
		}
		directory = directory || strings.HasSuffix(p, "/")
	}
	if !directory {
		reason = ""
	}
	return Record{"known": true, "paths": paths, "reason": reason}
}
func scopeSection(scope Record) string {
	if scope == nil {
		return ""
	}
	lines := []string{}
	var markerValue Record
	if scope["known"] == true {
		for _, v := range fleet.Elements(scope["paths"]) {
			p := fleet.Text(v)
			suffix := ""
			if strings.HasSuffix(p, "/") {
				suffix = "（ディレクトリ）"
			}
			lines = append(lines, "- `"+p+"`"+suffix)
		}
		reason := fleet.Text(scope["reason"])
		for _, v := range fleet.Elements(scope["paths"]) {
			if strings.HasSuffix(fleet.Text(v), "/") {
				if reason == "" {
					reason = "理由の記載なし"
				}
				lines = append(lines, "- ディレクトリで宣言した理由: "+reason)
				break
			}
		}
		markerValue = Record{"v": 1, "paths": scope["paths"]}
	} else {
		reason := fleet.Text(scope["reason"])
		if reason == "" {
			reason = "理由の記載なし"
		}
		lines = append(lines, "- 範囲不明: "+reason)
		markerValue = Record{"v": 1, "unknown": true}
	}
	return "\n\n## 変更範囲（受付の宣言）\n" + strings.Join(lines, "\n") + "\n<!-- issue-desk-scope:" + encoded(markerValue) + " -->"
}
func dependencySection(d Record) (string, error) {
	repo := fleet.Text(d["project"])
	entries, labels, shown := []any{}, []string{}, []string{}
	seen := map[string]bool{}
	add := func(target string, n int64) error {
		if n < 1 || n > 9007199254740991 || !repositoryPattern.MatchString(target) {
			return fail(409, "先行Issueの指定を確認できません。")
		}
		key := fmt.Sprintf("%s#%d", target, n)
		if seen[key] {
			return nil
		}
		seen[key] = true
		if target == repo {
			entries = append(entries, n)
			labels = append(labels, fmt.Sprintf("#%d", n))
		} else {
			entries = append(entries, key)
			labels = append(labels, key)
		}
		return nil
	}
	for _, raw := range fleet.Elements(d["dependencies"]) {
		r := row(raw)
		n, ok := integer(raw)
		target := repo
		if !ok {
			n, _ = integer(r["number"])
			target = fleet.Text(r["repo"])
		}
		if e := add(target, n); e != nil {
			return "", e
		}
	}
	for _, raw := range fleet.Elements(d["precedingIssues"]) {
		r := row(raw)
		n, _ := integer(r["gitea"])
		target := fleet.Text(r["repo"])
		key := fmt.Sprintf("%s#%d", target, n)
		was := seen[key]
		if e := add(target, n); e != nil {
			return "", e
		}
		if !was {
			desk, _ := integer(r["desk"])
			if desk > 0 {
				shown = append(shown, fmt.Sprintf("共通番号%d（%s）", desk, key))
			} else {
				shown = append(shown, key)
			}
		}
	}
	out := ""
	if len(shown) > 0 {
		out = "\n\n先行Issue（受付の指定）: " + strings.Join(shown, ", ")
	}
	if len(entries) > 0 {
		out += "\n\n先行IssueのPRマージ待ち: " + strings.Join(labels, ", ") + "。すべての先行PRのマージを確認してから開始します。\n<!-- issue-desk-dependencies:" + encoded(entries) + " -->"
	}
	return out, nil
}
func Prepare(ctx context.Context, c BuildConfig, d Record) (Prepared, error) {
	var out Prepared
	completion, repo, execution, e := checkFilingDraft(ctx, c, d)
	if e != nil {
		return out, e
	}
	fields := clone(d)
	if c.ResolvePreceding != nil {
		resolved, e := c.ResolvePreceding(ctx, d)
		if e != nil {
			return out, e
		}
		fields["precedingIssues"] = resolved
	} else if len(fleet.Elements(d["dependsOnIssues"])) > 0 {
		return out, fail(503, "先行Issueの確認を利用できません。")
	}
	tier := decideTier(c, fields)
	if tier != nil {
		fields["implementationTier"] = tier
	}
	provider := fleet.Text(d["provider"])
	if provider == "" {
		provider = "codex"
	}
	chosen, e := c.provider(ctx, provider, tier)
	if e != nil {
		return out, e
	}
	if chosen["provider"] != provider {
		return out, fail(409, "実行先の自動切替はできません。")
	}
	body, reference := referencePart(fleet.Text(d["body"]))
	body = inert(body)
	if execution != nil {
		body += "\n\n" + inert(execution.Section())
	}
	body, _ = allowBrokenReferenceFixes(body)
	deps, e := dependencySection(fields)
	if e != nil {
		return out, e
	}
	body += deps
	if completion != nil {
		body += "\n\n" + completionSection(completion)
	}
	references := referenceSections(c, d, reference)
	if c.ProxySection != nil {
		proxy, e := c.ProxySection(ctx, d)
		if e != nil {
			return out, e
		}
		if strings.TrimSpace(proxy) != "" {
			body += "\n\n" + inert(strings.TrimSpace(proxy))
		}
	}
	if lines, files, ok := estimate(d["estimate"]); ok {
		body += fmt.Sprintf("\n\n## 見込みの大きさ\n変更行 約%d行・変更ファイル %d本", lines, files)
	}
	if len(references) > 0 {
		body += "\n\n<!-- issue-desk-reference:start -->\n<details>\n<summary>受付の経緯・根拠（参照）</summary>\n\n" + inert(strings.TrimSpace(strings.Join(references, "\n\n"))) + "\n\n</details>\n<!-- issue-desk-reference:end -->"
	}
	scope := scopeDeclaration(d["changeScope"])
	body = withScopeHandling(body, scope)
	body += scopeSection(scope)
	markerValue, priority := settingsMarker(d, provider, tier)
	name := map[string]string{"codex": "Codex", "claude": "Claude", "antigravity": "Antigravity"}[provider]
	tierText := tierDescription(tier)
	body += fmt.Sprintf("\n\n---\n受付: 開発受付 / %s\n実行先: %s / 実装モデル: %s / 推論: %s%s\n\n<!-- issue-desk:%s -->\n<!-- issue-desk:settings %s -->", repo, name, fleet.Text(chosen["model"]), fleet.Text(chosen["effort"]), tierText, fleet.Text(d["id"]), encoded(markerValue))
	if exemption := sizeExemption(d); exemption != "" {
		body += "\n<!-- issue-desk:size-exempt:" + exemption + " -->"
	}
	if len(fleet.Elements(d["imageReviews"])) > 0 {
		body += "\n<!-- issue-desk-images-pending -->"
	}
	if utfSize(body) > bodyLimit(c.Config) {
		return out, fail(400, "起票本文が上限を超えています。依頼を分けてください。")
	}
	labels := []int64{}
	if c.ProviderLabels != nil {
		labels, e = c.ProviderLabels(ctx, repo, provider)
		if e != nil && provider != "codex" {
			return out, e
		}
	} else if provider != "codex" {
		return out, fail(503, "実行先ラベルを設定できません。")
	}
	paths := []string(nil)
	source := "estimated"
	if scope["known"] == true {
		paths = []string{}
		for _, v := range fleet.Elements(scope["paths"]) {
			paths = append(paths, fleet.Text(v))
		}
		source = "declared"
	}
	job := jobs.NewJob{Repo: repo, Title: title(d), Kind: "normal", Priority: priority, Scope: paths, ScopeSource: source, Provider: &provider, View: map[string]any{"workType": normalizedWork(d["workType"]), "draft": d["id"]}}
	dependencies := jobDependencies(repo, fields)
	job.View["dependsOnIssues"] = dependencies
	if tier != nil {
		level := fleet.Text(tier["level"])
		job.Tier = &level
	}
	out = Prepared{Title: job.Title, Body: body, Labels: labels, Fields: Record{"implementationTier": tier, "precedingIssues": fields["precedingIssues"], "changeScope": scope}, Job: job}
	attachCompletionPlan(&out, d, completion)
	if execution != nil {
		out.Job.View["executionPlan"] = execution.Record()
		out.Job.View["executionPlanSHA"] = execution.Fingerprint()
	}
	return out, nil
}

// checkFilingDraft validates the draft before its issue body is built and returns its
// completion plan and target repository.
func checkFilingDraft(ctx context.Context, c BuildConfig, d Record) (*completionplan.Plan, string, *workplan.Plan, error) {
	if !regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`).MatchString(fleet.Text(d["id"])) {
		return nil, "", nil, fail(400, "起票IDが不正です。")
	}
	if e := assertManual(d); e != nil {
		return nil, "", nil, e
	}
	if c.Records != nil {
		if e := CheckPolicy(ctx, c.Records, d); e != nil {
			return nil, "", nil, e
		}
	}
	if e := ValidateSplit(ctx, c, d); e != nil {
		return nil, "", nil, e
	}
	completion, e := prepareCompletionPlan(d)
	if e != nil {
		return nil, "", nil, e
	}
	if e = imageReviews(d); e != nil {
		return nil, "", nil, e
	}
	repo := fleet.Text(d["project"])
	if !repositoryPattern.MatchString(repo) {
		return nil, "", nil, fail(400, "起票先が不正です。")
	}
	if c.Project != nil {
		if e = c.Project(repo); e != nil {
			return nil, "", nil, e
		}
	}
	execution, e := workplan.Parse(d["executionPlan"])
	if e != nil {
		return nil, "", nil, fail(409, e.Error())
	}
	if execution != nil {
		if e = execution.Covers(workplan.Conditions(fleet.Text(d["body"]))); e != nil {
			return nil, "", nil, fail(409, e.Error())
		}
	}
	if utfSize(fleet.Text(d["body"])) > 20000 {
		return nil, "", nil, fail(400, "依頼本文は20000文字以内で指定してください。")
	}
	// Inputs that are already invalid must not consume a current-state AI check.
	// The current policy/comparison remains mandatory for inputs that can be filed.
	if c.CheckCurrent != nil {
		if e = c.CheckCurrent(ctx, d); e != nil {
			return nil, "", nil, e
		}
	}
	return completion, repo, execution, nil
}

// referenceSections collects the background and evidence shown in the folded reference part.
func referenceSections(c BuildConfig, d Record, reference string) []string {
	references := []string{}
	if reference != "" {
		references = append(references, "## 参照（経緯・根拠）\n"+reference)
	}
	// 分割案の子は PlanSplit が本文に発言を収めている。
	if d["parentSplit"] == nil {
		if userReferences := splitUserReferences(d["messages"]); userReferences != "" {
			references = append(references, "## 参照（経緯・根拠）\n"+userReferences)
		}
	}
	if background := fleet.Text(d["splitBackground"]); strings.TrimSpace(background) != "" {
		references = append(references, splitBackgroundSection(background))
	}
	if context := row(d["context"]); context != nil {
		evidence := issueContext(context)
		if c.ContextEvidence != nil {
			evidence = c.ContextEvidence(context)
		}
		references = append(references, evidence)
	}
	return references
}

// settingsMarker returns the settings recorded in the issue marker and the job priority.
func settingsMarker(d Record, provider string, tier Record) (Record, int) {
	markerValue := Record{"provider": provider}
	if d["consultationProvider"] == "claude" {
		markerValue["consultationProvider"] = "claude"
	}
	priority := 0
	if d["priority"] == "high" || d["priority"] == "normal" {
		markerValue["priority"] = d["priority"]
		if d["priority"] == "high" {
			priority = 1
		}
	}
	if tier != nil {
		markerValue["tier"] = tier["level"]
		markerValue["tierSource"] = tier["source"]
	}
	if _, ok := d["workType"]; ok {
		markerValue["workType"] = normalizedWork(d["workType"])
	}
	return markerValue, priority
}

// jobDependencies lists the issues the job waits for, from the split dependencies and
// the resolved preceding issues, without duplicates.
func jobDependencies(repo string, fields Record) []any {
	dependencies := []any{}
	seenDependencies := map[string]bool{}
	addDependency := func(repo string, n int64) {
		key := fmt.Sprintf("%s#%d", repo, n)
		if !seenDependencies[key] {
			seenDependencies[key] = true
			dependencies = append(dependencies, Record{"repo": repo, "issue": n})
		}
	}
	for _, raw := range fleet.Elements(fields["dependencies"]) {
		r := row(raw)
		n, ok := integer(raw)
		target := repo
		if !ok {
			n, _ = integer(r["number"])
			target = fleet.Text(r["repo"])
		}
		if n > 0 {
			addDependency(target, n)
		}
	}
	for _, raw := range fleet.Elements(fields["precedingIssues"]) {
		r := row(raw)
		n, _ := integer(r["gitea"])
		if n > 0 {
			addDependency(fleet.Text(r["repo"]), n)
		}
	}
	return dependencies
}

// SplitIssueSections returns the validated sections appended to a split child's issue.
func SplitIssueSections(d Record) (string, string, error) {
	p, err := prepareCompletionPlan(d)
	if err != nil {
		return "", "", err
	}
	completion := ""
	if p != nil {
		completion = completionSection(p)
	}
	return completion, splitBackgroundSection(fleet.Text(d["splitBackground"])), nil
}

func completionSection(p *completionplan.Plan) string { return inert(p.Section()) }
func splitBackgroundSection(background string) string { return inert(background) }
