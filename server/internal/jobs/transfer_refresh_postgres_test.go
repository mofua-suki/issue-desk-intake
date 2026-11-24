package jobs

import (
	"context"
	"database/sql"
	"errors"
	"issue-desk-intake/server/internal/fleet"

	"reflect"
	"testing"
	"time"
)

func TestPostgresTransferRefreshGraphReadsAndStableLock(t *testing.T) {
	db, ctx := testPostgres(t)
	source, destination, consumer := transferFixture()
	for _, j := range []*transferJob{source, destination, consumer} {
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view,result) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb)`, j.ID, j.Repo, j.Issue, j.Title, j.Status, transferJSON(j.View), transferJSON(j.Result)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES(3,1)`); err != nil {
		t.Fatal(err)
	}
	issueReads := 0
	s, err := New(db, Options{ReadIssue: func(ctx context.Context, repo string, number int64) (map[string]any, error) {
		issueReads++
		return map[string]any{"number": number, "body": "issue body"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	reads, lockedReads := 0, 0
	reader := func(ctx context.Context, q transferQuery) ([]*transferJob, error) {
		reads++
		if _, ok := q.(*sql.Tx); ok {
			lockedReads++
		}
		return readTransferGraph(ctx, q)
	}
	if err := s.refreshTransfersWithReader(ctx, reader); err != nil {
		t.Fatal(err)
	}
	if reads != 2 || lockedReads != 1 || issueReads != 1 {
		t.Fatalf("write refresh: reads=%d locked=%d issue reads=%d", reads, lockedReads, issueReads)
	}
	var checks int
	if err := db.QueryRowContext(ctx, `SELECT jsonb_array_length(view->'acceptanceTransferWait'->'checks') FROM jobs WHERE id=3`).Scan(&checks); err != nil || checks == 0 {
		t.Fatalf("missing transfer checks: %d %v", checks, err)
	}
	var body string
	var complete bool
	if err := db.QueryRowContext(ctx, `SELECT view->'acceptanceTransferInput'->>'body', (view->'acceptanceTransferInput'->>'complete')::boolean FROM jobs WHERE id=3`).Scan(&body, &complete); err != nil || body != "issue body" || !complete {
		t.Fatalf("missing complete issue body: %q complete=%v %v", body, complete, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `LOCK TABLE jobs,job_deps IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	reads, lockedReads, issueReads = 0, 0, 0
	if err := s.refreshTransfersWithReader(short, reader); err != nil {
		t.Fatalf("stable graph refresh under lock: %v", err)
	}
	if reads != 2 || lockedReads != 0 || issueReads != 1 {
		t.Fatalf("stable refresh: reads=%d locked=%d issue reads=%d", reads, lockedReads, issueReads)
	}
}

func TestPostgresTransferRefreshRechecksAfterIssueRead(t *testing.T) {
	db, ctx := testPostgres(t)
	source, destination, consumer := transferFixture()
	for _, j := range []*transferJob{source, destination, consumer} {
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view,result) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb)`, j.ID, j.Repo, j.Issue, j.Title, j.Status, transferJSON(j.View), transferJSON(j.Result)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES(3,1)`); err != nil {
		t.Fatal(err)
	}
	mutate := false
	readsAtMutation := -1
	reads, lockedReads := 0, 0
	s, err := New(db, Options{ReadIssue: func(ctx context.Context, repo string, number int64) (map[string]any, error) {
		if mutate {
			readsAtMutation = reads
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_set(view,'{acceptanceTransferWait,blocking}','false') WHERE id=3`); err != nil {
				return nil, err
			}
		}
		return map[string]any{"number": number, "body": "body"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshTransfers(ctx); err != nil {
		t.Fatal(err)
	}
	mutate = true
	reader := func(ctx context.Context, q transferQuery) ([]*transferJob, error) {
		reads++
		if _, ok := q.(*sql.Tx); ok {
			lockedReads++
		}
		return readTransferGraph(ctx, q)
	}
	if err := s.refreshTransfersWithReader(ctx, reader); err != nil {
		t.Fatal(err)
	}
	if readsAtMutation != 1 || reads != 3 || lockedReads != 1 {
		t.Fatalf("recheck refresh: reads at mutation=%d reads=%d locked=%d", readsAtMutation, reads, lockedReads)
	}
	var blocking bool
	if err := db.QueryRowContext(ctx, `SELECT (view->'acceptanceTransferWait'->>'blocking')::boolean FROM jobs WHERE id=3`).Scan(&blocking); err != nil || !blocking {
		t.Fatalf("concurrent change missed: blocking=%v err=%v", blocking, err)
	}
}

func TestPostgresNextRefreshesTransferGraph(t *testing.T) {
	db, ctx := testPostgres(t)
	source, destination, consumer := transferFixture()
	for _, j := range []*transferJob{source, destination, consumer} {
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view,result) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb)`, j.ID, j.Repo, j.Issue, j.Title, j.Status, transferJSON(j.View), transferJSON(j.Result)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES(3,1)`); err != nil {
		t.Fatal(err)
	}
	a, err := fleet.NormalizeAssignments(map[string]any{"pollSeconds": float64(300), "hosts": map[string]any{}, "repositories": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Next(ctx, "absent", 1, a); err != nil {
		t.Fatal(err)
	}
	var blocking bool
	if err := db.QueryRowContext(ctx, `SELECT (view->'acceptanceTransferWait'->>'blocking')::boolean FROM jobs WHERE id=3`).Scan(&blocking); err != nil || !blocking {
		t.Fatalf("Next did not refresh transfer: %v %v", blocking, err)
	}
}

func TestPostgresTransferRefreshLockAndReadErrors(t *testing.T) {
	db, ctx := testPostgres(t)
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view) VALUES(1,'r',1,'a','queued','{}')`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `LOCK TABLE jobs,job_deps IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err = s.RefreshTransfers(short); err != nil {
		t.Fatalf("unchanged refresh: %v", err)
	}
	// A stale wait requires a view update and therefore must wait for the lock.
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET view='{"acceptanceTransferWait":{"blocking":true}}' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `LOCK TABLE jobs,job_deps IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	blocked, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	defer stop()
	if err = s.RefreshTransfers(blocked); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write must wait: %v", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `ALTER TABLE jobs RENAME TO jobs_unavailable`); err != nil {
		t.Fatal(err)
	}
	defer db.ExecContext(ctx, `ALTER TABLE jobs_unavailable RENAME TO jobs`)
	if err = s.RefreshTransfers(ctx); err == nil {
		t.Fatal("refresh swallowed read error")
	}
	if _, err = s.TransferChecks(ctx); err == nil {
		t.Fatal("checks swallowed read error")
	}
}

func TestPostgresTransferProjectedPlanMatchesFull(t *testing.T) {
	db, ctx := testPostgres(t)
	source, destination, job := transferFixture()
	for _, j := range []*transferJob{source, destination, job} {
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,provider,view,result) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb)`, j.ID, j.Repo, j.Issue, j.Title, j.Status, "codex", transferJSON(j.View), transferJSON(j.Result)); err != nil {
			t.Fatal(err)
		}
		for _, dep := range j.Deps {
			if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES($1,$2)`, j.ID, dep); err != nil {
				t.Fatal(err)
			}
		}
	}
	fullInitial, err := readTransfers(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var initialJob *transferJob
	for _, j := range fullInitial {
		if j.ID == job.ID {
			initialJob = j
		}
	}
	initialPlan := newTransferIndex(fullInitial).plan(initialJob)
	key := fleet.Text(fleet.Object(initialPlan.Checks[0])["key"])
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_set(view,'{acceptanceTransferChecks}',$1::jsonb) WHERE id=$2`, transferJSON(map[string]any{key: map[string]any{"relation": "required", "evidence": "checked"}}), job.ID); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"closed", "merged"} {
		for _, commit := range []string{"", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET status=$1,result=$2::jsonb WHERE id=$3`, status, transferJSON(map[string]any{"mergeCommit": commit}), destination.ID); err != nil {
				t.Fatal(err)
			}
			full, err := readTransfers(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			projected, err := readTransferGraph(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			for i := range full {
				if full[i].Status != "queued" {
					continue
				}
				var other *transferJob
				for _, j := range projected {
					if j.ID == full[i].ID {
						other = j
					}
				}
				if other == nil || !reflect.DeepEqual(newTransferIndex(full).plan(full[i]), newTransferIndex(projected).plan(other)) {
					t.Fatalf("plans differ: %s %s", status, commit)
				}
			}
		}
	}
}

func TestPostgresTransferFullAndProjectedRefreshMatch(t *testing.T) {
	db, ctx := testPostgres(t)
	source, destination, consumer := transferFixture()
	consumer.View["dependsOnIssues"] = []any{map[string]any{"repo": source.Repo, "issue": source.Issue}}
	consumer.View["unresolvedDependencies"] = []any{map[string]any{"repo": source.Repo, "issue": source.Issue}}
	consumer.View["acceptanceTransferWait"] = map[string]any{"ownedTargets": map[string]any{"2": []any{"old"}}, "ownedPaths": map[string]any{"2": []any{[]any{"old"}}}}
	for _, j := range []*transferJob{source, destination, consumer} {
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,provider,view,result) VALUES($1,$2,$3,$4,$5,'codex',$6::jsonb,$7::jsonb)`, j.ID, j.Repo, j.Issue, j.Title, j.Status, transferJSON(j.View), transferJSON(j.Result)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES(3,1),(3,2)`); err != nil {
		t.Fatal(err)
	}
	for _, unavailable := range []bool{false, true} {
		if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_set(view,'{acceptanceTransferInput}',$1::jsonb) WHERE id=3`, transferJSON(map[string]any{"unavailable": unavailable})); err != nil {
			t.Fatal(err)
		}
		run := func(reader func(context.Context, transferQuery) ([]*transferJob, error)) (string, string) {
			t.Helper()
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			all, err := reader(ctx, tx)
			if err != nil {
				t.Fatal(err)
			}
			if err = applyTransferChanges(ctx, tx, all, nil, "2026-01-01T00:00:00Z"); err != nil {
				t.Fatal(err)
			}
			var view, deps string
			if err = tx.QueryRowContext(ctx, `SELECT view::text,(SELECT coalesce(json_agg(depends_on ORDER BY depends_on),'[]'::json)::text FROM job_deps WHERE job_id=3) FROM jobs WHERE id=3`).Scan(&view, &deps); err != nil {
				t.Fatal(err)
			}
			return view, deps
		}
		a, b := run(readTransfers)
		c, d := run(readTransferGraph)
		if a != c || b != d {
			t.Fatalf("refresh differs for unavailable=%v: %s %s / %s %s", unavailable, a, b, c, d)
		}
	}
}

func TestPostgresTransferChecksMatchesFullRead(t *testing.T) {
	db, ctx := testPostgres(t)
	for i, unavailable := range []bool{false, true} {
		view := map[string]any{"acceptanceTransferInput": map[string]any{"unavailable": unavailable}, "acceptanceTransferWait": map[string]any{"checks": []any{map[string]any{"key": "k", "relation": "unknown", "context": map[string]any{"criterion": "x"}}, map[string]any{"key": "done", "relation": "required"}, map[string]any{"key": "consult", "relation": "required", "consultationId": "c"}}}}
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view) VALUES($1::bigint,'r',$1::bigint,'title','queued',$2::jsonb)`, i+1, transferJSON(view)); err != nil {
			t.Fatal(err)
		}
	}
	for i, status := range []string{"merged", "closed"} {
		view := map[string]any{"acceptanceTransferWait": map[string]any{"checks": []any{map[string]any{"key": "excluded", "relation": "unknown"}}}}
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view) VALUES($1::bigint,'r',$1::bigint,'title',$2,$3::jsonb)`, i+3, status, transferJSON(view)); err != nil {
			t.Fatal(err)
		}
	}
	all, err := readTransfers(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	expected := []TransferCheck{}
	for _, j := range all {
		if j.Status != "queued" || fleet.Object(j.View["acceptanceTransferInput"])["unavailable"] == true {
			continue
		}
		for _, raw := range fleet.List(fleet.Object(j.View["acceptanceTransferWait"])["checks"]) {
			c := fleet.Object(raw)
			relation, consultation := fleet.Text(c["relation"]), fleet.Text(c["consultationId"])
			if relation != "unknown" && consultation == "" {
				continue
			}
			expected = append(expected, TransferCheck{JobID: j.ID, Repo: j.Repo, Provider: j.Provider, Issue: j.Issue, Key: fleet.Text(c["key"]), Context: fleet.Object(c["context"]), Relation: relation, ConsultationID: consultation})
		}
	}
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.TransferChecks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range got {
		if check.Key == "excluded" || check.JobID == 3 || check.JobID == 4 {
			t.Fatalf("merged (job 3) or closed (job 4) check included: %#v", check)
		}
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("checks differ: %#v %#v", got, expected)
	}
}
