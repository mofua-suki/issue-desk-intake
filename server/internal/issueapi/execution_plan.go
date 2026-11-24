package issueapi

import (
	"encoding/json"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/workplan"
)

func executionPlanProjection(job *jobs.Job) Object {
	p, err := workplan.Parse(job.View["executionPlan"])
	if err != nil {
		return Object{"unavailable": "受入条件の実行計画を確認できません"}
	}
	if p == nil {
		return nil
	}
	head := ""
	if job.HeadSHA != nil {
		head = *job.HeadSHA
	}
	evidence, err := workplan.DecodeEvidence(job.View["executionReceipts"])
	if err != nil {
		return Object{"unavailable": "受入条件の確認証拠を読めません"}
	}
	checks := p.Checks(head, evidence, job.Generation)
	pending := 0
	for _, c := range checks {
		if c.Status != "passed" {
			pending++
		}
	}
	raw, _ := json.Marshal(Object{"planSHA": p.Fingerprint(), "head": head, "generation": job.Generation, "checks": checks, "pending": pending})
	var out Object
	_ = json.Unmarshal(raw, &out)
	return redactedStrings(out).(Object)
}

func executionPlanSummary(progress Object) Object {
	if progress == nil {
		return nil
	}
	out := Object{}
	for _, key := range []string{"planSHA", "head", "generation", "pending", "unavailable"} {
		if v, ok := progress[key]; ok {
			out[key] = v
		}
	}
	return out
}

func unfinishedExecutionJob(job *jobs.Job) bool {
	if job == nil || job.Status == "closed" {
		return false
	}
	progress := executionPlanProjection(job)
	pending, _ := number(progress["pending"])
	return progress["unavailable"] != nil || pending > 0
}
