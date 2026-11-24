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

	"issue-desk-intake/server/internal/fleet"
)

// A pre-artifact row already has a head before dispatch. Historical leases use
// the persisted shape emitted by Next; the current lease is always issued by Next.
func legacyArtifactFixture(t *testing.T, previous int64, status string) (*sql.DB, context.Context, *Store, *fleet.Assignments, int64) {
	t.Helper()
	db, ctx := testPostgres(t)
	_, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('one',true,$1,1),('two',true,$1,1)`, now)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err = json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"one":{"maxImplementations":1,"repositories":{"owner/repo":{"rank":1}}},"two":{"maxImplementations":1,"repositories":{"owner/repo":{"rank":2}}}},"repositories":{}}`), &raw); err != nil {
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
	var id int64
	err = db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,branch,head_sha,lease_host,lease_epoch,view)
VALUES('owner/repo',1,$1,'old-branch',$2,'one',$3,'{"continuation":{"host":"one"}}') RETURNING id`, status, strings.Repeat("a", 40), previous).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data)
SELECT $1,'one','leased',$2,jsonb_build_object('epoch',n,'generation',0) FROM generate_series(1,$3::int) AS n ORDER BY n`, id, now.Add(-time.Hour), previous)
	if err != nil {
		t.Fatal(err)
	}
	return db, ctx, s, a, id
}

func takeLegacyArtifact(t *testing.T, ctx context.Context, s *Store, a *fleet.Assignments, id int64, host string) *Job {
	t.Helper()
	job, lease, err := s.Next(ctx, host, 1, a)
	if err != nil || job == nil || job.ID != id || lease == nil {
		t.Fatalf("dispatch: job=%+v lease=%+v err=%v", job, lease, err)
	}
	if !legacyArtifactMissing(job) || stringValue(job.HeadSHA) != strings.Repeat("a", 40) {
		t.Fatalf("dispatch changed legacy tuple: %+v", job)
	}
	return job
}

func legacyArtifactReport(epoch int64, status string) Report {
	head, branch := strings.Repeat("b", 40), "new-branch"
	return Report{Epoch: epoch, Status: status, HeadSHA: Optional[string]{Present: true, Value: &head}, Branch: Optional[string]{Present: true, Value: &branch},
		View: map[string]any{"continuation": map[string]any{"host": "fake", "artifact": map[string]any{"owner": "fake"}, "publication": map[string]any{"verified": true}}}}
}

func assertLegacyArtifactSaved(t *testing.T, ctx context.Context, s *Store, id int64, report Report) *Job {
	t.Helper()
	saved, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := artifactOf(saved)
	if !ok || identity != (artifactIdentity{saved.Generation, *report.Branch.Value, *report.HeadSHA.Value, "one"}) || saved.Status != report.Status || !same(saved.HoldReason, report.HoldReason) || artifactPublished(saved, identity) {
		t.Fatalf("saved tuple/state: %+v, artifact=%+v", saved, identity)
	}
	return saved
}

func TestPostgresLegacyArtifactStateReports(t *testing.T) {
	for _, status := range []string{"running", "merged", "failed", "stopped", "held", "queued"} {
		t.Run(status, func(t *testing.T) {
			db, ctx, s, a, id := legacyArtifactFixture(t, 2, "queued")
			job := takeLegacyArtifact(t, ctx, s, a, id, "one")
			r := legacyArtifactReport(job.LeaseEpoch, status)
			if status == "held" {
				reason := "review_held"
				r.HoldReason = &reason
			}
			if _, err := s.Report(ctx, id, "one", r); err != nil {
				t.Fatal(err)
			}
			saved := assertLegacyArtifactSaved(t, ctx, s, id, r)
			if (saved.LeaseExpiresAt != nil) != (status == "running") {
				t.Fatalf("lease release: %+v", saved)
			}
			if _, err := s.Report(ctx, id, "one", r); err != nil {
				t.Fatalf("resend: %v", err)
			}
			_, _, err := s.Renew(ctx, id, "one", r.Epoch, nil)
			var states int
			if e := db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='state'`, id).Scan(&states); e != nil {
				t.Fatal(e)
			}
			if status == "running" {
				if err != nil || states != 0 {
					t.Fatalf("checkpoint lost lease: %v, states=%d", err, states)
				}
			} else {
				code(t, err, 409, "lease_lost")
				if states != 1 {
					t.Fatalf("resend duplicated receipt: %d", states)
				}
			}
		})
	}
}

func TestPostgresLegacyArtifactControlRetryReportedConditions(t *testing.T) {
	// These numbers label synthetic reproductions of the reported conditions,
	// not copies of the four production jobs or proof of their recovery.
	for i, number := range []int{5473, 5497, 5498, 5507} {
		t.Run(fmt.Sprint(number), func(t *testing.T) {
			db, ctx, s, a, id := legacyArtifactFixture(t, 0, "queued")
			for n := 0; n < 3; n++ {
				takeLegacyArtifact(t, ctx, s, a, id, "one")
				if _, err := db.ExecContext(ctx, `UPDATE jobs SET lease_expires_at=$2 WHERE id=$1`, id, now); err != nil {
					t.Fatal(err)
				}
				if err := s.Sweep(ctx, func(string) []string { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			failed, err := s.Get(ctx, id)
			if err != nil || failed.Status != "failed" || failed.Result["leaseExpiredCount"] != float64(3) || !legacyArtifactMissing(failed) {
				t.Fatalf("three expired leases: %+v %v", failed, err)
			}
			job, err := s.Control(ctx, id, ControlInput{ID: "22222222-2222-4222-8222-222222222222", Action: "retry", By: "issue-screen"}, ControlOptions{})
			if err != nil || job.Status != "queued" || job.Generation != 1 || !legacyArtifactMissing(job) {
				t.Fatalf("ordinary retry: %+v %v", job, err)
			}
			job = takeLegacyArtifact(t, ctx, s, a, id, "one")
			if job.LeaseEpoch != 4 {
				t.Fatalf("retry lease epoch: %d", job.LeaseEpoch)
			}
			r := legacyArtifactReport(job.LeaseEpoch, []string{"running", "failed", "queued", "merged"}[i])
			if _, err = s.Report(ctx, id, "one", r); err != nil {
				t.Fatal(err)
			}
			assertLegacyArtifactSaved(t, ctx, s, id, r)
		})
	}
}

func TestPostgresLegacyArtifactRejectsUnprovenOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, sql, host, problem string
		epoch                    int64
	}{
		{name: "no history", sql: `DELETE FROM job_events WHERE job_id=$1 AND kind='leased'`},
		{name: "missing first", sql: `DELETE FROM job_events WHERE job_id=$1 AND kind='leased' AND data->>'epoch'='1'`},
		{name: "missing middle", sql: `DELETE FROM job_events WHERE job_id=$1 AND kind='leased' AND data->>'epoch'='2'`},
		{name: "missing current", sql: `DELETE FROM job_events WHERE job_id=$1 AND kind='leased' AND data->>'epoch'='3'`},
		{name: "past different PC", sql: `UPDATE job_events SET host='two' WHERE job_id=$1 AND kind='leased' AND data->>'epoch'='1'`},
		{name: "unknown host", sql: `UPDATE job_events SET host=NULL WHERE job_id=$1 AND kind='leased' AND data->>'epoch'='1'`},
		{name: "duplicate", sql: `INSERT INTO job_events(job_id,host,kind,data) VALUES($1,'one','leased','{"epoch":3}')`},
		{name: "future epoch", sql: `INSERT INTO job_events(job_id,host,kind,data) VALUES($1,'one','leased','{"epoch":4}')`},
		{name: "reordered epochs", sql: `UPDATE job_events SET data=jsonb_build_object('epoch',4-(data->>'epoch')::int) WHERE job_id=$1 AND kind='leased'`},
		{name: "missing epoch", sql: `UPDATE job_events SET data='{}' WHERE job_id=$1 AND kind='leased' AND data->>'epoch'='1'`},
		{name: "string epoch", sql: `UPDATE job_events SET data='{"epoch":"1"}' WHERE job_id=$1 AND kind='leased' AND data->>'epoch'='1'`},
		{name: "fractional epoch", sql: `UPDATE job_events SET data='{"epoch":1.5}' WHERE job_id=$1 AND kind='leased' AND data->>'epoch'='1'`},
		{name: "null artifact", sql: `UPDATE jobs SET view='{"continuation":{"artifact":null}}' WHERE id=$1`},
		{name: "empty artifact", sql: `UPDATE jobs SET view='{"continuation":{"artifact":{}}}' WHERE id=$1`},
		{name: "wrong head", sql: `UPDATE jobs SET view=jsonb_build_object('continuation',jsonb_build_object('artifact',jsonb_build_object('generation',0,'owner','one','headSha',repeat('c',40),'branch',branch))) WHERE id=$1`},
		{name: "future generation", sql: `UPDATE jobs SET view=jsonb_build_object('continuation',jsonb_build_object('artifact',jsonb_build_object('generation',generation+1,'owner','one','headSha',head_sha,'branch',branch))) WHERE id=$1`},
		{name: "known other owner", sql: `UPDATE jobs SET view=jsonb_build_object('continuation',jsonb_build_object('artifact',jsonb_build_object('generation',generation,'owner','two','headSha',head_sha,'branch',branch))) WHERE id=$1`},
		{name: "malformed continuation", sql: `UPDATE jobs SET view='{"continuation":"invalid"}' WHERE id=$1`},
		{name: "wrong reporter", problem: "lease_lost", host: "two", sql: `UPDATE jobs SET lease_host='one' WHERE id=$1`},
		{name: "old epoch", epoch: 2, problem: "lease_lost"},
		{name: "expired lease", sql: `UPDATE jobs SET lease_expires_at=$2 WHERE id=$1`, problem: "lease_lost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx, s, a, id := legacyArtifactFixture(t, 2, "queued")
			host := tc.host
			if host == "" {
				host = "one"
			}
			if host == "two" {
				// Model reassignment when the previous PC is unavailable.
				if _, err := db.ExecContext(ctx, `UPDATE hosts SET ready=false WHERE host='one'`); err != nil {
					t.Fatal(err)
				}
			}
			job := takeLegacyArtifact(t, ctx, s, a, id, host)
			if tc.sql != "" {
				args := []any{id}
				if tc.name == "expired lease" {
					args = append(args, now)
				}
				if _, err := db.ExecContext(ctx, tc.sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			epoch := job.LeaseEpoch
			if tc.epoch != 0 {
				epoch = tc.epoch
			}
			problem := tc.problem
			if problem == "" {
				problem = "artifact_unavailable"
			}
			for _, status := range []string{"running", "failed", "queued"} {
				_, err = s.Report(ctx, id, host, legacyArtifactReport(epoch, status))
				code(t, err, 409, problem)
				after, err := s.Get(ctx, id)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("rejected report changed saved job: before=%+v after=%+v err=%v", before, after, err)
				}
			}
		})
	}
}

func TestPostgresLegacyArtifactFullHistoryAndBranchOnly(t *testing.T) {
	db, ctx, s, a, id := legacyArtifactFixture(t, 101, "queued")
	job := takeLegacyArtifact(t, ctx, s, a, id, "one")
	r := legacyArtifactReport(job.LeaseEpoch, "running")
	r.HeadSHA.Value = job.HeadSHA
	if _, err := s.Report(ctx, id, "one", r); err != nil {
		t.Fatal(err)
	}
	assertLegacyArtifactSaved(t, ctx, s, id, r)
	// A different PC hidden before the recent 100 events must still block rescue.
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=view-'continuation' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE job_events SET host='two' WHERE job_id=$1 AND kind='leased' AND data->>'epoch'='1'`, id); err != nil {
		t.Fatal(err)
	}
	_, err := s.Report(ctx, id, "one", legacyArtifactReport(job.LeaseEpoch, "failed"))
	code(t, err, 409, "artifact_unavailable")
}

func TestPostgresLegacyArtifactTransactionRollback(t *testing.T) {
	db, ctx, s, a, id := legacyArtifactFixture(t, 0, "queued")
	job := takeLegacyArtifact(t, ctx, s, a, id, "one")
	before, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("reject merged hook")
	s.options.OnMerged = func(context.Context, *sql.Tx, *Job, map[string]any) error { return sentinel }
	_, err = s.Report(ctx, id, "one", legacyArtifactReport(job.LeaseEpoch, "merged"))
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected rollback: %v", err)
	}
	after, err := s.Get(ctx, id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed transaction persisted artifact/state: %+v %v", after, err)
	}
	var states int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='state'`, id).Scan(&states); err != nil || states != 0 {
		t.Fatalf("rollback left receipt: %d %v", states, err)
	}
}

func TestPostgresLegacyArtifactRetryKeepsInvalidRecord(t *testing.T) {
	for _, raw := range []string{`{"artifact":null}`, `{"artifact":{}}`, `"invalid"`} {
		t.Run(raw, func(t *testing.T) {
			db, ctx, s, a, id := legacyArtifactFixture(t, 2, "failed")
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_build_object('continuation',$2::jsonb) WHERE id=$1`, id, raw); err != nil {
				t.Fatal(err)
			}
			job, err := s.Control(ctx, id, ControlInput{ID: "22222222-2222-4222-8222-222222222222", Action: "retry", By: "issue-screen"}, ControlOptions{})
			if err != nil || legacyArtifactMissing(job) {
				t.Fatalf("retry lost invalid record: %+v %v", job, err)
			}
			job, _, err = s.Next(ctx, "one", 1, a)
			if err != nil || job == nil {
				t.Fatalf("dispatch: %+v %v", job, err)
			}
			// No PC can prove the invalid record, so dispatch restarts from the base.
			if fleet.Object(job.View["retryFromBase"])["by"] != artifactResetBy {
				t.Fatalf("dispatch kept an unreportable artifact: %#v", job.View)
			}
			r := legacyArtifactReport(job.LeaseEpoch, "failed")
			if _, err = s.Report(ctx, id, "one", r); err != nil {
				t.Fatal(err)
			}
			assertLegacyArtifactSaved(t, ctx, s, id, r)
		})
	}
}
