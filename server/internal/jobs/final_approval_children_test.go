package jobs

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/completionplan"
)

func splitFinalApprovalPlan(t *testing.T) completionplan.Plan {
	t.Helper()
	var saved any
	if err := json.Unmarshal([]byte(finalResumePlan), &saved); err != nil {
		t.Fatal(err)
	}
	plan, err := completionplan.Parse(saved)
	if err != nil {
		t.Fatal(err)
	}
	plan.Approvals[0].ID = "X"
	plan.Approvals[0].Children = []int{2}
	return plan
}

func TestPendingFinalApprovalsSplitChildren(t *testing.T) {
	for _, tc := range []struct {
		name     string
		index    any
		children []int
		want     bool
	}{
		{"other-child", 1, []int{2}, false},
		{"target-child", 2, []int{2}, true},
		{"multiple-children", 1, []int{1, 2}, true},
		{"unrestricted-first", 1, nil, true},
		{"unrestricted-second", 2, nil, true},
		{"missing", nil, []int{2}, true},
		{"zero", 0, []int{2}, true},
		{"negative", -1, []int{2}, true},
		{"string", "1", []int{2}, true},
		{"fraction", 1.5, []int{2}, true},
		{"boolean", true, []int{2}, true},
		{"unsafe-integer", float64(maxSafeInteger) + 1, []int{2}, true},
		{"nan", math.NaN(), []int{2}, true},
		{"json-integer", json.Number("1"), []int{2}, false},
		{"json-fraction", json.Number("1.5"), []int{2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := splitFinalApprovalPlan(t)
			plan.Approvals[0].Children = tc.children
			job := &Job{View: map[string]any{"completionPlan": plan.Record()}}
			if tc.index != nil {
				job.View["splitIndex"] = tc.index
			}
			pending, err := PendingFinalApprovals(job)
			if err != nil || len(pending) > 1 || (len(pending) == 1) != tc.want || len(pending) == 1 && pending[0].ID != "X" {
				t.Fatalf("pending=%+v err=%v", pending, err)
			}
		})
	}

	t.Run("invalid-plan-is-not-filtered", func(t *testing.T) {
		plan := splitFinalApprovalPlan(t)
		plan.Approvals[0].Children = []int{0}
		if pending, err := PendingFinalApprovals(&Job{View: map[string]any{"completionPlan": plan.Record(), "splitIndex": 1}}); err == nil {
			t.Fatalf("invalid children silently filtered: %+v", pending)
		}
	})

	t.Run("held-approval-is-retained", func(t *testing.T) {
		plan := splitFinalApprovalPlan(t)
		other := plan.Approvals[0]
		other.ID = "other"
		plan.Approvals = append(plan.Approvals, other)
		head, reason := strings.Repeat("a", 40), "review_held"
		job := &Job{Status: "held", HoldReason: &reason, HeadSHA: &head, Generation: 2, LeaseEpoch: 7, View: map[string]any{
			"completionPlan": plan.Record(), "splitIndex": 1,
			FinalApprovalHoldKey: map[string]any{"headSha": head, "generation": 2, "epoch": 7, "approvals": []any{"X"}},
		}}
		pending, err := PendingFinalApprovals(job)
		if err != nil || len(pending) != 1 || pending[0].ID != "X" {
			t.Fatalf("hold was discarded or extended to unrelated approvals: %+v %v", pending, err)
		}
		job.Generation++
		pending, err = PendingFinalApprovals(job)
		if err != nil || len(pending) != 0 {
			t.Fatalf("stale hold applied: %+v %v", pending, err)
		}
	})
}

func TestAcquireTurnFinalApprovalSplitChildren(t *testing.T) {
	for _, index := range []int{1, 2} {
		t.Run(fmt.Sprintf("child-%d", index), func(t *testing.T) {
			db, ctx := testPostgres(t)
			id := seedJob(t, ctx, db, "owner/repo", "nas", int64(index), 7)
			s, err := New(db, Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			plan := splitFinalApprovalPlan(t)
			raw, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_build_object('completionPlan',$2::jsonb,'splitIndex',$3::int) WHERE id=$1`, id, string(raw), index); err != nil {
				t.Fatal(err)
			}
			head := strings.Repeat("a", 40)
			publishFinalApprovalFixture(t, ctx, s, id, "nas", head)
			turn, err := s.AcquireTurn(ctx, "owner/repo", TurnRequest{Host: "nas", JobID: &id, Epoch: 7})
			if err != nil || turn.Granted != (index == 1) {
				t.Fatalf("turn=%+v err=%v", turn, err)
			}
			job, err := s.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if job.View["splitIndex"] != float64(index) || !reflect.DeepEqual(job.View["completionPlan"], plan.Record()) {
				t.Fatalf("saved child target or parent plan changed: %+v", job.View)
			}
			pending, err := PendingFinalApprovals(job)
			if err != nil {
				t.Fatal(err)
			}
			if index == 1 {
				if job.Status != "running" || job.HoldReason != nil || FinalApprovalHeld(job) || len(pending) != 0 || job.View["finalApprovals"] != nil {
					t.Fatalf("unrelated approval blocked child: %+v", job)
				}
				return
			}
			if !FinalApprovalHeld(job) || len(pending) != 1 || pending[0].ID != "X" {
				t.Fatalf("target child not held: %+v pending=%+v", job, pending)
			}
			approved, err := s.RecordFinalApproval(ctx, id, FinalApprovalRecord{ID: "X", HeadSHA: head, Fingerprint: pending[0].Fingerprint, MessageID: "00000000-0000-4000-8000-000000000001", Quote: "子2のこの差分と配置を承認します", Via: "token", PC: "nas", InstructionAt: now.Format(time.RFC3339Nano)})
			if err != nil || approved.Status != "queued" || approved.HoldReason != nil || FinalApprovalHeld(approved) {
				t.Fatalf("approval did not resume child: %+v %v", approved, err)
			}
			reread, err := s.Get(ctx, id)
			if err != nil || reread.Status != "queued" {
				t.Fatalf("resume not persisted: %+v %v", reread, err)
			}
			pending, err = PendingFinalApprovals(reread)
			records, _ := reread.View["finalApprovals"].([]any)
			if err != nil || len(pending) != 0 || len(records) != 1 {
				t.Fatalf("approval not persisted: %+v %+v %v", pending, records, err)
			}
			record := records[0].(map[string]any)
			if record["via"] != "token" || record["pc"] != "nas" || record["quote"] != "子2のこの差分と配置を承認します" {
				t.Fatalf("chat approval provenance lost: %+v", record)
			}
		})
	}
}

func TestFinalApprovalSplitIndexCannotBeReportedByWorker(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprintf("known=%v", known), func(t *testing.T) {
			db, ctx := testPostgres(t)
			id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
			s, err := New(db, Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			view := map[string]any{"completionPlan": splitFinalApprovalPlan(t).Record()}
			if known {
				view["splitIndex"] = 2
			}
			raw, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.ExecContext(ctx, `UPDATE jobs SET view=$2::jsonb WHERE id=$1`, id, string(raw)); err != nil {
				t.Fatal(err)
			}
			publishFinalApprovalFixture(t, ctx, s, id, "nas", strings.Repeat("a", 40))
			if _, _, err = s.Renew(ctx, id, "nas", 7, map[string]any{"splitIndex": 1}); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "running", View: map[string]any{"splitIndex": 1}}); err != nil {
				t.Fatal(err)
			}
			job, err := s.Get(ctx, id)
			if err != nil || known && job.View["splitIndex"] != float64(2) || !known && job.View["splitIndex"] != nil {
				t.Fatalf("worker replaced intake child index: %+v %v", job, err)
			}
			turn, err := s.AcquireTurn(ctx, "owner/repo", TurnRequest{Host: "nas", JobID: &id, Epoch: 7})
			if err != nil || turn.Granted || !strings.Contains(turn.Reason, "X") {
				t.Fatalf("worker bypassed approval: %+v %v", turn, err)
			}
		})
	}
}
