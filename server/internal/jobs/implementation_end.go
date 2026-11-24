package jobs

import (
	"regexp"
	"strings"
)

// ImplementationEndReasons is the vocabulary shared by the stored terminal
// record (job_events state data.implementationEnd) and the statistics that
// read it. Statistics report one count per entry in this order.
var ImplementationEndReasons = []string{"success", "fixed-test", "checks", "timeout", "question", "prerequisite", "guard", "usage-limit", "review-hold", "stopped", "unknown"}

// implementationEndInputs are the report fields read to classify a failed run.
// A field is missing only when it is absent or JSON null; false, 0 and other
// present values that match no rule are information that was not classified.
var implementationEndInputs = []struct{ section, key string }{
	{"result", "error"},
	{"result", "testTimeout"},
	{"result", "testFailures"},
	{"view", "implementationUsageStop"},
	{"view", "codexDepsUnavailable"},
}

var (
	usageLimitPattern   = regexp.MustCompile(`(?i)利用上限|usage limit|rate limit`)
	fixedTestPattern    = regexp.MustCompile(`固定テスト.{0,30}(?:失敗|不合格)`)
	checksPattern       = regexp.MustCompile(`(?i)(?:型検査|typecheck|lint).{0,30}(?:失敗|不合格|error|failed)`)
	prerequisitePattern = regexp.MustCompile(`前提不足|依存.{0,20}(?:不足|利用できません)|環境.{0,20}(?:不足|利用できません)`)
	guardPattern        = regexp.MustCompile(`(?i)ガード.{0,20}(?:停止|拒否)|guard.{0,20}(?:stop|reject)`)
)

// ClassifyImplementationFailure classifies a failed implementation from its
// result and view in the fixed priority: unknown cumulative usage (guard),
// usage-limit, timeout, fixed-test, checks, prerequisite, other guard.
// usageUnknown is true only for the first rule. It returns "unknown" when no
// rule matches.
func ClassifyImplementationFailure(result, view map[string]any) (reason string, usageUnknown bool) {
	errorText, _ := result["error"].(string)
	switch {
	case strings.Contains(errorText, "累計使用量が未知です"):
		return "guard", true
	case view["implementationUsageStop"] == true || usageLimitPattern.MatchString(errorText):
		return "usage-limit", false
	case completeObject(result["testTimeout"]):
		return "timeout", false
	case completeObject(result["testFailures"]) || fixedTestPattern.MatchString(errorText):
		return "fixed-test", false
	case checksPattern.MatchString(errorText):
		return "checks", false
	case view["codexDepsUnavailable"] == true || prerequisitePattern.MatchString(errorText):
		return "prerequisite", false
	case guardPattern.MatchString(errorText):
		return "guard", false
	}
	return "unknown", false
}

// ImplementationEnd builds the record saved with a terminal state event, or
// nil when the transition does not end an implementation (non-terminal
// states and the scope_wait/deletions holds). The reported state takes
// precedence over stale result/view data: merged is success and stopped is
// stopped. Only an unclassifiable end is unknown; it carries unknownCause
// ("missing" when none of the inputs was reported, otherwise "unclassified")
// and missingFields. The error text itself is not copied.
func ImplementationEnd(status, holdReason string, result, view map[string]any, generation int64) map[string]any {
	reason, usageUnknown := "", false
	switch status {
	case "merged":
		reason = "success"
	case "stopped":
		reason = "stopped"
	case "held":
		switch holdReason {
		case "question":
			reason = "question"
		case "review_held":
			reason = "review-hold"
		case "prerequisite_missing":
			reason = "prerequisite"
		default:
			return nil
		}
	case "failed":
		reason, usageUnknown = ClassifyImplementationFailure(result, view)
	default:
		return nil
	}
	end := map[string]any{"version": 1, "generation": generation, "reason": reason, "usageUnknown": usageUnknown}
	if reason == "unknown" {
		missing := []any{}
		for _, input := range implementationEndInputs {
			source := result
			if input.section == "view" {
				source = view
			}
			if value, present := source[input.key]; !present || value == nil {
				missing = append(missing, input.section+"."+input.key)
			}
		}
		end["unknownCause"] = "unclassified"
		if len(missing) == len(implementationEndInputs) {
			end["unknownCause"] = "missing"
		}
		end["missingFields"] = missing
	}
	return end
}

func completeObject(value any) bool {
	object, ok := value.(map[string]any)
	return ok && object["complete"] == true
}
