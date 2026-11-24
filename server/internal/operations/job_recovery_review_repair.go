package operations

import "issue-desk-intake/server/internal/fleet"

// The shipped Worker omits the "omitted" field for complete reviews and sets it
// to true only when report size limits replace the original review with a stub.
func recoveryRepairReview(review map[string]any) bool {
	if raw, exists := review["omitted"]; exists && raw != false {
		return false
	}
	if review["decision"] != "hold" && review["decision"] != "approve" || review["breaking"] != false || review["tooLarge"] != false || recoveryRepairJudgment(review) {
		return false
	}
	if _, ok := review["summary"].(string); !ok {
		return false
	}
	for _, key := range []string{"findings", "findingKinds", "findingSeverities", "userJudgments", "resolvableUncertainties"} {
		values, ok := review[key].([]any)
		if !ok {
			return false
		}
		for _, value := range values {
			if _, ok := value.(string); !ok {
				return false
			}
		}
	}
	findings, kinds, severities := fleet.List(review["findings"]), fleet.List(review["findingKinds"]), fleet.List(review["findingSeverities"])
	if len(findings) == 0 || len(kinds) != len(findings) || len(severities) != len(findings) {
		return false
	}
	for i := range findings {
		if severities[i] != "must" && severities[i] != "minor" {
			return false
		}
		switch kinds[i] {
		case "data", "security", "regression", "acceptance", "blocking", "other":
		default:
			return false
		}
	}
	specs, ok := review["findingSpecs"].([]any)
	if !ok {
		return false
	}
	for _, raw := range specs {
		spec, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		for _, key := range []string{"problem", "expected", "test"} {
			if _, ok := spec[key].(string); !ok {
				return false
			}
		}
	}
	return true
}
