package issueapi

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
)

// The issue list keeps its decoded jobs between requests, so a list, a search
// or a detail must leave every job as the row says. A request that writes into
// a job (or into a value of its view or result that it put into a row) makes
// the kept job differ from a new read of the table.
func TestListLeavesSharedJobsUnchangedPostgres(t *testing.T) {
	f := newFixture(t)
	head := strings.Repeat("a", 40)
	view := fmt.Sprintf(`{
		"workType":"feature","phase":"publishing","releasePending":true,"publishPending":true,
		"priority":{"source":"user","reason":"急ぎ","by":"nas"},
		"error":{"message":"m"},"dependencyReason":"待ち",
		"executionSettings":{"provider":"codex","model":{"name":"m","effort":["high"]}},
		"overLimitApprovals":[{"kind":"runs","by":{"host":"nas"}}],
		"executionWait":{"reason":"枠待ち","hosts":["nas","other"]},
		"executionMissingCapabilities":["gpu",{"name":"docker"}],
		"executionReportError":{"message":"届かない","codes":[1,2]},
		"scopeWait":{"note":"n","waitFor":[{"issue":2,"status":"running"}]},
		"dispatchSkip":{"reason":"scope","host":"nas"},
		"dispatchRecord":{"events":[{"kind":"leased","host":"nas"}]},
		"review":{"kind":"ai","provider":"codex","model":"m","effort":"high","basis":"override","overrideUntil":"2026-11-02T00:00:00Z","head":"%[1]s","providerDisable":{"from":"codex","until":"2026-11-02T00:00:00Z"}},
		"deletionApproval":{"head":"%[1]s","total":2,"items":[{"kind":"rename","path":"a","from":"b","identical":true},{"kind":"delete","path":"c"}]},
		"acceptanceTransferWait":{"blocking":true,"waits":[{"artifacts":["doc"],"source":"s","destination":{"project":"owner/repo","number":2},"reason":"r","consultationId":"c"}]},
		"dependsOnIssues":[{"repo":"owner/repo","issue":2}],
		"workerExecution":{"implementationRuns":3,"runs":[{"n":1}]},
		"permissionHold":{"paths":["AGENTS.md"]},
		"continuation":{"pending":true},
		"prerequisiteMissingGenerations":[0,1]
	}`, head)
	result := `{
		"status":"failed","summary":"要約","questions":["質問1","質問2"],"unverified":["未確認"],
		"incomplete":{"items":["残り",{"path":"a"}]},
		"prerequisiteMissing":{"issues":[{"repo":"owner/repo","issue":2}]},
		"fixedTestCause":{"target":{"failed":["TestA"]},"baselines":[{"failed":["TestA"]}],"acquisition":"取得"},
		"review":{"decision":"hold","summary":"s","findings":["f"],"userJudgments":["判断"],"followups":["後"]},
		"previousReview":{"decision":"hold","findings":["g"]},
		"error":"失敗の理由","reason":"lease_expired","times":3
	}`
	if _, err := f.pg.ExecContext(f.ctx, `UPDATE jobs SET priority=1, pr=5, branch='b', head_sha=$2, view=$3::jsonb, result=$4::jsonb WHERE id=$1`, f.job, head, view, result); err != nil {
		t.Fatal(err)
	}
	// Each case reaches different branches of the row projection.
	for _, tc := range []struct {
		name, status, hold, issue, shown string
	}{
		{"queued", "queued", "", "open", "dependency_waiting"},
		{"running", "running", "", "open", "publishing"},
		{"question", "held", "question", "open", "prerequisite_waiting"},
		{"review", "held", "review_held", "open", "review_held"},
		{"deletions", "held", "deletions", "open", "review_held"},
		{"scope wait", "held", "scope_wait", "open", "scope_waiting"},
		{"failed", "failed", "", "open", "failed"},
		{"stopped", "stopped", "", "open", "stopped"},
		{"merged", "merged", "", "closed", "completed"},
		{"closed", "closed", "", "closed", "closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hold, host, until any
			if tc.hold != "" {
				hold = tc.hold
			}
			if tc.status == "running" {
				host, until = "nas", f.options.Now().Add(time.Hour)
			}
			if _, err := f.pg.ExecContext(f.ctx, `UPDATE jobs SET status=$2, hold_reason=$3, lease_host=$4, lease_epoch=1, lease_expires_at=$5 WHERE id=$1`, f.job, tc.status, hold, host, until); err != nil {
				t.Fatal(err)
			}
			f.api.issue["state"] = tc.issue
			kept, err := f.options.Jobs.AllForIssues(f.ctx)
			if err != nil || len(kept) != 1 {
				t.Fatalf("jobs before the requests: %v %v", kept, err)
			}
			code, value := f.call("GET", "/api/issues", "screen", nil)
			rows := fleet.Items(value)
			if code != 200 || len(rows) != 1 || fleet.ObjectOrEmpty(rows[0])["status"] != tc.shown {
				t.Fatalf("list %d: %v", code, value)
			}
			for _, path := range []string{"/api/issues/search?limit=100", "/api/issues/search?state=" + tc.issue + "&limit=100&q=%E5%88%A9%E7%94%A8%E8%80%85", "/api/issue?project=owner/repo&number=1"} {
				if code, value = f.call("GET", path, "nas", nil); code != 200 {
					t.Fatalf("%s %d: %v", path, code, value)
				}
			}
			if _, err = f.s.List(f.ctx); err != nil {
				t.Fatal(err)
			}
			snapshot, err := f.s.listSnapshot(f.ctx, f.options.Config)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = f.s.row(context.WithValue(f.ctx, listSnapshotKey{}, snapshot), "owner/repo", f.api.issue); err != nil {
				t.Fatal(err)
			}
			after, err := f.options.Jobs.AllForIssues(f.ctx)
			if err != nil || len(after) != 1 {
				t.Fatalf("jobs after the requests: %v %v", after, err)
			}
			// The requests above must have used the kept job, or this test proves nothing.
			if after[0] != kept[0] {
				t.Fatal("the list decoded the unchanged job again")
			}
			reader, err := jobs.New(f.pg, jobs.Options{AppDB: f.options.AppDB, Now: f.options.Now})
			if err != nil {
				t.Fatal(err)
			}
			rowsNow, err := reader.AllForIssues(f.ctx)
			if err != nil || len(rowsNow) != 1 {
				t.Fatalf("new read: %v %v", rowsNow, err)
			}
			if !reflect.DeepEqual(after[0], rowsNow[0]) {
				t.Fatalf("a request changed the kept job:\nkept=%+v\nrow=%+v", after[0], rowsNow[0])
			}
		})
	}
}
