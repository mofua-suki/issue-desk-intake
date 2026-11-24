package filing

import (
	"crypto/sha256"
	"encoding/hex"
	"issue-desk-intake/server/internal/fleet"

	"issue-desk-intake/server/internal/completionplan"
)

func prepareCompletionPlan(d Record) (*completionplan.Plan, error) {
	if d["completionPlan"] == nil && d["completionPlanVersion"] == nil && fleet.Text(d["completionReconfirmation"]) == "" {
		return nil, nil
	}
	value := d["completionPlan"]
	// Plans without a deleting, irreversible or permission item do not wait for
	// a reply, unless a reconfirmation (for example of a dropped item) is pending.
	if p, err := completionplan.Parse(value); err == nil && !completionplan.HasUserGate(p) && fleet.Text(d["completionReconfirmation"]) == "" {
		if err := validatePlanChildren(d, p); err != nil {
			return nil, err
		}
		return &p, nil
	}
	if fleet.Text(d["completionReconfirmation"]) != "" {
		pending, err := completionplan.Parse(value)
		if err != nil || pending.Fingerprint() != fleet.Text(d["completionReconfirmation"]) {
			return nil, fail(409, "変更した前提への明示回答を確認していません。")
		}
		p, err := completionplan.CheckPresented(value, d["messages"], d["completionApproval"], completionplan.PendingAfter(d["messages"], d["completionReconfirmationAfter"]))
		if err != nil {
			return nil, fail(409, err.Error())
		}
		if err := validatePlanChildren(d, p); err != nil {
			return nil, err
		}
		return &p, nil
	}
	p, err := completionplan.Check(value, d["messages"], d["completionApproval"])
	if err != nil {
		return nil, fail(409, err.Error())
	}
	if err := validatePlanChildren(d, p); err != nil {
		return nil, err
	}
	return &p, nil
}

func validatePlanChildren(d Record, p completionplan.Plan) error {
	count := len(fleet.Elements(d["splitIssues"]))
	// A leaf inherits its parent's plan; the parent checks its split count.
	if fleet.Text(d["parentSplit"]) != "" && count == 0 {
		return nil
	}
	for _, approval := range p.Approvals {
		for _, child := range approval.Children {
			if child > count {
				return fail(409, "完了計画の承認対象が分割案の件数と一致しません。")
			}
		}
	}
	return nil
}
func attachCompletionPlan(prepared *Prepared, d Record, p *completionplan.Plan) {
	if index, ok := integer(d["splitIndex"]); fleet.Text(d["parentSplit"]) != "" && ok && index > 0 {
		prepared.Job.View["splitIndex"] = index
	}
	if p == nil {
		return
	}
	prepared.Job.View["completionPlan"] = p.Record()
	if d["completionApproval"] != nil {
		prepared.Job.View["completionApproval"] = d["completionApproval"]
	}
	sum := sha256.Sum256([]byte(prepared.Body))
	prepared.Job.View["completionPlanEvidence"] = Record{"bodySHA256": hex.EncodeToString(sum[:])}
}
