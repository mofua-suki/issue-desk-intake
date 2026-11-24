package screenapi

import "testing"

// 起票前確認の上限通知（server/internal/filingcheck の FilingCheckLimitAlerts）は、止まりの一覧の汎用の状態へ丸めず
// 専用の状態のまま残り、利用者の要対応として数える。
func TestFilingCheckStoppedStallKeepsStatusAndNeedsUser(t *testing.T) {
	alert := Object{"id": "filing-check-limit-d1", "kind": "intake_error", "status": "filing_check_stopped", "project": "owner/repo", "draftId": "d1", "title": "起票前の確認を止めました", "detail": "相談を更新して再送してください。", "reason": "判定が上限5回に達しました。", "requiresUser": true, "nextAction": "相談を更新して再送する"}
	rows := classifiedListRows(Snapshot{Aux: map[string]any{"stalls": []any{alert}}})
	var row Object
	for _, r := range rows {
		if r["id"] == "stall-filing-check-limit-d1" {
			row = r
		}
	}
	if row == nil || row["kind"] != "stall" || row["stallKind"] != "intake_error" || row["status"] != "filing_check_stopped" {
		t.Fatalf("上限通知の行: %v", row)
	}
	work := listWorkState(row)
	if work["state"] != "attention" || work["nextActor"] != "利用者" || work["nextAction"] != "相談を更新して再送する" || listAttentionClass(row) != "decision" {
		t.Fatalf("上限通知が要対応になりません: %v %s", work, listAttentionClass(row))
	}
}
