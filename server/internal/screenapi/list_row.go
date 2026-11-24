// 一覧の1行の扱い（要約、行の鍵、数値の検査、プロジェクト・種類ごとの閲覧可否）。

package screenapi

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strconv"
	"strings"

	"issue-desk-intake/server/internal/contract"
)

func projectAllowed(c Object, name string) bool {
	for _, v := range fleet.Items(c["repositories"]) {
		if fleet.ObjectOrEmpty(v)["name"] == name {
			return true
		}
	}
	return false
}

func permitted(c Object, kind string, v Object) bool {
	return kind == "draft" && v["unassigned"] == true || projectAllowed(c, fleet.Text(v["project"]))
}

// issueRowOmitted は、Issue の行のうち画面の一覧が読まない項目（配布の記録・実行設定・公開待ちの印など）。
// 一覧の行（GET /api/list・GET /api/list/entry）からは外し、差分の比較にも使わない。
// Issue の一覧・詳細・検索（issueapi）の応答は別の経路で、変えない。
var issueRowOmitted = map[string]bool{
	"dispatchRecord":    true,
	"executionSettings": true,
	"dispatchSkip":      true,
	"workType":          true,
	"publishPending":    true,
	"releasePending":    true,
	"executionWait":     true,
}

func Summarize(kind string, v Object) Object {
	out := Object{}
	switch kind {
	case "issue":
		// 外す項目は複製の前に除き、大きな配布の記録を複製しない。
		kept := make(Object, len(v))
		for key, value := range v {
			if !issueRowOmitted[key] {
				kept[key] = value
			}
		}
		return clone(kept)
	case "draft":
		for _, key := range strings.Fields("executionActive executionScheduled executionUntil familyProgress id status project unassigned title shownTitle createdAt updatedAt priority retryAt waitStartedAt turnStopped queued continuationLimited investigationNotice investigations priorityReviewRound usageReviewRound inventoryRound parentSplit parentConsultation questions issue issues projectHandoff handoffRow origin followup actions priorityChangeable finishingTurn") {
			if value, has := v[key]; has {
				out[key] = value
			}
		}
		if v["hasChildren"] == true {
			out["hasChildren"] = true
		}
		for _, field := range []string{"splitChildren", "consultationChildren"} {
			for _, child := range fleet.Items(v[field]) {
				if fleet.Text(child) != "" {
					out["hasChildren"] = true
					break
				}
			}
		}
		messages := []any{}
		first := false
		images := false
		for _, raw := range fleet.Items(v["messages"]) {
			m := fleet.ObjectOrEmpty(raw)
			if !first && m["role"] == "user" {
				messages = append(messages, Object{"role": "user", "text": contract.ClipUTF16(fleet.Text(m["text"]), 60)})
				first = true
			}
			images = images || len(fleet.Items(m["images"])) > 0
		}
		if images {
			messages = append(messages, Object{"role": "image-marker", "images": []any{Object{}}})
		}
		if len(messages) > 0 {
			out["messages"] = messages
		}
		if v["splitIssues"] != nil {
			plans := []any{}
			for _, raw := range fleet.Items(v["splitIssues"]) {
				p := Object{}
				if target, ok := fleet.ObjectOrEmpty(raw)["project"].(string); ok {
					p["project"] = target
				}
				plans = append(plans, p)
			}
			out["splitIssues"] = plans
		}
		if v["proposal"] != nil {
			out["proposal"] = Object{"kind": fleet.ObjectOrEmpty(v["proposal"])["kind"]}
		} else if strings.Contains(fleet.Text(v["body"]), "<!-- issue-desk:followup -->") {
			out["body"] = "<!-- issue-desk:followup -->"
		}
	case "todo":
		for k, value := range v {
			switch value.(type) {
			case nil, string, bool, float64, int, int64:
				out[k] = value
			}
		}
		if v["actions"] != nil {
			out["actions"] = v["actions"]
		}
		out["body"] = ""
		for _, line := range strings.Split(fleet.Text(v["body"]), "\n") {
			if text := strings.TrimSpace(line); text != "" {
				out["body"] = contract.ClipUTF16(text, 61)
				break
			}
		}
	case "closeRequest":
		for _, key := range strings.Fields("executionActive id project number desk title url status pc createdAt updatedAt actions") {
			if value, has := v[key]; has {
				out[key] = value
			}
		}
		if target := fleet.ObjectOrEmpty(v["target"]); len(target) > 0 {
			out["target"] = Object{"title": target["title"], "followup": target["followup"] == true, "desk": target["desk"]}
		}
	}
	return out
}

func rowKey(kind string, v Object) (string, error) {
	if kind == "issue" {
		n, ok := safeNumber(v["number"])
		if !ok || fleet.Text(v["project"]) == "" {
			return "", fail(503, "Issueの識別子を確認できません。")
		}
		return fmt.Sprintf("issue:%s#%d", fleet.Text(v["project"]), n), nil
	}
	if !uuid.MatchString(fleet.Text(v["id"])) {
		return "", fail(503, "一覧項目のIDを確認できません。")
	}
	return kind + ":" + fleet.Text(v["id"]), nil
}

func safeNumber(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), n > 0 && n <= 9007199254740991 && n == float64(int64(n))
	case int:
		return int64(n), n > 0
	case int64:
		return n, n > 0 && n <= 9007199254740991
	case string:
		parsed, e := strconv.ParseInt(n, 10, 64)
		return parsed, e == nil && parsed > 0 && parsed <= 9007199254740991
	}
	return 0, false
}
