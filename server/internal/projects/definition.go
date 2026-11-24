package projects

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
)

// Fingerprint preserves the ordered JSON contract used by stored approvals.
func Fingerprint(d Object) string {
	type scope struct {
		Known  bool   `json:"known"`
		Paths  any    `json:"paths"`
		Reason string `json:"reason"`
	}
	type proposal struct {
		Title       any `json:"title"`
		Purpose     any `json:"purpose"`
		Acceptance  any `json:"acceptance"`
		DependsOn   any `json:"dependsOn"`
		ChangeScope any `json:"changeScope"`
	}
	type definition struct {
		Name         any        `json:"name"`
		Description  any        `json:"description"`
		Technology   any        `json:"technology"`
		Placement    any        `json:"placement"`
		TestCommand  any        `json:"testCommand"`
		SetupCommand any        `json:"setupCommand"`
		AutoMerge    any        `json:"autoMerge"`
		AutoDeploy   any        `json:"autoDeploy"`
		SplitIssues  []proposal `json:"splitIssues"`
	}
	plans := []proposal{}
	for _, v := range fleet.List(d["splitIssues"]) {
		p := fleet.ObjectOrEmpty(v)
		var cs any
		if p["changeScope"] != nil {
			m := fleet.ObjectOrEmpty(p["changeScope"])
			cs = scope{m["known"] == true, m["paths"], fleet.Text(m["reason"])}
		}
		plans = append(plans, proposal{p["title"], p["purpose"], p["acceptance"], p["dependsOn"], cs})
	}
	return hash(jsonBytes(definition{d["name"], d["description"], d["technology"], d["placement"], d["testCommand"], d["setupCommand"], d["autoMerge"], d["autoDeploy"], plans}))
}
func command(v any) bool {
	a := fleet.List(v)
	if len(a) == 0 || len(a) > 20 {
		return false
	}
	for _, x := range a {
		t, ok := x.(string)
		if !ok || strings.TrimSpace(t) == "" || size(t) > 500 {
			return false
		}
	}
	return true
}
func validate(d Object, owner string, repositories []string) (Object, error) {
	d = clone(d)
	name := strings.TrimSpace(fleet.Text(d["name"]))
	desc := strings.TrimSpace(fleet.Text(d["description"]))
	if !repoName.MatchString(name) || size(name) > 100 || name == "." || name == ".." || name == "-" || regexp.MustCompile(`(?i)\.(git|wiki|rss|atom)$`).MatchString(name) {
		return nil, fmt.Errorf("リポジトリ名が不正です。")
	}
	for _, repo := range repositories {
		if strings.EqualFold(repo, owner+"/"+name) {
			return nil, fmt.Errorf("同じ名前のリポジトリは登録済みです。")
		}
	}
	if desc == "" || size(desc) > 4000 {
		return nil, fmt.Errorf("説明は4000文字以内で入力してください。")
	}
	tech, placement := fleet.Text(d["technology"]), fleet.Text(d["placement"])
	if tech != "node" && tech != "python" || placement != "a9max" && placement != "nas" {
		return nil, fmt.Errorf("技術と配置先が不正です。")
	}
	if !command(d["testCommand"]) {
		return nil, fmt.Errorf("固定テストのコマンドが不正です。")
	}
	if _, ok := d["autoMerge"].(bool); !ok {
		return nil, fmt.Errorf("autoMergeを指定してください。")
	}
	if _, ok := d["autoDeploy"].(bool); !ok {
		return nil, fmt.Errorf("autoDeployを指定してください。")
	}
	if tech == "node" {
		if fleet.List(d["testCommand"])[0] != "{node}" || len(fleet.List(d["setupCommand"])) > 0 {
			return nil, fmt.Errorf("Node.jsの固定テストは{node}を使い、準備コマンドは指定しません。")
		}
		d["setupCommand"] = nil
	} else if !command(d["setupCommand"]) {
		return nil, fmt.Errorf("Pythonの準備コマンドが必要です。")
	}
	if placement == "nas" && (tech != "node" || d["autoDeploy"] != false) {
		return nil, fmt.Errorf("NASはNode.js・自動デプロイ無効で指定してください。")
	}
	plans := fleet.List(d["splitIssues"])
	if len(plans) < 1 || len(plans) > 25 {
		return nil, fmt.Errorf("最初の分割案は1〜25件必要です。")
	}
	clean := []any{}
	for i, v := range plans {
		p := fleet.ObjectOrEmpty(v)
		title, purpose := strings.TrimSpace(fleet.Text(p["title"])), strings.TrimSpace(fleet.Text(p["purpose"]))
		a := fleet.List(p["acceptance"])
		deps := fleet.List(p["dependsOn"])
		if title == "" || size(title) > 200 || purpose == "" || size(purpose) > 8000 || len(a) == 0 || len(a) > 30 {
			return nil, fmt.Errorf("分割案のタイトル・目的・受入条件が不正です。")
		}
		accept := []any{}
		for _, v := range a {
			t, ok := v.(string)
			if !ok || strings.TrimSpace(t) == "" || size(t) > 2000 {
				return nil, fmt.Errorf("分割案の受入条件が不正です。")
			}
			accept = append(accept, strings.TrimSpace(t))
		}
		if _, ok := p["dependsOn"].([]any); !ok {
			return nil, fmt.Errorf("分割案の依存先が不正です。")
		}
		seen := map[int64]bool{}
		for _, v := range deps {
			n, ok := v.(float64)
			if !ok || float64(int64(n)) != n || n < 1 || n > float64(i) || seen[int64(n)] {
				return nil, fmt.Errorf("分割案の依存先は前の番号だけ指定してください。")
			}
			seen[int64(n)] = true
		}
		target := fleet.Text(p["project"])
		if target != "" && !strings.EqualFold(target, owner+"/"+name) {
			return nil, fmt.Errorf("最初の分割案は新しいリポジトリだけを対象にしてください。")
		}
		if p["kind"] != nil && p["kind"] != "implementation" {
			return nil, fmt.Errorf("分割案はimplementationだけ指定できます。")
		}
		q := Object{"title": title, "purpose": purpose, "acceptance": accept, "dependsOn": deps}
		if p["changeScope"] != nil {
			q["changeScope"] = normalizeScope(fleet.ObjectOrEmpty(p["changeScope"]))
		}
		clean = append(clean, q)
	}
	d["name"] = name
	d["description"] = desc
	d["splitIssues"] = clean
	return d, nil
}

var invalidScope = regexp.MustCompile("[\\x00-\\x1f\\x7f*?\\[\\]{}<>|\"`:／＼]")
var reservedScope = regexp.MustCompile(`^(CON|PRN|AUX|NUL|COM[0-9¹²³]|LPT[0-9¹²³]|CONIN\$|CONOUT\$)$`)

func normalizeScope(v Object) Object {
	unknown := func(reason string) Object { return Object{"known": false, "paths": []any{}, "reason": reason} }
	reason := strings.TrimSpace(regexp.MustCompile(`[\s<>]+`).ReplaceAllString(fleet.Text(v["reason"]), " "))
	rr := []rune(reason)
	if len(rr) > 200 {
		reason = string(rr[:200])
	}
	if v["known"] != true {
		if reason == "" {
			reason = "受付AIが範囲を特定できないと判断しました"
		}
		return unknown(reason)
	}
	a := fleet.List(v["paths"])
	if len(a) == 0 || len(a) > 40 {
		return unknown("宣言のパスが空か40件を超えています")
	}
	paths := []any{}
	seen := map[string]bool{}
	directory := false
	for _, raw := range a {
		p := strings.ReplaceAll(strings.TrimSpace(fleet.Text(raw)), "\\", "/")
		if p == "" || size(p) > 200 || strings.HasPrefix(p, "/") || invalidScope.MatchString(p) {
			return unknown("読めないパスを含みます")
		}
		parts := []string{}
		for _, part := range strings.Split(p, "/") {
			if part == "" || part == "." {
				continue
			}
			if part == ".." || strings.EqualFold(part, ".git") || strings.HasSuffix(part, " ") || strings.HasSuffix(part, ".") || reservedScope.MatchString(strings.ToUpper(strings.TrimRight(strings.Split(part, ".")[0], " "))) {
				return unknown("読めないパスを含みます")
			}
			parts = append(parts, part)
		}
		if len(parts) == 0 {
			return unknown("読めないパスを含みます")
		}
		q := strings.Join(parts, "/")
		if strings.HasSuffix(p, "/") {
			q += "/"
			directory = true
		}
		key := strings.ToUpper(q)
		if !seen[key] {
			seen[key] = true
			paths = append(paths, q)
		}
	}
	if !directory {
		reason = ""
	}
	return Object{"known": true, "paths": paths, "reason": reason}
}
