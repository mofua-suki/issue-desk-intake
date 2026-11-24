package jobs

import (
	"encoding/json"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
)

func TestPostgresDispatchDependencyOrder(t *testing.T) {
	for _, tc := range []struct {
		name, jobs, edges string
		want              int64
	}{
		{"direct inherited", ` (1,'owner/repo',1,'queued',0,'2025-01-01'),(2,'owner/repo',2,'queued',0,'2025-01-02'),(3,'owner/repo',3,'queued',1,'2025-01-03')`, `(3,2)`, 2},
		{"inherited", ` (1,'owner/repo',1,'queued',0,'2025-01-01'),(2,'owner/repo',2,'queued',0,'2025-01-02'),(3,'owner/repo',3,'queued',1,'2025-01-03'),(4,'owner/repo',4,'queued',0,'2025-01-04')`, `(3,4),(4,2)`, 2},
		{"successor count", ` (1,'owner/repo',1,'queued',0,'2025-01-01'),(2,'owner/repo',2,'queued',0,'2025-01-02'),(3,'owner/repo',3,'queued',0,'2025-01-03'),(4,'owner/repo',4,'queued',0,'2025-01-04'),(5,'owner/repo',5,'queued',0,'2025-01-05')`, `(3,1),(4,2),(5,2)`, 2},
		{"equal count", ` (1,'owner/repo',1,'queued',0,'2025-01-01'),(2,'owner/repo',2,'queued',0,'2025-01-02'),(3,'owner/repo',3,'queued',0,'2025-01-03'),(4,'owner/repo',4,'queued',0,'2025-01-04')`, `(3,1),(4,2)`, 1},
		{"equal count mixed priority", ` (1,'owner/repo',1,'queued',0,'2025-01-01'),(2,'owner/repo',2,'queued',1,'2025-01-02'),(3,'owner/repo',3,'queued',1,'2025-01-03'),(4,'owner/repo',4,'queued',0,'2025-01-04')`, `(3,1),(4,2)`, 1},
		{"blocked successor", ` (1,'owner/prior',1,'queued',0,'2025-01-01'),(2,'owner/repo',2,'queued',1,'2025-01-02')`, `(2,1)`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now()
			if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,1)`, at); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,priority,filed_at) VALUES`+tc.jobs); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES`+tc.edges); err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":1,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw); err != nil {
				t.Fatal(err)
			}
			assignments, err := fleet.NormalizeAssignments(raw)
			if err != nil {
				t.Fatal(err)
			}
			s, err := New(db, Options{Now: func() time.Time { return at }})
			if err != nil {
				t.Fatal(err)
			}
			job, _, err := s.Next(ctx, "nas", 1, assignments)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == 0 {
				if job != nil {
					t.Fatalf("blocked job dispatched: %+v", job)
				}
				return
			}
			if job == nil || job.ID != tc.want {
				t.Fatalf("job=%+v want id %d", job, tc.want)
			}
			if job.Priority != 0 {
				t.Fatalf("priority changed: %d", job.Priority)
			}
			var priority int
			if err := db.QueryRowContext(ctx, `SELECT priority FROM jobs WHERE id=$1`, tc.want).Scan(&priority); err != nil || priority != 0 {
				t.Fatalf("stored priority=%d err=%v", priority, err)
			}
		})
	}
}

func TestDispatchBeforeDefersFollowups(t *testing.T) {
	rank := func(string) int { return 0 }
	order := map[int64]dispatchOrder{
		1: {priority: 0, filedRank: 1}, // old followup
		2: {priority: 0, filedRank: 2}, // newer normal issue
		3: {priority: 1, filedRank: 3}, // interrupt followup
		4: {priority: 0, successors: 1, filedRank: 4},
		5: {priority: 0, filedRank: 5},
	}
	followup := &Job{ID: 1, Kind: "followup"}
	normal := &Job{ID: 2, Kind: "normal"}
	interrupt := &Job{ID: 3, Kind: "followup"}
	awaited := &Job{ID: 4, Kind: "followup"}
	later := &Job{ID: 5, Kind: "normal"}
	if !dispatchBefore(normal, followup, order, rank, time.Now()) || dispatchBefore(followup, normal, order, rank, time.Now()) {
		t.Fatal("normal issue must precede an older followup of the same priority")
	}
	if !dispatchBefore(interrupt, normal, order, rank, time.Now()) {
		t.Fatal("higher priority must precede regardless of kind")
	}
	if !dispatchBefore(awaited, later, order, rank, time.Now()) {
		t.Fatal("a followup another job waits on must not be deferred")
	}
	if !dispatchBefore(normal, later, order, rank, time.Now()) {
		t.Fatal("filing order must remain among normal issues")
	}
}

func TestPostgresDispatchOrderReachability(t *testing.T) {
	db, ctx := testPostgres(t)
	// The unrelated component must not appear, and view references alone are not edges.
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,priority,filed_at,view) VALUES
 (1,'owner/repo',1,'queued',0,'2025-01-01','{}'),
 (2,'owner/other',2,'queued',1,'2025-01-02','{}'),
 (3,'owner/other',3,'queued',1,'2025-01-03','{}'),
 (4,'owner/repo',4,'queued',0,'2025-01-04','{}'),
 (5,'owner/other',5,'queued',1,'2025-01-05','{"dependsOnIssues":[{"repo":"owner/repo","issue":4}]}'),
 (6,'owner/other',6,'merged',1,'2025-01-06','{}'),
 (7,'owner/other',7,'closed',1,'2025-01-07','{}'),
 (8,'owner/other',8,'queued',1,'2025-01-08','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES (2,1),(3,2),(3,1),(1,3),(6,1),(7,1),(8,6)`)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	got, err := loadDispatchOrder(ctx, tx, []*Job{{ID: 1}, {ID: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != (dispatchOrder{priority: 1, successors: 2, filedRank: 1}) || got[4] != (dispatchOrder{priority: 0, filedRank: 2}) {
		t.Fatalf("unexpected candidate orders: %+v", got)
	}
}

func TestPostgresDispatchReadyStatusConsistency(t *testing.T) {
	db, ctx := testPostgres(t)
	// Evaluate the actual predecessor predicate and the successor traversal for
	// every status admitted by the shipped schema.
	statuses := []string{"queued", "running", "held", "failed", "stopped", "merged", "closed"}
	for i, status := range statuses {
		id := int64(i + 1)
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,priority,filed_at,hold_reason) VALUES($1,'owner/repo',$3,$2,1,'2025-01-01',CASE WHEN $2='held' THEN 'question' ELSE NULL END)`, id, status, int(id)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range statuses {
		id := int64(i + 1)
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,priority,filed_at) VALUES($1,'owner/root',$2,'queued',0,'2025-01-01')`, id+100, int(id+100)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES($1,$2)`, id, id+100); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i, status := range statuses {
		id := int64(i + 1)
		var ready bool
		if err := tx.QueryRowContext(ctx, `SELECT `+PredecessorReadySQL+` FROM jobs p WHERE p.id=$1`, id).Scan(&ready); err != nil {
			t.Fatal(err)
		}
		orders, err := loadDispatchOrder(ctx, tx, []*Job{{ID: id + 100}})
		if err != nil {
			t.Fatal(err)
		}
		if got := orders[id+100].successors; (got == 0) != ready {
			t.Errorf("status %s: ready=%v successors=%d", status, ready, got)
		}
	}
}

func TestPostgresAgedFollowupDispatch(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		age                  time.Duration
		interrupt, successor bool
		want                 int64
	}{
		{"before threshold", time.Hour - time.Microsecond, false, false, 2},
		{"at threshold", time.Hour, false, false, 1},
		{"interrupt", 2 * time.Hour, true, false, 2},
		{"successor", 2 * time.Hour, false, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now().UTC().Truncate(time.Microsecond)
			if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,1)`, at); err != nil {
				t.Fatal(err)
			}
			priority := 0
			if tc.interrupt {
				priority = 1
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,kind,status,priority,filed_at,scope,scope_source) VALUES
				(1,'owner/repo',1,'followup','queued',0,$1,ARRAY['a.go'],'declared'),
				(2,'owner/repo',2,'normal','queued',$2,$3,ARRAY['b.go'],'declared')`, at.Add(-tc.age), priority, at); err != nil {
				t.Fatal(err)
			}
			if tc.successor {
				if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,filed_at) VALUES(3,'other/repo',3,'queued',$1)`, at); err != nil {
					t.Fatal(err)
				}
			}
			if tc.successor {
				if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES(3,2)`); err != nil {
					t.Fatal(err)
				}
			}
			var raw map[string]any
			if err := json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":1,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw); err != nil {
				t.Fatal(err)
			}
			a, err := fleet.NormalizeAssignments(raw)
			if err != nil {
				t.Fatal(err)
			}
			s, err := New(db, Options{Now: func() time.Time { return at }})
			if err != nil {
				t.Fatal(err)
			}
			job, _, err := s.Next(ctx, "nas", 1, a)
			if err != nil || job == nil || job.ID != tc.want {
				t.Fatalf("got %+v err=%v, want %d", job, err, tc.want)
			}
			var stored int
			if err := db.QueryRowContext(ctx, `SELECT priority FROM jobs WHERE id=1`).Scan(&stored); err != nil || stored != 0 {
				t.Fatalf("priority=%d err=%v", stored, err)
			}
		})
	}
}

func TestPostgresAgedFollowupSurvivesContinuousArrivals(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,1)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,kind,status,filed_at,scope,scope_source) VALUES
		(1,'owner/repo',1,'followup','queued',$1,ARRAY['a.go'],'declared'),
		(2,'owner/repo',2,'followup','queued',$1,ARRAY['b.go'],'declared'),
		(3,'owner/repo',3,'followup','queued',$2,ARRAY['c.go'],'declared')`, at.Add(-2*time.Hour), at.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":1,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw); err != nil {
		t.Fatal(err)
	}
	a, err := fleet.NormalizeAssignments(raw)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 3; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,kind,status,filed_at,scope,scope_source) VALUES($1,'owner/repo',$3,'normal','queued',$2,ARRAY['normal.go'],'declared')`, 10+i, at, int(10+i)); err != nil {
			t.Fatal(err)
		}
		job, _, err := s.Next(ctx, "nas", 1, a)
		if err != nil || job == nil || job.ID != i {
			t.Fatalf("arrival %d: got %+v err=%v", i, job, err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE jobs SET status='closed',lease_host=NULL,lease_expires_at=NULL WHERE id=$1`, i); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgresAgedFollowupRespectsDispatchConstraints(t *testing.T) {
	for _, tc := range []struct {
		name, repo, scope string
		busy              bool
	}{
		{"scope held", "owner/repo", "src/a.go", true},
		{"unassigned repository", "owner/other", "free.go", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now().UTC().Truncate(time.Microsecond)
			if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,2)`, at); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,kind,status,filed_at,scope,scope_source) VALUES(1,$1,1,'followup','queued',$2,ARRAY[$3]::text[],'declared'),(2,'owner/repo',2,'normal','queued',$4,ARRAY['free.go'],'declared')`, tc.repo, at.Add(-2*time.Hour), tc.scope, at); err != nil {
				t.Fatal(err)
			}
			if tc.busy {
				if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,lease_host,lease_epoch,lease_expires_at) VALUES(3,'owner/repo',3,'running',ARRAY['src/'],'declared','other',1,$1)`, at.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			var raw map[string]any
			if err := json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":2,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw); err != nil {
				t.Fatal(err)
			}
			a, err := fleet.NormalizeAssignments(raw)
			if err != nil {
				t.Fatal(err)
			}
			s, err := New(db, Options{Now: func() time.Time { return at }})
			if err != nil {
				t.Fatal(err)
			}
			job, _, err := s.Next(ctx, "nas", 2, a)
			if err != nil || job == nil || job.ID != 2 {
				t.Fatalf("got %+v err=%v; expected eligible normal job", job, err)
			}
		})
	}
}
