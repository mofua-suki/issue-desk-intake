package operations

import (
	"context"
	"encoding/json"
	"testing"

	"issue-desk-intake/server/internal/jobs"
)

func reviewResult(source, field string) map[string]any {
	review := map[string]any{
		"decision": "hold", "findingKinds": []any{"other"}, "findingSeverities": []any{"minor"},
		"breaking": false, "tooLarge": false, "omitted": false, "head": "head1", "model": "model1", "provider": "provider1", "createdAt": "first",
		"findings": []any{"finding"}, "userJudgments": []any{"judgment"}, "resolvableUncertainties": []any{"uncertainty"},
		"findingSpecs": []any{map[string]any{"problem": "problem", "expected": "expected", "test": "test"}},
	}
	switch field {
	case "findings", "userJudgments", "resolvableUncertainties":
		review[field] = []any{"changed"}
	case "problem", "expected", "test":
		review["findingSpecs"].([]any)[0].(map[string]any)[field] = "changed"
	case "metadata":
		review["decision"] = "approve"
		review["findingKinds"] = []any{"regression"}
		review["findingSeverities"] = []any{"major"}
		review["breaking"] = true
		review["tooLarge"] = true
		review["omitted"] = true
		review["head"] = "head2"
		review["model"] = "model2"
		review["provider"] = "provider2"
		review["createdAt"] = "second"
	}
	return map[string]any{"status": "held", "summary": "summary", source: review}
}

func TestRecoveryStoredReviewCause(t *testing.T) {
	hold := "review_held"
	for _, source := range []string{"review", "previousReview"} {
		base := jobs.Job{Status: "held", HoldReason: &hold, Result: reviewResult(source, "")}
		cause := recoveryCause(&base)
		if cause == "" {
			t.Fatal(source, "empty cause")
		}
		for _, field := range []string{"findings", "problem", "expected", "test", "userJudgments", "resolvableUncertainties", "metadata"} {
			j := base
			j.Result = reviewResult(source, field)
			if (recoveryCause(&j) == cause) != (field == "metadata") {
				t.Fatalf("%s %s", source, field)
			}
		}
	}
}

func TestRecoveryStoredReviewCandidatesRecheckPostgres(t *testing.T) {
	for _, source := range []string{"review", "previousReview"} {
		t.Run(source, func(t *testing.T) {
			r := recoveryFixture(t)
			ctx := context.Background()
			encode := func(field string) string {
				b, err := json.Marshal(reviewResult(source, field))
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			recoveryInsert(t, r, 1, "held", "review_held", "normal", "owner/repo", `{"reviewHolds":0}`, encode(""), 0, nil, "1 hour")
			candidates, err := r.Candidates(ctx)
			if err != nil || len(candidates) != 1 {
				t.Fatal(candidates, err)
			}
			original := candidates[0]
			for _, field := range []string{"findings", "problem", "expected", "test", "userJudgments", "resolvableUncertainties", "metadata"} {
				recoveryExec(t, r, `UPDATE jobs SET result=$1::jsonb WHERE issue=1`, encode(field))
				ok, err := r.Recheck(ctx, original)
				if err != nil || ok != (field == "metadata") {
					t.Fatalf("%s: %v %v", field, ok, err)
				}
				next, err := r.Candidates(ctx)
				if err != nil || len(next) != 1 || (next[0].Cause == original.Cause) != (field == "metadata") {
					t.Fatalf("%s: %v %v", field, next, err)
				}
			}
		})
	}
}
