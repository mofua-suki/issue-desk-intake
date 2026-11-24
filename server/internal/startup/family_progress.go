package startup

import (
	"context"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/numbers"
	"issue-desk-intake/server/internal/usage"
	"issue-desk-intake/server/internal/workplan"
	"strings"
)

func (a *App) familyJobs(ctx context.Context, issues []consultations.FamilyIssue) ([]consultations.FamilyJob, error) {
	wanted := make([]jobs.Issue, 0, len(issues))
	for _, issue := range issues {
		wanted = append(wanted, jobs.Issue{Repo: issue.Project, Number: issue.Number})
	}
	all, err := a.Jobs.ForIssues(ctx, wanted)
	if err != nil {
		return nil, err
	}
	waiting, err := a.Jobs.FamilyDependencies(ctx, all)
	if err != nil {
		return nil, err
	}
	numberIssues := make([]numbers.Issue, 0, len(all))
	for _, j := range all {
		numberIssues = append(numberIssues, numbers.Issue{Project: j.Repo, Number: j.Issue})
	}
	desk, err := a.Numbers.ForIssues(ctx, numberIssues)
	if err != nil {
		return nil, err
	}
	out := make([]consultations.FamilyJob, 0, len(all))
	for _, j := range all {
		var n any
		if v, ok := desk[numbers.Issue{Project: j.Repo, Number: j.Issue}]; ok {
			n = v
		}
		reason := ""
		if j.Status == "held" && j.HoldReason != nil {
			reason = *j.HoldReason
		}
		if j.Status == "failed" {
			if v, ok := j.Result["error"].(string); ok {
				reason = v
			}
		}
		reason = usage.Redact(strings.Join(strings.Fields(reason), " "))
		if len([]rune(reason)) > 500 {
			reason = string([]rune(reason)[:500])
		}
		pending := []string{}
		if plan, err := workplan.Parse(j.View["executionPlan"]); err != nil {
			pending = append(pending, "実行計画を確認できません")
		} else if plan != nil {
			head := ""
			if j.HeadSHA != nil {
				head = *j.HeadSHA
			}
			evidence, evidenceErr := workplan.DecodeEvidence(j.View["executionReceipts"])
			if evidenceErr != nil {
				pending = append(pending, "確認証拠を読めません")
			}
			for _, check := range plan.Checks(head, evidence, j.Generation) {
				if check.Status != "passed" {
					pending = append(pending, check.Owner+": "+check.Criterion)
				}
			}
		}
		out = append(out, consultations.FamilyJob{PendingChecks: pending, Project: j.Repo, Number: j.Issue, Desk: n, Status: j.Status, Reason: reason, Waiting: waiting[j.ID]})
	}
	return out, nil
}
