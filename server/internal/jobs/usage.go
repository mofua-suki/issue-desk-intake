package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"math"
	"math/big"
	"regexp"
)

func ImplementationRuns(job *Job) int64 {
	excluded := map[int64]bool{}
	for _, key := range []string{"retestPassedGenerations", "prerequisiteMissingGenerations", "codexNotStartedGenerations"} {
		for _, raw := range fleet.List(job.View[key]) {
			if n, ok := count(raw); ok && n <= job.Generation && (key != "retestPassedGenerations" || n > 0) {
				excluded[n] = true
			}
		}
	}
	resumes, _ := count(job.View["implementationResumes"])
	return job.Generation + 1 - int64(len(excluded)) - resumes
}
func usageRowKey(row map[string]any) string { raw, _ := json.Marshal(row); return string(raw) }
func usageDecision(rows []map[string]any, skipped int, job *Job, limit float64, weights map[string]any) (any, any) {
	unknown := func(reason string, estimate any) (any, any) {
		return "累計使用量が未知です（" + reason + "）。上限を確認できないため自動再実装を停止しました。", estimate
	}
	if skipped != 0 {
		return unknown("使用量データに読めない行があります", nil)
	}
	tally := TallyUsage(rows, skipped, job, weights)
	if tally.Rows == 0 {
		// 初回は使用量の行がなくても正常。2回目以降は行の欠落が疑われる。
		if ImplementationRuns(job) <= 1 {
			return nil, nil
		}
		return unknown("この依頼の使用量の行が届いていません", nil)
	}
	missing, total, resolved := tally.MissingRows, tally.Total, len(tally.UnresolvedSteps) == 0
	parts := []any{}
	for _, step := range tally.Steps {
		if step.Missing == 0 {
			continue
		}
		var estimated any
		if step.Maximum != nil && !math.IsInf(step.Estimated, 0) {
			estimated = step.Estimated
		}
		parts = append(parts, map[string]any{"step": step.Step, "missingRows": step.Missing, "maximum": step.Maximum, "estimated": estimated, "fallbackRows": step.FallbackRows})
	}
	var estimate any
	if missing > 0 {
		var value any
		if resolved && !math.IsInf(total, 0) {
			value = total
		}
		estimate = map[string]any{"missingRows": missing, "steps": parts, "total": value}
	}
	if !resolved && ImplementationRuns(job) <= 1 {
		// 補完できない欠測は残し、補完できた工程を含む合計で上限を判定する。
		if total > limit {
			return fmt.Sprintf("累計使用量（重み付き%v）が上限（重み付き%v）を超えました。自動再実装を停止しました。", total, limit), estimate
		}
		return nil, estimate
	}
	if !resolved || math.IsInf(total, 0) {
		return unknown("欠測を同じ工程の実測最大値で補完できません", estimate)
	}
	if total > limit {
		return fmt.Sprintf("累計使用量（重み付き%v）が上限（重み付き%v）を超えました。自動再実装を停止しました。", total, limit), estimate
	}
	return nil, estimate
}

// UsageStepTally describes missing rows and the measured maximum for one step.
type UsageStepTally struct {
	Step         string
	Missing      int
	Maximum      *float64
	Estimated    float64
	FallbackRows int
}

// UsageTally contains the available weighted usage and missing-data details.
// Estimated contains only replacements that could be calculated. Unreadable
// indicates that the inputs omit rows; Rows counts matching readable rows.
// Total adds replacements to Measured in step order, as the limit decision does;
// it may differ from Measured+Estimated due to floating-point rounding.
// Estimated or Total may be infinite; callers should check math.IsInf.
type UsageTally struct {
	Measured        float64
	Estimated       float64
	Total           float64
	MissingRows     int
	UnresolvedSteps []string
	Unreadable      bool
	// Rows counts readable matching rows; read it with Unreadable because skipped rows are omitted.
	Rows  int
	Steps []UsageStepTally
}

// TallyUsage computes the same per-step maxima and replacements used by the
// automatic usage limit. Rows are filtered by job repository and issue. A
// nonzero skipped count signals unreadable input, so the tally is incomplete.
func TallyUsage(rows []map[string]any, skipped int, job *Job, weights map[string]any) UsageTally {
	steps := map[string]*usageBucket{}
	order := []string{}
	result := UsageTally{Unreadable: skipped != 0}

	for _, row := range rows {
		if row["repo"] != job.Repo || fleet.Number(row["issue"]) != float64(job.Issue) {
			continue
		}
		result.Rows++
		step := tallyStepName(fleet.Text(row["step"]))
		group := steps[step]
		if group == nil {
			group = &usageBucket{}
			steps[step] = group
			order = append(order, step)
		}
		value, ok := usage.Weighted(row, weights)["weighted"].(float64)
		if row["usagePartial"] == true || !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			group.Missing++
			group.missingTimes = append(group.missingTimes, usageElapsed(row))
		} else {
			if elapsed := usageElapsed(row); elapsed > 0 {
				rate := new(big.Rat).Quo(new(big.Rat).SetFloat64(value), new(big.Rat).SetFloat64(elapsed))
				if group.maxRate == nil || rate.Cmp(group.maxRate) > 0 {
					group.maxRate = rate
				}
			} else {
				group.untimedRows++
			}
			result.Measured += value
			if step != "工程不明" && (group.Maximum == nil || value > *group.Maximum) {
				copy := value
				group.Maximum = &copy
			}
		}
	}
	result.Total = result.Measured
	for _, step := range order {
		b := steps[step]
		b.Step = step
		if b.Missing == 0 {
			result.Steps = append(result.Steps, b.UsageStepTally)
			continue
		}
		result.MissingRows += b.Missing
		if b.Maximum == nil {
			result.UnresolvedSteps = append(result.UnresolvedSteps, step)
		} else {
			estimateMissingUsage(b, &result)
		}
		result.Steps = append(result.Steps, b.UsageStepTally)
	}
	return result
}

// usageElapsed accepts the same positive integer range as persisted usage rows.
func usageElapsed(row map[string]any) float64 {
	n, ok := row["elapsedMs"].(float64)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 || n > 9007199254740991 || n != math.Trunc(n) {
		return 0
	}
	return n
}

func (s *Store) CheckAutomaticUsage(ctx context.Context, id int64, host string, epoch int64, local map[string]any, limit float64, weights map[string]any) (any, error) {
	var result any
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		job, err := fenced(ctx, tx, id, host, epoch, s.options.Now(), false)
		if err != nil {
			return err
		}
		admitted, err := consumeUsageAdmission(ctx, tx, job, local)
		if err != nil {
			return err
		}
		if admitted != nil {
			result = map[string]any{"job": admitted, "usageLimitReason": nil}
			return nil
		}
		all, skipped, err := collectUsageRows(ctx, tx, job, local)
		if err != nil {
			return err
		}
		reason, estimate := usageDecision(all, skipped, job, limit, weights)
		if reason != nil {
			if err = addEvent(ctx, tx, id, host, "implementation_usage_limit", map[string]any{"epoch": epoch, "reason": reason, "estimate": estimate}, s.options.Now()); err != nil {
				return err
			}
		} else if estimate != nil {
			if err = addEvent(ctx, tx, id, host, "implementation_usage_estimate", map[string]any{"epoch": epoch, "estimate": estimate}, s.options.Now()); err != nil {
				return err
			}
		}
		result = map[string]any{"job": job, "usageLimitReason": reason}
		return nil
	})
	return result, err
}

// tallyStepName は工程名をそろえ、数える工程に無い名前を「工程不明」にする。
func tallyStepName(raw string) string {
	step := usage.CanonicalStep(raw)
	known := false
	for _, name := range []string{"実装", "固定テストの修正", "競合解消", "独立レビュー", "受付", "固定テスト", usage.ConsultationStep, usage.ProxyStep, "難易度の判定", "優先度の判定", "調査", "実装の注意点", "運用の見張り", usage.FilingStep} {
		if step == name {
			known = true
		}
	}
	if !known {
		step = "工程不明"
	}
	return step
}

// usageBucket は工程ごとの使用量の集計と、欠けた行を見積もるための材料。
type usageBucket struct {
	UsageStepTally
	missingTimes []float64
	maxRate      *big.Rat
	untimedRows  int
}

// estimateMissingUsage は使用量が欠けた行を、同じ工程の最大の使用率×所要時間（最大値を上限）で見積もり、時間の無い行は最大値で数えて、工程と全体の見積もりに足す。
func estimateMissingUsage(b *usageBucket, result *UsageTally) {
	fallbacks := 0
	for _, elapsed := range b.missingTimes {
		if elapsed == 0 || b.untimedRows > 0 || b.maxRate == nil || b.maxRate.Sign() <= 0 || *b.Maximum == 0 {
			fallbacks++
			continue
		}
		replacement := *b.Maximum
		estimate := new(big.Rat).Mul(b.maxRate, new(big.Rat).SetFloat64(elapsed))
		if estimate.Cmp(new(big.Rat).SetFloat64(replacement)) < 0 {
			replacement, _ = estimate.Float64()
			if replacement == 0 && estimate.Sign() > 0 {
				replacement = math.SmallestNonzeroFloat64
			}
		}
		b.Estimated += replacement
		result.Estimated += replacement
		result.Total += replacement
	}
	b.FallbackRows = fallbacks
	if fallbacks > 0 {
		replacement := float64(fallbacks) * *b.Maximum
		b.Estimated += replacement
		result.Estimated += replacement
		result.Total += replacement
	}
}

// consumeUsageAdmission は再実装に付いた使用量の承認を1回だけ使い、使えたら読み直した依頼を返す。使えなければ nil を返す。
func consumeUsageAdmission(ctx context.Context, tx *sql.Tx, job *Job, local map[string]any) (*Job, error) {
	var err error
	expected := fleet.Object(job.View["reimplementExpected"])
	admission := fleet.Text(local["admissionId"])
	generation := fleet.Number(expected["generation"])
	used := fleet.Text(expected["usageAdmissionUsed"])
	if generation == float64(job.Generation) && expected["usageAdmission"] == true && regexp.MustCompile(`^[0-9a-f-]{36}$`).MatchString(admission) && (used == "" || used == admission) && (local["admissionRecall"] != true || used == admission && expected["usageAdmissionRecallUsed"] != true) {
		expected["usageAdmissionUsed"] = admission
		if local["admissionRecall"] == true {
			expected["usageAdmissionRecallUsed"] = true
		}
		if _, err = tx.ExecContext(ctx, "UPDATE jobs SET view=jsonb_set(view,'{reimplementExpected}',$2::jsonb)WHERE id=$1", job.ID, usageRowKey(expected)); err != nil {
			return nil, err
		}
		job, err = readJob(ctx, tx, job.ID, false)
		if err != nil {
			return nil, err
		}
		return job, nil
	}
	return nil, nil
}

// collectUsageRows は保存済みとPCが送った使用量の行を重複なく集め、読めなかった行の数を返す。
func collectUsageRows(ctx context.Context, tx *sql.Tx, job *Job, local map[string]any) ([]map[string]any, int, error) {
	rows, err := tx.QueryContext(ctx, "SELECT json FROM usage_rows WHERE repo=$1 AND issue=$2 ORDER BY at,key", job.Repo, job.Issue)
	if err != nil {
		return nil, 0, err
	}
	saved, skipped, err := appsql.JSONObjects[string](rows, appsql.JSONRowsOptions{SkipInvalid: true})
	if err != nil {
		return nil, 0, err
	}
	all := []map[string]any{}
	keys := map[string]bool{}
	for _, row := range saved {
		// A JSON null row cannot be counted, the same as one that does not decode.
		if row == nil {
			skipped++
			continue
		}
		key := usageRowKey(row)
		if !keys[key] {
			all = append(all, row)
			keys[key] = true
		}
	}
	localRows, ok := local["rows"].([]any)
	if !ok || local["skipped"] != 0.0 {
		skipped++
	}
	for _, raw := range localRows {
		row := fleet.Object(raw)
		if !usage.ValidUsage(row) {
			skipped++
			continue
		}
		key := usageRowKey(row)
		if !keys[key] {
			all = append(all, row)
			keys[key] = true
		}
	}
	return all, skipped, nil
}

func usageSnapshot(ctx context.Context, db transferQuery, job *Job, weights map[string]any) (any, error) {
	rows, err := db.QueryContext(ctx, "SELECT json FROM usage_rows WHERE repo=$1 AND issue=$2 ORDER BY at,key", job.Repo, job.Issue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sum := 0.0
	count := 0
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var row map[string]any
		if json.Unmarshal([]byte(raw), &row) != nil || row["usagePartial"] == true {
			return nil, nil
		}
		n, ok := usage.Weighted(row, weights)["weighted"].(float64)
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, nil
		}
		count++
		sum += n
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if count == 0 || math.IsInf(sum, 0) {
		return nil, nil
	}
	return sum, nil
}
