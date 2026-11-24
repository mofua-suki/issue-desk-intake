package screenapi

import (
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"math"
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// This projection follows shared/src/desk-attention.mts. Behavioral parity with
// the browser classifier is checked in list_counts_test.go, including input
// assembly and cause grouping, rather than counting raw issues or status names.
func listWorkGroups(snapshot Snapshot) Object {
	rows := appendAlertListRows(classifiedListRows(snapshot), snapshot)
	parents := listParentConsultationIDs(snapshot)
	groups := Object{"attention": []any{}, "running": []any{}, "waiting": []any{}, "stopped": []any{}}
	core := attachAlertsToWork(rows, parents)
	attention := []Object{}
	for _, row := range core {
		status := fleet.Text(row["status"])
		if oneOf(status, "completed", "closed") && (!listUnfinishedExecution(row) || listAcceptanceOnlyCompleted(row)) {
			continue
		}
		if listFollowupGroup(row) {
			continue
		}
		work := listWorkState(row)
		if override := fleet.ObjectOrEmpty(row["countWork"]); len(override) > 0 {
			work = override
		}
		row["work"] = work
		delete(row, "countWork")
		state := fleet.Text(work["state"])
		if state == "attention" {
			attention = append(attention, row)
		} else {
			groups[state] = append(fleet.Items(groups[state]), row)
		}
	}
	for _, row := range groupListAttention(attention) {
		groups["attention"] = append(fleet.Items(groups["attention"]), row)
	}
	return groups
}

// appendAlertListRows は main の全部のテストの失敗と要対応のお知らせを、一覧の行として足す。
func appendAlertListRows(rows []Object, snapshot Snapshot) []Object {
	for _, raw := range fleet.Items(snapshot.Aux["fullTests"]) {
		a := fleet.ObjectOrEmpty(raw)
		if a["id"] == "full-test" {
			if _, ok := a["title"].(string); ok {
				project := fleet.Text(a["repo"])
				if project == "" {
					project = "mofua-suki/issue-desk-intake"
				}
				rows = append(rows, with(a, Object{"kind": "full_test", "status": "full_test_alert", "followup": false, "project": project}))
			}
		}
	}
	for index, raw := range fleet.Items(snapshot.Aux["attentionAlerts"]) {
		a, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind := fleet.Text(a["kind"])
		status := kind
		if kind == "prerequisite_missing" {
			status = "prerequisite_waiting"
		} else if oneOf(kind, "incident", "job_recovery", "ops_watch", "rc_update_missing", "main_broken", "attention_pending", "usage_limit_wait", "heavy_rework", "unfiled_proposal", "split_scope_held", "consultation_budget", "acceptance_transfer", "consultation_stop") {
			status = "waiting"
		}
		id := a["key"]
		if id == nil {
			id = a["id"]
		}
		if id == nil {
			id = fmt.Sprintf("unkeyed:%d", index)
		}
		rows = append(rows, with(a, Object{"kind": "attention", "caseKind": kind, "followup": false, "status": status, "id": "attention:" + fmt.Sprint(id)}))
	}

	return rows
}

// attachAlertsToWork はお知らせの行を判断が要るものから並べ、同じ依頼・相談の行があればその行に付け、無ければ単独の行として残す。
func attachAlertsToWork(rows []Object, parents map[string]bool) []Object {
	core, aux := []Object{}, []Object{}
	for _, row := range rows {
		if oneOf(fleet.Text(row["kind"]), "attention", "full_test") {
			aux = append(aux, row)
		} else {
			core = append(core, row)
		}
	}
	decisionAux, monitoringAux := []Object{}, []Object{}
	for _, row := range aux {
		if listAttentionClass(row) == "monitoring" {
			monitoringAux = append(monitoringAux, row)
		} else {
			decisionAux = append(decisionAux, row)
		}
	}
	aux = append(groupListAttention(decisionAux), monitoringAux...)
	// Notifications about an already listed task belong to that task, not another count.
	for _, row := range aux {
		if parents[fleet.Text(row["draftId"])] {
			continue
		}
		if oneOf(fleet.Text(row["state"]), "closed", "resolved") {
			continue
		}
		matched := false
		for _, target := range core {
			sameIssue := target["kind"] == "issue" && row["number"] != nil && fleet.Number(row["number"]) == fleet.Number(target["number"])
			sameDraft := target["kind"] == "draft" && fleet.Text(row["draftId"]) != "" && row["draftId"] == target["id"]
			sameProject := row["project"] == target["project"] || sameDraft && target["unassigned"] == true && !truthy(row["project"])
			if (sameIssue || sameDraft) && (!oneOf(fleet.Text(target["status"]), "completed", "closed") || listUnfinishedExecution(target) && !listAcceptanceOnlyCompleted(target)) && !listFollowupGroup(target) && sameProject {
				matched = true
				members := fleet.Items(row["attentionMembers"])
				if len(members) == 0 {
					members = []any{row}
				}
				for _, member := range members {
					alert := fleet.ObjectOrEmpty(member)
					work := fleet.ObjectOrEmpty(alert["work"])
					if len(work) == 0 {
						work = listWorkState(alert)
					}
					target["relatedAlerts"] = append(fleet.Items(target["relatedAlerts"]), with(alert, Object{"work": work}))
				}
				rank := map[string]int{"attention": 0, "stopped": 1, "running": 2, "waiting": 3}
				targetState, noticeState := listWorkState(target), listWorkState(row)
				if prior := fleet.ObjectOrEmpty(target["countWork"]); len(prior) > 0 {
					targetState = prior
				}
				monitoringOnly := listAttentionClass(row) == "monitoring" && listWorkState(with(row, Object{"executionActive": true}))["state"] == "running"
				if !monitoringOnly && rank[fleet.Text(noticeState["state"])] < rank[fleet.Text(targetState["state"])] {
					target["countWork"] = noticeState
				}
				break
			}
		}
		if !matched {
			core = append(core, row)
		}
	}
	return core
}

func with(source, updates Object) Object {
	out := Object{}
	for k, v := range source {
		out[k] = v
	}
	for k, v := range updates {
		out[k] = v
	}
	return out
}
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		n, _ := x.Float64()
		return n != 0
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	}
	return true
}
func listSafeInteger(v any) bool {
	switch v.(type) {
	case json.Number, float64, int, int64:
	default:
		return false
	}
	n := fleet.Number(v)
	return !math.IsNaN(n) && !math.IsInf(n, 0) && math.Trunc(n) == n && math.Abs(n) <= 9007199254740991
}
func draftListRow(d, changes Object) Object {
	row := with(d, changes)
	if d["unassigned"] == true && !truthy(d["project"]) {
		row["project"] = "プロジェクト未定"
	}
	return row
}
func listDraftStatus(d Object) string {
	status := fleet.Text(d["status"])
	if status != "thinking" {
		return status
	}
	if truthy(d["turnStopped"]) {
		return "ai_stopped"
	}
	if truthy(d["retryAt"]) {
		return "ai_recovering"
	}
	if truthy(d["queued"]) {
		return "ai_queued"
	}
	return status
}
func listFollowupDraft(d Object) bool {
	if truthy(d["proposal"]) {
		return fleet.ObjectOrEmpty(d["proposal"])["kind"] != "split"
	}
	return strings.Contains(fleet.Text(d["body"]), "<!-- issue-desk:followup -->")
}
func listIssueProject(d, i Object) string {
	if truthy(i["project"]) {
		return fleet.Text(i["project"])
	}
	return fleet.Text(d["project"])
}
func listDraftIssues(d Object) []any {
	if issues := fleet.Items(d["issues"]); len(issues) > 0 {
		return issues
	}
	if truthy(d["issue"]) {
		return []any{d["issue"]}
	}
	return nil
}
func listSameIssue(i, d Object) bool {
	refs := append([]any{d["issue"]}, fleet.Items(d["issues"])...)
	for _, raw := range refs {
		r := fleet.ObjectOrEmpty(raw)
		if len(r) > 0 && i["project"] == listIssueProject(d, r) && fleet.Number(i["number"]) == fleet.Number(r["number"]) {
			return true
		}
	}
	return false
}
func listFromDraft(i, d Object) bool {
	if i["intakeId"] != d["id"] {
		return false
	}
	for _, raw := range fleet.Items(d["investigations"]) {
		r := fleet.ObjectOrEmpty(raw)
		if i["project"] == r["project"] && fleet.Number(i["number"]) == fleet.Number(r["number"]) {
			return false
		}
	}
	if i["project"] == d["project"] {
		return true
	}
	for _, raw := range fleet.Items(d["splitIssues"]) {
		if i["project"] == listIssueProject(d, fleet.ObjectOrEmpty(raw)) {
			return true
		}
	}
	return false
}
func listHasIssue(rows []Object, project string, number any) bool {
	for _, i := range rows {
		if fleet.Text(i["project"]) == project && fleet.Number(i["number"]) == fleet.Number(number) {
			return true
		}
	}
	return false
}
func listParentConsultationIDs(snapshot Snapshot) map[string]bool {
	parents := map[string]bool{}
	for _, row := range snapshot.Rows {
		if row.Type != "draft" {
			continue
		}
		d := row.Item
		for _, field := range []string{"parentSplit", "parentConsultation"} {
			if id := fleet.Text(d[field]); id != "" {
				parents[id] = true
			}
		}
		if id := fleet.Text(d["id"]); id != "" {
			if d["hasChildren"] == true {
				parents[id] = true
			}
			for _, field := range []string{"splitChildren", "consultationChildren"} {
				for _, child := range fleet.Items(d[field]) {
					if fleet.Text(child) != "" {
						parents[id] = true
					}
				}
			}
		}
	}
	return parents
}

func classifiedListRows(snapshot Snapshot) []Object {
	rows, issues, drafts, closes := []Object{}, []Object{}, []Object{}, []Object{}
	for _, raw := range snapshot.Rows {
		switch raw.Type {
		case "issue":
			i := with(raw.Item, Object{"kind": "issue", "followup": raw.Item["followup"] == true})
			issues = append(issues, i)
			rows = append(rows, i)
		case "draft":
			drafts = append(drafts, raw.Item)
		case "closeRequest":
			closes = append(closes, raw.Item)
		}
	}
	parents := listParentConsultationIDs(snapshot)
	// Issues of unread candidates are unknown, not missing: never fill them in
	// as pending rows (the client's requestRows skips them the same way).
	unread := unreadSet(snapshot)
	for _, d := range drafts {
		if parents[fleet.Text(d["id"])] {
			continue
		}
		rows = appendDraftListRows(rows, d, issues, unread)
	}
	rows = appendCloseListRows(rows, closes)
	rows = appendAuxiliaryListRows(rows, snapshot)
	return visibleListRows(rows, parents)
}

// appendDraftListRows は相談1件を、相談の状態に応じた相談の行・起票待ちの Issue の行として一覧に足す。
func appendDraftListRows(rows []Object, d Object, issues []Object, unread map[string]bool) []Object {
	if truthy(d["parentSplit"]) && d["status"] == "submitted" {
		return rows
	}
	status := fleet.Text(d["status"])
	// A closed record remains terminal even when legacy data has no cleanup marker.
	if status == "closed" {
		rows = append(rows, draftListRow(d, Object{"status": "closed", "followup": false, "kind": "draft"}))
		return rows
	}
	if status == "project_handoff" || d["handoffRow"] == true {
		rows = append(rows, draftListRow(d, Object{"status": "completed", "followup": false, "kind": "draft"}))
		return rows
	}

	if oneOf(status, "investigation_waiting", "investigation_reported", "investigation_ended") {
		for _, raw := range fleet.Items(d["investigations"]) {
			i := fleet.ObjectOrEmpty(raw)
			if !unread[fleet.Text(i["project"])] && !listHasIssue(rows, fleet.Text(i["project"]), i["number"]) {
				rows = append(rows, with(i, Object{"id": d["id"], "intakeId": d["id"], "title": d["title"], "investigation": true, "status": "pending", "executionActive": false, "executionScheduled": false, "followup": false, "kind": "issue"}))
			}
		}
		rows = append(rows, draftListRow(d, Object{"status": listDraftStatus(d), "followup": false, "kind": "draft"}))
		return rows
	}
	if oneOf(status, "draft", "submit_error") && draftFamilyFinished(d) {
		rows = append(rows, draftListRow(d, Object{"status": "completed", "followup": false, "kind": "draft"}))
		return rows
	}
	if status != "submitted" && (len(fleet.Items(d["splitIssues"])) > 0 || draftHasImages(d)) {
		kept := []Object{}
		for _, i := range rows {
			if i["kind"] != "issue" || !listFromDraft(i, d) && !listSameIssue(i, d) {
				kept = append(kept, i)
			}
		}
		rows = kept
		rows = append(rows, draftListRow(d, Object{"status": listDraftStatus(d), "followup": listFollowupDraft(d), "kind": "draft"}))
		return rows
	}
	if truthy(d["issue"]) || status == "submitted" {

		for _, raw := range listDraftIssues(d) {
			i := fleet.ObjectOrEmpty(raw)
			project := listIssueProject(d, i)
			if !unread[project] && !listHasIssue(rows, project, i["number"]) {
				rows = append(rows, with(with(d, i), Object{"project": project, "status": "pending", "executionActive": false, "executionScheduled": false, "followup": listFollowupDraft(d), "kind": "issue"}))
			}
		}
		return rows
	}
	present := false
	for _, i := range issues {
		if listFromDraft(i, d) {
			present = true
			break
		}
	}
	if !present {
		rows = append(rows, draftListRow(d, Object{"status": listDraftStatus(d), "followup": listFollowupDraft(d), "kind": "draft"}))
	}
	return rows
}

// draftFamilyFinished は分割の無い相談で、起票した依頼がすべて完了か閉じた状態になり、相談を終える操作が出ているかを返す。
func draftFamilyFinished(d Object) bool {
	p := fleet.ObjectOrEmpty(d["familyProgress"])
	n := fleet.Number(p["total"])
	_, splitPresent := d["splitIssues"]
	_, splitArray := d["splitIssues"].([]any)
	noSplit := !truthy(d["splitIssues"]) || splitArray && len(fleet.Items(d["splitIssues"])) == 0
	if !splitPresent {
		noSplit = true
	}
	if !noSplit || !(n > 0) || fleet.Number(p["completed"])+fleet.Number(p["closed"]) != n || p["remaining"] == nil || fleet.Number(p["remaining"]) != 0 || p["unknown"] == nil || fleet.Number(p["unknown"]) != 0 {
		return false
	}
	for _, action := range fleet.Items(d["actions"]) {
		if action == "finish" {
			return true
		}
	}
	return false
}

// draftHasImages は相談の発言に画像が付いているかを返す。
func draftHasImages(d Object) bool {
	for _, raw := range fleet.Items(d["messages"]) {
		if len(fleet.Items(fleet.ObjectOrEmpty(raw)["images"])) > 0 {
			return true
		}
	}
	return false
}

// appendCloseListRows はクローズ依頼を一覧の行として足す。対象の Issue がフォローアップならその印を引き継ぐ。
func appendCloseListRows(rows []Object, closes []Object) []Object {
	for _, c := range closes {
		followup := fleet.ObjectOrEmpty(c["target"])["followup"] == true
		for _, r := range rows {
			if r["kind"] == "issue" && truthy(r["followup"]) && r["project"] == c["project"] && fleet.Number(r["number"]) == fleet.Number(c["number"]) {
				followup = true
			}
		}
		status := fleet.Text(c["status"])
		if mapped := map[string]string{"confirm": "close_confirm", "reviewing": "close_reviewing", "closing": "close_closing"}[status]; mapped != "" {
			status = mapped
		}
		rows = append(rows, with(c, Object{"kind": "close", "status": status, "state": nil, "followup": followup}))
	}
	return rows
}

// appendAuxiliaryListRows は利用量の見直しと止まりのお知らせを一覧の行として足す。
func appendAuxiliaryListRows(rows []Object, snapshot Snapshot) []Object {
	for _, raw := range fleet.Items(snapshot.Aux["reviews"]) {
		r := fleet.ObjectOrEmpty(raw)
		if listSafeInteger(r["round"]) {
			rows = append(rows, with(r, Object{"id": fmt.Sprintf("usage-review-%.0f", fleet.Number(r["round"])), "title": fmt.Sprintf("利用量の見直し 第%.0f回: 見直しの費用が減った量を上回っています", fleet.Number(r["round"])), "updatedAt": r["startedAt"], "kind": "usage_review", "status": "usage_review_alert", "followup": false, "project": "mofua-suki/issue-desk-intake"}))
		}
	}
	for _, raw := range fleet.Items(snapshot.Aux["stalls"]) {
		a := fleet.ObjectOrEmpty(raw)
		_, idOK := a["id"].(string)
		_, titleOK := a["title"].(string)
		_, detailOK := a["detail"].(string)
		if !idOK || !titleOK || !detailOK {
			continue
		}
		status := fleet.Text(a["status"])
		if !oneOf(status, "stalled_pipeline", "monitor_stopped", "monitor_restarted", "intake_error_fixed", "intake_error_stopped", "filing_check_stopped") {
			status = "stall_alert"
		}
		rows = append(rows, with(a, Object{"id": "stall-" + fleet.Text(a["id"]), "stallKind": a["kind"], "kind": "stall", "status": status, "followup": false}))
	}
	return rows
}

// visibleListRows は親の相談に含まれる行を外し、閉じた Issue で実行の続きが無い行を閉じた状態にする。
func visibleListRows(rows []Object, parents map[string]bool) []Object {
	visible := []Object{}
	for _, r := range rows {
		if parents[fleet.Text(r["draftId"])] {
			continue
		}
		if r["state"] == "closed" && !oneOf(fleet.Text(r["status"]), "running", "publishing", "scope_waiting", "releasing", "deploying", "completed") && !listUnfinishedExecution(r) {
			r["status"] = "closed"
		}
		visible = append(visible, r)
	}
	return visible
}
func listAttentionClass(row Object) string {
	if !listUnfinishedExecution(row) && (oneOf(fleet.Text(row["status"]), "completed", "closed") || oneOf(fleet.Text(row["state"]), "resolved", "closed")) {
		return "monitoring"
	}
	if listWorkState(row)["state"] == "attention" || fleet.ObjectOrEmpty(row["work"])["state"] == "attention" {
		return "decision"
	}
	if listDraftHandoff(row) {
		return "handoff"
	}
	if listTrim(fleet.Text(row["reason"])) != "" && (row["handoff"] == true || oneOf(fleet.Text(row["recoveryState"]), "abandoned", "send_unknown", "send_rejected", "permission_denied", "action_failed", "limit_reached", "stopped") || oneOf(fleet.Text(row["state"]), "abandoned", "send_unknown", "send_rejected", "permission_denied", "action_failed", "limit_reached", "stopped")) {
		return "handoff"
	}
	return "monitoring"
}

func listWorkState(row Object) Object {
	state := func(state, reason, actor, action string) Object {
		return Object{"state": state, "reason": reason, "nextActor": actor, "nextAction": action}
	}
	first := func(values ...string) string {
		for _, value := range values {
			if value != "" {
				return value
			}
		}
		return ""
	}
	kind, status := fleet.Text(row["kind"]), fleet.Text(row["status"])
	questions := []string{}
	for _, q := range fleet.Items(row["questions"]) {
		if listTrim(fleet.Text(q)) != "" {
			questions = append(questions, fleet.Text(q))
		}
	}
	if len(fleet.Items(row["pendingFinalApprovals"])) > 0 || truthy(row["permissionApproval"]) || truthy(row["deletionApproval"]) {
		actions := []string{}
		for _, a := range fleet.Items(row["pendingFinalApprovals"]) {
			actions = append(actions, fleet.Text(fleet.ObjectOrEmpty(a)["reason"]))
		}
		action := strings.Join(actions, " / ")
		if action == "" {
			if truthy(row["permissionApproval"]) {
				action = "権限保護の変更を承認するか判断する"
			} else {
				action = "削除・移動を承認するか判断する"
			}
		}
		return state("attention", "利用者の個別承認を待っています。", "利用者", action)
	}
	if kind == "draft" && len(questions) > 0 && oneOf(status, "draft", "intake_waiting", "investigation_reported", "investigation_ended") {
		return state("attention", "相談の質問への回答を待っています。", "利用者", strings.Join(questions, " / "))
	}
	if kind == "close" && status == "close_confirm" {
		return state("attention", "クローズの判断を待っています。", "利用者", "この依頼を閉じるか判断する")
	}
	if kind == "attention" && row["requiresUser"] != false {
		action := ""
		if row["caseKind"] == "consultation_budget" {
			action = "相談の呼び出し回数・使用量の上限を足して続行するか判断する"
		}
		if row["caseKind"] == "usage_limit_wait" {
			action = "再開見込みと停止理由を確認し、再試行するか判断する"
		}
		if action != "" {
			return state("attention", first(fleet.Text(row["reason"]), "利用者の上限判断・再開判断を待っています。"), "利用者", action)
		}
	}
	if row["requiresUser"] == true && (listTrim(fleet.Text(row["nextAction"])) != "" || listTrim(fleet.Text(row["gist"])) != "" || kind == "issue") {
		return state("attention", first(fleet.Text(row["reason"]), "利用者の回答・上限判断を待っています。"), "利用者", first(listTrim(fleet.Text(row["nextAction"])), listTrim(fleet.Text(row["gist"])), "依頼の質問・上限判断に回答する"))
	}
	recovery := first(fleet.Text(row["recoveryState"]), fleet.Text(row["state"]))
	if truthy(row["finalApprovalsUnavailable"]) || truthy(row["handoff"]) || oneOf(status, "failed", "stopped", "untracked", "review_held", "error", "stalled", "submit_error", "submit_unknown", "ai_stopped", "monitor_stopped", "intake_error_stopped") || oneOf(recovery, "abandoned", "send_unknown", "send_rejected", "permission_denied", "action_failed", "limit_reached", "stopped") {
		return state("stopped", first(fleet.Text(row["finalApprovalsUnavailable"]), fleet.Text(row["reason"]), fleet.Text(row["error"]), fleet.Text(row["detail"]), "自動処理が停止しています。"), first(fleet.Text(row["nextActor"]), "受付・担当AI"), first(fleet.Text(row["nextAction"]), "原因と再開条件を確認する（自動再開の予定なし）"))
	}
	actor := "受付"
	if kind == "issue" {
		actor = "担当Worker"
	}
	if row["executionActive"] == true {
		return state("running", "実行元が処理の所有を確認しています。", actor, "現在の処理を完了し、結果を反映する")
	}
	if row["executionScheduled"] == true {
		reason, condition := "自動の実行順・条件を待っています。", first(fleet.Text(fleet.ObjectOrEmpty(row["projectWait"])["condition"]), fleet.Text(fleet.ObjectOrEmpty(row["scopeWait"])["note"]), fleet.Text(row["dependencyReason"]), "依存作業・実行順・実行枠の条件が満たされる")
		if truthy(row["retryAt"]) {
			reason, condition = "自動再試行を待っています。", "再試行時刻 "+fleet.Text(row["retryAt"])+" に達する"
		}
		out := state("waiting", reason, actor, "条件が満たされたら自動で処理を開始する")
		out["waitCondition"], out["resumeBy"] = condition, "受付の相談Engine"
		if kind == "issue" {
			out["resumeBy"] = "受付の配布・担当Worker"
		}
		return out
	}
	if kind == "draft" && status == "investigation_waiting" && len(fleet.Items(row["investigations"])) > 0 {
		out := state("waiting", "調査Issueの報告を待っています。", "受付", "調査報告を取り込み相談を再開する")
		out["waitCondition"], out["resumeBy"] = "紐付いた調査Issueの報告が届く", "受付の調査報告収集"
		return out
	}
	return state("stopped", first(fleet.Text(row["reason"]), fleet.Text(row["error"]), fleet.Text(row["detail"]), "実処理・自動再開の根拠を確認できません。"), first(fleet.Text(row["nextActor"]), "受付・担当AI"), first(fleet.Text(row["nextAction"]), "担当と再開条件を確認する（未確認のまま実行中にしない）"))
}

// ECMAScript trim/\s deliberately excludes U+0085 and includes U+FEFF.
const listSpaceChars = "\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"

var listWhitespace = regexp.MustCompile("[" + regexp.QuoteMeta(listSpaceChars) + "]+")

func listTrim(value string) string { return strings.Trim(value, listSpaceChars) }

func groupListAttention(rows []Object) []Object {
	out := []Object{}
	causes := map[string]int{}
	for _, r := range rows {
		class := listAttentionClass(r)
		if class == "monitoring" {
			continue
		}
		approval := len(fleet.Items(r["pendingFinalApprovals"])) > 0 || truthy(r["permissionApproval"]) || truthy(r["deletionApproval"])
		key := attentionCauseKey(r, class)
		if key != "" && !approval {
			if index, exists := causes[key]; exists {
				out[index] = mergeAttention(out[index], r)
				continue
			}
			causes[key] = len(out)
		}
		out = append(out, r)
	}
	return out
}

// attentionCauseKey は同じ原因の要対応をまとめる鍵を作る。依頼・相談・クローズの行や、原因を特定できない行は空を返す。
func attentionCauseKey(r Object, class string) string {
	var parts []any
	project := r["project"]
	if project == nil {
		project = r["repo"]
	}
	if oneOf(fleet.Text(r["kind"]), "issue", "draft", "close") {
		parts = nil
	} else if truthy(r["causeKey"]) {
		target := r["number"]
		if target == nil {
			target = r["draftId"]
		}
		parts = []any{project, r["causeKey"], target}
	} else if class == "handoff" {
		var target any
		for _, k := range []string{"number", "issue", "draftId", "id", "key"} {
			if r[k] != nil {
				target = r[k]
				break
			}
		}
		if target != nil {
			kind := r["caseKind"]
			if kind == nil {
				kind = r["kind"]
			}
			reason := listWhitespace.ReplaceAllString(norm.NFKC.String(fleet.Text(r["reason"])), " ")
			parts = []any{project, target, kind, listTrim(reason), r["generation"]}
		}
	}
	if parts == nil {
		return ""
	}
	encoded, _ := json.Marshal(parts)
	return string(encoded)
}

// mergeAttention は同じ原因の要対応を1行にまとめ、状態の順位が高い方を代表にして、まとめた行を members に残す。
func mergeAttention(prev, r Object) Object {
	merged := prev
	members := fleet.Items(prev["attentionMembers"])
	if len(members) == 0 {
		members = []any{with(prev, Object{"work": listWorkState(prev)})}
	}
	nextMembers := fleet.Items(r["attentionMembers"])
	if len(nextMembers) == 0 {
		nextMembers = []any{with(r, Object{"work": listWorkState(r)})}
	}
	members = append(members, nextMembers...)
	rank := map[string]int{"attention": 0, "stopped": 1, "running": 2, "waiting": 3}
	before, after := listWorkState(prev), listWorkState(r)
	if w := fleet.ObjectOrEmpty(prev["work"]); len(w) > 0 {
		before = w
	}
	if w := fleet.ObjectOrEmpty(r["work"]); len(w) > 0 {
		after = w
	}
	if rank[fleet.Text(after["state"])] < rank[fleet.Text(before["state"])] {
		merged = with(r, Object{"work": after})
	}
	merged["attentionMembers"] = members
	return merged
}

func countWorkGroups(groups Object) Object {
	counts := Object{}
	for _, name := range []string{"attention", "running", "waiting", "stopped"} {
		counts[name] = len(fleet.Items(groups[name]))
	}
	counts["inProgress"] = counts["running"]
	return counts
}

func countSnapshotGroups(groups Object, snapshot Snapshot) Object {
	counts := countWorkGroups(groups)
	followups := 0
	for _, row := range classifiedListRows(snapshot) {
		if oneOf(fleet.Text(row["status"]), "completed", "closed") && (!listUnfinishedExecution(row) || listAcceptanceOnlyCompleted(row)) {
			continue
		}
		if listFollowupGroup(row) {
			followups++
		}
	}
	counts["followups"] = followups
	return counts
}
func listFamilies(snapshot Snapshot) []any {
	out := []any{}
	for _, row := range snapshot.Rows {
		d := row.Item
		progress := fleet.ObjectOrEmpty(d["familyProgress"])
		if row.Type == "draft" && d["status"] == "submitted" && !truthy(d["parentSplit"]) && (fleet.Number(progress["remaining"]) > 0 || fleet.Number(progress["unknown"]) > 0) {
			out = append(out, d)
		}
	}
	return out
}

func listUnfinishedExecution(row Object) bool {
	progress := fleet.ObjectOrEmpty(row["executionProgress"])
	return row["kind"] == "issue" && row["status"] != "closed" && (row["status"] == "completed" || row["state"] == "closed") && (truthy(progress["unavailable"]) || fleet.Number(progress["pending"]) > 0)
}

func listAcceptanceOnlyCompleted(row Object) bool {
	progress := fleet.ObjectOrEmpty(row["executionProgress"])
	return row["kind"] == "issue" && row["state"] == "closed" && row["status"] == "completed" &&
		!truthy(progress["unavailable"]) && fleet.Number(progress["pending"]) > 0 &&
		row["executionActive"] != true && row["executionScheduled"] != true &&
		listWorkState(with(row, Object{"executionActive": true}))["state"] == "running"
}
