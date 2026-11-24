package issueapi

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/jobs"
)

func TestDependencyDisplayMatchesGatePostgres(t *testing.T) {
	f := newFixture(t)
	var predecessor int64
	if err := f.pg.QueryRow(`INSERT INTO jobs(repo,issue,status) VALUES('owner/repo',2,'queued') RETURNING id`).Scan(&predecessor); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pg.Exec(`INSERT INTO job_deps(job_id,depends_on) VALUES($1,$2)`, f.job, predecessor); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pg.Exec(`UPDATE jobs SET view='{"dependsOnIssues":[{"repo":"owner/repo","issue":2}]}' WHERE id=$1`, f.job); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status  string
		waiting bool
	}{{"merged", false}, {"closed", true}, {"queued", true}, {"running", true}, {"held", true}, {"failed", true}, {"stopped", true}} {
		t.Run(tc.status, func(t *testing.T) {
			_, err := f.pg.Exec(`UPDATE jobs SET status=$1, hold_reason=CASE WHEN $1='held' THEN 'question' ELSE NULL END WHERE id=$2`, tc.status, predecessor)
			if err != nil {
				t.Fatal(err)
			}
			detail, job, err := f.s.row(f.ctx, "owner/repo", f.api.issue)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := f.s.listSnapshot(f.ctx, f.options.Config)
			if err != nil {
				t.Fatal(err)
			}
			batch, _, err := f.s.row(context.WithValue(f.ctx, listSnapshotKey{}, snapshot), "owner/repo", f.api.issue)
			if err != nil {
				t.Fatal(err)
			}
			ready, err := jobs.DependenciesReady(f.ctx, f.pg, job.ID, job.View)
			if err != nil {
				t.Fatal(err)
			}
			if (detail["status"] == "dependency_waiting") != tc.waiting || detail["status"] != batch["status"] || detail["dependencyReason"] != batch["dependencyReason"] || ready == tc.waiting {
				t.Fatalf("detail=%v batch=%v ready=%v", detail, batch, ready)
			}
		})
	}
	for _, tc := range []struct {
		name, view string
		waiting    bool
	}{
		{"unrelated", fmt.Sprintf(`{"dependsOnIssues":[{"repo":"owner/repo","issue":2}],"dependencyReviews":{"owner/repo#2":{"relation":"unrelated","evidence":"checked","consultationId":"%s","at":"2026-11-06T00:00:00Z"}},"unused":%%d}`, jobs.TransferConsultationID(f.job, "owner/repo#2")), false},
		{"invalid evidence", fmt.Sprintf(`{"dependsOnIssues":[{"repo":"owner/repo","issue":2}],"dependencyReviews":{"owner/repo#2":{"relation":"unrelated","evidence":"","consultationId":"%s","at":"2026-11-06T00:00:00Z"}},"unused":%%d}`, jobs.TransferConsultationID(f.job, "owner/repo#2")), true},
		{"owned", `{"dependsOnIssues":[{"repo":"owner/repo","issue":2}],"acceptanceTransferWait":{"ownedTargets":{"%d":["key"]}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.pg.Exec(`UPDATE jobs SET status='closed' WHERE id=$1`, predecessor); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pg.Exec(`UPDATE jobs SET view=$1::jsonb WHERE id=$2`, fmt.Sprintf(tc.view, predecessor), f.job); err != nil {
				t.Fatal(err)
			}
			detail, job, err := f.s.row(f.ctx, "owner/repo", f.api.issue)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := f.s.listSnapshot(f.ctx, f.options.Config)
			if err != nil {
				t.Fatal(err)
			}
			batch, _, err := f.s.row(context.WithValue(f.ctx, listSnapshotKey{}, snapshot), "owner/repo", f.api.issue)
			if err != nil {
				t.Fatal(err)
			}
			ready, err := jobs.DependenciesReady(f.ctx, f.pg, job.ID, job.View)
			if err != nil || ready == tc.waiting || (detail["status"] == "dependency_waiting") != tc.waiting || detail["status"] != batch["status"] || detail["dependencyReason"] != batch["dependencyReason"] {
				t.Fatalf("detail=%v batch=%v ready=%v err=%v", detail, batch, ready, err)
			}
		})
	}
	for _, field := range []string{"dependsOnIssues", "unresolvedDependencies"} {
		for _, missing := range []string{"edge", "job"} {
			t.Run(fmt.Sprintf("%s/%s", field, missing), func(t *testing.T) {
				if _, err := f.pg.Exec(`DELETE FROM job_deps WHERE job_id=$1`, f.job); err != nil {
					t.Fatal(err)
				}
				issue := 2
				if missing == "job" {
					issue = 3
				}
				if _, err := f.pg.Exec(`UPDATE jobs SET view=$1::jsonb WHERE id=$2`, fmt.Sprintf(`{"%s":[{"repo":"owner/repo","issue":%d}]}`, field, issue), f.job); err != nil {
					t.Fatal(err)
				}
				detail, job, err := f.s.row(f.ctx, "owner/repo", f.api.issue)
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := f.s.listSnapshot(f.ctx, f.options.Config)
				if err != nil {
					t.Fatal(err)
				}
				batch, _, err := f.s.row(context.WithValue(f.ctx, listSnapshotKey{}, snapshot), "owner/repo", f.api.issue)
				if err != nil {
					t.Fatal(err)
				}
				ready, err := jobs.DependenciesReady(f.ctx, f.pg, job.ID, job.View)
				if err != nil {
					t.Fatal(err)
				}
				if ready || detail["status"] != "dependency_waiting" || detail["status"] != batch["status"] || detail["dependencyReason"] != batch["dependencyReason"] || !strings.Contains(fleet.Text(detail["dependencyReason"]), "辺が未作成") {
					t.Fatalf("detail=%v batch=%v ready=%v", detail, batch, ready)
				}
			})
		}
	}
}

func TestDependencyWaitsDetailPostgres(t *testing.T) {
	for _, name := range []string{"recorded", "unknown", "none"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			var pred int64
			if err := f.pg.QueryRow(`INSERT INTO jobs(repo,issue,status) VALUES('other/project',2,'running') RETURNING id`).Scan(&pred); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pg.Exec(`INSERT INTO job_deps(job_id,depends_on) VALUES($1,$2)`, f.job, pred); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "recorded":
				if _, err := f.pg.Exec(`UPDATE job_deps SET wait_started_at='2026-01-02T03:04:05Z', wait_unknown=false WHERE job_id=$1`, f.job); err != nil {
					t.Fatal(err)
				}
			case "unknown":
				if _, err := f.pg.Exec(`UPDATE job_deps SET wait_started_at=NULL, wait_unknown=true WHERE job_id=$1`, f.job); err != nil {
					t.Fatal(err)
				}
			case "none":
				if _, err := f.pg.Exec(`UPDATE jobs SET status='merged' WHERE id=$1`, pred); err != nil {
					t.Fatal(err)
				}
			}
			code, result := f.call("GET", "/api/issue?project=owner%2Frepo&number=1", "nas", nil)
			waits := fleet.Items(fleet.ObjectOrEmpty(result)["dependencyWaits"])
			if code != 200 {
				t.Fatalf("%d %v", code, result)
			}
			if name == "none" {
				if len(waits) != 0 || fleet.ObjectOrEmpty(result)["dependencyWaits"] == nil {
					t.Fatalf("%v", result)
				}
				if _, err := f.pg.Exec(`DELETE FROM job_deps WHERE job_id=$1`, f.job); err != nil {
					t.Fatal(err)
				}
				code, result = f.call("GET", "/api/issue?project=owner%2Frepo&number=1", "nas", nil)
				if code != 200 || fleet.ObjectOrEmpty(result)["dependencyWaits"] == nil || len(fleet.Items(fleet.ObjectOrEmpty(result)["dependencyWaits"])) != 0 {
					t.Fatalf("%d %v", code, result)
				}
				return
			}
			if len(waits) != 1 {
				t.Fatalf("%v", result)
			}
			wait := fleet.ObjectOrEmpty(waits[0])
			if wait["project"] != "other/project" || wait["number"] != float64(2) {
				t.Fatalf("%v", wait)
			}
			if name == "recorded" && (wait["startedAt"] != "2026-01-02T03:04:05Z" || wait["unknown"] != false) {
				t.Fatalf("%v", wait)
			}
			if name == "unknown" && (wait["startedAt"] != nil || wait["unknown"] != true) {
				t.Fatalf("%v", wait)
			}
		})
	}
}

func TestDependencyWaitsAbsentFromSummaryAndList(t *testing.T) {
	f := newFixture(t)
	f.api.issues = []Object{clone(f.api.issue)}
	for _, route := range []string{"/api/issue?project=owner%2Frepo&number=1&view=summary", "/api/issues", "/api/issues/search"} {
		code, value := f.call("GET", route, "nas", nil)
		if code != 200 {
			t.Fatalf("%s: %d %v", route, code, value)
		}
		rows := []any{value}
		if route == "/api/issues" {
			rows = fleet.Items(value)
		} else if route == "/api/issues/search" {
			rows = fleet.Items(fleet.ObjectOrEmpty(value)["issues"])
		}
		if len(rows) == 0 {
			t.Fatalf("%s: no rows: %v", route, value)
		}
		for _, row := range rows {
			if _, ok := fleet.ObjectOrEmpty(row)["dependencyWaits"]; ok {
				t.Fatalf("%s: %v", route, row)
			}
		}
	}
}
