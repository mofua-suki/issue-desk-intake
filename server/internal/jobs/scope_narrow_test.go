package jobs

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
)

func TestPostgresNarrowReleasesWaitersAndPreservesFence(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now()
	s, _ := New(db, Options{Now: func() time.Time { return at }})
	_, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,2)`, at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,scope,scope_source,lease_host,lease_epoch,lease_expires_at,hold_reason) VALUES
 (1,'owner/repo',1,'busy','running',ARRAY['src/','other','other2'], 'declared','other',1,$1,NULL),
 (2,'owner/repo',2,'free','queued',ARRAY['other'], 'declared',NULL,0,NULL,NULL),
 (3,'owner/repo',3,'held free','held',ARRAY['other2'], 'declared',NULL,0,NULL,'scope_wait'),
 (4,'owner/repo',4,'held busy','held',ARRAY['src/a'], 'declared',NULL,0,NULL,'scope_wait')`, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Narrow(ctx, 1, "other", 2, []string{"src/a"})
	code(t, err, 409, "lease_lost")
	for _, paths := range [][]string{nil, {"*"}, {"src2/a"}, {"src/", "absent"}, make([]string, 201)} {
		_, err = s.Narrow(ctx, 1, "other", 1, paths)
		code(t, err, 400, "")
	}
	result, err := s.Narrow(ctx, 1, "other", 1, []string{"SRC/a", "src/b", "SRC/a"})
	if err != nil || !reflect.DeepEqual(result.Job.Scope, []string{"SRC/a", "src/b"}) || result.Result != "granted" {
		t.Fatalf("narrow: %+v %v", result, err)
	}
	var source string
	var data []byte
	if err = db.QueryRowContext(ctx, `SELECT scope_source FROM jobs WHERE id=1`).Scan(&source); err != nil || source != "declared" {
		t.Fatalf("source %s %v", source, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT data FROM job_events WHERE job_id=1 AND kind='scope_narrowed'`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var event map[string][]string
	if err = json.Unmarshal(data, &event); err != nil || !reflect.DeepEqual(event["before"], []string{"src/", "other", "other2"}) || !reflect.DeepEqual(event["after"], []string{"SRC/a", "src/b"}) || !reflect.DeepEqual(event["actual"], []string{"SRC/a", "src/b", "SRC/a"}) {
		t.Fatalf("event %s %v", data, err)
	}
	var raw map[string]any
	_ = json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":2,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw)
	a, err := fleet.NormalizeAssignments(raw)
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := s.Next(ctx, "nas", 2, a)
	if err != nil || job == nil || job.ID != 2 {
		t.Fatalf("next %v %v", job, err)
	}
	if err = s.Sweep(ctx, nil); err != nil {
		t.Fatal(err)
	}
	for id, expected := range map[int]string{3: "queued", 4: "held"} {
		var status string
		if err = db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id=$1`, id).Scan(&status); err != nil || status != expected {
			t.Fatalf("job %d: %s %v", id, status, err)
		}
	}
	// Job 2 is running in the same repository on other; other2 is disjoint.
	var runningStatus, runningRepo string
	if err = db.QueryRowContext(ctx, `SELECT status,repo FROM jobs WHERE id=2`).Scan(&runningStatus, &runningRepo); err != nil || runningStatus != "running" || runningRepo != "owner/repo" {
		t.Fatalf("other job: %s %s %v", runningStatus, runningRepo, err)
	}
	widened, err := s.Widen(ctx, 1, "other", 1, []string{"other2"}, nil)
	if err != nil || widened.Result != "granted" || !reflect.DeepEqual(widened.Job.Scope, []string{"SRC/a", "src/b", "other2"}) {
		t.Fatalf("widen granted %v %v", widened, err)
	}
	widened, err = s.Widen(ctx, 1, "other", 1, []string{"other"}, nil)
	if err != nil || widened.Result != "wait" {
		t.Fatalf("widen wait %v %v", widened, err)
	}
}

func TestPostgresNarrowDoesNotWaitForRepositoryAdvisoryLock(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now()
	s, _ := New(db, Options{Now: func() time.Time { return at }})
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,lease_host,lease_epoch,lease_expires_at) VALUES(1,'owner/repo',1,'running',ARRAY['src/'],'nas',1,$1)`, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, jobsAdvisoryLockKey("owner/repo")); err != nil {
		t.Fatal(err)
	}
	widenCtx, cancelWiden := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelWiden()
	if _, err = s.Widen(widenCtx, 1, "nas", 1, []string{"other"}, nil); err == nil || widenCtx.Err() != context.DeadlineExceeded {
		t.Fatalf("widen did not wait for the held repository lock: %v", err)
	}
	narrowCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result, err := s.Narrow(narrowCtx, 1, "nas", 1, []string{"src/a"})
	if err != nil || result.Result != "granted" || !reflect.DeepEqual(result.Job.Scope, []string{"src/a"}) {
		t.Fatalf("narrow while repository lock is held: %+v %v", result, err)
	}
}

func TestJobsAdvisoryLockKey(t *testing.T) {
	if got, want := jobsAdvisoryLockKey("Owner/Repo"), "issue-desk:jobs:owner/repo"; got != want {
		t.Fatalf("lock key = %q, want %q", got, want)
	}
}
