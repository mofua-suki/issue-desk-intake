package jobs

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"issue-desk-intake/server/internal/contract"
)

// connectPrerequisite runs ConnectPrerequisiteTx in its own transaction, as the
// production resolver does with its metadata.
func connectPrerequisite(ctx context.Context, s *Store, id, expectedGeneration, producerID int64) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		return s.ConnectPrerequisiteTx(ctx, tx, id, expectedGeneration, producerID)
	})
}

func prerequisiteFixture(t *testing.T) (*Store, func() (string, int64, []int64)) {
	t.Helper()
	db, ctx := testPostgres(t)
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		_, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view,result) VALUES($1::bigint,'owner/app',$1::integer,'test','queued','{}','{}')`, i)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = db.ExecContext(ctx, `UPDATE jobs SET status='held',hold_reason='question',generation=2,view='{"prerequisiteMissingGenerations":[2]}',result='{"status":"failed","prerequisiteMissing":{"artifacts":[{"artifact":"x","repo":"owner/app","issue":2}]}}' WHERE id=1`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES(1,3)`); err != nil {
		t.Fatal(err)
	}
	snapshot := func() (string, int64, []int64) {
		t.Helper()
		var status string
		var generation int64
		if err := db.QueryRowContext(ctx, `SELECT status,generation FROM jobs WHERE id=1`).Scan(&status, &generation); err != nil {
			t.Fatal(err)
		}
		rows, err := db.QueryContext(ctx, `SELECT depends_on FROM job_deps WHERE job_id=1 ORDER BY depends_on`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		deps := []int64{}
		for rows.Next() {
			var dep int64
			if err := rows.Scan(&dep); err != nil {
				t.Fatal(err)
			}
			deps = append(deps, dep)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return status, generation, deps
	}
	return s, snapshot
}

func TestPrerequisiteConnectAddsDependencyAndQueues(t *testing.T) {
	t.Run("commit", func(t *testing.T) {
		s, snapshot := prerequisiteFixture(t)
		if err := connectPrerequisite(t.Context(), s, 1, 2, 2); err != nil {
			t.Fatal(err)
		}
		status, generation, deps := snapshot()
		if status != "queued" || generation != 3 || !reflect.DeepEqual(deps, []int64{2, 3}) {
			t.Fatalf("status=%s generation=%d deps=%v", status, generation, deps)
		}
		if err := connectPrerequisite(t.Context(), s, 1, 2, 2); err == nil {
			t.Fatal("stale candidate accepted")
		}
	})
	t.Run("rollback after dependency insert", func(t *testing.T) {
		s, snapshot := prerequisiteFixture(t)
		_, err := s.db.ExecContext(t.Context(), `CREATE FUNCTION reject_queue() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='queued' THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_queue BEFORE UPDATE ON jobs FOR EACH ROW EXECUTE FUNCTION reject_queue()`)
		if err != nil {
			t.Fatal(err)
		}
		if err = connectPrerequisite(t.Context(), s, 1, 2, 2); err == nil {
			t.Fatal("expected update failure")
		}
		status, generation, deps := snapshot()
		if status != "held" || generation != 2 || !reflect.DeepEqual(deps, []int64{3}) {
			t.Fatalf("rollback: status=%s generation=%d deps=%v", status, generation, deps)
		}
	})
}

func TestPrerequisiteConnectRejectsCycle(t *testing.T) {
	for _, tc := range []struct{ name, edges string }{{"direct", "(2,1)"}, {"indirect", "(2,4),(4,1)"}} {
		t.Run(tc.name, func(t *testing.T) {
			s, snapshot := prerequisiteFixture(t)
			if _, err := s.db.ExecContext(t.Context(), `INSERT INTO job_deps(job_id,depends_on) VALUES `+tc.edges); err != nil {
				t.Fatal(err)
			}
			var before string
			if err := s.db.QueryRowContext(t.Context(), `SELECT string_agg(job_id || ':' || depends_on, ',' ORDER BY job_id,depends_on) FROM job_deps`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			err := connectPrerequisite(t.Context(), s, 1, 2, 2)
			var after string
			if e := s.db.QueryRowContext(t.Context(), `SELECT string_agg(job_id || ':' || depends_on, ',' ORDER BY job_id,depends_on) FROM job_deps`).Scan(&after); e != nil || after != before {
				t.Fatalf("all dependencies changed: before=%s after=%s err=%v", before, after, e)
			}
			var problem *contract.Problem
			if !errors.As(err, &problem) || problem.Status != 409 || problem.Code != "acceptance_transfer_cycle" {
				t.Fatalf("cycle: %v", err)
			}
			status, generation, deps := snapshot()
			if status != "held" || generation != 2 || !reflect.DeepEqual(deps, []int64{3}) {
				t.Fatalf("status=%s generation=%d deps=%v", status, generation, deps)
			}
		})
	}
}
