package jobs

import (
	"encoding/json"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"issue-desk-intake/server/internal/contract"
)

var intakeViewKeys = map[string]bool{
	"implementationResumes": true, "publicationResume": true, "implementationRuns": true, "implementationWeightedUsage": true,
	"instructions": true, "priority": true, "retryFromBase": true, "usageLimitRetry": true, "retryRetest": true,
	"reimplementExpected": true, "deletionApproved": true, "liveCheck": true, "continuation": true,
	"permissionApproval": true, "overLimitApprovals": true, "prerequisiteMissingGenerations": true,
	"prerequisiteResolution": true, "acceptanceTransferRecords": true, "acceptanceTransferInput": true,
	"acceptanceTransferChecks": true, "acceptanceTransferWait": true, "dependencyReviews": true, "dependencyMerges": true,
	"finalApprovals": true, "completionPlan": true, "completionApproval": true, "completionPlanEvidence": true,
	"finalApprovalHold": true, "finalApprovalResume": true, "diffFingerprint": true, "finalApprovalTransfers": true,
	"splitIndex": true, "executionPlan": true, "executionPlanSHA": true, "executionReceipts": true, "executionTestRecord": true, "executionProofFingerprint": true,
}

func clientView(view map[string]any) map[string]any {
	result := map[string]any{}
	for key, value := range view {
		if !intakeViewKeys[key] {
			result[key] = value
		}
	}
	return result
}

func count(value any) (int64, bool) {
	var number float64
	switch n := value.(type) {
	case float64:
		number = n
	case int:
		number = float64(n)
	case int64:
		number = float64(n)
	case json.Number:
		var err error
		number, err = n.Float64()
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > float64(maxSafeInteger) || math.Trunc(number) != number {
		return 0, false
	}
	return int64(number), true
}

func reportedHolds(view map[string]any) any {
	if view["reviewHoldsUnknown"] == true {
		return nil
	}
	if value, ok := count(view["reviewHolds"]); ok {
		return value
	}
	return nil
}

func mergeHolds(previous, incoming map[string]any) map[string]any {
	var lower int64
	for _, value := range []any{previous["reviewHolds"], previous["reviewHoldsLowerBound"], incoming["reviewHolds"], incoming["reviewHoldsLowerBound"], reportedHolds(incoming)} {
		if n, ok := count(value); ok && n > lower {
			lower = n
		}
	}
	unknown := previous["reviewHoldsUnknown"] == true || previous != nil && reportedHolds(previous) == nil || reportedHolds(incoming) == nil
	var reported any = lower
	if unknown {
		reported = nil
	}
	return map[string]any{"reviewHolds": reported, "reviewHoldsLowerBound": lower, "reviewHoldsUnknown": unknown}
}

func reportView(current *Job, report Report) (map[string]any, error) {
	view := clientView(report.View)
	missing := report.Result["prerequisiteMissing"]
	if report.HoldReason != nil && *report.HoldReason == "prerequisite_missing" {
		if !validPrerequisite(report) {
			return nil, contract.NewProblem(400, "前提待ちには正しい prerequisiteMissing の結果が必要です。", "")
		}
		previous, _ := current.View["prerequisiteMissingGenerations"].([]any)
		generations := append([]any{}, previous...)
		found := false
		for _, generation := range generations {
			if n, ok := count(generation); ok && n == current.Generation {
				found = true
			}
		}
		if !found {
			generations = append(generations, current.Generation)
		}
		view["prerequisiteMissingGenerations"] = generations
	} else if missing != nil {
		return nil, contract.NewProblem(400, "prerequisiteMissing には held・prerequisite_missing が必要です。", "")
	}
	for key, value := range mergeHolds(current.View, view) {
		view[key] = value
	}
	return view, nil
}

func validPrerequisite(report Report) bool {
	result := report.Result
	missing, ok := result["prerequisiteMissing"].(map[string]any)
	questions, qok := result["questions"].([]any)
	if !ok || result["status"] != "failed" || result["incomplete"] != nil || !qok || len(questions) != 0 || !only(missing, "reason", "artifacts") || !contract.FilledText(missing["reason"], 1000) {
		return false
	}
	artifacts, ok := missing["artifacts"].([]any)
	if !ok || len(artifacts) < 1 || len(artifacts) > 10 {
		return false
	}
	for _, value := range artifacts {
		artifact, ok := value.(map[string]any)
		if !ok || !only(artifact, "artifact", "repo", "issue") || !contract.FilledText(artifact["artifact"], 200) || !contract.FilledText(artifact["repo"], 200) {
			return false
		}
		repo, _ := artifact["repo"].(string)
		issue, ok := count(artifact["issue"])
		if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repo) || !ok || issue < 1 {
			return false
		}
	}
	return true
}

func only(value map[string]any, keys ...string) bool {
	for key := range value {
		if !oneOf(key, keys...) {
			return false
		}
	}
	return true
}

// PrerequisiteMissingHeld reports whether the persisted question hold represents a prerequisite wait.
func PrerequisiteMissingHeld(job *Job) bool {
	copy := *job
	restorePrerequisiteReason(&copy)
	return copy.HoldReason != nil && *copy.HoldReason == "prerequisite_missing"
}

func restorePrerequisiteReason(job *Job) {
	if job.Status != "held" || job.HoldReason == nil || *job.HoldReason != "question" || job.Result["status"] != "failed" || job.Result["prerequisiteMissing"] == nil {
		return
	}
	generations, _ := job.View["prerequisiteMissingGenerations"].([]any)
	for _, value := range generations {
		if n, ok := count(value); ok && n == job.Generation {
			reason := "prerequisite_missing"
			job.HoldReason = &reason
			return
		}
	}
}

func sameReport(job *Job, report Report, view map[string]any) bool {
	if job.Status != report.Status || !same(job.HoldReason, report.HoldReason) || report.Branch.Present && !same(job.Branch, report.Branch.Value) || report.PR.Present && !same(job.PR, report.PR.Value) || report.HeadSHA.Present && !same(job.HeadSHA, report.HeadSHA.Value) {
		return false
	}
	for key, value := range view {
		if oneOf(key, "reviewHolds", "reviewHoldsLowerBound", "reviewHoldsUnknown") {
			_, old := job.View[key]
			_, incoming := report.View[key]
			if !old && !incoming {
				continue
			}
		}
		if !equivalent(job.View[key], value) {
			return false
		}
	}
	return report.Result == nil || equivalent(job.Result, report.Result)
}

func same[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func equivalent(a, b any) bool {
	encode := func(value any) any {
		raw, err := json.Marshal(value)
		if err != nil {
			return struct{}{}
		}
		var parsed any
		if json.Unmarshal(raw, &parsed) != nil {
			return struct{}{}
		}
		return parsed
	}
	return reflect.DeepEqual(encode(a), encode(b))
}

func checkObject(value map[string]any, name string) error {
	if value == nil {
		return nil
	}
	length, ok := jsonUnits(value)
	if !ok || length > 32*1024 {
		return contract.NewProblem(400, name+" は32768文字以内のJSONオブジェクトにしてください。", "")
	}
	return nil
}

func jsonNumber(number int64) string { return strconv.FormatInt(number, 10) }

func jsonUnits(value any) (int, bool) {
	switch v := value.(type) {
	case nil:
		return 4, true
	case bool:
		if v {
			return 4, true
		}
		return 5, true
	case string:
		return stringUnits(v), true
	case json.Number:
		n, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return numberUnits(n)
	case float64:
		return numberUnits(v)
	case int:
		return numberUnits(float64(v))
	case int64:
		return numberUnits(float64(v))
	case map[string]any:
		length := 2
		for key, value := range v {
			n, ok := jsonUnits(value)
			if !ok {
				return 0, false
			}
			length += stringUnits(key) + 1 + n
		}
		if len(v) > 0 {
			length += len(v) - 1
		}
		return length, true
	case []any:
		length := 2
		for _, value := range v {
			n, ok := jsonUnits(value)
			if !ok {
				return 0, false
			}
			length += n
		}
		if len(v) > 0 {
			length += len(v) - 1
		}
		return length, true
	default:
		return 0, false
	}
}

func stringUnits(value string) int {
	length := 2
	for _, r := range value {
		switch r {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			length += 2
		default:
			if r < 0x20 {
				length += 6
			} else if r > 0xffff {
				length += 2
			} else {
				length++
			}
		}
	}
	return length
}

func numberUnits(value float64) (int, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 4, true
	}
	if value == 0 {
		return 1, true
	}
	abs := math.Abs(value)
	if abs >= 1e-6 && abs < 1e21 {
		return len(strconv.FormatFloat(value, 'f', -1, 64)), true
	}
	text := strconv.FormatFloat(value, 'e', -1, 64)
	parts := strings.Split(text, "e")
	exponent, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, false
	}
	sign := ""
	if exponent >= 0 {
		sign = "+"
	}
	return len(parts[0]) + 1 + len(sign) + len(strconv.Itoa(exponent)), true
}

// validateFingerprintTarget prevents stale or incomplete bindings from being used.
func validateFingerprintTarget(job *Job) {
	value, exists := job.View["diffFingerprint"]
	if !exists || value == nil {
		return
	}
	binding, ok := value.(map[string]any)
	target := map[string]any{"jobId": job.ID, "generation": job.Generation, "branch": job.Branch, "pr": job.PR, "headSha": job.HeadSHA}
	for key, expected := range target {
		actual, present := binding[key]
		if !ok || !present || !equivalent(actual, expected) {
			job.View["diffFingerprint"] = nil
			return
		}
	}
}

// bindReportFingerprint keeps the server-owned value tied to the exact target.
func bindReportFingerprint(current *Job, report Report, view map[string]any) {
	branch, pr, head := current.Branch, current.PR, current.HeadSHA
	if report.Branch.Present {
		branch = report.Branch.Value
	}
	if report.PR.Present {
		pr = report.PR.Value
	}
	if report.HeadSHA.Present {
		head = report.HeadSHA.Value
	}
	target := map[string]any{"jobId": current.ID, "generation": current.Generation, "branch": branch, "pr": pr, "headSha": head}
	if report.DiffFingerprint.Present {
		if report.DiffFingerprint.Value == nil {
			view["diffFingerprint"] = nil
			return
		}
		target["value"] = *report.DiffFingerprint.Value
		view["diffFingerprint"] = target
		return
	}
	old, ok := current.View["diffFingerprint"].(map[string]any)
	if !ok {
		if _, exists := current.View["diffFingerprint"]; exists {
			view["diffFingerprint"] = nil
		}
		return
	}
	for _, key := range []string{"jobId", "generation", "branch", "pr", "headSha"} {
		if !equivalent(old[key], target[key]) {
			view["diffFingerprint"] = nil
			return
		}
	}
}
