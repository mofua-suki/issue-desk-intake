package consultationai

import (
	"issue-desk-intake/server/internal/fleet"
	"strings"

	"issue-desk-intake/server/internal/completionplan"
)

func completionPlanProblem(d, answer Record) string {
	if answer["ready"] != true {
		return ""
	}
	if answer["completionPlan"] == nil && d["completionPlanVersion"] == nil {
		return ""
	} // Saved legacy drafts remain usable.
	_, err := completionplan.Parse(answer["completionPlan"])
	if err != nil {
		return err.Error()
	}
	return ""
}

// adoptCompletionPlan saves a plan that needs no user confirmation as written, drops the
// model's own approval questions, and shows the plan in the reply.
func adoptCompletionPlan(answer Record, candidate completionplan.Plan) {
	answer["completionPlan"] = candidate.Record()
	if dropCompletionApprovalQuestions(answer, "") && len(fleet.Elements(answer["questions"])) == 0 && len(fleet.Elements(answer["readFiles"])) == 0 {
		answer["ready"] = true
	}
	if section := candidate.Section(); !strings.Contains(fleet.Text(answer["reply"]), section) {
		answer["reply"] = strings.TrimSpace(fleet.Text(answer["reply"]) + "\n\n" + section)
	}
}

// completionConfirmationQuestion is the controller question that asks the user to confirm
// the presented plan, labelled with the categories of its approvals before implementation.
func completionConfirmationQuestion(p completionplan.Plan, comparison completionplan.Comparison) string {
	labels := []string{}
	seen := map[string]bool{}
	for _, a := range p.Approvals {
		if a.Phase == "before_implementation" && completionplan.NeedsUser(a.Category) && !seen[a.Category] {
			labels = append(labels, "【質問分類："+completionplan.CategoryLabel(a.Category)+"】")
			seen[a.Category] = true
		}
	}
	if len(labels) == 0 {
		labels = append(labels, "【質問分類：判断の根拠の変化】")
	}
	change := ""
	if len(comparison.Changes) > 0 {
		change = completionChangeQuestion(comparison)
	}
	return change + strings.Join(labels, "") + "起票前に上記の対象・操作・条件を確認してください。了承する場合は A / 1-A / 承認します / この計画を承認します のいずれかだけで回答してください（おすすめ。個別操作の最終承認は別途必要）。B: 計画を修正する。補足・条件・複数回答を付けた返答は新たな了承になりません。"
}

func existingGatesOnly(p completionplan.Plan) bool {
	for _, a := range p.Approvals {
		if a.Evidence != nil || (completionplan.NeedsUser(a.Category) && (a.Phase != "after_diff" || a.Gate == nil)) {
			return false
		}
	}
	return true
}

func prepareCompletionPlan(d, answer Record) {
	// These are controller records, never accepted from an AI declaration.
	for _, key := range []string{"completionApproval", "completionProposal", "completionReconfirmation", "completionReconfirmationAfter", "completionComparison"} {
		answer[key] = nil
	}
	previous, previousErr := completionplan.Parse(d["completionPlan"])
	if answer["completionPlan"] == nil {
		if previousErr != nil {
			if fleet.Text(d["completionReconfirmation"]) != "" || d["completionPlan"] != nil {
				answer["ready"] = false
				answer["completionReconfirmation"] = d["completionReconfirmation"]
				answer["completionReconfirmationAfter"] = d["completionReconfirmationAfter"]
			}
			return
		}
		answer["completionPlan"] = previous.Record()
	}
	candidate, err := completionplan.Parse(answer["completionPlan"])
	if err != nil {
		answer["ready"] = false
		// A malformed turn cannot erase a saved reconfirmation barrier.
		answer["completionReconfirmation"] = d["completionReconfirmation"]
		answer["completionReconfirmationAfter"] = d["completionReconfirmationAfter"]
		if previousErr == nil {
			answer["completionPlan"] = previous.Record()
		}
		return
	}
	// Only deleting, irreversible and permission items wait for the user. A plan
	// without them is adopted as written. A saved plan that had such an item,
	// or a reconfirmation that is still pending, goes through the comparison
	// below, so a candidate cannot drop or relabel the item to skip the user.
	if !completionplan.HasUserGate(candidate) && fleet.Text(d["completionReconfirmation"]) == "" && (previousErr == nil && !completionplan.HasUserGate(previous) || d["completionPlan"] == nil) {
		adoptCompletionPlan(answer, candidate)
		return
	}
	pending := fleet.Text(d["completionReconfirmation"])
	after := 0
	if pending != "" {
		after = completionplan.PendingAfter(d["messages"], d["completionReconfirmationAfter"])
	}
	presented, receipt, explicit := completionplan.PresentedAfter(previous, d["messages"], after)
	if pending != "" && receipt.Fingerprint != pending {
		explicit = false
	}
	p := candidate
	if previousErr == nil {
		p = previous
	}
	if explicit {
		p = presented
	}
	comparison := completionplan.Compare(p, candidate)
	if explicit && completionplan.AllowsCandidateRewrite(receipt, d["messages"]) {
		comparison = completionplan.ComparePresentedReply(p, candidate)
	}
	// An invalid saved plan cannot be treated as a new consultation: doing so
	// would let a candidate erase an unknown earlier approval boundary. A valid
	// saved presentation and its explicit reply can still supply that boundary.
	if previousErr != nil && d["completionPlan"] != nil && !explicit {
		comparison.Equivalent = false
		comparison.Changes = append(comparison.Changes, "savedPlan.format")
	}
	answer["completionComparison"] = comparison
	reconfirm := pending != "" || !comparison.Equivalent
	confirmed := false
	if explicit && (comparison.PresentedReplyCompatible || comparison.Equivalent) {
		confirmed, reconfirm = true, false
	} else if !reconfirm {
		// This path validates literal earlier instructions only. A candidate
		// cannot supply the missing snapshot for a historical A response.
		receipt, confirmed = completionplan.QuotedConfirmation(p, d["messages"])
	}
	if !comparison.Equivalent && !confirmed {
		p = candidate
		after = len(fleet.Elements(d["messages"]))
	}
	// Losing a previously recorded reply (for example by cancellation) must
	// not make a plan silently ready after its pending change
	// was once confirmed. Require a fresh presentation/reply in that case too.
	if !confirmed && d["completionApproval"] != nil {
		reconfirm = true
		if pending == "" {
			after = len(fleet.Elements(d["messages"]))
		}
	}
	if previousErr == nil && pending == "" && !explicit && !confirmed && d["completionApproval"] == nil && comparison.Equivalent && existingGatesOnly(previous) && existingGatesOnly(candidate) {
		candidateSection := candidate.Section()
		candidate.Approvals = previous.Approvals
		answer["reply"] = strings.ReplaceAll(fleet.Text(answer["reply"]), candidateSection, candidate.Section())
		answer["reply"] = strings.ReplaceAll(fleet.Text(answer["reply"]), previous.Section(), candidate.Section())
		p = candidate
	}
	if reconfirm {
		p.Approvals = append([]completionplan.Approval{}, p.Approvals...)
		for i := range p.Approvals {
			p.Approvals[i].Evidence = nil
		}
	}
	answer["completionPlan"] = p.Record()
	if confirmed {
		answer["completionApproval"] = receipt
	}
	// An accepted reply refers to the presentation, never the candidate text.
	if confirmed && candidate.Fingerprint() != p.Fingerprint() && strings.Contains(fleet.Text(answer["reply"]), candidate.Section()) {
		if completionReplyClaimsApproval(fleet.Text(answer["reply"])) {
			reconcileCompletionReply(answer)
		} else {
			answer["reply"] = strings.ReplaceAll(fleet.Text(answer["reply"]), candidate.Section(), p.Section())
		}
	}
	section := p.Section()
	if !strings.Contains(fleet.Text(answer["reply"]), section) {
		answer["reply"] = strings.TrimSpace(fleet.Text(answer["reply"]) + "\n\n" + section)
	}
	if (completionplan.RequiresConfirmation(p) || reconfirm) && !confirmed {
		// Never echo a model's invented acknowledgement or source quotation
		// while the controller is still waiting for an exact reply.
		if reconfirm {
			answer["completionReconfirmation"] = p.Fingerprint()
			answer["completionReconfirmationAfter"] = after
		}
		answer["completionProposal"] = p.Fingerprint()
		question := completionConfirmationQuestion(p, comparison)
		// The plan confirmation is exactly one controller question; the model's
		// own approval questions are not evidence and would be answered against
		// a row without a presentation.
		dropCompletionApprovalQuestions(answer, question)
		controllerQuestion(answer, question, completionQuestionReason)
		if completionReplyNeedsCorrection(d, answer) {
			reconcileCompletionReply(answer)
		} else {
			answer["reply"] = strings.TrimSpace(fleet.Text(answer["reply"]) + "\n\n" + question)
		}
	}
}
