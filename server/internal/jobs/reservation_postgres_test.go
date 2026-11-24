package jobs

import (
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
	"time"
)

func TestPostgresPublicationResumeAvoidsScopeReimplementation(t *testing.T) {
	for _, scenario := range []string{"pr-only", "owner", "published", "different-head", "different-generation", "old-artifact", "instruction"} {
		t.Run(scenario, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := now
			_, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,2)`, at)
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err = json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":2,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw); err != nil {
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
			owner := "other"
			if scenario == "owner" {
				owner = "nas"
			}
			id := seedJob(t, ctx, db, "owner/repo", owner, 2, 7)
			head, branch := strings.Repeat("a", 40), "chat/publish"
			_, err = s.Report(ctx, id, owner, Report{Epoch: 7, Status: "running", Branch: Optional[string]{Present: true, Value: &branch}, HeadSHA: Optional[string]{Present: true, Value: &head}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.ExecContext(ctx, `UPDATE jobs SET status='held',hold_reason='review_held',pr=9,scope=ARRAY['server/a.go'],view=view || '{"publishPending":true}'::jsonb WHERE id=$1`, id)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "pr-only" && scenario != "owner" {
				operation := "44444444-4444-4444-8444-444444444444"
				identity := map[string]any{"id": operation, "identity": map[string]any{"head": head}, "approvedBy": "screen"}
				hold := map[string]any{"project": "owner/repo", "head": head, "pr": int64(9), "generation": int64(0), "epoch": int64(7), "files": []any{"host-ssh.json"}}
				if _, err = s.PermissionApproval(ctx, id, identity, hold, false); err != nil {
					t.Fatal(err)
				}
				if _, err = s.PermissionReceipt(ctx, id, operation, map[string]any{"issuePosted": true, "prPosted": true}); err != nil {
					t.Fatal(err)
				}
				if _, err = s.PermissionApproval(ctx, id, identity, hold, true); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err = db.ExecContext(ctx, `UPDATE jobs SET status='queued',hold_reason=NULL WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES($1,$2,'leased',$3,'{"epoch":7,"generation":0}')`, id, owner, at.Add(-time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "different-head":
				_, err = db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_set(view,'{continuation,publication,headSha}',to_jsonb($2::text)) WHERE id=$1`, id, strings.Repeat("b", 40))
			case "different-generation":
				_, err = db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_set(view,'{continuation,publication,generation}','1') WHERE id=$1`, id)
			case "old-artifact":
				_, err = db.ExecContext(ctx, `UPDATE jobs SET generation=1 WHERE id=$1`, id)
			case "instruction":
				_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES($1,'nas','control',$2,'{"action":"instruction"}')`, id, at)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.ExecContext(ctx, `INSERT INTO jobs(repo,issue,status,scope,lease_host,lease_epoch,lease_expires_at,view) VALUES('owner/repo',1,'running',ARRAY['server/'],'other',1,$1,'{}')`, at.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			job, _, err := s.Next(ctx, "nas", 2, a)
			if err != nil {
				t.Fatal(err)
			}
			want := scenario == "owner" || scenario == "published"
			if (job != nil) != want || job != nil && job.ID != id {
				t.Fatalf("Next = %v, want dispatched=%v", job, want)
			}
			saved, err := s.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if saved.View["publicationResume"] != want {
				t.Fatalf("saved publicationResume = %v, want %v", saved.View["publicationResume"], want)
			}
		})
	}
}

func TestPostgresEstimatedWildcardReservationAndDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, source, scope, busyScope string
		wantHolder, wantReservation    bool
		wantPicked                     int64
	}{
		{"test-only holder", "declared", "tests/shared.go", "tests/shared.go", false, true, 2},
		{"test-only reservation", "declared", "src/shared_test.go", "", false, true, 2},
		{"estimated wildcard holder", "estimated", "*", "busy/", false, false, 3},
		{"estimated wildcard reservation", "estimated", "*", "", false, false, 2},
		{"declared wildcard holder", "declared", "*", "busy/", true, false, 3},
		{"widened wildcard holder", "widened", "*", "busy/", true, false, 3},
		{"estimated path holder", "estimated", "src/", "src/busy.go", true, false, 3},
		{"estimated path reservation", "estimated", "src/", "", false, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now().UTC().Truncate(time.Millisecond)
			if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,3)`, at); err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":3,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw); err != nil {
				t.Fatal(err)
			}
			a, err := fleet.NormalizeAssignments(raw)
			if err != nil {
				t.Fatal(err)
			}
			// 優先度0は1より配布順が低く、古い優先度1の保持者によって待機する。
			if _, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,priority,filed_at,scope,scope_source,view) VALUES
				(2,'owner/repo',2,'waiting','queued',CASE WHEN $3='estimated' AND $2='*' THEN 0 ELSE 1 END,$1,ARRAY[$2]::text[],$3,'{}'),
				(3,'owner/repo',3,'candidate','queued',0,$4,ARRAY['src/candidate.go'],'declared','{}')`, at.Add(-time.Hour), tc.scope, tc.source, at.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			if tc.busyScope != "" {
				if _, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,scope,scope_source,lease_host,lease_epoch,lease_expires_at,view) VALUES(1,'owner/repo',1,'busy','running',ARRAY[$1]::text[],'declared','other',1,$2,'{}')`, tc.busyScope, at.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			queued, err := dispatchJobs(ctx, tx, "WHERE status='queued'")
			if err != nil {
				t.Fatal(err)
			}
			running, err := dispatchJobs(ctx, tx, "WHERE status='running'")
			if err != nil {
				t.Fatal(err)
			}
			r, err := newReservation(ctx, tx, queued, running, a, "nas", at)
			if err != nil {
				t.Fatal(err)
			}
			if (r.holders["owner/repo"] != nil) != tc.wantHolder || (len(r.reservations) > 0) != tc.wantReservation {
				t.Fatalf("holder=%v reservations=%v", r.holders, r.reservations)
			}
			var candidate *Job
			for _, q := range queued {
				if q.ID == 3 {
					candidate = q
				}
			}
			deferred, err := r.deferJob(candidate, "")
			if err != nil {
				t.Fatal(err)
			}
			wantDeferred := tc.wantReservation
			if strings.HasPrefix(tc.name, "test-only") {
				wantDeferred = false // A reservation can exist without blocking an independent candidate.
			}
			if deferred != wantDeferred {
				t.Fatalf("deferred=%v", deferred)
			}
			var recorded int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=3 AND kind='dispatch_skip' AND data->>'reason'='scope_reserved' AND data->>'by'='2'`).Scan(&recorded); err != nil {
				t.Fatal(err)
			}
			if (recorded > 0) != deferred {
				t.Fatalf("deferred=%v recorded=%d", deferred, recorded)
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			s, _ := New(db, Options{Now: func() time.Time { return at }})
			job, _, err := s.Next(ctx, "nas", 3, a)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantPicked == 0 && job != nil || tc.wantPicked != 0 && (job == nil || job.ID != tc.wantPicked) {
				t.Fatalf("picked %v, want %d", job, tc.wantPicked)
			}
			var skips int
			if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE kind='dispatch_skip' AND data->>'reason'='scope_reserved' AND data->>'by'='2'`).Scan(&skips); err != nil {
				t.Fatal(err)
			}
			if tc.source == "estimated" && tc.scope == "*" && skips != 0 {
				t.Fatalf("estimated wildcard produced %d scope_reserved events", skips)
			}
			if tc.wantHolder && skips != 0 {
				t.Fatal("blocked holder recorded scope_reserved")
			}
		})
	}
}

func TestEstimatedWildcard(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		scope        []string
		want         bool
	}{
		{"estimated wildcard", "estimated", []string{"*"}, true},
		{"wildcard among paths", "estimated", []string{"src/", "*"}, true},
		{"estimated path", "estimated", []string{"src/"}, false},
		{"estimated empty", "estimated", nil, false},
		{"declared wildcard", "declared", []string{"*"}, false},
		{"widened wildcard", "widened", []string{"*"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := estimatedWildcard(&Job{ScopeSource: tc.source, Scope: tc.scope}); got != tc.want {
				t.Fatalf("estimatedWildcard = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResetsReservationAge(t *testing.T) {
	for _, tc := range []struct {
		name, kind, key, value string
		want                   bool
	}{
		{"requeued", "requeued", "from", "scope_wait", true},
		{"state queued", "state", "to", "queued", true},
		{"state running", "state", "to", "running", false},
		{"retry", "control", "action", "retry", true},
		{"resume", "control", "action", "resume", true},
		{"approve deletions", "control", "action", "approve_deletions", true},
		{"permission approval", "control", "action", "permission_approval", true},
		{"instruction", "control", "action", "instruction", false},
		{"leased", "leased", "", "", false},
		{"lease expired queued", "lease_expired", "to", "queued", false},
		{"lease expired failed", "lease_expired", "to", "failed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resetsReservationAge(tc.kind, map[string]any{tc.key: tc.value}); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPostgresLeaseExpiryPreservesReservationAge(t *testing.T) {
	for _, busy := range []bool{false, true} {
		name := "dispatch"
		if busy {
			name = "holder"
		}
		t.Run(name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now().UTC().Truncate(time.Millisecond)
			if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,3)`, at); err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":3,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw); err != nil {
				t.Fatal(err)
			}
			a, err := fleet.NormalizeAssignments(raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,filed_at,scope,view) VALUES
				(1,'owner/repo',1,'A','queued',$1,ARRAY['src/'],'{}'),
				(2,'owner/repo',2,'B','queued',$2,ARRAY['src/'],'{}')`, at.Add(-2*time.Hour), at.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES
				(1,'nas','leased',$1,'{}'),(1,NULL,'lease_expired',$2,'{"generation":0,"to":"queued"}')`, at.Add(-45*time.Minute), at.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			if busy {
				if _, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,scope,lease_host,lease_epoch,lease_expires_at,view) VALUES(3,'owner/repo',3,'busy','running',ARRAY['src/busy.go'],'other',1,$1,'{}')`, at.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			queued, err := dispatchJobs(ctx, tx, "WHERE status='queued'")
			if err != nil {
				t.Fatal(err)
			}
			running, err := dispatchJobs(ctx, tx, "WHERE status='running'")
			if err != nil {
				t.Fatal(err)
			}
			r, err := newReservation(ctx, tx, queued, running, a, "nas", at)
			if err != nil {
				t.Fatal(err)
			}
			if busy && (r.holders["owner/repo"] == nil || r.holders["owner/repo"].ID != 1) {
				t.Fatalf("holders = %v", r.holders)
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if !busy {
				s, _ := New(db, Options{Now: func() time.Time { return at }})
				job, _, err := s.Next(ctx, "nas", 3, a)
				if err != nil || job == nil || job.ID != 1 {
					t.Fatalf("Next = %v, %v", job, err)
				}
				var skips int
				if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=1 AND kind='dispatch_skip' AND data->>'reason'='scope_reserved' AND data->>'by'='2'`).Scan(&skips); err != nil || skips != 0 {
					t.Fatalf("skips = %d, %v", skips, err)
				}
			}
		})
	}
}

func TestPostgresLeaseExpiryPreservesScopeWaitMark(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,filed_at,scope,view) VALUES(1,'owner/repo',1,'A','queued',$1,ARRAY['src/'],'{}')`, at.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var mark int64
	if err := db.QueryRowContext(ctx, `INSERT INTO job_events(job_id,kind,at,data) VALUES(1,'requeued',$1,'{"from":"scope_wait"}') RETURNING seq`, at.Add(-time.Hour)).Scan(&mark); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES(1,'nas','leased',$1,'{}'),(1,NULL,'lease_expired',$2,'{"generation":0,"to":"queued"}')`, at.Add(-30*time.Minute), at.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":3,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw); err != nil {
		t.Fatal(err)
	}
	a, err := fleet.NormalizeAssignments(raw)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	queued, err := dispatchJobs(ctx, tx, "WHERE status='queued'")
	if err != nil {
		t.Fatal(err)
	}
	r, err := newReservation(ctx, tx, queued, nil, a, "nas", at)
	if err != nil {
		t.Fatal(err)
	}
	if age := r.ages[1]; age.Mark != mark || !age.Since.Equal(at.Add(-time.Hour)) {
		t.Fatalf("age = %+v, want mark %d and scope wait time", age, mark)
	}
}

func TestPostgresReservationAgeLimit(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		age                         time.Duration
		reset                       bool
		busy                        bool
		wantHolder, wantReservation bool
		picked                      int64
	}{
		{"holder below", 2*time.Hour - time.Millisecond, false, true, true, false, 3},
		{"holder boundary", 2 * time.Hour, false, true, true, false, 3},
		{"holder exceeded", 2*time.Hour + time.Millisecond, false, true, false, false, 3},
		{"running overlap remains blocked", 2*time.Hour + time.Millisecond, false, true, false, false, 0},
		{"holder reset", 3 * time.Hour, true, true, true, false, 3},
		{"reservation boundary", 2 * time.Hour, false, false, false, true, 2},
		{"reservation exceeded", 2*time.Hour + time.Millisecond, false, false, false, false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now().UTC().Truncate(time.Millisecond)
			if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,3)`, at); err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":3,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw); err != nil {
				t.Fatal(err)
			}
			a, err := fleet.NormalizeAssignments(raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,priority,filed_at,scope,scope_source,view) VALUES
    (2,'owner/repo',2,'waiting','queued',1,$1,ARRAY['src/'],'declared','{}'),
    (3,'owner/repo',3,'candidate','queued',0,$2,ARRAY['src/candidate.go'],'declared','{}')`, at.Add(-tc.age), at.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			if tc.reset {
				if _, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at,data) VALUES(2,'control',$1,'{"action":"retry"}')`, at.Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.busy {
				if _, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,scope,lease_host,lease_epoch,lease_expires_at,view) VALUES(1,'owner/repo',1,'busy','running',ARRAY['src/busy.go'],'other',1,$1,'{}')`, at.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "running overlap remains blocked" {
				if _, err = db.ExecContext(ctx, `UPDATE jobs SET scope=ARRAY['src/busy.go'] WHERE id=3`); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			queued, err := dispatchJobs(ctx, tx, "WHERE status='queued'")
			if err != nil {
				t.Fatal(err)
			}
			running, err := dispatchJobs(ctx, tx, "WHERE status='running'")
			if err != nil {
				t.Fatal(err)
			}
			r, err := newReservation(ctx, tx, queued, running, a, "nas", at)
			if err != nil {
				t.Fatal(err)
			}
			if (r.holders["owner/repo"] != nil) != tc.wantHolder || (len(r.reservations) > 0) != tc.wantReservation {
				t.Fatalf("holders=%v reservations=%v", r.holders, r.reservations)
			}
			var candidate *Job
			for _, q := range queued {
				if q.ID == 3 {
					candidate = q
				}
			}
			deferred, err := r.deferJob(candidate, "")
			if err != nil {
				t.Fatal(err)
			}
			if deferred != tc.wantReservation {
				t.Fatalf("deferred=%v", deferred)
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			s, _ := New(db, Options{Now: func() time.Time { return at }})
			job, _, err := s.Next(ctx, "nas", 3, a)
			if err != nil {
				t.Fatal(err)
			}
			if tc.picked == 0 && job != nil || tc.picked != 0 && (job == nil || job.ID != tc.picked) {
				t.Fatalf("picked=%v want=%d", job, tc.picked)
			}
			var status string
			if err = db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id=2`).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if tc.busy && status != "queued" {
				t.Fatalf("blocked job status=%s", status)
			}
			var skips int
			if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=3 AND kind='dispatch_skip' AND data->>'reason'='scope_reserved' AND data->>'by'='2'`).Scan(&skips); err != nil {
				t.Fatal(err)
			}
			if tc.busy && skips != 0 {
				t.Fatalf("skips=%d", skips)
			}
		})
	}
}

func TestPostgresDispatchSkipDedupKeysAndBoundary(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status) VALUES (1,'owner/repo',1,'queued')`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	job := &Job{ID: 1}
	record := func(host, reason string, by int64, preferred string, when time.Time) {
		t.Helper()
		data := map[string]any{"reason": reason}
		if by != 0 {
			data["by"] = by
		}
		if preferred != "" {
			data["preferredHost"] = preferred
		}
		if err := recordDispatchSkip(ctx, tx, job, host, data, when); err != nil {
			t.Fatal(err)
		}
	}
	record("a", "overlap", 2, "", at)
	record("a", "overlap", 2, "", at.Add(time.Minute))
	record("a", "overlap", 3, "", at.Add(time.Minute))
	record("b", "overlap", 2, "", at.Add(time.Minute))
	record("a", "preference", 0, "b", at)
	record("a", "preference", 0, "c", at)
	record("a", "dependency", 0, "", at)
	record("a", "no_slot", 0, "", at)
	if err := recordDispatchSkip(ctx, tx, job, "a", map[string]any{"reason": "no_slot", "slots": 99}, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	reservationA := &reservation{ctx: ctx, tx: tx, host: "a", at: at}
	reservationB := &reservation{ctx: ctx, tx: tx, host: "b", at: at.Add(time.Minute)}
	if err := reservationA.record(job, 9, false); err != nil {
		t.Fatal(err)
	}
	if err := reservationB.record(job, 9, false); err != nil {
		t.Fatal(err)
	}
	if err := reservationB.record(job, 9, true); err != nil {
		t.Fatal(err)
	}

	record("a", "preference", 0, "b", at.Add(5*time.Minute))
	record("a", "dependency", 0, "", at.Add(5*time.Minute))
	record("a", "no_slot", 0, "", at.Add(5*time.Minute))
	record("a", "overlap", 2, "", at.Add(5*time.Minute))
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=1 AND kind='dispatch_skip'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 13 {
		t.Fatalf("got %d dispatch skips, want 13", count)
	}
}

func TestPostgresIneligibleReservationDoesNotRecordRelease(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	a := balanceAssignments(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4),('nab-9s',true,$1,4)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,filed_at,scope,scope_source,view) VALUES
 (1,'mofua-suki/issue-desk-intake',1,'queued',$1,ARRAY['src/'],'declared',jsonb_build_object('continuation',jsonb_build_object('pending',true,'generation',0,'host','nab-9s','at',$3::text))),
 (2,'mofua-suki/issue-desk-intake',2,'queued',$2,ARRAY['src/a.go'],'declared','{}')`, at.Add(-time.Hour), at.Add(-time.Minute), at.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	s, _ := New(db, Options{Now: func() time.Time { return at }})
	job, _, err := s.Next(ctx, "gt12", 4, a)
	if err != nil || job == nil || job.ID != 2 {
		t.Fatalf("fallback %v %v", job, err)
	}
	rows, err := db.QueryContext(ctx, `SELECT data FROM job_events WHERE job_id=2 AND kind='dispatch_skip' AND data->>'reason'='scope_reserved' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var values []map[string]any
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var d map[string]any
		if err = json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		values = append(values, d)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 {
		t.Fatalf("records %v", values)
	}
}

func TestPostgresReservationReimplementation(t *testing.T) {
	for _, scenario := range []string{"rework", "busy", "normal", "expired", "higher-priority", "old-generation", "age-limit", "estimated-wildcard"} {
		t.Run(scenario, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now().UTC().Truncate(time.Millisecond)
			if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,3)`, at); err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":3,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw); err != nil {
				t.Fatal(err)
			}
			a, err := fleet.NormalizeAssignments(raw)
			if err != nil {
				t.Fatal(err)
			}
			filed := at.Add(-20 * time.Minute)
			if scenario == "age-limit" {
				filed = at.Add(-3 * time.Hour)
			}
			if _, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,priority,filed_at,scope,scope_source,lease_host,lease_epoch,lease_expires_at,view) VALUES
			(1,'owner/repo',1,'A','running',0,$1,ARRAY['src/'],'declared','nas',1,$3,'{"reimplementExpected":{"generation":0,"reimplements":true}}'),
			(2,'owner/repo',2,'B','queued',0,$2,ARRAY['src/a.go'],'declared',NULL,0,NULL,'{}')`, filed, at.Add(-10*time.Minute), at.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES(1,'nas','leased',$1,'{"epoch":1,"generation":0}')`, at.Add(-15*time.Minute)); err != nil {
				t.Fatal(err)
			}
			s, err := New(db, Options{Now: func() time.Time { return at }})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "normal" || scenario == "old-generation" {
				view := `{"reimplementExpected":{"generation":0,"reimplements":false}}`
				if scenario == "old-generation" {
					view = `{"reimplementExpected":{"generation":1,"reimplements":true}}`
				}
				if _, err = db.ExecContext(ctx, `UPDATE jobs SET view=$2::jsonb WHERE id=$1`, 1, view); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "expired" {
				if _, err = db.ExecContext(ctx, `UPDATE jobs SET status='queued',lease_expires_at=NULL WHERE id=1`); err != nil {
					t.Fatal(err)
				}
				if _, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at,data) VALUES(1,'lease_expired',$1,'{"to":"queued"}')`, at); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err = s.Report(ctx, 1, "nas", Report{Epoch: 1, Status: "queued"}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "busy" {
				if _, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,lease_host,lease_epoch,lease_expires_at,view) VALUES(3,'owner/repo',3,'running',ARRAY['src/'],'other',1,$1,'{}')`, at.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "higher-priority" {
				if _, err = db.ExecContext(ctx, `UPDATE jobs SET priority=1 WHERE id=2`); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "estimated-wildcard" {
				if _, err = db.ExecContext(ctx, `UPDATE jobs SET scope=ARRAY['*'],scope_source='estimated' WHERE id=1`); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			queued, err := dispatchJobs(ctx, tx, "WHERE status='queued'")
			if err != nil {
				t.Fatal(err)
			}
			running, err := dispatchJobs(ctx, tx, "WHERE status='running'")
			if err != nil {
				t.Fatal(err)
			}
			r, err := newReservation(ctx, tx, queued, running, a, "nas", at)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "normal" || scenario == "old-generation" {
				if !r.ages[1].Since.Equal(at) || len(r.reservations) != 0 {
					t.Fatalf("normal age=%v reservations=%v", r.ages[1], r.reservations)
				}
			}
			if scenario == "expired" && (!r.ages[1].Since.Equal(filed) || len(r.reservations) != 0) {
				t.Fatalf("expired age=%v reservations=%v", r.ages[1], r.reservations)
			}
			if scenario == "rework" && (!r.ages[1].Since.Equal(filed) || len(r.reservations) != 1) {
				t.Fatalf("rework age=%v reservations=%v", r.ages[1], r.reservations)
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			job, _, err := s.Next(ctx, "nas", 3, a)
			if err != nil {
				t.Fatal(err)
			}
			want := int64(2)
			if scenario != "higher-priority" {
				want = 1
			}
			if scenario == "busy" {
				want = 0
			}
			if want == 0 && job != nil || want != 0 && (job == nil || job.ID != want) {
				t.Fatalf("Next=%v want=%d", job, want)
			}
			var n int
			if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=2 AND kind='dispatch_skip' AND data->>'reason'='scope_reserved' AND data->>'by'='1'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if (n > 0) != (scenario == "rework") {
				t.Fatalf("scope_reserved count=%d", n)
			}
			if scenario == "busy" {
				var status string
				if err = db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id=3`).Scan(&status); err != nil || status != "running" {
					t.Fatalf("busy status=%s err=%v", status, err)
				}
				if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=1 AND kind='dispatch_skip' AND data->>'reason'='overlap' AND data->>'by'='3'`).Scan(&n); err != nil || n == 0 {
					t.Fatalf("overlap=%d err=%v", n, err)
				}
			}
		})
	}
}
