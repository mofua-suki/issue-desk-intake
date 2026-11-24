package issueapi

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"strings"

	"issue-desk-intake/server/internal/usage"
)

// 保存済みの送信済み記録を、新形式のfield不在を理由に再送しない。
func closeNotePending(r Object) bool {
	if value, exists := r["notePending"]; exists {
		return value == true
	}
	return r["noteAttempted"] == true && r["notePosted"] != true
}
func closeNoteMarker(r Object) string {
	return "<!-- gcr:issue-desk-close-request:" + fleet.Text(r["id"]) + " -->"
}
func closeNoteBody(r Object) string {
	inert := func(s string) string { return strings.ReplaceAll(s, "<!--", "&lt;!--") }
	decision := fleet.ObjectOrEmpty(r["decision"])
	actor := "PC別トークン " + strings.TrimPrefix(fleet.Text(decision["via"]), "token:")
	if decision["via"] == "cli-key" {
		actor = "受付と同じPCのスクリプト（一時キー）"
	}
	method := "受付画面で利用者が判断"
	switch decision["method"] {
	case "ai":
		method = "受付AIが内容と理由を確認"
	case "machine":
		method = "コントローラーが機械判定（説明と題名の照合・後続の不存在。AIは未使用）"
	case "explicit":
		method = "利用者の明示指示（" + actor + "）による終了"
	case "delegate":
		via := fleet.Text(decision["via"])
		name := "PC別トークン " + strings.TrimPrefix(via, "token:")
		if via == "cli-key" {
			name = "受付と同じPCのスクリプト（一時キー）"
		}
		method = "運用の代行（" + name + "）が判断"
	}
	source := "PC別トークンからのクローズ依頼"
	if r["owner"] == "issue-screen" {
		source = "受付画面の「閉じる」"
	} else if decision["method"] == "explicit" {
		source = "利用者の明示指示（" + actor + "）"
	}
	names := map[string]string{"voluntary": "任意終了", "resolved": "対応済み", "duplicate": "重複", "other_route": "別ルートでマージ／直接実装"}
	reason := fleet.Text(r["reason"])
	lines := []string{"受付がこのIssueを閉じました（" + source + "）。", "- 依頼元PC: " + fleet.Text(r["pc"]), "- 終了理由: " + names[reason] + "（" + reason + "）", "- 判断: " + method, "- 判断理由: " + inert(clipText(fleet.Text(decision["reason"]), 2000)), "- 依頼者の説明: " + inert(fleet.Text(r["description"]))}
	unresolved, posted := []string{}, []string{}
	for _, raw := range fleet.Items(r["unresolvedActions"]) {
		name := fleet.Text(fleet.ObjectOrEmpty(raw)["action"]) + "（送信ID " + fleet.Text(fleet.ObjectOrEmpty(raw)["id"]) + "）"
		if fleet.ObjectOrEmpty(raw)["posted"] == true {
			posted = append(posted, name)
		} else {
			unresolved = append(unresolved, name)
		}
	}
	if len(unresolved) > 0 {
		lines = append(lines, "- 結果を照合できない操作: "+inert(strings.Join(unresolved, "、"))+"。送信を始めた後に結果を記録できず残った操作で、成功とは扱っていません。")
	}
	if len(posted) > 0 {
		lines = append(lines, "- 画像の転送が終わっていない操作: "+inert(strings.Join(posted, "、"))+"。コメントは投稿済みで、画像の転送と本文の差し替えは終わっていません。")
	}
	if r["unresolvedActionsUnknown"] == true {
		lines = append(lines, "- 閉じた時点で、結果の記録が無い操作を確認できませんでした。")
	}
	if note := fleet.Text(r["comment"]); note != "" {
		lines = append(lines, "", "補足・根拠:", inert(note))
	}
	// 目印は伏せた後に足す。伏せる処理が本文の続きとして目印を消すと、終了メモを見つけられなくなる。
	return usage.Redact(strings.Join(lines, "\n")) + "\n\n" + closeNoteMarker(r)
}
func (s *Service) postCloseNote(ctx context.Context, r Object) error {
	if !closeNotePending(r) {
		return nil
	}
	repo := fleet.Text(r["project"])
	n, _ := number(r["number"])
	comments, e := s.o.Gitea.Comments(ctx, repo, n)
	if e != nil {
		return e
	}
	marker := closeNoteMarker(r)
	for _, c := range comments {
		if s.o.Own(fleet.ObjectOrEmpty(c["user"])) && strings.Contains(fleet.Text(c["body"]), marker) {
			r["notePending"] = false
			r["notePosted"] = true
			r["retryAt"] = nil
			return s.saveClose(ctx, r)
		}
	}
	if r["noteAttempted"] == true {
		return fail(503, "終了コメントの前回結果を照合できません。")
	}
	r["noteAttempted"] = true
	if e = s.saveClose(ctx, r); e != nil {
		return e
	}
	if _, e = s.o.Gitea.Comment(ctx, repo, n, closeNoteBody(r)); e != nil {
		return e
	}
	r["notePending"] = false
	r["notePosted"] = true
	r["retryAt"] = nil
	return s.saveClose(ctx, r)
}
