package instructionacceptance

import (
	"issue-desk-intake/server/internal/jobs"
	"testing"
)

func TestClosedUnknownRunAndReviewBoundaries(t *testing.T) {
	held := "review_held"
	cases := []struct {
		name, state string
		job         *jobs.Job
		readable    bool
		want        string
	}{{"closed", "closed", nil, true, "closed"}, {"unreadable", "open", nil, false, "unknown"}, {"missing-review", "open", &jobs.Job{Generation: 0, View: map[string]any{}}, true, "unknown"}, {"run-limit", "open", &jobs.Job{Generation: 3, ReviewHolds: 0, View: map[string]any{}}, true, "run_limit"}, {"review-limit", "open", &jobs.Job{Status: "held", HoldReason: &held, Generation: 0, ReviewHolds: 2, View: map[string]any{}}, true, "review_round_limit"}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Check(c.state, "", c.job, nil, nil, c.readable)
			if got["acceptsInstructions"] != false {
				t.Fatal(got)
			}
			found := false
			for _, v := range got["instructionBlocks"].([]any) {
				found = found || v.(Record)["reason"] == c.want
			}
			if !found {
				t.Fatal(got)
			}
		})
	}
}
func TestNoJobKnownOpenAcceptsWithoutInventingUnknown(t *testing.T) {
	got := Check("open", "", nil, nil, nil, true)
	if got["acceptsInstructions"] != true {
		t.Fatal(got)
	}
}
