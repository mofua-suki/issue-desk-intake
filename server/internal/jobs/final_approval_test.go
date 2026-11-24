package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/completionplan"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

func TestPendingFinalApprovalsWithoutGateCompatibility(t *testing.T) {
	var plan any
	if err := json.Unmarshal([]byte(finalResumePlan), &plan); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	job := &Job{ID: 7, Repo: "owner/repo", Issue: 1, HeadSHA: &head, View: map[string]any{"completionPlan": plan}}
	want := []PendingFinalApproval{{
		ID: "deploy", Target: "配置", Operation: "配置", Condition: "差分確認", HeadSHA: head,
		Reason:      "ジョブ owner/repo#1 (ID 7) の承認 deploy が未了です。head SHA: " + head,
		Fingerprint: "f56e3de779c67c969e1a00ad3253678748cad6d76a25f8cdfd0f28426961a51d",
	}}
	before, err := json.Marshal(job.View)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := PendingFinalApprovals(job)
	if err != nil || !reflect.DeepEqual(pending, want) {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	after, err := json.Marshal(job.View)
	if err != nil || string(after) != string(before) {
		t.Fatalf("pending read modified saved plan: %s %v", after, err)
	}
	// A persisted receipt with the fixed legacy fingerprint still authorizes this head.
	job.View["finalApprovals"] = []any{map[string]any{"id": "deploy", "headSha": head, "fingerprint": want[0].Fingerprint}}
	pending, err = PendingFinalApprovals(job)
	if err != nil || len(pending) != 0 {
		t.Fatalf("legacy receipt no longer matches: %+v %v", pending, err)
	}
}

func TestPendingFinalApprovalsExistingGate(t *testing.T) {
	for _, onlyGates := range []bool{false, true} {
		t.Run(fmt.Sprintf("only-gates=%v", onlyGates), func(t *testing.T) {
			db, ctx := testPostgres(t)
			id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
			var record any
			if err := json.Unmarshal([]byte(finalResumePlan), &record); err != nil {
				t.Fatal(err)
			}
			plan, err := completionplan.Parse(record)
			if err != nil {
				t.Fatal(err)
			}
			if onlyGates {
				plan.Approvals = []completionplan.Approval{}
			}
			for _, gate := range []string{"permission", "deletion"} {
				value := gate
				category := "permission"
				if gate == "deletion" {
					category = "destructive"
				}
				plan.Approvals = append(plan.Approvals, completionplan.Approval{ID: gate, Category: category, Phase: "after_diff", Target: "既存ゲートの対象", Operation: "既存ゲートで承認", Condition: "既存ゲートが適用される場合のみ", Gate: &value})
			}
			raw, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_build_object('completionPlan',$2::jsonb) WHERE id=$1`, id, string(raw)); err != nil {
				t.Fatal(err)
			}
			s, err := New(db, Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			head := strings.Repeat("a", 40)
			publishFinalApprovalFixture(t, ctx, s, id, "nas", head)
			job, err := s.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := PendingFinalApprovals(job)
			if err != nil || (onlyGates && len(pending) != 0) || (!onlyGates && (len(pending) != 1 || pending[0].ID != "deploy" || pending[0].Fingerprint != "f56e3de779c67c969e1a00ad3253678748cad6d76a25f8cdfd0f28426961a51d")) {
				t.Fatalf("gate filter: %+v %v", pending, err)
			}
			who := TurnRequest{Host: "nas", JobID: &id, Epoch: 7}
			turn, err := s.AcquireTurn(ctx, "owner/repo", who)
			if err != nil || turn.Granted != onlyGates {
				t.Fatalf("merge turn: %+v %v", turn, err)
			}
			job, err = s.Get(ctx, id)
			if err != nil || FinalApprovalHeld(job) == onlyGates {
				t.Fatalf("hold state: %+v %v", job, err)
			}
			if !onlyGates {
				_, err = s.RecordFinalApproval(ctx, id, FinalApprovalRecord{ID: "deploy", HeadSHA: head, Fingerprint: pending[0].Fingerprint, MessageID: "00000000-0000-4000-8000-000000000001", Quote: "この差分の配置を承認します"})
				if err != nil {
					t.Fatal(err)
				}
				releaseAgain(t, ctx, db, id)
				turn, err = s.AcquireTurn(ctx, "owner/repo", who)
				if err != nil || !turn.Granted {
					t.Fatalf("existing gates kept approved job waiting: %+v %v", turn, err)
				}
			}
		})
	}
}

func TestAcquireTurnFinalApproval(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
	head := strings.Repeat("a", 40)
	plan := `{"version":1,"basis":"根拠","steps":[{"phase":"implementation","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"verification","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"merge","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"deployment","owner":"担当","prerequisite":"前提","evidence":"確認"}],"approvals":[{"category":"irreversible","id":"deploy","phase":"after_diff","target":"配置","operation":"配置","condition":"差分確認"}]}`
	_, err := db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_build_object('completionPlan',$2::jsonb) WHERE id=$1`, id, plan)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	publishFinalApprovalFixture(t, ctx, s, id, "nas", head)
	who := TurnRequest{Host: "nas", JobID: &id, Epoch: 7}
	got, err := s.AcquireTurn(ctx, "owner/repo", who)
	if err != nil || got.Granted || !strings.Contains(got.Reason, "deploy") || !strings.Contains(got.Reason, head) || !strings.Contains(got.Reason, fmt.Sprint(id)) {
		t.Fatalf("pending: %+v %v", got, err)
	}
	var holder *string
	var epoch int64
	if err = db.QueryRowContext(ctx, `SELECT holder_host,epoch FROM merge_turns WHERE repo='owner/repo'`).Scan(&holder, &epoch); err != nil || holder != nil {
		t.Fatalf("holder %v %v", holder, err)
	}
	j, err := s.ByIssue(ctx, "owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	// The refused job leaves its lease so that it no longer occupies the PC slot.
	if j.Status != "held" || j.HoldReason == nil || *j.HoldReason != "review_held" || !FinalApprovalHeld(j) || j.View["releasePending"] != true || !strings.Contains(fmt.Sprint(j.Result["summary"]), "deploy") {
		t.Fatalf("not held for approval: %s %v %v %v", j.Status, j.HoldReason, j.View, j.Result)
	}
	if _, _, err = s.Renew(ctx, id, "nas", 7, nil); !leaseLost(err) {
		t.Fatalf("worker kept the lease: %v", err)
	}
	var held int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='state' AND data->>'to'='held' AND data->>'finalApproval'='true'`, id).Scan(&held); err != nil || held != 1 {
		t.Fatalf("hold event %d %v", held, err)
	}
	pending, err := PendingFinalApprovals(j)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	approved, err := s.RecordFinalApproval(ctx, id, FinalApprovalRecord{Fingerprint: pending[0].Fingerprint, ID: "deploy", HeadSHA: head, MessageID: "00000000-0000-4000-8000-000000000001", Quote: "この差分と配置を承認します"})
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != "queued" || approved.HoldReason != nil || approved.View[FinalApprovalHoldKey] != nil || len(approved.View["finalApprovals"].([]any)) != 1 {
		t.Fatalf("approval did not requeue: %s %v %v", approved.Status, approved.HoldReason, approved.View)
	}
	releaseAgain(t, ctx, db, id)
	job, _, err := s.Renew(ctx, id, "nas", 7, map[string]any{"finalApprovals": []any{}})
	if err != nil || len(job.View["finalApprovals"].([]any)) != 1 {
		t.Fatalf("worker overwrite: %+v %v", job, err)
	}
	job, err = s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "running", View: map[string]any{"finalApprovals": []any{}}})
	if err != nil || len(job.View["finalApprovals"].([]any)) != 1 {
		t.Fatalf("worker report overwrite: %+v %v", job, err)
	}
	got, err = s.AcquireTurn(ctx, "owner/repo", who)
	if err != nil || !got.Granted || got.Turn.Epoch != epoch+1 {
		t.Fatalf("approved: %+v %v", got, err)
	}
	other := strings.Repeat("b", 40)
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET head_sha=$2 WHERE id=$1`, id, other); err != nil {
		t.Fatal(err)
	}
	got, err = s.AcquireTurn(ctx, "owner/repo", who)
	if err != nil || got.Granted || !strings.Contains(got.Reason, other) {
		t.Fatalf("changed head: %+v %v", got, err)
	}
	releaseAgain(t, ctx, db, id)
	for _, value := range []string{`{}`, strings.Replace(plan, `[{"category":"irreversible","id":"deploy","phase":"after_diff","target":"配置","operation":"配置","condition":"差分確認"}]`, `[]`, 1)} {
		var view any
		if value == `{}` {
			view = map[string]any{}
		} else {
			var p any
			if err = json.Unmarshal([]byte(value), &p); err != nil {
				t.Fatal(err)
			}
			view = map[string]any{"completionPlan": p}
		}
		raw, _ := json.Marshal(view)
		if _, err = db.ExecContext(ctx, `UPDATE jobs SET view=$2::jsonb WHERE id=$1`, id, string(raw)); err != nil {
			t.Fatal(err)
		}
		got, err = s.AcquireTurn(ctx, "owner/repo", who)
		if err != nil || !got.Granted {
			t.Fatalf("no approvals: %+v %v", got, err)
		}
		var savedHost string
		var savedID, savedEpoch int64
		var savedExpiry time.Time
		if err = db.QueryRowContext(ctx, `SELECT holder_host,job_id,epoch,expires_at FROM merge_turns WHERE repo='owner/repo'`).Scan(&savedHost, &savedID, &savedEpoch, &savedExpiry); err != nil || savedHost != "nas" || savedID != id || savedEpoch != got.Turn.Epoch || iso(savedExpiry) != got.Turn.ExpiresAt {
			t.Fatalf("turn state: %s %d %d %v %v", savedHost, savedID, savedEpoch, savedExpiry, err)
		}
		var saved []byte
		if err = db.QueryRowContext(ctx, `SELECT view FROM jobs WHERE id=$1`, id).Scan(&saved); err != nil {
			t.Fatal(err)
		}
		var gotView any
		json.Unmarshal(saved, &gotView)
		if !equivalent(gotView, view) {
			t.Fatalf("view changed: %s", saved)
		}
	}
}

// A widened scope is recorded and reported; it does not wait for the user.
func TestAcquireTurnWidenedScopeProceedsWithoutApproval(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
	head := strings.Repeat("a", 40)
	plan := `{"version":1,"basis":"根拠","steps":[{"phase":"implementation","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"verification","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"merge","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"deployment","owner":"担当","prerequisite":"前提","evidence":"確認"}],"approvals":[{"category":"scope","id":"scope-extension","phase":"before_implementation","target":"範囲","operation":"追加","condition":"確認"}]}`
	_, err := db.ExecContext(ctx, `UPDATE jobs SET head_sha=$2,scope_source='widened',scope=ARRAY['src/','extra.go'],view=jsonb_build_object('completionPlan',$3::jsonb) WHERE id=$1`, id, head, plan)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	who := TurnRequest{Host: "nas", JobID: &id, Epoch: 7}
	got, err := s.AcquireTurn(ctx, "owner/repo", who)
	if err != nil || !got.Granted {
		t.Fatalf("widened scope waited for the user: %+v %v", got, err)
	}
	j, err := s.ByIssue(ctx, "owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := PendingFinalApprovals(j); err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
}

// releaseAgain stands for the dispatcher leasing the job to the same worker again.
func releaseAgain(t *testing.T, ctx context.Context, db *sql.DB, id int64) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET status='running',hold_reason=NULL,view=view-'finalApprovalHold' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
}

func leaseLost(err error) bool {
	var problem *contract.Problem
	return errors.As(err, &problem) && problem.Code == "lease_lost"
}

func TestFinalApprovalHoldWaitsForEveryApproval(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
	head := strings.Repeat("a", 40)
	plan := `{"version":1,"basis":"根拠","steps":[{"phase":"implementation","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"verification","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"merge","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"deployment","owner":"担当","prerequisite":"前提","evidence":"確認"}],"approvals":[{"category":"irreversible","id":"deploy","phase":"after_diff","target":"配置","operation":"配置","condition":"差分確認"},{"category":"destructive","id":"drop","phase":"after_diff","target":"表","operation":"削除","condition":"差分確認"},{"category":"scope","id":"extra","phase":"after_diff","target":"追加ファイル","operation":"変更","condition":"差分確認"}]}`
	// The scope item never waits: it neither causes the hold nor joins it afterwards.
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_build_object('completionPlan',$2::jsonb) WHERE id=$1`, id, plan); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	publishFinalApprovalFixture(t, ctx, s, id, "nas", head)
	who := TurnRequest{Host: "nas", JobID: &id, Epoch: 7}
	if got, err := s.AcquireTurn(ctx, "owner/repo", who); err != nil || got.Granted {
		t.Fatalf("pending: %+v %v", got, err)
	}
	// A second refusal from the stale worker does not hold the job again.
	if _, err := s.AcquireTurn(ctx, "owner/repo", who); !leaseLost(err) {
		t.Fatalf("stale worker: %v", err)
	}
	j, err := s.ByIssue(ctx, "owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := PendingFinalApprovals(j)
	if err != nil || len(pending) != 2 {
		t.Fatal(pending, err)
	}
	first, err := s.RecordFinalApproval(ctx, id, FinalApprovalRecord{Fingerprint: pending[0].Fingerprint, ID: pending[0].ID, HeadSHA: head, MessageID: "00000000-0000-4000-8000-000000000003", Quote: "この配置を承認します。"})
	if err != nil || first.Status != "held" || !FinalApprovalHeld(first) {
		t.Fatalf("requeued before every approval: %+v %v", first, err)
	}
	second, err := s.RecordFinalApproval(ctx, id, FinalApprovalRecord{Fingerprint: pending[1].Fingerprint, ID: pending[1].ID, HeadSHA: head, MessageID: "00000000-0000-4000-8000-000000000004", Quote: "この表の削除を承認します。"})
	if err != nil || second.Status != "queued" || second.View[FinalApprovalHoldKey] != nil {
		t.Fatalf("not requeued: %+v %v", second, err)
	}
	var resumed int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='state' AND data->>'to'='queued' AND data->>'finalApproval'='true'`, id).Scan(&resumed); err != nil || resumed != 1 {
		t.Fatalf("resume event %d %v", resumed, err)
	}
}

func TestFinalApprovalOnOtherHoldDoesNotRequeue(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
	head := strings.Repeat("a", 40)
	plan := `{"version":1,"basis":"根拠","steps":[{"phase":"implementation","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"verification","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"merge","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"deployment","owner":"担当","prerequisite":"前提","evidence":"確認"}],"approvals":[{"category":"irreversible","id":"deploy","phase":"after_diff","target":"配置","operation":"配置","condition":"差分確認"}]}`
	// Held by the worker's review, not by the merge turn: the approval only records.
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET status='held',hold_reason='review_held',head_sha=$2,view=jsonb_build_object('completionPlan',$3::jsonb) WHERE id=$1`, id, head, plan); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.ByIssue(ctx, "owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := PendingFinalApprovals(j)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	saved, err := s.RecordFinalApproval(ctx, id, FinalApprovalRecord{Fingerprint: pending[0].Fingerprint, ID: "deploy", HeadSHA: head, MessageID: "00000000-0000-4000-8000-000000000005", Quote: "この差分と配置を承認します"})
	if err != nil || saved.Status != "held" || saved.HoldReason == nil || *saved.HoldReason != "review_held" {
		t.Fatalf("other hold changed: %+v %v", saved, err)
	}
}

// The worker learns of the ended lease only on its next renewal. An approval
// recorded before then must not hand the same job back to the same PC, which
// the worker refuses as a duplicate assignment.
func TestFinalApprovalResumeWaitsForPreviousLease(t *testing.T) {
	db, ctx := testPostgres(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('one',true,$1,1)`, now); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"one":{"maxImplementations":1,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw); err != nil {
		t.Fatal(err)
	}
	a, err := fleet.NormalizeAssignments(raw)
	if err != nil {
		t.Fatal(err)
	}
	id := seedJob(t, ctx, db, "owner/repo", "one", 1, 7)
	head := strings.Repeat("a", 40)
	plan := `{"version":1,"basis":"根拠","steps":[{"phase":"implementation","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"verification","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"merge","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"deployment","owner":"担当","prerequisite":"前提","evidence":"確認"}],"approvals":[{"category":"irreversible","id":"deploy","phase":"after_diff","target":"配置","operation":"配置","condition":"差分確認"}]}`
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_build_object('completionPlan',$2::jsonb) WHERE id=$1`, id, plan); err != nil {
		t.Fatal(err)
	}
	clock := now
	s, err := New(db, Options{Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	publishFinalApprovalFixture(t, ctx, s, id, "one", head)
	if got, err := s.AcquireTurn(ctx, "owner/repo", TurnRequest{Host: "one", JobID: &id, Epoch: 7}); err != nil || got.Granted {
		t.Fatalf("pending: %+v %v", got, err)
	}
	j, err := s.ByIssue(ctx, "owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := PendingFinalApprovals(j)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	if _, err = s.RecordFinalApproval(ctx, id, FinalApprovalRecord{Fingerprint: pending[0].Fingerprint, ID: "deploy", HeadSHA: head, MessageID: "00000000-0000-4000-8000-000000000006", Quote: "この差分と配置を承認します"}); err != nil {
		t.Fatal(err)
	}
	queued, err := s.ByIssue(ctx, "owner/repo", 1)
	if err != nil || queued.Status != "queued" || queued.View[FinalApprovalResumeKey] == nil {
		t.Fatalf("resume mark: %+v %v", queued, err)
	}
	job, _, err := s.Next(ctx, "one", 1, a)
	if err != nil || job != nil {
		t.Fatalf("handed back during the previous lease: %+v %v", job, err)
	}
	// seedJob's lease ends one hour after now; the PC keeps reporting in.
	clock = now.Add(61 * time.Minute)
	if _, err = db.ExecContext(ctx, `UPDATE hosts SET heartbeat_at=$1 WHERE host='one'`, clock); err != nil {
		t.Fatal(err)
	}
	job, lease, err := s.Next(ctx, "one", 1, a)
	if err != nil || job == nil || job.ID != id || lease == nil || lease.Epoch != 8 || job.View[FinalApprovalResumeKey] != nil {
		t.Fatalf("not dispatched after the lease: %+v %+v %v", job, lease, err)
	}
}

// publishFinalApprovalFixture exercises the ordinary worker report path before a turn request.
func publishFinalApprovalFixture(t *testing.T, ctx context.Context, s *Store, id int64, host, head string) {
	t.Helper()
	branch, pr := "work/published", int64(12)
	_, err := s.Report(ctx, id, host, Report{Epoch: 7, Status: "running", Branch: Optional[string]{Present: true, Value: &branch}, HeadSHA: Optional[string]{Present: true, Value: &head}, PR: Optional[int64]{Present: true, Value: &pr}, View: map[string]any{"phase": "publishing"}})
	if err != nil {
		t.Fatal(err)
	}
}

const finalResumePlan = `{"version":1,"basis":"根拠","steps":[{"phase":"implementation","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"verification","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"merge","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"deployment","owner":"担当","prerequisite":"前提","evidence":"確認"}],"approvals":[{"category":"irreversible","id":"deploy","phase":"after_diff","target":"配置","operation":"配置","condition":"差分確認"}]}`

func finalResumeFixture(t *testing.T) (*sql.DB, context.Context, *Store, *fleet.Assignments, int64, string) {
	t.Helper()
	db, ctx := testPostgres(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('other',true,$1,1)`, now); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"other":{"maxImplementations":1,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw); err != nil {
		t.Fatal(err)
	}
	a, err := fleet.NormalizeAssignments(raw)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	id := seedJob(t, ctx, db, "owner/repo", "one", 1, 7)
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_build_object('completionPlan',$2::jsonb) WHERE id=$1`, id, finalResumePlan); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	publishFinalApprovalFixture(t, ctx, s, id, "one", head)
	got, err := s.AcquireTurn(ctx, "owner/repo", TurnRequest{Host: "one", JobID: &id, Epoch: 7})
	if err != nil || got.Granted {
		t.Fatalf("hold: %+v %v", got, err)
	}
	return db, ctx, s, a, id, head
}

func approveFinalResumeFixture(t *testing.T, ctx context.Context, s *Store, id int64, head string) *Job {
	t.Helper()
	j, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := PendingFinalApprovals(j)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	j, err = s.RecordFinalApproval(ctx, id, FinalApprovalRecord{ID: pending[0].ID, Fingerprint: pending[0].Fingerprint, HeadSHA: head, MessageID: "00000000-0000-4000-8000-000000000099", Quote: "この差分と配置を承認します"})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestFinalApprovalResumeAcrossHosts(t *testing.T) {
	_, ctx, s, a, id, head := finalResumeFixture(t)
	saved := approveFinalResumeFixture(t, ctx, s, id, head)
	if saved.Status != "queued" {
		t.Fatalf("not queued: %+v", saved)
	}
	job, lease, err := s.Next(ctx, "other", 1, a)
	if err != nil || job == nil || lease == nil {
		t.Fatalf("Next: %+v %v", job, err)
	}
	artifact, ok := artifactOf(job)
	publicationPR, validPR := count(fleet.Object(fleet.Object(job.View["continuation"])["publication"])["pr"])
	if !ok || !artifactPublished(job, artifact) || !validPR || publicationPR != 12 || artifact.Head != head || artifact.Branch != "work/published" || job.PR == nil || *job.PR != 12 || job.View["publicationResume"] != true || job.View["retryFromBase"] != nil || reimplementationExpected(job) || job.View["permissionApproval"] != nil {
		t.Fatalf("handoff: %+v", job)
	}
	changedPR := int64(13)
	job, err = s.Report(ctx, id, "other", Report{Epoch: lease.Epoch, Status: "running", PR: Optional[int64]{Present: true, Value: &changedPR}})
	if err != nil {
		t.Fatal(err)
	}
	artifact, ok = artifactOf(job)
	if !ok || artifactPublished(job, artifact) {
		t.Fatalf("old PR evidence reused: %+v", job)
	}
}

func TestFinalApprovalResumeAcquiresTurn(t *testing.T) {
	_, ctx, s, a, id, head := finalResumeFixture(t)
	approveFinalResumeFixture(t, ctx, s, id, head)
	job, lease, err := s.Next(ctx, "other", 1, a)
	if err != nil || job == nil || lease == nil {
		t.Fatalf("Next: %+v %v", job, err)
	}
	pending, err := PendingFinalApprovals(job)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	turn, err := s.AcquireTurn(ctx, "owner/repo", TurnRequest{Host: "other", JobID: &id, Epoch: lease.Epoch})
	if err != nil || !turn.Granted {
		t.Fatalf("turn: %+v %v", turn, err)
	}
}

func TestFinalApprovalResumeRejectsChangedHead(t *testing.T) {
	_, ctx, s, a, id, head := finalResumeFixture(t)
	approveFinalResumeFixture(t, ctx, s, id, head)
	job, lease, err := s.Next(ctx, "other", 1, a)
	if err != nil || job == nil || lease == nil {
		t.Fatalf("Next: %+v %v", job, err)
	}
	changed := strings.Repeat("b", 40)
	job, err = s.Report(ctx, id, "other", Report{Epoch: lease.Epoch, Status: "running", HeadSHA: Optional[string]{Present: true, Value: &changed}})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := PendingFinalApprovals(job)
	if err != nil || len(pending) != 1 || pending[0].HeadSHA != changed {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	turn, err := s.AcquireTurn(ctx, "owner/repo", TurnRequest{Host: "other", JobID: &id, Epoch: lease.Epoch})
	if err != nil || turn.Granted || !strings.Contains(turn.Reason, changed) {
		t.Fatalf("turn: %+v %v", turn, err)
	}
}

func TestFinalApprovalResumeUnavailableArtifact(t *testing.T) {
	for _, tc := range []struct{ name, update string }{
		{"missing", `UPDATE jobs SET view=view-'continuation' WHERE id=$1`},
		{"head", `UPDATE jobs SET view=jsonb_set(view,'{continuation,artifact,headSha}',to_jsonb(repeat('b',40))) WHERE id=$1`},
		{"generation", `UPDATE jobs SET view=jsonb_set(view,'{continuation,artifact,generation}','1') WHERE id=$1`},
		{"branch", `UPDATE jobs SET view=jsonb_set(view,'{continuation,artifact,branch}',to_jsonb('other'::text)) WHERE id=$1`},
		{"pr", `UPDATE jobs SET pr=13 WHERE id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx, s, a, id, head := finalResumeFixture(t)
			if _, err := db.ExecContext(ctx, tc.update, id); err != nil {
				t.Fatal(err)
			}
			saved := approveFinalResumeFixture(t, ctx, s, id, head)
			reread, err := s.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := PendingFinalApprovals(reread)
			if saved.Status != "held" || reread.Status != "held" || len(pending) != 0 || err != nil || len(fleet.List(reread.View["finalApprovals"])) != 1 || !strings.Contains(fmt.Sprint(reread.Result["summary"]), "続行できません") {
				t.Fatalf("held: %+v pending=%v err=%v", reread, pending, err)
			}
			job, _, err := s.Next(ctx, "other", 1, a)
			if err != nil || job != nil {
				t.Fatalf("unexpected dispatch: %+v %v", job, err)
			}
		})
	}
}

func TestFinalApprovalResumeLegacyHoldEvidence(t *testing.T) {
	for _, verified := range []bool{true, false} {
		t.Run(fmt.Sprint(verified), func(t *testing.T) {
			db, ctx, s, a, id, head := finalResumeFixture(t)
			query := `UPDATE jobs SET view=jsonb_set(view, '{finalApprovalHold}', (view->'finalApprovalHold') - ARRAY['pr','branch','published']) WHERE id=$1`
			if _, err := db.ExecContext(ctx, query, id); err != nil {
				t.Fatal(err)
			}
			if !verified {
				if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=view-'phase' WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `DELETE FROM job_events WHERE job_id=$1 AND kind='running_publication'`, id); err != nil {
					t.Fatal(err)
				}
			}
			saved := approveFinalResumeFixture(t, ctx, s, id, head)
			job, _, err := s.Next(ctx, "other", 1, a)
			if err != nil {
				t.Fatal(err)
			}
			if verified && (saved.Status != "queued" || job == nil) || !verified && (saved.Status != "held" || job != nil || !strings.Contains(fmt.Sprint(saved.Result["summary"]), "続行できません")) {
				t.Fatalf("saved=%+v next=%+v", saved, job)
			}
		})
	}
}

func TestFinalApprovalResumePreservesInstruction(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(fmt.Sprint(before), func(t *testing.T) {
			_, ctx, s, a, id, head := finalResumeFixture(t)
			instruction := ControlInput{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Action: "instruction", By: "issue-screen", Text: "再実装と再検証を行ってください"}
			if !before {
				approveFinalResumeFixture(t, ctx, s, id, head)
			}
			if _, err := s.Control(ctx, id, instruction, ControlOptions{}); err != nil {
				t.Fatal(err)
			}
			if before {
				job := approveFinalResumeFixture(t, ctx, s, id, head)
				if job.Status != "held" || len(fleet.List(job.View["instructions"])) != 1 {
					t.Fatalf("instruction overwritten: %+v", job)
				}
			} else {
				job, _, err := s.Next(ctx, "other", 1, a)
				if err != nil || job == nil || job.View["publicationResume"] == true || len(fleet.List(job.View["instructions"])) != 1 {
					t.Fatalf("instruction overwritten: %+v %v", job, err)
				}
			}
		})
	}
}

func TestFinalApprovalLegacyInstructionStaysHeld(t *testing.T) {
	db, ctx, s, a, id, head := finalResumeFixture(t)
	// Model a hold written before tuple snapshots existed.
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_set(view, '{finalApprovalHold}', (view->'finalApprovalHold') - ARRAY['pr','branch','published']) WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	_, err := s.Control(ctx, id, ControlInput{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Action: "instruction", By: "issue-screen", Text: "再実装と再検証を行ってください"}, ControlOptions{})
	if err != nil {
		t.Fatal(err)
	}
	saved := approveFinalResumeFixture(t, ctx, s, id, head)
	job, _, err := s.Next(ctx, "other", 1, a)
	if err != nil || saved.Status != "held" || job != nil || len(fleet.List(saved.View["instructions"])) != 1 {
		t.Fatalf("saved=%+v next=%+v err=%v", saved, job, err)
	}
}

func TestPendingFinalApprovalsWaitOnlyForUserGatedCategories(t *testing.T) {
	var record any
	if err := json.Unmarshal([]byte(finalResumePlan), &record); err != nil {
		t.Fatal(err)
	}
	plan, err := completionplan.Parse(record)
	if err != nil {
		t.Fatal(err)
	}
	gate := "permission"
	plan.Approvals = []completionplan.Approval{
		{ID: "protected", Category: "permission", Phase: "after_diff", Target: "権限", Operation: "変更", Condition: "差分確認", Gate: &gate},
		{ID: "scope", Category: "scope", Phase: "before_implementation", Target: "追加ファイル", Operation: "変更", Condition: "範囲拡大"},
		{ID: "scope-diff", Category: "scope", Phase: "after_diff", Target: "追加ファイル", Operation: "変更", Condition: "差分確認"},
		{ID: "basis", Category: "basis", Phase: "after_diff", Target: "前提", Operation: "見直し", Condition: "差分確認"},
		{ID: "erase", Category: "irreversible", Phase: "after_diff", Target: "保存データ", Operation: "移行", Condition: "差分確認"},
	}
	job := &Job{ScopeSource: "widened", Scope: []string{"extra.go"}, View: map[string]any{"completionPlan": plan.Record()}}
	pending, err := PendingFinalApprovals(job)
	if err != nil || len(pending) != 1 || pending[0].ID != "erase" || len(pending[0].Scope) != 0 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	// A job already held for its final approvals keeps the ones it was held
	// for, because recording them is the only way it is queued again. Items
	// that did not cause the hold still do not wait.
	head, reason := strings.Repeat("a", 40), "review_held"
	job.Status, job.HoldReason, job.HeadSHA, job.Generation, job.LeaseEpoch = "held", &reason, &head, 2, 7
	job.View[FinalApprovalHoldKey] = map[string]any{"headSha": head, "generation": float64(2), "epoch": float64(7), "approvals": []any{"scope", "basis", "erase"}}
	pending, err = PendingFinalApprovals(job)
	ids := []string{}
	for _, item := range pending {
		ids = append(ids, item.ID)
	}
	if err != nil || !reflect.DeepEqual(ids, []string{"scope", "basis", "erase"}) || !reflect.DeepEqual(pending[0].Scope, job.Scope) {
		t.Fatalf("held pending=%+v err=%v", pending, err)
	}
}

func TestFinalApprovalHoldEventMetadataPostgres(t *testing.T) {
	cases := []struct {
		name, phaseJSON string
		phase           any
		present         bool
	}{
		{"missing", "", nil, false}, {"null", `,"phase":null`, nil, true}, {"empty", `,"phase":""`, "", true}, {"value", `,"phase":"review"`, "review", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
			s, err := New(db, Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			publishFinalApprovalFixture(t, ctx, s, id, "nas", strings.Repeat("a", 40))
			view := `{"completionPlan":` + finalResumePlan + tc.phaseJSON + `}`
			if _, err = db.ExecContext(ctx, `UPDATE jobs SET view=$2::jsonb,generation=3 WHERE id=$1`, id, view); err != nil {
				t.Fatal(err)
			}
			got, err := s.AcquireTurn(ctx, "owner/repo", TurnRequest{Host: "nas", JobID: &id, Epoch: 7})
			if err != nil || got.Granted {
				t.Fatalf("turn: %+v %v", got, err)
			}
			var raw []byte
			var host string
			if err = db.QueryRowContext(ctx, `SELECT data,host FROM job_events WHERE job_id=$1 AND kind='state' AND data->>'finalApproval'='true'`, id).Scan(&raw, &host); err != nil {
				t.Fatal(err)
			}
			var event map[string]any
			if err = json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			phase, present := event["phase"]
			if host != "nas" || event["leaseHost"] != "nas" || event["generation"] != float64(3) || event["epoch"] != float64(7) || event["to"] != "held" || event["holdReason"] != "review_held" || event["finalApproval"] != true || present != tc.present || phase != tc.phase {
				t.Fatal(host, event)
			}
			job, err := s.Get(ctx, id)
			if err != nil || job.Status != "held" || job.HoldReason == nil || *job.HoldReason != "review_held" {
				t.Fatal(job, err)
			}
			if _, _, err = s.Renew(ctx, id, "nas", 7, nil); !leaseLost(err) {
				t.Fatal(err)
			}
		})
	}
}
