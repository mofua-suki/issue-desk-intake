package consultationai

import (
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"issue-desk-intake/server/internal/completionplan"
)

var completionReplySentences = regexp.MustCompile(`[^。\n]+[。\n]?|\n`)
var completionFinalityClaim = regexp.MustCompile(`確定(?:しました|いたしました|した|しています|している|しており|されました|された|されています|されている|されており|済み|です|でございます|となりました|となった|となっています|となっている|となります)`)
var completionFinalityDenial = regexp.MustCompile(`確定(?:(?:しており|されており)ません|(?:しておら|されておら)ず|(?:されてい|してい)ません|されませんでした|となりません|(?:した|している|されました|された|されています|されている|となりました|となった|となっています|となっている|となります|済み|です)(?:わけでは|とは限り|のでは|では|じゃ|とは言え|とはいえ))`)

func completionReplyClaimsFinality(reply string) bool {
	return completionFinalityClaim.MatchString(completionFinalityDenial.ReplaceAllString(reply, ""))
}

// Correct an invented acknowledgement or an attempted non-exact approval. A
// normal explanation of a pending plan must still reach the user.
func completionReplyClaimsApproval(reply string) bool {
	for _, marker := range []string{"了承済み", "承認済み", "了承を確認", "承認を確認", "了承として", "承認として", "保存発言"} {
		if strings.Contains(reply, marker) {
			return true
		}
	}
	return false
}

func completionReplyNeedsCorrection(d, answer Record) bool {
	if completionReplyClaimsApproval(fleet.Text(answer["reply"])) {
		return true
	}
	for i := len(fleet.Elements(d["messages"])) - 1; i >= 0; i-- {
		row := fleet.Object(fleet.Elements(d["messages"])[i])
		if row["role"] == "user" {
			value := strings.TrimSpace(fleet.Text(row["text"]))
			for _, prefix := range []string{"A", "1-A", "承認します", "この計画を承認します"} {
				if strings.HasPrefix(value, prefix) {
					rest := strings.TrimSpace(strings.TrimPrefix(value, prefix))
					if rest == "" {
						return true
					}
					r, _ := utf8.DecodeRuneInString(rest)
					if unicode.IsPunct(r) || unicode.IsSymbol(r) {
						return true
					}
				}
			}
			break
		}
	}
	return false
}

// Rebuild only the summary of a resolved plan question. Other controllers append
// notices after this point; preserve those notices and use the final ready state.
func completionReplyFinalizer(answer Record, resolved bool, before []any) func() {
	base := fleet.Text(answer["reply"])
	removed := []string{}
	for _, raw := range before {
		q := fleet.Text(raw)
		found := false
		for _, remaining := range fleet.Elements(answer["questions"]) {
			found = found || fleet.Text(remaining) == q
		}
		if !found && q != "" {
			removed = append(removed, q)
		}
	}
	return func() {
		if !resolved {
			return
		}
		current := strings.TrimSpace(fleet.Text(answer["reply"]))
		suffix := current
		// Retain independent model explanations, but not resolved questions or
		// assertions of finality while other controller checks may still hold.
		explanation := base
		if strings.HasPrefix(current, strings.TrimSpace(base)) {
			suffix = strings.TrimPrefix(current, strings.TrimSpace(base))
		} else {
			explanation = ""
		}
		if plan, err := completionplan.Parse(answer["completionPlan"]); err == nil {
			explanation = strings.ReplaceAll(explanation, plan.Section(), "")
		}
		for _, q := range removed {
			explanation = strings.ReplaceAll(explanation, q, "")
		}
		parts := completionReplySentences.FindAllString(explanation, -1)
		kept := []string{}
		for _, part := range parts {
			if !completionReplyClaimsFinality(part) && !completionReplyClaimsApproval(part) && !completionApprovalQuestion(part) {
				kept = append(kept, part)
			}
		}
		reconcileCompletionReply(answer)
		answer["reply"] = strings.TrimSpace(strings.Join(kept, "") + "\n" + fleet.Text(answer["reply"]) + "\n" + suffix)
	}
}

// A finality claim must not contradict the controller's final hold state.
func clarifyIncompleteReply(answer Record) {
	if answer["ready"] == true && len(fleet.Elements(answer["questions"])) == 0 {
		return
	}
	reply := fleet.Text(answer["reply"])
	claim := false
	for _, marker := range []string{"確認を終えました", "確認が完了しました", "確認は完了しました", "起票できます", "起票可能です"} {
		if strings.Contains(reply, marker) {
			claim = true
			break
		}
	}
	if !claim {
		return
	}
	message := "未確認の質問が残っています。回答を確認してから起票可否を判断します。"
	if len(fleet.Elements(answer["questions"])) == 0 {
		message = "起票条件の確認が完了していないため、まだ起票できません。"
	}
	for _, q := range fleet.Elements(answer["questions"]) {
		if value := strings.TrimSpace(fleet.Text(q)); value != "" {
			message += "\n- " + value
		}
	}
	answer["reply"] = strings.TrimSpace(reply + "\n\n" + message)
}

func completionChangeQuestion(comparison completionplan.Comparison) string {
	if comparison.Rule != "completion-plan-presented-reply-v2" && comparison.Rule != "completion-plan-instruction-v2" {
		return "変更箇所（同一性未確認）: " + strings.Join(comparison.Changes, ", ") + "。"
	}
	protected, prose := []string{}, []string{}
	for _, path := range comparison.Changes {
		if path == "basis" || strings.HasPrefix(path, "steps.") {
			prose = append(prose, path)
		} else {
			protected = append(protected, path)
		}
	}
	message := "再確認が必要な変更箇所: " + strings.Join(protected, ", ") + "。"
	if len(prose) > 0 {
		message += "監査記録のみの自由文差（再確認の原因ではありません）: " + strings.Join(prose, ", ") + "。"
	}
	return message
}
