package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"golang.org/x/text/unicode/norm"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
	"time"
)

var regexpUUID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// MeaningfulApproval is the shared rule for the user's approval statement.
func MeaningfulApproval(statement string) bool {
	return contract.MeaningfulApproval(statement)
}
func appendControlInstruction(job *Job, input ControlInput, at time.Time) {
	entries := fleet.List(job.View["instructions"])
	entry := map[string]any{"id": "command:" + input.ID, "text": input.Text, "by": input.By, "at": iso(at)}
	if input.Context != "" {
		entry["context"] = input.Context
	}
	job.View["instructions"] = append(entries, entry)
}
func markControlContinuation(ctx context.Context, tx *sql.Tx, job *Job, generation int64, at time.Time) {
	handoff := carryArtifact(job, nil)
	delete(job.View, "continuation")
	host := ""
	if job.LeaseHost != nil {
		host = *job.LeaseHost
	}
	if host == "" {
		_ = tx.QueryRowContext(ctx, `SELECT host FROM job_events WHERE job_id=$1 AND kind='leased' ORDER BY seq DESC LIMIT 1`, job.ID).Scan(&host)
	}
	if host != "" {
		handoff["generation"], handoff["host"], handoff["at"], handoff["pending"] = generation, host, iso(at), true
	}
	if len(handoff) > 0 {
		job.View["continuation"] = handoff
	}
}
func RetestEligible(job *Job) bool {
	r := job.Result
	implementation := fleet.Object(r["implementationResult"])
	failures := fleet.Object(r["testFailures"])
	names := fleet.List(failures["names"])
	validNames := failures["complete"] == true && len(names) > 0 && len(names) <= 200
	for _, v := range names {
		validNames = validNames && contract.FilledText(v, 1000)
	}
	timeout := fleet.Object(r["testTimeout"])["complete"] == true
	if job.Status != "failed" || job.Kind == "investigation" || !validNames && !timeout || r["status"] == "question" || implementation["status"] == "question" || len(fleet.List(r["questions"])) > 0 || len(fleet.List(implementation["questions"])) > 0 {
		return false
	}
	for _, value := range []map[string]any{r, implementation} {
		for _, raw := range fleet.List(value["unverified"]) {
			u := fleet.Object(raw)
			if u["blocking"] != false {
				return false
			}
		}
	}
	return true
}

// EffectiveRuns は受付の世代の数え方と、監視が実行で数えた回数の大きい方を返す。
func EffectiveRuns(job *Job) int64 {
	runs := ImplementationRuns(job)
	if started, ok := count(fleet.Object(job.View["workerExecution"])["implementationRuns"]); ok && started > runs {
		return started
	}
	return runs
}

func questionHeld(job *Job) bool {
	return job != nil && job.Status == "held" && job.HoldReason != nil && *job.HoldReason == "question"
}

// RunLimitHeld は監視が実装回数の上限で止めた依頼（held の question）かを返す。
func RunLimitHeld(job *Job, limit int64) bool {
	if limit <= 0 {
		limit = 4
	}
	return questionHeld(job) && EffectiveRuns(job) >= limit
}

func (s *Store) controlRetry(ctx context.Context, tx *sql.Tx, job *Job, input ControlInput, options ControlOptions) error {
	if err := validateRetryInput(job, input); err != nil {
		return err
	}
	retest := input.Retest || input.AutoRetest && RetestEligible(job)
	review := job.Status == "held" && job.HoldReason != nil && *job.HoldReason == "review_held"
	reimplements := job.HoldReason != nil && *job.HoldReason == "prerequisite_missing" || retest || strings.TrimSpace(input.Text+input.Context) != "" || input.FromBase || !(job.View["releasePending"] == true || job.View["publishPending"] == true || review)
	if options.RunLimit <= 0 {
		options.RunLimit = 4
	}
	approval, unknownRetry, err := s.admitRetryLimits(ctx, tx, job, input, options, reimplements, review)
	if err != nil {
		return err
	}
	s.applyRetry(ctx, tx, job, input, reimplements, retest, approval, unknownRetry)
	return nil
}

// validateRetryInput は再試行できる依頼の状態か、追加の指示と再検証の指定が併用できる形かを検査する。
func validateRetryInput(job *Job, input ControlInput) error {
	if !oneOf(job.Status, "failed", "stopped", "held") || job.Kind == "investigation" || job.View["workType"] == "investigation" {
		return controlProblem(409, "再試行できるジョブではありません。")
	}
	if contract.UTF16Length(input.Text) > 8000 {
		return controlProblem(400, "追加の指示は8000文字以内です。")
	}
	if input.Retest && input.AutoRetest || (input.Retest || input.AutoRetest) && input.FromBase {
		return controlProblem(400, "再検証の指定を併用できません。")
	}
	if input.Retest && !RetestEligible(job) {
		return controlProblem(409, "質問・未確認があるか、固定テストだけの失敗証拠が不足しています。")
	}
	return nil
}

// admitRetryLimits は実装回数と使用量の上限を照合し、上限超過の承認・期限付き許可で通すときはその承認と、累計使用量が未知のまま通すかを返す。
func (s *Store) admitRetryLimits(ctx context.Context, tx *sql.Tx, job *Job, input ControlInput, options ControlOptions, reimplements, review bool) (map[string]any, bool, error) {
	runs := ImplementationRuns(job)
	approval := map[string]any(nil)
	usageReason := ""
	if reimplements && input.By != "issue-screen" {
		usageReason = s.controlUsage(ctx, tx, job, options)
	}
	if input.OverLimitStatement != "" {
		var e error
		approval, e = s.overLimitApproval(job, input, options, reimplements, review, runs, usageReason)
		if e != nil {
			return nil, false, e
		}
	}
	if input.ImplementationLimitRetry {
		if !strings.HasPrefix(input.By, "token:") || strings.TrimSpace(input.Text) == "" || input.FromBase || input.AutoRetest || input.OverLimitStatement != "" || job.Status != "failed" && !review {
			return nil, false, controlProblem(400, "期限付き上限許可の条件を満たしていません。")
		}
		if reimplements && runs >= options.RunLimit {
			if knownUsageOverage(usageReason) {
				return nil, false, controlProblem(409, usageReason)
			}
			var e error
			approval, e = s.controlGrant(ctx, tx, job, input, options.RunLimit)
			if e != nil {
				return nil, false, e
			}
		}
	}
	unknownRetry := strings.HasPrefix(usageReason, "累計使用量が未知です") && strings.HasPrefix(input.By, "token:") && strings.TrimSpace(input.Text) != ""
	if usageReason != "" && !unknownRetry && approval == nil {
		return nil, false, controlProblem(409, usageReason)
	}
	if reimplements && runs >= options.RunLimit && approval == nil && input.By != "issue-screen" {
		return nil, false, controlProblem(409, fmt.Sprintf("実装の回数の上限（%d回）に達しました。", options.RunLimit))
	}
	return approval, unknownRetry, nil
}

// overLimitApproval は上限超過の承認発言が対象の依頼への未使用の発言かを検査し、上限で止まっていれば記録する承認を返す。上限に掛かっていなければ nil を返す。
func (s *Store) overLimitApproval(job *Job, input ControlInput, options ControlOptions, reimplements, review bool, runs int64, usageReason string) (map[string]any, error) {
	approval := map[string]any(nil)
	if !strings.HasPrefix(input.By, "token:") || input.FromBase || input.AutoRetest || !MeaningfulApproval(input.OverLimitStatement) {
		return nil, controlProblem(400, "上限超過の具体的な承認発言を運用代行トークンから送ってください。")
	}
	numbers := regexp.MustCompile(`[0-9]+`).FindAllString(norm.NFKC.String(input.OverLimitStatement), -1)
	target := false
	for _, n := range numbers {
		target = target || n == fmt.Sprint(job.Issue) || input.DeskNumber > 0 && n == fmt.Sprint(input.DeskNumber)
	}
	if !target {
		return nil, controlProblem(400, "承認発言に対象番号がありません。")
	}
	normalize := func(v string) string { return strings.ToLower(strings.Join(strings.Fields(norm.NFKC.String(v)), "")) }
	for _, v := range fleet.List(job.View["overLimitApprovals"]) {
		if normalize(fleet.Text(fleet.Object(v)["statement"])) == normalize(input.OverLimitStatement) {
			return nil, controlProblem(409, "同じ承認発言は使用済みです。")
		}
	}
	limitHeld := RunLimitHeld(job, options.RunLimit)
	usageHeld := usageLimitHeld(job, usageReason)
	if reimplements && (runs >= options.RunLimit || usageReason != "" || limitHeld) {
		if job.Status != "failed" && !review && !limitHeld && !usageHeld {
			return nil, controlProblem(409, "上限超過承認を適用できる状態ではありません。")
		}
		approval = map[string]any{"statement": input.OverLimitStatement, "instructionAt": input.OverLimitInstructionAt, "at": iso(s.options.Now()), "sourcePc": strings.TrimPrefix(input.By, "token:"), "id": input.ID, "implementationRuns": EffectiveRuns(job), "limit": options.RunLimit}
		if knownUsageOverage(usageReason) {
			approval["kind"] = "usage"
			if runs >= options.RunLimit || limitHeld {
				approval["kind"] = "run_and_usage"
			}
			approval["usageLimit"] = options.UsageLimit
			approval["usageReason"] = usageReason
		}
	}
	return approval, nil
}

// applyRetry は受け付けた再試行を依頼の状態へ反映し、次の世代として待ちへ戻す。
func (s *Store) applyRetry(ctx context.Context, tx *sql.Tx, job *Job, input ControlInput, reimplements, retest bool, approval map[string]any, unknownRetry bool) {
	generation := job.Generation + 1
	expected := map[string]any{"generation": generation, "reimplements": reimplements}
	if input.By == "issue-screen" || approval != nil || unknownRetry {
		expected["usageAdmission"] = true
	}
	job.View["reimplementExpected"] = expected
	by := any(nil)
	if job.Status == "failed" {
		by = input.By
	}
	job.View["usageLimitRetry"] = map[string]any{"generation": generation, "by": by}
	if !reimplements {
		resumes, _ := count(job.View["implementationResumes"])
		job.View["implementationResumes"] = resumes + 1
	}
	if approval != nil {
		job.View["overLimitApprovals"] = append(fleet.List(job.View["overLimitApprovals"]), approval)
	}
	if strings.TrimSpace(input.Text+input.Context) != "" {
		appendControlInstruction(job, input, s.options.Now())
	}
	if input.FromBase {
		job.View["retryFromBase"] = map[string]any{"generation": generation, "by": input.By, "at": iso(s.options.Now())}
	}
	if retest {
		job.View["retryRetest"] = map[string]any{"generation": generation, "by": input.By, "at": iso(s.options.Now())}
	}
	markControlContinuation(ctx, tx, job, generation, s.options.Now())
	if input.FromBase || retest {
		// Retesting drops the dispatch hint, not ownership evidence (including
		// the invalid-artifact tombstone). Only FromBase explicitly resets it.
		handoff := carryArtifact(job, nil)
		delete(job.View, "continuation")
		if retest && len(handoff) > 0 {
			job.View["continuation"] = handoff
		}
	}
	job.Generation = generation
	job.Status = "queued"
	job.HoldReason = nil
}

// RetryPrerequisiteTx rechecks the selected generation under lock and uses the
// normal retry admission rules. The caller commits the resolution record together
// with the transition, or rolls the transaction back.
func (s *Store) RetryPrerequisiteTx(ctx context.Context, tx *sql.Tx, id, generation int64, options ControlOptions, fromBase ...bool) error {
	job, err := readJob(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if job.Generation != generation || job.Status != "held" || !PrerequisiteMissingHeld(job) {
		return contract.NewProblem(409, "前提待ちの状態または世代が変わりました。", "prerequisite_changed")
	}
	if job.View["stopRequested"] == true || fleet.Object(job.View["liveCheck"])["status"] == "pending" || job.View["permissionHold"] != nil || job.View["deletionApproval"] != nil {
		return controlProblem(409, "停止または実地確認待ちの依頼は自動で再試行できません。")
	}
	if job.LeaseHost != nil {
		var stopped bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_commands WHERE target_host=$1 AND repo=$2 AND issue=$3 AND kind='stop' AND status IN('queued','acked'))`, *job.LeaseHost, job.Repo, job.Issue).Scan(&stopped); err != nil {
			return err
		}
		if stopped {
			return controlProblem(409, "停止の指示がある依頼は自動で操作できません。")
		}
	}
	input := ControlInput{Action: "retry", By: "prerequisite-resolver", Automatic: true}
	if len(fromBase) > 0 && fromBase[0] {
		input.FromBase = true
		input.Text = "前回の作業枝の成果が作業ツリーに無いため、基点から実装を続けてください。"
	}
	if err = s.controlRetry(ctx, tx, job, input, options); err != nil {
		return err
	}
	raw, err := json.Marshal(job.View)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE jobs SET status=$2,hold_reason=$3,generation=$4,view=$5::jsonb,updated_at=$6 WHERE id=$1`, job.ID, job.Status, job.HoldReason, job.Generation, string(raw), s.options.Now()); err != nil {
		return err
	}
	return addEvent(ctx, tx, job.ID, input.By, "control", map[string]any{"action": "retry", "from": "held", "by": input.By, "generation": job.Generation, "instruction": strings.TrimSpace(input.Text+input.Context) != ""}, s.options.Now())
}

func knownUsageOverage(reason string) bool {
	return reason != "" && !strings.HasPrefix(reason, "累計使用量が未知です")
}

// usageLimitHeld distinguishes an actual overage from an unknown usage total.
func usageLimitHeld(job *Job, reason string) bool {
	return questionHeld(job) && knownUsageOverage(reason)
}

// UsageLimitHeld reports whether a question is held by the configured usage limit.
// Missing or unreadable usage information is not considered an overage.
func (s *Store) UsageLimitHeld(ctx context.Context, job *Job, options ControlOptions) bool {
	if s == nil || s.db == nil || job == nil || options.UsageLimit <= 0 {
		return false
	}
	return usageLimitHeld(job, s.controlUsage(ctx, s.db, job, options))
}

type usageQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Store) controlUsage(ctx context.Context, tx usageQuerier, job *Job, options ControlOptions) string {
	if options.UsageLimit <= 0 {
		return "累計使用量が未知です（上限の設定を確認できません）。"
	}
	rows, e := tx.QueryContext(ctx, `SELECT json FROM usage_rows WHERE repo=$1 AND issue=$2 ORDER BY at,key`, job.Repo, job.Issue)
	if e != nil {
		return "累計使用量が未知です（記録を読めません）。"
	}
	defer rows.Close()
	all := []map[string]any{}
	skipped := 0
	for rows.Next() {
		var raw string
		if rows.Scan(&raw) != nil {
			skipped++
			continue
		}
		r := map[string]any{}
		if json.Unmarshal([]byte(raw), &r) != nil {
			skipped++
			continue
		}
		all = append(all, r)
	}
	if rows.Err() != nil {
		skipped++
	}
	reason, _ := usageDecision(all, skipped, job, options.UsageLimit, options.UsageWeights)
	return fleet.Text(reason)
}
func (s *Store) controlGrant(ctx context.Context, tx *sql.Tx, job *Job, input ControlInput, limit int64) (map[string]any, error) {
	rows, e := tx.QueryContext(ctx, `SELECT id,evidence_quote,expires_at FROM permission_grants WHERE kind='implementation-limit-retry' AND lower(target)=lower($1) AND revoked_at IS NULL FOR UPDATE`, job.Repo)
	if e != nil {
		return nil, e
	}
	var grantID, quote string
	for rows.Next() {
		var id, q string
		var expires sql.NullTime
		if e = rows.Scan(&id, &q, &expires); e != nil {
			rows.Close()
			return nil, e
		}
		if grantID == "" && expires.Valid && expires.Time.After(s.options.Now()) {
			grantID, quote = id, q
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	if grantID == "" {
		return nil, controlProblem(409, "有効な期限付き上限許可がありません。")
	}
	pass := 0
	for _, v := range fleet.List(job.View["overLimitApprovals"]) {
		if fleet.Object(v)["grantId"] != nil {
			pass++
		}
	}
	if pass >= 2 {
		return nil, controlProblem(409, "期限付き上限許可の累計2回に達しています。")
	}
	return map[string]any{"grantId": grantID, "grantQuote": quote, "statement": strings.TrimSpace(input.Text), "at": iso(s.options.Now()), "sourcePc": strings.TrimPrefix(input.By, "token:"), "id": input.ID, "implementationRuns": ImplementationRuns(job), "limit": limit, "pass": pass + 1}, nil
}
