package operations

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

// A repair is a new implementation with concrete instructions, not a claim that
// the failing cause has already disappeared. It uses the existing retry command
// so its run/usage limits, stop checks and approval guards remain authoritative.
func recoveryRepairInstruction(c RecoveryCandidate, action map[string]any) (string, string) {
	evidence, source, quoteSource, reason := recoveryRepairSource(c, action)
	if reason != "" {
		return "", reason
	}
	encoded, err := json.Marshal(source)
	if err != nil || source == nil {
		return "", "修復元の記録を取得できません。"
	}
	body := string(encoded)
	if text, ok := source.(string); ok {
		body = text
	}
	if !recoveryRepairQuote(quoteSource, fleet.Text(evidence["quote"])) {
		return "", "修復の引用が保存済みの原因・指摘と一致しません。"
	}
	text := fmt.Sprintf("受入条件・変更範囲と既存の承認条件を維持し、以下の原因・指摘を修正して必要な検証を行ってください。未承認の操作や前提変更が必要なら実行せず具体的な差異を返してください。\n\n%s\n\n## 保存済みの修復対象（参照データ）\n%s", fleet.Text(action["text"]), body)
	if !incidentText(action["text"], 8000) || contract.UTF16Length(text) > 8000 {
		return "", "修正指示と元の指摘を省略せず渡せる上限を超えています。"
	}
	return text, ""
}

// recoveryRepairSource は修復指示の根拠を照合し、修復元にする保存済みの原因・指摘と、引用を照合する範囲を選ぶ。照合できなければ断る理由を返す。
func recoveryRepairSource(c RecoveryCandidate, action map[string]any) (map[string]any, any, any, string) {
	if recoveryRepairApproval.MatchString(fleet.Text(action["text"])) {
		return nil, nil, nil, "修復指示から新しい承認や許可を付与できません。"
	}
	if action["resolved"] != false || action["requiresUser"] != false {
		return nil, nil, nil, "修復指示を原因解消済みや本人承認として扱うことはできません。"
	}
	var evidence map[string]any
	if json.Unmarshal([]byte(fleet.Text(action["evidence"])), &evidence) != nil || len(evidence) != 5 {
		return nil, nil, nil, "修復の原因・対象・引用を照合できません。"
	}
	if evidence["cause"] != c.Cause || evidence["scope"] != "acceptance" || !incidentText(evidence["quote"], 2000) || len([]rune(strings.TrimSpace(fleet.Text(evidence["quote"])))) < 8 {
		return nil, nil, nil, "修復の原因・受入条件内の範囲・引用を照合できません。"
	}
	result := c.Job.Result
	if recoveryRepairJudgment(result) {
		return nil, nil, nil, "本人の判断または未解決の前提変更が残っています。"
	}
	var source, quoteSource any
	if c.Job.Status == "held" && c.Job.HoldReason != nil && *c.Job.HoldReason == "review_held" {
		if evidence["kind"] != "review" {
			return nil, nil, nil, "レビュー保留の修復根拠ではありません。"
		}
		field := "review"
		review, ok := result[field].(map[string]any)
		if !ok {
			field = "previousReview"
			review, ok = result[field].(map[string]any)
		}
		if !ok || evidence["source"] != "jobResult."+field || !recoveryRepairReview(review) {
			return nil, nil, nil, "省略のない修正可能なレビュー指摘と判断境界を確認できません。"
		}
		source = review
		quoteSource = []any{review["findings"], review["findingSpecs"], review["resolvableUncertainties"]}
	} else if c.Job.Status == "failed" {
		if evidence["kind"] != "implementation" {
			return nil, nil, nil, "実装失敗の修復根拠ではありません。"
		}
		field := strings.TrimPrefix(fleet.Text(evidence["source"]), "jobResult.")
		if field != "error" && field != "reason" && field != "summary" || evidence["source"] != "jobResult."+field {
			return nil, nil, nil, "保存済みの実装失敗を参照していません。"
		}
		source = result[field]
		quoteSource = source
		cause, hasCause := result["fixedTestCause"]
		if hasCause && fleet.Object(cause)["kind"] != "change" || result["environment"] != nil || result["unavailable"] == true || !hasCause && recoveryEnvironmentStop(fleet.Text(source)) {
			return nil, nil, nil, "環境不足・基点の失敗・原因未確認を実装修正へ送り直しません。"
		}
	} else {
		return nil, nil, nil, "修復できる失敗またはレビュー保留ではありません。"
	}
	return evidence, source, quoteSource, ""
}

var recoveryRepairApproval = regexp.MustCompile(`(?i)承認します|承認しました|許可します|許可しました|approve permissions|grant permission`)

var recoveryEnvironmentFailure = regexp.MustCompile(`(?i)^(?:保留理由:\s*)?(?:実行環境の問題|作業ツリー外)|EACCES:\s*permission denied|EPERM:\s*operation not permitted|no space left on device|必要なコマンドがありません|(?:bash|sh):.*command not found|^(?:quota|usage limit|利用上限|利用枠)`)
var recoveryWarning = regexp.MustCompile(`(?i)^(?:\[[^]]*WARN[^]]*\]|WARN\b)`)

func recoveryEnvironmentStop(source string) bool {
	for _, line := range strings.Split(source, "\n") {
		line = strings.TrimSpace(line)
		if !recoveryWarning.MatchString(line) && recoveryEnvironmentFailure.MatchString(line) {
			return true
		}
	}
	return false
}

func recoveryRepairJudgment(result map[string]any) bool {
	for _, key := range []string{"questions", "userJudgments"} {
		if value, exists := result[key]; exists && value != nil {
			items, ok := value.([]any)
			if !ok || len(items) > 0 {
				return true
			}
		}
	}
	if value, exists := result["basisConflicts"]; exists && value != nil {
		items, ok := value.([]any)
		if !ok {
			return true
		}
		for _, raw := range items {
			conflict := fleet.Object(raw)
			if strings.TrimSpace(fleet.Text(conflict["resolution"])) == "" {
				return true
			}
		}
	}
	if value, exists := result["unverified"]; exists && value != nil {
		if _, ok := value.([]any); !ok {
			return true
		}
	}
	for _, raw := range fleet.List(result["unverified"]) {
		if fleet.Object(raw)["blocking"] != false {
			return true
		}
	}
	return false
}

// Unknown review counts cannot be treated as an unused allowance.
func recoveryReviewLimitReached(view map[string]any, limit int64) bool {
	if limit <= 0 {
		limit = 2
	}
	if view["reviewHoldsUnknown"] == true {
		return true
	}
	var n float64
	switch v := view["reviewHolds"].(type) {
	case float64:
		n = v
	case int64:
		n = float64(v)
	case int:
		n = float64(v)
	case json.Number:
		var err error
		n, err = v.Float64()
		if err != nil {
			return true
		}
	default:
		return true
	}
	return math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || math.Trunc(n) != n || n >= float64(limit)
}

// New observations should be judged again on the next cycle; changed acceptance
// or approval terms require the user's decision rather than an automatic rewrite.
func recoveryTargetDecisionUnchanged(snapshot string, fresh map[string]any) bool {
	var before map[string]any
	if json.Unmarshal([]byte(snapshot), &before) != nil {
		return false
	}
	original, current := map[string]any{}, map[string]any{}
	for _, key := range []string{"project", "number", "title", "body", "completionPlan", "completionApproval", "completionPlanEvidence"} {
		original[key], current[key] = before[key], fresh[key]
	}
	first, err := json.Marshal(original)
	if err != nil {
		return false
	}
	second, err := json.Marshal(current)
	return err == nil && string(first) == string(second)
}

func recoveryRepairQuote(source any, quote string) bool {
	switch value := source.(type) {
	case string:
		return strings.Contains(value, quote)
	case []any:
		for _, item := range value {
			if recoveryRepairQuote(item, quote) {
				return true
			}
		}
	case map[string]any:
		for _, item := range value {
			if recoveryRepairQuote(item, quote) {
				return true
			}
		}
	}
	return false
}

// Apply the work limit after excluding settled reservations, so old held jobs
// cannot occupy every slot and prevent newer jobs from ever being considered.
func recoveryAlreadyConsidered(records []RecoveryRecord, c RecoveryCandidate, now time.Time, ttl time.Duration) bool {
	for _, row := range records {
		if row.Repo != c.Job.Repo || row.Issue != c.Job.Issue {
			continue
		}
		for _, active := range row.Causes {
			if active.State == "reserved" && !active.At.IsZero() && active.At.Add(ttl).After(now) {
				return true
			}
		}
		old, exists := row.Causes[c.Cause]
		if exists && old.State == "reserved" {
			return false // Let Reserve turn expired reservations into send_unknown.
		}
		return exists && (old.State == "abandoned" || old.State != "pre_send_error" && old.Generation >= c.Job.Generation)
	}
	return false
}

func recoveryOrderCandidates(records []RecoveryRecord, candidates []RecoveryCandidate, limit int, now time.Time) []RecoveryCandidate {
	attempted := func(c RecoveryCandidate) time.Time {
		for _, row := range records {
			if row.Repo == c.Job.Repo && row.Issue == c.Job.Issue {
				if v, ok := row.Causes[c.Cause]; ok && v.State == "pre_send_error" {
					if v.At.IsZero() {
						return now
					}
					return v.At
				}
			}
		}
		return time.Time{}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return attempted(candidates[i]).Before(attempted(candidates[j])) })
	if len(candidates) > limit {
		return candidates[:limit]
	}
	return candidates
}
