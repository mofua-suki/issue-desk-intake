package consultationai

import (
	"issue-desk-intake/server/internal/completionplan"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
)

const completionQuestionReason = "実装前に確認する承認事項があります。計画の了承は個別の削除・権限・対象SHAの承認を代替しません。"

var completionQuestionPrefix = regexp.MustCompile(`^\s*[0-9]+\.\s*`)

// completionApprovalQuestion recognizes a model-written request to approve the
// completion plan: one question offering all four exact reply forms. Questions
// that also carry another numbered decision (2-A, 2-B ...) are not plan-only
// and are never retired by this rule.
func completionApprovalQuestion(question string) bool {
	q := strings.TrimSpace(question)
	// A list renderer may add an outer number to an already numbered question.
	// Only remove leading labels; numbered decisions inside the question remain.
	for {
		next := completionQuestionPrefix.ReplaceAllString(q, "")
		if next == q {
			break
		}
		q = next
	}
	if !strings.Contains(q, "この計画を承認します") || strings.Count(q, "承認します") < 2 || !strings.Contains(q, "1-A") || strings.Count(q, "A") <= strings.Count(q, "1-A") {
		return false
	}
	if strings.Count(q, "?")+strings.Count(q, "？") > 1 {
		return false
	}
	for _, other := range []string{"2-A", "2-B", "2-C", "2."} {
		if strings.Contains(q, other) {
			return false
		}
	}
	return strings.Contains(q, "了承") || strings.Contains(q, "計画")
}

// dropCompletionApprovalQuestions removes model-written plan approval questions
// except keep. The controller owns the only plan confirmation question.
func dropCompletionApprovalQuestions(answer Record, keep string) bool {
	removed := map[string]bool{}
	questions := []any{}
	for _, raw := range fleet.Elements(answer["questions"]) {
		if q := fleet.Text(raw); q != keep && completionApprovalQuestion(q) {
			removed[q] = true
			continue
		}
		questions = append(questions, raw)
	}
	if len(removed) == 0 {
		return false
	}
	reasons := []any{}
	for _, raw := range fleet.Elements(answer["questionReasons"]) {
		if removed[fleet.Text(fleet.Object(raw)["question"])] {
			continue
		}
		reasons = append(reasons, raw)
	}
	answer["questions"], answer["questionReasons"] = questions, reasons
	return true
}

// settledCompletionApproval reports whether the controller itself recorded an
// approval in this turn that passes the same provenance check as filing.
// Neither the model's classification nor its approval claim is evidence.
func settledCompletionApproval(d, answer Record) (completionplan.Receipt, bool) {
	receipt, ok := answer["completionApproval"].(completionplan.Receipt)
	comparison, compared := answer["completionComparison"].(completionplan.Comparison)
	if !ok || !compared || receipt.Fingerprint == "" || (!comparison.PresentedReplyCompatible && !comparison.Equivalent) || answer["completionReconfirmation"] != nil || answer["completionProposal"] != nil {
		return completionplan.Receipt{}, false
	}
	effective, err := completionplan.Parse(answer["completionPlan"])
	if err != nil || effective.Fingerprint() != receipt.Fingerprint || comparison.Presented != receipt.Fingerprint {
		return completionplan.Receipt{}, false
	}
	if _, err := completionplan.Check(answer["completionPlan"], d["messages"], receipt); err != nil {
		return completionplan.Receipt{}, false
	}
	return receipt, true
}

// Questions the controller recorded beside the answered proposal are retired by
// their saved provenance. Model-written approval questions for the same plan are
// retired only after the controller has verified the approval.
func resolveConfirmedCompletionQuestions(d, answer Record) bool {
	receipt, ok := settledCompletionApproval(d, answer)
	if !ok {
		return false
	}
	fingerprint := receipt.Fingerprint
	eligible := map[string]bool{}
	if previous, err := completionplan.Parse(d["completionPlan"]); err == nil && fleet.Text(d["completionProposal"]) == fingerprint {
		after := 0
		if fleet.Text(d["completionReconfirmation"]) != "" {
			after = completionplan.PendingAfter(d["messages"], d["completionReconfirmationAfter"])
		}
		_, presentedReceipt, explicit := completionplan.PresentedAfter(previous, d["messages"], after)
		if explicit && presentedReceipt.Fingerprint == fingerprint && presentedReceipt.MessageID == receipt.MessageID {
			for _, raw := range fleet.Elements(d["questionReasons"]) {
				reason := fleet.Object(raw)
				q := fleet.Text(reason["question"])
				if q != "" && reason["controller"] == true && fleet.Text(reason["reason"]) == completionQuestionReason {
					for _, saved := range fleet.Elements(d["questions"]) {
						if fleet.Text(saved) == q {
							eligible[q] = true
						}
					}
				}
			}
		}
	}
	removed := false
	questions := []any{}
	for _, raw := range fleet.Elements(answer["questions"]) {
		if eligible[fleet.Text(raw)] {
			removed = true
			continue
		}
		questions = append(questions, raw)
	}
	reasons := []any{}
	for _, raw := range fleet.Elements(answer["questionReasons"]) {
		if eligible[fleet.Text(fleet.Object(raw)["question"])] {
			continue
		}
		reasons = append(reasons, raw)
	}
	if removed {
		answer["questions"], answer["questionReasons"] = questions, reasons
	}
	if dropCompletionApprovalQuestions(answer, "") {
		removed = true
	}
	if !removed {
		return false
	}
	answer["ready"] = len(fleet.Elements(answer["questions"])) == 0
	return true
}

func reconcileCompletionReply(answer Record) {
	// The model's summary can describe the removed question or claim finality
	// despite remaining questions. Replace it with the actual controller outcome.
	message := "未確認の質問が残っています。回答を確認してから起票可否を判断します。"
	if answer["ready"] == true {
		message = "計画への了承を確認し、下書きの確認を終えました。"
	} else if len(fleet.Elements(answer["questions"])) == 0 {
		message = "計画への了承を確認しました。起票条件の確認を続けます。"
	}
	for _, question := range fleet.Elements(answer["questions"]) {
		if q := strings.TrimSpace(fleet.Text(question)); q != "" {
			message += "\n- " + q
		}
	}
	if plan, err := completionplan.Parse(answer["completionPlan"]); err == nil {
		message += "\n\n" + plan.Section()
	}
	answer["reply"] = message
}
