package deskcleanup

import "testing"

// 起票前確認の上限通知は codeRevisionChanged が配置中の版との比較で変わる。版の更新だけでは非表示を解かず、
// 判定した版や理由が変われば別の通知として出し直す。
func TestAlertFingerprintIgnoresCodeRevisionChanged(t *testing.T) {
	base := func() Object {
		return Object{"id": "filing-check-limit-d1", "status": "filing_check_stopped", "draftId": "d1", "title": "起票前の確認を止めました", "detail": "判定が上限5回に達しました。", "codeRevision": "aaaa", "codeRevisionChanged": false}
	}
	changed := base()
	changed["codeRevisionChanged"] = true
	if AlertFingerprint(base()) != AlertFingerprint(changed) {
		t.Fatal("codeRevisionChanged だけの違いで照合値が変わりました")
	}
	for key, value := range map[string]any{"codeRevision": "bbbb", "detail": "判定が上限6回に達しました。"} {
		other := base()
		other[key] = value
		if AlertFingerprint(base()) == AlertFingerprint(other) {
			t.Fatalf("%s が変わっても照合値が同じです", key)
		}
	}
}
