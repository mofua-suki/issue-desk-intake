package issueapi

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
)

// 送信を始めたまま結果の残らなかった操作は、送信中の間だけクローズを止め、
// 残っているだけなら閉じて、成功扱いにせず終了の記録とコメントに残す。
func TestLeftoverAttemptedOperationDoesNotBlockCloseForever(t *testing.T) {
	f := newFixture(t)
	f.options.Answer = func(_ context.Context, _ Object) (Object, error) {
		return Object{"decision": "close", "contentMatch": true, "reasonConfirmed": true, "reason": "対応を確認", "evidence": []any{}}, nil
	}
	f.restart()
	actionID := "66666666-6666-4666-8666-666666666666"
	if _, err := f.app.DB().ExecContext(f.ctx, bindSQL(`INSERT INTO actions(id,json) VALUES(?,?)`), actionID, `{"payload":{"repo":"owner/repo","number":1,"action":"priority","value":"high"},"attempted":true}`); err != nil {
		t.Fatal(err)
	}
	input := Object{"id": "77777777-7777-4777-8777-777777777777", "project": "owner/repo", "number": 1, "reason": "other_route", "route": "owner/repo#12", "description": "別のPRで対応"}

	unlock := lock(&f.s.locks, "issue:owner/repo#1")
	code, value := f.call("POST", "/api/issues/close", "nas", input)
	unlock()
	if code != 409 || !strings.Contains(fmt.Sprint(value), "この依頼への操作を送信中です。") || f.api.closed != 0 {
		t.Fatalf("in-flight operation: status=%d value=%v closed=%d", code, value, f.api.closed)
	}

	code, value = f.call("POST", "/api/issues/close", "nas", input)
	if code != 200 {
		t.Fatal(code, value)
	}
	f.s.wg.Wait()
	request, err := f.s.read(f.ctx, "close_requests", fleet.Text(input["id"]))
	if err != nil || request["status"] != "closed" || f.api.closed != 1 {
		t.Fatal(request, err, f.api.closed)
	}
	unresolved := fleet.Items(request["unresolvedActions"])
	if len(unresolved) != 1 || fleet.ObjectOrEmpty(unresolved[0])["id"] != actionID || fleet.ObjectOrEmpty(unresolved[0])["action"] != "priority" {
		t.Fatalf("unresolved actions = %v", request["unresolvedActions"])
	}
	note := ""
	for _, c := range f.api.comments {
		if strings.Contains(fleet.Text(c["body"]), closeNoteMarker(request)) {
			note = fleet.Text(c["body"])
		}
	}
	if !strings.Contains(note, "結果を照合できない操作: priority（送信ID "+actionID+"）") || !strings.Contains(note, "成功とは扱っていません") {
		t.Fatalf("close note = %q", note)
	}
	// 結果不明の操作を適用済みに書き換えない。
	saved, err := f.s.read(f.ctx, "actions", actionID)
	if err != nil || saved["attempted"] != true || saved["applied"] == true {
		t.Fatal(saved, err)
	}
}

// 終了コメントは、結果不明の操作・投稿済みで画像の転送だけ残る返信・確認できなかった事実を分けて書き、
// 何も残っていなければ行を足さない。
func TestCloseNoteSeparatesUnresolvedPostedAndUnknownActions(t *testing.T) {
	base := Object{"id": "fixture", "owner": "token:pc", "reason": "voluntary", "decision": Object{"method": "explicit", "via": "token:pc"}}
	body := closeNoteBody(base)
	for _, unwanted := range []string{"結果を照合できない操作", "画像の転送が終わっていない操作", "確認できませんでした"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("note without leftovers contains %q: %s", unwanted, body)
		}
	}
	r := clone(base)
	r["unresolvedActions"] = []any{Object{"id": "a", "action": "retry"}, Object{"id": "b", "action": "reply", "posted": true}}
	r["unresolvedActionsUnknown"] = true
	body = closeNoteBody(r)
	if !strings.Contains(body, "結果を照合できない操作: retry（送信ID a）。") || strings.Contains(body, "結果を照合できない操作: retry（送信ID a）、reply") {
		t.Fatalf("unresolved line: %s", body)
	}
	if !strings.Contains(body, "画像の転送が終わっていない操作: reply（送信ID b）。コメントは投稿済み") {
		t.Fatalf("posted line: %s", body)
	}
	if !strings.Contains(body, "結果の記録が無い操作を確認できませんでした") {
		t.Fatalf("unknown line: %s", body)
	}
}
