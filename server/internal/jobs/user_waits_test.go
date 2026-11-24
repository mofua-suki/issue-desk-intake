package jobs

import (
	"encoding/json"
	"reflect"
	"testing"
)

func waitJob(reason string) *Job {
	return &Job{Status: "held", HoldReason: &reason, View: map[string]any{}, Result: map[string]any{}}
}
func waitPlan(t *testing.T) any {
	t.Helper()
	var p any
	if err := json.Unmarshal([]byte(finalResumePlan), &p); err != nil {
		t.Fatal(err)
	}
	return p
}
func checkWaits(t *testing.T, job *Job, limit int64, usage bool, want ...UserWaitKind) {
	t.Helper()
	got, err := UserWaits(job, limit, usage)
	if err != nil || len(got) != len(want) || (len(want) > 0 && !reflect.DeepEqual(got, want)) {
		t.Fatalf("got %v, error %v; want %v", got, err, want)
	}
}

func TestUserWaitsKinds(t *testing.T) {
	permission := waitJob("review_held")
	permission.View["permissionHold"] = map[string]any{"files": []any{"a"}}
	deletion := waitJob("deletions")
	deletion.View["deletionApproval"] = map[string]any{"head": "abc"}
	final := waitJob("review_held")
	final.View["completionPlan"] = waitPlan(t)
	runs := waitJob("question")
	runs.Generation = 3
	usage := waitJob("question")
	for _, tc := range []struct {
		name  string
		job   *Job
		usage bool
		want  UserWaitKind
	}{{"permission", permission, false, UserWaitPermission}, {"deletion", deletion, false, UserWaitDeletion}, {"final", final, false, UserWaitFinalApproval}, {"runs", runs, false, UserWaitRunLimit}, {"usage", usage, true, UserWaitUsageLimit}} {
		t.Run(tc.name, func(t *testing.T) { checkWaits(t, tc.job, 4, tc.usage, tc.want) })
	}
}

func TestUserWaitsExcluded(t *testing.T) {
	active := waitJob("question")
	active.Status = "running"
	active.View["completionPlan"] = waitPlan(t)
	checkWaits(t, active, 4, false)
	prerequisite := waitJob("question")
	prerequisite.Generation = 4
	prerequisite.Result = map[string]any{"status": "failed", "prerequisiteMissing": map[string]any{"artifact": "a"}}
	prerequisite.View["prerequisiteMissingGenerations"] = []any{float64(4)}
	stopped := waitJob("question")
	stopped.View["stopRequested"] = true
	failed := waitJob("question")
	failed.Status = "failed"
	for _, tc := range []struct {
		name string
		job  *Job
	}{{"nil", nil}, {"active", active}, {"prerequisite", prerequisite}, {"stopped", stopped}, {"below", waitJob("question")}, {"failed", failed}} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.job != nil {
				tc.job.View["completionPlan"] = "invalid"
			}
			if tc.name == "below" {
				delete(tc.job.View, "completionPlan")
			}
			checkWaits(t, tc.job, 4, false)
		})
	}
}

func TestUserWaitsOrder(t *testing.T) {
	job := waitJob("question")
	job.Generation = 3
	job.View["completionPlan"] = waitPlan(t)
	checkWaits(t, job, 4, true, UserWaitFinalApproval, UserWaitRunLimit, UserWaitUsageLimit)
	job = waitJob("review_held")
	job.View["permissionHold"] = map[string]any{"files": true}
	job.View["completionPlan"] = waitPlan(t)
	checkWaits(t, job, 4, true, UserWaitPermission, UserWaitFinalApproval)
	job = waitJob("deletions")
	job.View["deletionApproval"] = map[string]any{}
	job.View["completionPlan"] = waitPlan(t)
	checkWaits(t, job, 4, true, UserWaitDeletion, UserWaitFinalApproval)
}

func TestUserWaitsBoundaries(t *testing.T) {
	job := waitJob("question")
	job.Generation = 3
	checkWaits(t, job, 4, false, UserWaitRunLimit)
	checkWaits(t, job, 0, false, UserWaitRunLimit)
	checkWaits(t, job, -1, false, UserWaitRunLimit)
	job.Generation = 2
	checkWaits(t, job, 4, false)
	checkWaits(t, job, 0, false)
	job = waitJob("review_held")
	job.View["permissionApproval"] = map[string]any{"files": true}
	checkWaits(t, job, 4, true)
	job.View["permissionHold"] = map[string]any{}
	checkWaits(t, job, 4, true)
}

func TestUserWaitsFinalApprovalError(t *testing.T) {
	job := waitJob("question")
	job.View["completionPlan"] = "invalid"
	_, want := PendingFinalApprovals(job)
	got, err := UserWaits(job, 4, false)
	if want == nil || err == nil || err.Error() != want.Error() || len(got) != 0 {
		t.Fatalf("got %v, %v; expected error %v", got, err, want)
	}
}

func TestUserWaitsDoesNotMutateJob(t *testing.T) {
	for _, prerequisite := range []bool{false, true} {
		job := waitJob("question")
		job.Generation = 4
		job.View["completionPlan"] = waitPlan(t)
		job.Result = map[string]any{"status": "failed", "prerequisiteMissing": map[string]any{"artifact": "a"}}
		if prerequisite {
			job.View["prerequisiteMissingGenerations"] = []any{float64(4)}
		}
		before, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		_, err = UserWaits(job, 4, true)
		if err != nil {
			t.Fatal(err)
		}
		after, err := json.Marshal(job)
		if err != nil || string(before) != string(after) {
			t.Fatalf("job changed: %s -> %s (%v)", before, after, err)
		}
	}
}
