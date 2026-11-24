package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/testcause"
	"issue-desk-intake/server/internal/workplan"
	"os"
	"strings"
	"testing"
	"time"
)

func executionFixture(kind, owner, phase string) *workplan.Plan {
	return &workplan.Plan{Version: 1, Requirements: []workplan.Requirement{{ID: "one", Criterion: "受入条件の全文", Owner: owner, Phase: phase, Method: "対象版を照合", Expected: "一致", EvidenceKind: kind, Capabilities: []string{}, Inputs: []string{}, Children: []int{}}}}
}

func TestControllerObservationIsFencedAndIdempotent(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
	plan := executionFixture("external_check", "controller", "deployment")
	head := strings.Repeat("a", 40)
	raw, _ := json.Marshal(map[string]any{"executionPlan": plan.Record(), "executionWait": "controller"})
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET status='held',hold_reason='review_held',head_sha=$2,generation=1,view=$3::jsonb,lease_expires_at=NULL WHERE id=$1`, id, head, raw); err != nil {
		t.Fatal(err)
	}
	store, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	check := ExecutionCheck{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Requirement: "one", PlanSHA: plan.Fingerprint(), Head: head, Generation: 1, Status: "passed", Source: "host-ssh:status", ObservedAt: now.Format(time.RFC3339Nano), Detail: "観測した版が期待するSHAと一致"}
	job, err := store.RecordExecutionCheck(ctx, id, check, "token:nas", "")
	if err != nil || job.Status != "merged" {
		t.Fatal(job, err)
	}
	evidence, err := workplan.DecodeEvidence(job.View["executionReceipts"])
	if err != nil || len(evidence) != 1 || evidence[0].Generation != 1 || evidence[0].Source != "controller" {
		t.Fatal(evidence, err)
	}
	if _, err := store.RecordExecutionCheck(ctx, id, check, "token:nas", ""); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='execution_check'`, id).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	other := check
	other.Detail = "別の結果"
	if _, err := store.RecordExecutionCheck(ctx, id, other, "token:nas", ""); err == nil {
		t.Fatal("同じIDで内容を変更")
	}
	stale := check
	stale.ID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	stale.Generation = 2
	if _, err := store.RecordExecutionCheck(ctx, id, stale, "token:nas", ""); err == nil {
		t.Fatal("別世代の確認を採用")
	}
}

func TestModelResultCannotCreateControllerReceipts(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
	plan := executionFixture("external_check", "controller", "deployment")
	head := strings.Repeat("a", 40)
	raw, _ := json.Marshal(map[string]any{"executionPlan": plan.Record()})
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET head_sha=$2,view=$3::jsonb WHERE id=$1`, id, head, raw); err != nil {
		t.Fatal(err)
	}
	store, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	forged := []any{map[string]any{"requirement": "one", "planSHA": plan.Fingerprint(), "head": head, "source": "controller", "status": "passed", "detail": "モデルの自己申告"}}
	job, err := store.Report(ctx, id, "nas", Report{Epoch: 7, Status: "running", View: map[string]any{"executionReceipts": forged}, Result: map[string]any{"executionEvidence": forged}})
	if err != nil {
		t.Fatal(err)
	}
	if job.View["executionReceipts"] != nil {
		t.Fatal("モデル結果が正本になった", job.View)
	}
}

func TestControllerTestReceiptSurvivesLostFinalResponse(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
	plan := executionFixture("fixed_test", "implementer", "verification")
	head := strings.Repeat("a", 40)
	raw, _ := json.Marshal(map[string]any{"executionPlan": plan.Record()})
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET head_sha=$2,view=$3::jsonb WHERE id=$1`, id, head, raw); err != nil {
		t.Fatal(err)
	}
	at := now
	store, err := New(db, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile("testdata/controller-test.json")
	if err != nil {
		t.Fatal(err)
	}
	var measured map[string]any
	if err = json.Unmarshal(raw, &measured); err != nil {
		t.Fatal(err)
	}
	hold := "review_held"
	report := Report{Epoch: 7, Status: "held", HoldReason: &hold, ControllerTest: &ControllerTest{PlanSHA: plan.Fingerprint(), Test: measured}}
	job, err := store.Report(ctx, id, "nas", report)
	if err != nil {
		current, _ := store.Get(ctx, id)
		parsed, _ := workplan.Parse(current.View["executionPlan"])
		t.Fatalf("%v: original=%s parsed=%s cause=%+v", err, plan.Fingerprint(), parsed.Fingerprint(), testcause.Classify(measured["causeObservation"], nil))
	}
	evidence, err := workplan.DecodeEvidence(job.View["executionReceipts"])
	if err != nil || len(evidence) != 1 || evidence[0].Source != "implementer" || plan.Checks(head, evidence, job.Generation)[0].Status != "passed" {
		t.Fatal(evidence, err)
	}
	at = now.Add(time.Minute)
	if _, err = store.Report(ctx, id, "nas", report); err != nil {
		t.Fatal("応答不明後の同じ送信を拒否", err)
	}
	report.ControllerTest.PlanSHA = "other"
	if _, err = store.Report(ctx, id, "nas", report); err == nil {
		t.Fatal("内容の違う証拠を再送として採用")
	}
}

func TestControllerReportRequiresExistingAuthorAndBody(t *testing.T) {
	cases := []struct {
		name, author, marker, history      string
		badSHA, unreadable, badLease, want bool
	}{
		{name: "previous same generation", author: "worker-a", marker: "new", history: `{"epoch":6,"generation":1}`, want: true},
		{name: "previous other generation", author: "worker-a", marker: "new", history: `{"epoch":6,"generation":2}`},
		{name: "unassigned", author: "worker-x", marker: "new", history: `{"epoch":6,"generation":1}`},
		{name: "wrong body SHA", author: "worker-a", marker: "new", history: `{"epoch":6,"generation":1}`, badSHA: true},
		{name: "invalid marker", author: "worker-a", marker: "invalid", history: `{"epoch":6,"generation":1}`},
		{name: "comments unavailable", author: "worker-a", marker: "new", history: `{"epoch":6,"generation":1}`, unreadable: true},
		{name: "invalid lease", author: "worker-a", marker: "new", history: `{"epoch":6,"generation":1}`, badLease: true},
		{name: "legacy current", author: "worker-b", marker: "old", history: `{"epoch":6,"generation":1}`, want: true},
		{name: "legacy previous", author: "worker-a", marker: "old", history: `{"epoch":6,"generation":1}`},
		{name: "new current", author: "worker-b", marker: "new", want: true},
		{name: "future epoch", author: "worker-a", marker: "new", history: `{"epoch":7,"generation":1}`},
		{name: "incomplete history", author: "worker-a", marker: "new", history: `{"epoch":6}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			id := seedJob(t, ctx, db, "owner/repo", "b", 1, 7)
			plan := executionFixture("report_comment", "reporter", "report")
			head := strings.Repeat("a", 40)
			raw, _ := json.Marshal(map[string]any{"executionPlan": plan.Record()})
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET head_sha=$2,view=$3::jsonb,generation=1 WHERE id=$1`, id, head, raw); err != nil {
				t.Fatal(err)
			}
			if tc.history != "" {
				if _, err := db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,data) VALUES($1,'a','leased',$2::jsonb)`, id, tc.history); err != nil {
					t.Fatal(err)
				}
			}
			marker := fmt.Sprintf("<!-- issue-desk-execution-report:%d:g1:%s:%s -->", id, head, plan.Fingerprint())
			if tc.marker == "old" {
				marker = fmt.Sprintf("<!-- issue-desk-execution-report:%d:%s:%s -->", id, head, plan.Fingerprint())
			}
			if tc.marker == "invalid" {
				marker += "invalid"
			}
			body := "訂正と根拠\n" + marker
			sum := sha256.Sum256([]byte(body))
			bodySHA := hex.EncodeToString(sum[:])
			if tc.badSHA {
				bodySHA = strings.Repeat("0", 64)
			}
			store, err := New(db, Options{Now: func() time.Time { return now }, ReportAuthor: func(host string) string {
				if host == "a" || host == "b" {
					return "worker-" + host
				}
				return ""
			}, ReadComments: func(_ context.Context, repo string, issue int64) ([]map[string]any, error) {
				if repo != "owner/repo" || issue != 1 {
					t.Fatalf("unexpected issue %s %d", repo, issue)
				}
				if tc.unreadable {
					return nil, fmt.Errorf("unavailable")
				}
				return []map[string]any{{"id": float64(7), "body": body, "user": map[string]any{"login": tc.author}}}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			epoch := int64(7)
			if tc.badLease {
				epoch = 6
			}
			hold := "review_held"
			job, err := store.Report(ctx, id, "b", Report{Epoch: epoch, Status: "held", HoldReason: &hold, ControllerReport: &ControllerReport{PlanSHA: plan.Fingerprint(), Head: head, Marker: marker, BodySHA: bodySHA}})
			if !tc.want {
				if err == nil {
					t.Fatal("invalid report accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := workplan.DecodeEvidence(job.View["executionReceipts"])
			if err != nil || len(evidence) != 1 || evidence[0].RecordID != "comment:7" || evidence[0].Source != "reporter" {
				t.Fatal(evidence, err)
			}
		})
	}
}

func TestMergedCodeReportHoldBindsActualPull(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			db, ctx := testPostgres(t)
			id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
			plan := executionFixture("fixed_test", "implementer", "verification")
			tested, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
			viewData := artifactJob("nas", true).View
			viewData["executionPlan"] = plan.Record()
			view, _ := json.Marshal(viewData)
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET head_sha=$2,pr=3,branch='codex/issue-1-job-1',view=$3::jsonb WHERE id=$1`, id, tested, view); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile("testdata/controller-test.json")
			if err != nil {
				t.Fatal(err)
			}
			var measured map[string]any
			if err = json.Unmarshal(raw, &measured); err != nil {
				t.Fatal(err)
			}
			store, err := New(db, Options{Now: func() time.Time { return now }, ReadPull: func(context.Context, string, int64) (map[string]any, error) {
				actual := head
				if !valid {
					actual = strings.Repeat("c", 40)
				}
				return map[string]any{"merged": true, "merge_commit_sha": actual, "head": map[string]any{"sha": tested, "ref": "codex/issue-1-job-1", "repo": map[string]any{"full_name": "owner/repo"}}, "base": map[string]any{"repo": map[string]any{"full_name": "owner/repo"}}}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			hold := "review_held"
			report := Report{Epoch: 7, Status: "held", HoldReason: &hold, HeadSHA: Optional[string]{Present: true, Value: &head}, View: map[string]any{"codeMerged": true, "executionWait": "reporter"}, ControllerTest: &ControllerTest{PlanSHA: plan.Fingerprint(), Test: measured}}
			job, err := store.Report(ctx, id, "nas", report)
			if !valid {
				if err == nil {
					t.Fatal("別マージのSHAへ証拠を移送")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := workplan.DecodeEvidence(job.View["executionReceipts"])
			if err != nil || len(evidence) != 1 || evidence[0].Head != head || job.Status != "held" {
				t.Fatal(job, evidence, err)
			}

			if _, err = store.Control(ctx, id, ControlInput{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Action: "retry", By: "token:nas"}, ControlOptions{}); err != nil {
				t.Fatal(err)
			}
			var assignments map[string]any
			if err = json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":1,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &assignments); err != nil {
				t.Fatal(err)
			}
			a, err := fleet.NormalizeAssignments(assignments)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity,capabilities) VALUES('nas',true,$1,1,'{"executionPlanVersion":1}')`, now); err != nil {
				t.Fatal(err)
			}
			leased, _, err := store.Next(ctx, "nas", 1, a)
			if err != nil || leased == nil {
				t.Fatal(leased, err)
			}
			report.Epoch = leased.LeaseEpoch
			report.Status = "merged"
			report.HoldReason = nil
			completed, err := store.Report(ctx, id, "nas", report)
			if err != nil || completed.Status != "merged" {
				t.Fatal(completed, err)
			}
			evidence, err = workplan.DecodeEvidence(completed.View["executionReceipts"])
			if err != nil || len(evidence) != 1 || evidence[0].Generation != leased.Generation || evidence[0].Head != head {
				t.Fatal(evidence, err)
			}
		})
	}
}

func TestExecutionPlanDispatchRequiresCompatibleMeasuredCapability(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	a := balanceAssignments(t)
	plan := executionFixture("fixed_test", "implementer", "verification")
	plan.Requirements[0].Capabilities = []string{"race"}
	raw, _ := json.Marshal(map[string]any{"executionPlan": plan.Record()})
	if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,view) VALUES(1,'mofua-suki/issue-desk-intake',1,'queued',ARRAY['one.go'],'declared',$1::jsonb)`, raw); err != nil {
		t.Fatal(err)
	}
	store, err := New(db, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	for _, caps := range []map[string]any{{}, {"executionPlanVersion": 1, "environment": map[string]any{"race": false}}, {"executionPlanVersion": 1, "environment": map[string]any{"race": true}}} {
		raw, _ := json.Marshal(caps)
		if _, err := db.ExecContext(ctx, `UPDATE hosts SET capabilities=$1::jsonb WHERE host='gt12'`, raw); err != nil {
			t.Fatal(err)
		}
		job, _, err := store.Next(ctx, "gt12", 4, a)
		want := false
		if env, ok := caps["environment"].(map[string]any); ok {
			want = env["race"] == true
		}
		if err != nil || (job != nil) != want {
			t.Fatal(caps, job, err)
		}
	}
}

func TestMergedReportHoldCompletesWhenLastReceiptArrives(t *testing.T) {
	for _, tc := range []struct {
		name           string
		base           string
		testGeneration int64
		want           string
	}{
		{"matching merge and receipts", "main", 1, "merged"},
		{"different target branch", "other", 1, "held"},
		{"different receipt generation", "main", 0, "held"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
			plan := executionFixture("fixed_test", "implementer", "verification")
			report := executionFixture("report_comment", "reporter", "report").Requirements[0]
			report.ID = "report"
			plan.Requirements = append(plan.Requirements, report)
			head := strings.Repeat("a", 40)
			branch := "codex/issue-1-job-1"
			receipt := workplan.Evidence{
				Requirement: "one", Generation: tc.testGeneration, RecordID: "fixed-test:nas:7",
				PlanSHA: plan.Fingerprint(), Head: head, Status: "passed", Source: "implementer", Detail: "fixed test passed",
			}
			view, err := json.Marshal(map[string]any{
				"executionPlan": plan.Record(), "executionWait": "reporter",
				"codeMerged": true, "executionReceipts": []workplan.Evidence{receipt},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.ExecContext(ctx, `UPDATE jobs SET status='held',hold_reason='review_held',head_sha=$2,pr=3,branch=$3,generation=1,view=$4::jsonb,lease_expires_at=NULL WHERE id=$1`, id, head, branch, view); err != nil {
				t.Fatal(err)
			}
			base := tc.base
			store, err := New(db, Options{Now: func() time.Time { return now }, ReadPull: func(_ context.Context, repo string, pr int64) (map[string]any, error) {
				if repo != "owner/repo" || pr != 3 {
					t.Fatalf("wrong PR requested: %s #%d", repo, pr)
				}
				return map[string]any{
					"number": pr, "merged": true, "merge_commit_sha": head,
					"head": map[string]any{"ref": branch, "sha": head, "repo": map[string]any{"full_name": repo}},
					"base": map[string]any{"ref": base, "repo": map[string]any{"full_name": repo, "default_branch": "main"}},
				}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			check := ExecutionCheck{
				ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Requirement: "report",
				PlanSHA: plan.Fingerprint(), Head: head, Generation: 1, Status: "passed",
				Source: "checked comment", ObservedAt: now.Format(time.RFC3339Nano), Detail: "matched report body",
			}
			job, err := store.RecordExecutionCheck(ctx, id, check, "token:nas", "comment:27")
			if err != nil || job.Status != tc.want || job.Generation != 1 {
				t.Fatalf("job=%v err=%v want=%s", job, err, tc.want)
			}
			if tc.want == "merged" && (job.HoldReason != nil || job.View["executionWait"] != nil || job.Result["mergeCommit"] != head) {
				t.Fatalf("closed without usable merge evidence: %+v", job)
			}
			if tc.base != "main" {
				base = "main"
				job, err = store.RecordExecutionCheck(ctx, id, check, "token:nas", "comment:27")
				if err != nil || job.Status != "merged" || job.Result["mergeCommit"] != head {
					t.Fatalf("same evidence could not finish after confirming the PR: %v %v", job, err)
				}
				var events int
				if err := db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='execution_check'`, id).Scan(&events); err != nil || events != 1 {
					t.Fatalf("replay changed the number of observations: %d, %v", events, err)
				}
			}
		})
	}
}

func TestPostgresDispatchExecutionPlanPreference(t *testing.T) {
	for _, mode := range []string{"normal", "continuation", "unpublished"} {
		t.Run(mode, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now().UTC().Truncate(time.Millisecond)
			a := balanceAssignments(t)
			caps := `{"executionPlanVersion":1,"environment":{"race":true}}`
			if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity,capabilities) VALUES('gt12',true,$1,4,$2::jsonb),('nab-9s',true,$1,4,$2::jsonb)`, at, caps); err != nil {
				t.Fatal(err)
			}
			plan := executionFixture("fixed_test", "implementer", "verification")
			plan.Requirements[0].Capabilities = []string{"race"}
			view := map[string]any{"executionPlan": plan.Record()}
			var branch, head any
			if mode != "normal" {
				artifact := artifactJob("gt12", mode == "continuation")
				continuation := artifact.View["continuation"].(map[string]any)
				continuation["pending"] = true
				continuation["generation"] = 0
				continuation["host"] = "gt12"
				continuation["at"] = at.Format(time.RFC3339Nano)
				view["continuation"] = continuation
				branch, head = *artifact.Branch, *artifact.HeadSHA
			}
			raw, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,view) VALUES(1,'mofua-suki/issue-desk-intake',1,'queued',ARRAY['invalid.go'],'declared','{"executionPlan":{"version":999}}')`); err != nil {
				t.Fatal(err)
			}
			if _, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,branch,head_sha,view) VALUES(2,'mofua-suki/issue-desk-intake',2,'queued',ARRAY['valid.go'],'declared',$1,$2,$3::jsonb)`, branch, head, raw); err != nil {
				t.Fatal(err)
			}
			store, err := New(db, Options{Now: func() time.Time { return at }})
			if err != nil {
				t.Fatal(err)
			}
			check := func(want bool) {
				t.Helper()
				job, _, err := store.Next(ctx, "nab-9s", 4, a)
				if err != nil || (job != nil) != want || (job != nil && job.ID != 2) {
					t.Fatalf("job=%v want=%v err=%v", job, want, err)
				}
			}
			check(false)
			if _, err = db.ExecContext(ctx, `UPDATE hosts SET capabilities='{"executionPlanVersion":1,"environment":{"race":false}}' WHERE host='gt12'`); err != nil {
				t.Fatal(err)
			}
			check(mode != "unpublished")
		})
	}
}
