package operations

import (
	"context"
	"encoding/json"
	"testing"
)

// Shape copied from issue-desk-worker e9ec5189 internal/providers/provider_test.go
// TestReviewContractRejectsMissingFieldsAndExtraJSON. Findings are populated as
// in internal/execution/review_decision_test.go: approve plus must still holds.
// The producer does not send omitted:false for a complete review.
func workerReviewFixture() map[string]any {
	var review map[string]any
	raw := `{"decision":"approve","summary":"期限切れCookieの検査不足","breaking":false,"tooLarge":false,"findings":["Expired cookies are accepted by the new route"],"findingKinds":["acceptance"],"findingSeverities":["must"],"findingSpecs":[{"group":"acceptance","problem":"expired cookie","expected":"401","test":"TestSessionRejectsExpiredCookie"}],"followups":[],"followupSpecs":[],"improvements":[],"postMergeChecks":[],"userJudgments":[],"resolvableUncertainties":[]}`
	if err := json.Unmarshal([]byte(raw), &review); err != nil {
		panic(err)
	}
	return review
}

func TestRecoveryRepairAcceptsCompleteWorkerReviewWithoutOmittedFlag(t *testing.T) {
	review := workerReviewFixture()
	if _, exists := review["omitted"]; exists || review["decision"] != "approve" || !recoveryRepairReview(review) {
		t.Fatal(review)
	}
	for _, key := range []string{"summary", "breaking", "tooLarge", "findings", "findingKinds", "findingSeverities", "findingSpecs", "userJudgments", "resolvableUncertainties"} {
		review := workerReviewFixture()
		delete(review, key)
		if recoveryRepairReview(review) {
			t.Fatal("missing producer field accepted", key)
		}
	}
	for _, omitted := range []any{true, "false", nil} {
		review := workerReviewFixture()
		review["omitted"] = omitted
		if recoveryRepairReview(review) {
			t.Fatal("unknown or omitted review accepted", omitted)
		}
	}
}

func TestRecoveryReadErrorsDoNotStarveUntriedJobsPostgres(t *testing.T) {
	r := recoveryFixture(t)
	r.Limit = 3
	for n := 1; n <= 4; n++ {
		recoveryInsert(t, r, n, "failed", nil, "normal", "owner/repo", `{}`, `{"error":"specific failing assertion"}`, 0, nil, "1 hour")
	}
	first, err := r.Candidates(context.Background())
	if err != nil || len(first) != 3 {
		t.Fatal(first, err)
	}
	for _, c := range first {
		decision, id, err := r.Reserve(context.Background(), c)
		if err != nil || decision != "proceed" {
			t.Fatal(decision, err)
		}
		if err = r.Finish(context.Background(), c, id, "pre_send_error"); err != nil {
			t.Fatal(err)
		}
	}
	next, err := r.Candidates(context.Background())
	if err != nil || len(next) != 3 || next[0].Job.Issue != 4 {
		t.Fatal(next, err)
	}
}
