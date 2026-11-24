package issueapi

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/usage"
	"strings"
	"time"
)

func clipped(value any, max int) string { return contract.ClipUTF16(fleet.Text(value), max) }
func normalizedWorkType(value any) string {
	if oneOf(fleet.Text(value), "bugfix", "feature", "refactor", "docs", "test", "operations", "investigation") {
		return fleet.Text(value)
	}
	return "unknown"
}

var workTypeLabels = map[string]string{"bugfix": "不具合の修正", "feature": "機能追加", "refactor": "作り替え", "docs": "資料", "test": "テスト", "operations": "運用", "investigation": "調査", "unknown": "不明"}

func legacyStatus(issue Object) string {
	seen := map[string]bool{}
	for _, raw := range fleet.Items(issue["labels"]) {
		label := fleet.Text(raw)
		if label == "" {
			label = fleet.Text(fleet.ObjectOrEmpty(raw)["name"])
		}
		if !strings.HasPrefix(label, "state/") {
			continue
		}
		status := strings.TrimPrefix(label, "state/")
		if oneOf(status, "queued", "dependency_waiting", "project_waiting", "running", "waiting", "publishing", "scope_waiting", "releasing", "review_held", "deploying", "completed", "failed", "stopped") {
			seen[status] = true
		}
	}
	if len(seen) == 1 {
		for status := range seen {
			return status
		}
	}
	return ""
}
func reviewProjection(value any) Object {
	r := fleet.ObjectOrEmpty(value)
	head := fleet.Text(r["head"])
	if !sha.MatchString(head) {
		return nil
	}
	if r["kind"] == "machine" {
		return Object{"kind": "machine", "head": head}
	}
	model := clipped(r["model"], 100)
	if r["kind"] != "ai" || !oneOf(fleet.Text(r["provider"]), "codex", "claude") || strings.TrimSpace(model) == "" {
		return nil
	}
	nullable := func(value any, max int) any {
		if text := clipped(value, max); strings.TrimSpace(text) != "" {
			return text
		}
		return nil
	}
	out := Object{"kind": "ai", "provider": r["provider"], "model": model, "effort": nullable(r["effort"], 20), "basis": nil, "head": head}
	if oneOf(fleet.Text(r["basis"]), "override", "opposite", "fixed", "default") {
		out["basis"] = r["basis"]
	}
	if r["basis"] == "opposite" && oneOf(fleet.Text(r["implementation"]), "codex", "claude", "antigravity") {
		out["implementation"] = r["implementation"]
	}
	if r["basis"] == "override" && clipped(r["overrideUntil"], 40) != "" {
		out["overrideUntil"] = clipped(r["overrideUntil"], 40)
	}
	disable := fleet.ObjectOrEmpty(r["providerDisable"])
	if until := clipped(disable["until"], 40); until != "" {
		from := fleet.Text(disable["from"])
		if !oneOf(from, "codex", "claude") {
			from = "claude"
		}
		out["providerDisable"] = Object{"from": from, "until": until, "setAt": nullable(disable["setAt"], 40), "source": nullable(disable["source"], 20)}
	}
	return out
}
func deletionProjection(job *jobs.Job) Object {
	if job.Status != "held" || job.HoldReason == nil || *job.HoldReason != "deletions" {
		return nil
	}
	d := fleet.ObjectOrEmpty(job.View["deletionApproval"])
	if !sha.MatchString(fleet.Text(d["head"])) {
		return nil
	}
	raw, ok := d["items"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	if len(raw) > 100 {
		raw = raw[:100]
	}
	items := []any{}
	sql := false
	for _, r := range raw {
		// For a data-deletion SQL hold the worker sends every file the PR changes as a plain path
		// (git diff --name-status), not only the files holding the SQL.
		if text, ok := r.(string); ok {
			path := clipped(text, 500)
			if path == "" {
				return nil
			}
			items = append(items, Object{"kind": "changed", "path": path})
			sql = true
			continue
		}
		item := fleet.ObjectOrEmpty(r)
		kind, path := fleet.Text(item["kind"]), clipped(item["path"], 500)
		if !oneOf(kind, "delete", "rename", "type") || path == "" {
			return nil
		}
		clean := Object{"kind": kind, "path": path}
		if kind == "rename" {
			from := clipped(item["from"], 500)
			if from == "" {
				return nil
			}
			clean["from"], clean["identical"] = from, item["identical"] == true
		}
		items = append(items, clean)
	}
	total := len(items)
	if n, ok := number(d["total"]); ok && n >= int64(total) {
		total = int(n)
	}
	out := Object{"head": d["head"], "total": total, "items": items, "reason": *job.HoldReason}
	if sql {
		out["scope"] = "data_deletion_sql"
	}
	return out
}

type usageDecisionKey struct{}
type usageDecision struct {
	set   bool
	jobID int64
	held  bool
}

func (s *Service) projectedUsageHeld(ctx context.Context, job *jobs.Job) bool {
	if job == nil {
		return false
	}
	cache, _ := ctx.Value(usageDecisionKey{}).(*usageDecision)
	if cache != nil && cache.set && cache.jobID == job.ID {
		return cache.held
	}
	held := s.o.Jobs.UsageLimitHeld(ctx, job, jobs.ControlOptions{UsageLimit: s.o.UsageLimit, UsageWeights: s.o.UsageWeights})
	if cache != nil {
		cache.set, cache.jobID, cache.held = true, job.ID, held
	}
	return held
}

func (s *Service) jobProjection(ctx context.Context, repo, status string, job *jobs.Job) (Object, error) {
	wait, e := s.scopeWait(ctx, repo, job)
	if e != nil {
		return nil, e
	}
	return s.jobProjectionWithWait(ctx, repo, status, job, wait)
}

func (s *Service) jobProjectionWithWait(ctx context.Context, repo, status string, job *jobs.Job, wait Object) (Object, error) {
	out := Object{"workType": normalizedWorkType(job.View["workType"]), "releasePending": job.View["releasePending"] == true, "publishPending": job.View["publishPending"] == true, "followup": job.Kind == "followup"}
	if execution := executionPlanProjection(job); execution != nil {
		out["executionProgress"] = execution
	}
	active := s.leaseActive(job)
	out["executionActive"] = active && status != "scope_waiting"
	out["executionScheduled"] = job.Status == "queued" || active && status == "scope_waiting"
	if active {
		out["executionUntil"] = *job.LeaseExpiresAt
	}

	decision := requiresUserAnswer(job, status, s.o.RunLimit)
	usageHeld := !decision && status == "waiting" && s.projectedUsageHeld(ctx, job)
	if decision || usageHeld {
		out["requiresUser"] = true
	}
	if result := resultProjection(job.Result); result != nil {
		out["result"] = result
	}
	for _, key := range []string{"executionSettings", "overLimitApprovals", "executionWait", "executionMissingCapabilities", "executionReportError"} {
		if value := job.View[key]; value != nil {
			if key == "executionSettings" {
				if _, ok := value.(map[string]any); !ok {
					continue
				}
			}
			out[key] = value
		}
	}
	if job.Status == "held" || job.Status == "failed" || job.Status == "stopped" {
		pending, err := jobs.PendingFinalApprovals(job)
		if err != nil {
			out["finalApprovalsUnavailable"] = "完了計画を確認できません。"
		} else if len(pending) > 0 {
			out["pendingFinalApprovals"] = pending
			if jobs.FinalApprovalHeld(job) {
				out["requiresUser"] = true
			}
		}
	}
	if review := reviewProjection(job.View["review"]); review != nil {
		out["review"] = review
	}
	if deletion := deletionProjection(job); deletion != nil {
		out["deletionApproval"] = deletion
	}
	if oneOf(status, "failed", "stopped", "review_held", "closed") || status == "waiting" && out["requiresUser"] != true {
		if errorText := jobErrorText(job); errorText != "" {
			out["error"] = errorText
		}
		if job.Status == "held" && out["requiresUser"] != true {
			incomplete := fleet.ObjectOrEmpty(job.Result["incomplete"])
			parts := []string{clipped(out["error"], 750), clipped(incomplete["reason"], 750)}
			remaining := []string{}
			for _, item := range stringList(incomplete["remaining"]) {
				remaining = append(remaining, fleet.Text(item))
			}
			parts = append(parts, clipped(strings.Join(remaining, " "), 450))
			out["error"] = clipped(usage.Redact(strings.TrimSpace(strings.Join(parts, " "))), 2000)
		}
	}
	if status == "waiting" && jobs.QuestionDataUnconfirmed(job) {
		out["error"] = strings.TrimSpace(clipped(out["error"], 1900) + " 質問の記録が欠けているため、判断の有無と再開条件は未確認です。")
	}
	if reason := transferReason(job); reason != "" {
		out["dependencyReason"] = shownReason(reason, s.numberLookup(ctx, repo))
	}
	if wait != nil {
		out["scopeWait"] = wait
	}
	if s.o.Postgres != nil && job.Status == "queued" {
		lines, e := s.queuedDependencyLines(ctx, repo, job)
		if e != nil {
			return nil, e
		}
		if len(lines) > 0 {
			prefix := fleet.Text(out["dependencyReason"])
			if prefix != "" {
				prefix += "\n"
			}
			out["dependencyReason"] = prefix + shownReason(strings.Join(lines, "\n"), s.numberLookup(ctx, repo))
		}
	}
	detailedHistory, _ := ctx.Value(dispatchDetailKey{}).(bool)
	if record, skip := s.dispatchProjection(ctx, job, detailedHistory); record != nil {
		out["dispatchRecord"] = record
		if skip != nil {
			out["dispatchSkip"] = skip
		}
	}
	if out["requiresUser"] == true {
		action := userNextAction(job, s.o.RunLimit)
		if usageHeld {
			action = "使用量の上限を超えて続けるか判断する"
		}
		out["nextActor"], out["nextAction"] = "user", clipped(usage.Redact(action), 2000)
	}
	return out, nil
}

// leaseActive reports whether the job runs under a lease that has not expired.
func (s *Service) leaseActive(job *jobs.Job) bool {
	if job.Status != "running" || job.LeaseHost == nil || *job.LeaseHost == "" || job.LeaseExpiresAt == nil {
		return false
	}
	until, err := time.Parse(time.RFC3339Nano, *job.LeaseExpiresAt)
	now := time.Now()
	if s.o.Now != nil {
		now = s.o.Now()
	}
	return err == nil && until.After(now)
}

// fixedTestHoldWithoutDecision requires explicit empty questions and fixed-test
// evidence; missing question data alone cannot clear a decision hold.
func fixedTestHoldWithoutDecision(job *jobs.Job, runLimit int64) bool {
	if job == nil || job.Status != "held" || job.HoldReason == nil || *job.HoldReason != "question" {
		return false
	}
	questions, present := job.Result["questions"].([]any)
	if !present || len(questions) != 0 || job.Result["status"] == "question" || runLimit > 0 && jobs.EffectiveRuns(job) >= runLimit {
		return false
	}
	if len(fleet.ObjectOrEmpty(job.View["permissionHold"])) > 0 || strings.Contains(fleet.Text(job.Result["error"]), "権限の範囲を定めるファイルの変更") || jobs.FinalApprovalHeld(job) {
		return false
	}
	pending, err := jobs.PendingFinalApprovals(job)
	if err != nil || len(pending) > 0 || len(stringList(fleet.ObjectOrEmpty(job.Result["review"])["userJudgments"])) > 0 {
		return false
	}
	result := make(map[string]any, len(job.Result))
	for key, value := range job.Result {
		if key != "testTimeout" {
			result[key] = value
		}
	}
	reason, _ := jobs.ClassifyImplementationFailure(result, job.View)
	return reason == "fixed-test"
}

// requiresUserAnswer reports whether the job's hold waits for the user, apart
// from pending final approvals (which the projection checks with its output).
func requiresUserAnswer(job *jobs.Job, status string, runLimit int64) bool {
	if job.HoldReason == nil {
		return false
	}
	hold := *job.HoldReason
	if job.Status == "held" {
		if hold == "question" && status == "waiting" {
			for _, question := range stringList(job.Result["questions"]) {
				if strings.TrimSpace(fleet.Text(question)) != "" {
					return true
				}
			}
			if runLimit > 0 && jobs.EffectiveRuns(job) >= runLimit || len(fleet.ObjectOrEmpty(job.View["permissionHold"])) > 0 || strings.Contains(fleet.Text(job.Result["error"]), "権限の範囲を定めるファイルの変更") {
				return true
			}
			review := fleet.ObjectOrEmpty(job.Result["review"])
			if review["omitted"] != true {
				for _, judgment := range stringList(review["userJudgments"]) {
					if strings.TrimSpace(fleet.Text(judgment)) != "" {
						return true
					}
				}
			}
		}
		if hold == "review_held" && runLimit > 0 && jobs.EffectiveRuns(job) >= runLimit {
			return true
		}
		if hold == "review_held" {
			review := fleet.ObjectOrEmpty(job.Result["review"])
			if review["omitted"] != true {
				for _, value := range stringList(review["userJudgments"]) {
					if strings.TrimSpace(fleet.Text(value)) != "" {
						return true
					}
				}
			}
		}
	}
	if job.Status == "held" || job.Status == "failed" || job.Status == "stopped" {
		if hold == "deletions" || hold == "review_held" && (len(fleet.ObjectOrEmpty(job.View["permissionHold"])) > 0 || strings.Contains(fleet.Text(job.Result["error"]), "権限の範囲を定めるファイルの変更")) {
			return true
		}
	}
	return false
}

// userNextAction is what the user is asked to do: the questions of the job,
// else the judgments of its review, else a generic or over-limit action.
func userNextAction(job *jobs.Job, runLimit int64) string {
	questions := []string{}
	for _, q := range fleet.Items(job.Result["questions"]) {
		if text := strings.TrimSpace(fleet.Text(q)); text != "" {
			questions = append(questions, text)
		}
	}
	if len(questions) == 0 {
		for _, q := range fleet.Items(fleet.ObjectOrEmpty(job.Result["review"])["userJudgments"]) {
			if text := strings.TrimSpace(fleet.Text(q)); text != "" {
				questions = append(questions, text)
			}
		}
	}
	if len(questions) > 0 {
		return strings.Join(questions, " / ")
	}
	if runLimit > 0 && jobs.EffectiveRuns(job) >= runLimit {
		return "実装回数・使用量の上限を超えて続けるか判断する"
	}
	return "依頼の判断対象を確認して回答する"
}

// queuedDependencyLines は待ちの依頼が待っている依存の一覧を、一覧の読み取り結果があればそこから、無ければDBから読む。
func (s *Service) queuedDependencyLines(ctx context.Context, repo string, job *jobs.Job) ([]string, error) {
	lines := []string{}
	if snapshot, ok := ctx.Value(listSnapshotKey{}).(*listSnapshot); ok {
		lines = snapshot.dependencyLines(job.ID, repo)
	} else {
		rows, e := s.o.Postgres.QueryContext(ctx, `SELECT p.repo,p.issue, NOT (`+jobs.DependencyPairReadySQL+`) FROM job_deps d JOIN jobs p ON p.id=d.depends_on JOIN jobs j ON j.id=d.job_id WHERE d.job_id=$1 ORDER BY p.id`, job.ID)
		if e != nil {
			return nil, e
		}

		edges := map[string]bool{}
		for rows.Next() {
			var project string
			var n int64
			var waiting bool
			if e = rows.Scan(&project, &n, &waiting); e != nil {
				rows.Close()
				return nil, e
			}
			edges[fmt.Sprintf("%s#%d", strings.ToLower(project), n)] = true
			if !waiting {
				continue
			}
			prefix := project
			if strings.EqualFold(project, repo) {
				prefix = ""
			}
			lines = append(lines, fmt.Sprintf("%s#%d: PRマージ待ち", prefix, n))
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		present, _ := jobs.DependencyReferences(job.View, func(project string, n int64) (bool, error) {
			return edges[fmt.Sprintf("%s#%d", strings.ToLower(project), n)], nil
		})
		if !present {
			lines = append(lines, "依存の辺が未作成です")
		}
	}
	return lines, nil
}

// jobErrorText は止まった依頼の失敗の理由を、画面に出す長さと伏せ字にして文にする。
func jobErrorText(job *jobs.Job) string {
	errorLimit := 2000
	if strings.HasPrefix(fleet.Text(job.Result["error"]), "変更範囲が目安を超えました（") {
		// Scope-limit messages include up to ten validated paths and actions.
		errorLimit = 4096
	}
	errorText := clipped(usage.Redact(fleet.Text(job.Result["error"])), errorLimit)
	if errorText == "" {
		errorText = clipped(usage.Redact(fleet.Text(job.Result["reason"])), 2000)
	}
	if job.Result["reason"] == "lease_expired" {
		times := job.Result["times"]
		if times == nil {
			times = "?"
		}
		errorText = fmt.Sprintf("実行中のPCからの延長が途絶え、続けて %v 回手放されたため失敗にしました（server の掃除）。", times)
	}
	return errorText
}
