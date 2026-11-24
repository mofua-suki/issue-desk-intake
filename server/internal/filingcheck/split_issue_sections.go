package filingcheck

import (
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/workplan"
)

func addSplitIssueSections(target, d Record) error {
	completion, background, err := filing.SplitIssueSections(d)
	if err != nil {
		return err
	}
	plan, err := workplan.Parse(d["executionPlan"])
	if err != nil {
		return err
	}
	if plan != nil {
		if err := plan.Covers(workplan.Conditions(fleet.Text(d["body"]))); err != nil {
			return err
		}
		requirements := make([]Record, 0, len(plan.Requirements))
		for _, r := range plan.Requirements {
			requirements = append(requirements, Record{"id": r.ID, "criterion": r.Criterion, "owner": r.Owner, "phase": r.Phase, "evidenceKind": r.EvidenceKind})
		}
		target["executionPlanRequirements"] = requirements
	}
	if completion != "" {
		target["completionPlanSection"] = completion
	}
	if fleet.Text(d["parentSplit"]) != "" && background != "" {
		target["splitBackgroundSection"] = background
	}
	return nil
}

// reduceSplitBackground records omission without changing the protected target body or plan.
func reduceSplitBackground(material Record) Record {
	out := clone(material)
	target := clone(obj(out["target"]))
	if fleet.Text(target["splitBackgroundSection"]) != "" {
		delete(target, "splitBackgroundSection")
		out["target"] = target
		addInputLimitOmission(out, "分割背景", []string{"target"})
	}
	return out
}
