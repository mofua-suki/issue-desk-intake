package issueapi

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
)

func (s *Service) ReadIncidentTarget(ctx context.Context, repo string, n int64) (Object, error) {
	detail, e := s.Detail(ctx, repo, n, false, false)
	if e != nil {
		return nil, e
	}
	if detail["project"] != repo {
		return nil, fail(409, "対象の依頼を照合できません。")
	}
	actual, _ := number(detail["number"])
	if actual != n {
		return nil, fail(409, "対象の依頼を照合できません。")
	}
	job, e := s.o.Jobs.ByIssue(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	stopped := detail["status"] == "stopped"
	status, problem := detail["status"], detail["error"]
	if job != nil {
		stopped = stopped || job.Status == "stopped" || job.View["stopRequested"] == true
		status = job.Status
		if job.Status == "held" && job.HoldReason != nil {
			status = *job.HoldReason
			if status == "question" {
				status = "waiting"
			}
		}
		problem = job.Result["error"]
		if problem == nil || problem == "" {
			problem = job.Result["reason"]
		}
		if job.LeaseHost != nil {
			if s.o.Postgres == nil {
				return nil, fmt.Errorf("停止の指示を照合できません")
			}
			var pending bool
			e = s.o.Postgres.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_commands WHERE target_host=$1 AND repo=$2 AND issue=$3 AND kind='stop' AND status IN('queued','acked'))`, *job.LeaseHost, repo, n).Scan(&pending)
			if e != nil {
				return nil, e
			}
			stopped = stopped || pending
		}
	}
	comments := []any{}
	for _, raw := range fleet.Items(detail["comments"]) {
		c := fleet.ObjectOrEmpty(raw)
		comments = append(comments, Object{"kind": c["kind"], "bot": c["bot"], "body": c["body"]})
	}
	target := Object{"project": repo, "number": n, "title": detail["title"], "body": detail["body"], "comments": comments, "status": status, "state": detail["state"], "error": problem, "stopped": stopped}
	if job != nil {
		// These are stored controller records, not approval claims parsed from
		// comments. Recovery uses them both as context and for its final recheck.
		target["result"] = job.Result
		target["generation"] = job.Generation
		target["completionPlan"] = job.View["completionPlan"]
		target["completionApproval"] = job.View["completionApproval"]
		target["completionPlanEvidence"] = job.View["completionPlanEvidence"]
	}
	return target, nil
}

// SendIncidentAction only accepts the two actions authorized by deadline policy.
// It cannot supply permission changes, user quotations or limit overrides.
func (s *Service) SendIncidentAction(ctx context.Context, repo string, n int64, id, action, text string) (Object, error) {
	return s.sendAutomaticAction(ctx, repo, n, id, action, text, "有事の対処からはこの操作を実行できません。", "有事の期限切れの対処")
}

// SendRecoveryAction accepts only recovery retry and reply, without overrides.
func (s *Service) SendRecoveryAction(ctx context.Context, repo string, n int64, id, action, text string) (Object, error) {
	return s.sendAutomaticAction(ctx, repo, n, id, action, text, "自動復旧からはこの操作を実行できません。", "止まった依頼の自動復旧")
}

// sendAutomaticAction sends retry or reply on behalf of an automatic route.
// It refuses other actions and never lifts an explicit stop.
func (s *Service) sendAutomaticAction(ctx context.Context, repo string, n int64, id, action, text, refused, via string) (Object, error) {
	if !oneOf(action, "retry", "reply") {
		return nil, fail(403, refused)
	}
	current, e := s.ReadIncidentTarget(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	if current["stopped"] == true {
		return nil, fail(409, "明示された停止は自動で解除できません。")
	}
	saved, e := s.read(ctx, "actions", id)
	if e != nil {
		return nil, e
	}
	if saved["applied"] == true {
		return s.commentWithVia(ctx, Object{"project": repo, "number": n, "id": id, "action": action, "text": text}, auth.Principal{Kind: auth.CLI}, via)
	}
	job, e := s.o.Jobs.ByIssue(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	if jobs.QuestionDataUnconfirmed(job) {
		if s.o.Jobs.UsageLimitHeld(ctx, job, jobs.ControlOptions{UsageLimit: s.o.UsageLimit, UsageWeights: s.o.UsageWeights}) {
			return nil, fail(409, "累計使用量の上限を超えています。承認付き再試行を使ってください。")
		}
		return nil, fail(409, "質問の記録が欠けているため自動で操作できません。")
	}
	return s.commentWithVia(ctx, Object{"project": repo, "number": n, "id": id, "action": action, "text": text}, auth.Principal{Kind: auth.CLI}, via)
}
