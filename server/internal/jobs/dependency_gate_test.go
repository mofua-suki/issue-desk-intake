package jobs

import (
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
	"time"
)

func TestPostgresExplicitDependenciesCannotBypassMissingEdges(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "nas", 100, 1)
	if _, e := db.ExecContext(ctx, `UPDATE jobs SET status='queued',view='{"unresolvedDependencies":[{"repo":"owner/repo","issue":101}]}' WHERE id=$1`, id); e != nil {
		t.Fatal(e)
	}
	check := func(expected bool) {
		t.Helper()
		tx, e := db.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		job, e := scanJob(tx.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id=$1", id))
		if e != nil {
			t.Fatal(e)
		}
		ready, e := explicitDependenciesReady(ctx, tx, job)
		if e != nil || ready != expected {
			t.Fatalf("ready=%v expected=%v err=%v", ready, expected, e)
		}
	}
	check(false)
	predecessor := seedJob(t, ctx, db, "owner/repo", "nas", 101, 2)
	if _, e := db.ExecContext(ctx, "UPDATE jobs SET status='merged' WHERE id=$1", predecessor); e != nil {
		t.Fatal(e)
	}
	check(false)
	if _, e := db.ExecContext(ctx, "INSERT INTO job_deps(job_id,depends_on) VALUES($1,$2)", id, predecessor); e != nil {
		t.Fatal(e)
	}
	check(true)
	if _, e := db.ExecContext(ctx, `UPDATE jobs SET view='{"unresolvedDependencies":null}' WHERE id=$1`, id); e != nil {
		t.Fatal(e)
	}
	check(false)
	if _, e := db.ExecContext(ctx, `UPDATE jobs SET view='{"unresolvedDependencies":[{"repo":"owner/repo","issue":101}]}' WHERE id=$1`, id); e != nil {
		t.Fatal(e)
	}
	if _, e := db.ExecContext(ctx, "UPDATE jobs SET status='queued' WHERE id=$1", predecessor); e != nil {
		t.Fatal(e)
	}
	check(false)
}

func TestPostgresDispatchPredecessorStates(t *testing.T) {
	for _, tc := range []struct {
		name, status, view string
		ready              bool
	}{{"merged", "merged", `{}`, true}, {"closed", "closed", `{}`, false}, {"unrelated", "closed", fmt.Sprintf(`{"dependencyReviews":{"owner/prior#1":{"relation":"unrelated","evidence":"checked","consultationId":"%s","at":"2026-11-06T00:00:00Z"}}}`, TransferConsultationID(2, "owner/prior#1")), true}, {"invalid", "closed", `{"dependencyRelations":{"1":{"relation":"unrelated","evidence":""}}}`, false}, {"owned", "closed", `{"acceptanceTransferWait":{"ownedTargets":{"1":["key"]}}}`, true}, {"queued", "queued", `{}`, false}, {"running", "running", `{}`, false}, {"held", "held", `{}`, false}, {"failed", "failed", `{}`, false}, {"stopped", "stopped", `{}`, false}} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now()
			if _, err := db.Exec(`INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,1)`, at); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO jobs(id,repo,issue,status,hold_reason) VALUES(1,'owner/prior',1,$1,CASE WHEN $1='held' THEN 'question' ELSE NULL END),(2,'owner/repo',20,'queued',NULL)`, tc.status); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE jobs SET view=$1::jsonb WHERE id=2`, tc.view); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO job_deps(job_id,depends_on) VALUES(2,1)`); err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			_ = json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":1,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw)
			a, err := fleet.NormalizeAssignments(raw)
			if err != nil {
				t.Fatal(err)
			}
			s, _ := New(db, Options{Now: func() time.Time { return at }})
			if tc.name == "unrelated" {
				if _, err := db.Exec(`INSERT INTO jobs(id,repo,issue,status) VALUES(3,'owner/other',3,'queued'); INSERT INTO job_deps(job_id,depends_on) VALUES(2,3)`); err != nil {
					t.Fatal(err)
				}
				job, _, err := s.Next(ctx, "nas", 1, a)
				if err != nil || job != nil {
					t.Fatalf("other dependency: job=%v err=%v", job, err)
				}

				source, destination, consumer := transferFixture()
				if _, err := db.Exec(`UPDATE jobs SET status='merged' WHERE id=3`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO jobs(id,repo,issue,status,view,result) VALUES(10,$1,$2,$3,$4::jsonb,$5::jsonb),(11,$6,$7,$8,$9::jsonb,$10::jsonb)`, source.Repo, source.Issue, source.Status, transferJSON(source.View), transferJSON(source.Result), destination.Repo, destination.Issue, destination.Status, transferJSON(destination.View), transferJSON(destination.Result)); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO job_deps(job_id,depends_on) VALUES(2,10)`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`UPDATE jobs SET view=view || $1::jsonb WHERE id=2`, transferJSON(consumer.View)); err != nil {
					t.Fatal(err)
				}
				job, _, err = s.Next(ctx, "nas", 1, a)
				if err != nil || job != nil {
					t.Fatalf("transfer wait: job=%v err=%v", job, err)
				}
				if _, err := db.Exec(`DELETE FROM job_deps WHERE job_id=2 AND depends_on=10; UPDATE jobs SET view=view-'acceptanceTransferWait' WHERE id=2`); err != nil {
					t.Fatal(err)
				}
			}
			job, _, err := s.Next(ctx, "nas", 1, a)
			if err != nil || (job != nil) != tc.ready {
				t.Fatalf("job=%v err=%v", job, err)
			}
		})
	}
}

func TestPostgresDispatchRejectsMissingExplicitEdges(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now()
	if _, err := db.Exec(`INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,1)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO jobs(id,repo,issue,status) VALUES(1,'owner/prior',1,'closed'),(2,'owner/repo',2,'queued')`); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	_ = json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":1,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw)
	a, err := fleet.NormalizeAssignments(raw)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := New(db, Options{Now: func() time.Time { return at }})
	for _, field := range []string{"dependsOnIssues", "unresolvedDependencies"} {
		for _, issue := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s/%d", field, issue), func(t *testing.T) {
				view := fmt.Sprintf(`{"%s":[{"repo":"owner/prior","issue":%d}]}`, field, issue)
				if _, err := db.Exec(`UPDATE jobs SET view=$1::jsonb WHERE id=2`, view); err != nil {
					t.Fatal(err)
				}
				job, _, err := s.Next(ctx, "nas", 1, a)
				if err != nil || job != nil {
					t.Fatalf("job=%v err=%v", job, err)
				}
			})
		}
	}
}

func TestPostgresDependencyPairGate(t *testing.T) {
	db, ctx := testPostgres(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status) VALUES(1,'owner/prior',1,'closed'),(2,'owner/repo',2,'queued'); INSERT INTO job_deps(job_id,depends_on) VALUES(2,1)`); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	validID := TransferConsultationID(2, "owner/prior#1")
	record := func(relation, evidence, id, at string) string {
		data, _ := json.Marshal(map[string]any{"dependencyReviews": map[string]any{"owner/prior#1": map[string]any{"relation": relation, "evidence": evidence, "consultationId": id, "at": at}}})
		return string(data)
	}
	for _, tc := range []struct {
		name, status, view string
		ready              bool
	}{
		{"merged", "merged", `{}`, true},
		{"closed missing", "closed", `{}`, false},
		{"verified merge", "closed", `{"dependencyMerges":{"1":{"pr":12,"mergeCommit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}`, true},
		{"invalid commit", "closed", `{"dependencyMerges":{"1":{"pr":12,"mergeCommit":"abc"}}}`, false},
		{"missing pr", "closed", `{"dependencyMerges":{"1":{"mergeCommit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}`, false},
		{"legacy", "closed", `{"dependencyRelations":{"1":{"relation":"unrelated","evidence":"reason"}}}`, false},
		{"required", "closed", record("required", "reason", validID, "2026-11-06T00:00:00Z"), false},
		{"unrelated", "closed", record("unrelated", "reason", validID, "2026-11-06T00:00:00Z"), true},
		{"max UTF16", "closed", record("unrelated", strings.Repeat("😀", 2000), validID, "2026-11-06T00:00:00Z"), true},
		{"too long UTF16", "closed", record("unrelated", strings.Repeat("😀", 2001), validID, "2026-11-06T00:00:00Z"), false},
		{"empty evidence", "closed", record("unrelated", " \t", validID, "2026-11-06T00:00:00Z"), false},
		{"wrong consultation", "closed", record("unrelated", "reason", TransferConsultationID(3, "owner/prior#1"), "2026-11-06T00:00:00Z"), false},
		{"invalid timestamp", "closed", record("unrelated", "reason", validID, "2026-02-30T00:00:00Z"), false},
		{"invalid record", "closed", `{"dependencyReviews":{"owner/prior#1":"unrelated"}}`, false},
		{"legacy required", "closed", `{"dependencyRelations":{"1":{"relation":"required","evidence":"reason"}}}`, false},
		{"legacy empty evidence", "closed", `{"dependencyRelations":{"1":{"relation":"unrelated","evidence":" "}}}`, false},
		{"legacy invalid evidence", "closed", `{"dependencyRelations":{"1":{"relation":"unrelated","evidence":true}}}`, false},
		{"legacy invalid record", "closed", `{"dependencyRelations":{"1":"unrelated"}}`, false},
		{"owned", "closed", `{"acceptanceTransferWait":{"ownedTargets":{"1":["key"]}}}`, true},
		{"running unrelated", "running", record("unrelated", "reason", validID, "2026-11-06T00:00:00Z"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET status=$1 WHERE id=1`, tc.status); err != nil {
				t.Fatal(err)
			}
			view := `{"dependsOnIssues":[{"repo":"owner/prior","issue":1}]}`
			var v map[string]any
			if err := json.Unmarshal([]byte(tc.view), &v); err != nil {
				t.Fatal(err)
			}
			var refs map[string]any
			_ = json.Unmarshal([]byte(view), &refs)
			v["dependsOnIssues"] = refs["dependsOnIssues"]
			data, _ := json.Marshal(v)
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=$1::jsonb WHERE id=2`, string(data)); err != nil {
				t.Fatal(err)
			}
			ready, err := DependenciesReady(ctx, db, 2, v)
			if err != nil || ready != tc.ready {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
			merges, err := s.DependencyMerges(ctx, 2)
			if err != nil || (len(merges) > 0) != tc.ready {
				t.Fatalf("merges=%v err=%v", merges, err)
			}
		})
	}
	t.Run("worker cannot authorize", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence('jobs','id'), 2)`); err != nil {
			t.Fatal(err)
		}
		successor := seedJob(t, ctx, db, "owner/repo", "nas", 3, 7)
		s, err := New(db, Options{Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE jobs SET status='closed' WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES($1,1)`, successor); err != nil {
			t.Fatal(err)
		}
		answer := map[string]any{"owner/prior#1": map[string]any{"relation": "unrelated", "evidence": "reason", "consultationId": TransferConsultationID(successor, "owner/prior#1"), "at": "2026-11-06T00:00:00Z"}}
		merge := map[string]any{"1": map[string]any{"pr": 12, "mergeCommit": strings.Repeat("a", 40)}}
		if _, err := s.Report(ctx, successor, "nas", Report{Epoch: 7, Status: "running", View: map[string]any{"dependencyReviews": answer, "dependencyMerges": merge, "reviewHolds": 2}}); err != nil {
			t.Fatal(err)
		}
		job, err := s.Get(ctx, successor)
		if err != nil {
			t.Fatal(err)
		}
		job.View["dependsOnIssues"] = []any{map[string]any{"repo": "owner/prior", "issue": float64(1)}}
		ready, err := DependenciesReady(ctx, db, successor, job.View)
		if err != nil || ready {
			t.Fatalf("worker report released dependency: ready=%v err=%v", ready, err)
		}
	})

}
