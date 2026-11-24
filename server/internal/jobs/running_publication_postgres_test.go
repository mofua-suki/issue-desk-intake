package jobs

import (
	"strings"
	"testing"
	"time"
)

// Exercise the shipped assignments and the real Report/Control persistence, not
// a synthetic publicationResume flag. A broad queued job would have become a
// holder under the old reservation blocked predicate.
func TestPostgresPublishedRunningDispatchLifecycle(t *testing.T) {
	for _, scenario := range []string{"published", "releasing", "deploying", "pending", "tuple-only", "invalid-head", "implementation", "instruction", "publication-resume", "review-retry", "retest", "from-base", "republished", "instruction-after-republish", "lease-expiry"} {
		t.Run(scenario, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now().UTC().Truncate(time.Millisecond)
			a := balanceAssignments(t)
			const repo = "mofua-suki/issue-desk-intake"
			if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4),('nab-9s',true,$1,4);
`, at); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,lease_host,lease_epoch,lease_expires_at,view)
VALUES(1,$1,1,'running',ARRAY['src/busy.go'],'declared','gt12',1,$2,'{}')`, repo, at.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			s, err := New(db, Options{Now: func() time.Time { return at }})
			if err != nil {
				t.Fatal(err)
			}
			branch, head, pr := "work/published", strings.Repeat("a", 40), int64(12)
			phase := "publishing"
			if scenario == "releasing" || scenario == "deploying" {
				phase = scenario
			}
			if scenario == "pending" {
				phase = ""
			}
			if scenario == "tuple-only" {
				phase = ""
			}
			if scenario == "implementation" {
				phase = "running"
			}
			report := Report{Epoch: 1, Status: "running", Branch: Optional[string]{true, &branch}, HeadSHA: Optional[string]{true, &head}, PR: Optional[int64]{true, &pr}, View: map[string]any{"phase": phase, "publishPending": scenario != "tuple-only"}}
			if _, err = s.Report(ctx, 1, "gt12", report); err != nil {
				t.Fatal(err)
			}
			protected := !(scenario == "published" || scenario == "releasing" || scenario == "deploying" || scenario == "pending" || scenario == "republished" || scenario == "publication-resume" || scenario == "lease-expiry")

			if scenario == "invalid-head" {
				if _, err = db.ExecContext(ctx, `UPDATE jobs SET head_sha='invalid' WHERE id=1`); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "instruction" || scenario == "instruction-after-republish" || scenario == "publication-resume" {
				if _, err = s.Control(ctx, 1, ControlInput{ID: "11111111-1111-4111-8111-111111111111", Action: "instruction", By: "issue-screen", Text: "追加の修正"}, ControlOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "instruction" {
				if _, err = db.ExecContext(ctx, `UPDATE jobs SET view=view || '{"publicationResume":true}'::jsonb WHERE id=1;
INSERT INTO job_events(job_id,kind,data) SELECT 1,'dispatch_offer','{}' FROM generate_series(1,230)`); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "publication-resume" || scenario == "review-retry" || scenario == "retest" || scenario == "from-base" || scenario == "republished" || scenario == "instruction-after-republish" {
				hold := "review_held"
				terminal := Report{Epoch: 1, Status: "held", HoldReason: &hold, Result: map[string]any{"status": "completed"}}
				if scenario == "retest" {
					terminal = Report{Epoch: 1, Status: "failed", Result: map[string]any{"status": "failed", "testFailures": map[string]any{"complete": true, "names": []any{"build"}}}}
				}
				if _, err = s.Report(ctx, 1, "gt12", terminal); err != nil {
					t.Fatal(err)
				}
				input := ControlInput{ID: "22222222-2222-4222-8222-222222222222", Action: "retry", By: "issue-screen", Text: "レビューの修正"}
				if scenario == "publication-resume" {
					input.Text = ""
				}
				if scenario == "retest" {
					input.Text = ""
					input.Retest = true
				}
				if scenario == "from-base" {
					input.Text = ""
					input.FromBase = true
				}
				retried, err := s.Control(ctx, 1, input, ControlOptions{})
				if err != nil || retried.Generation != 1 {
					t.Fatalf("retry=%+v err=%v", retried, err)
				}
				// Reacquire through dispatch while the retained phase/PR tuple is stale.
				job, _, err := s.Next(ctx, "gt12", 4, a)
				if err != nil || job == nil || job.ID != 1 {
					t.Fatalf("reacquire=%v err=%v", job, err)
				}
				if job.PR == nil || *job.PR != pr || job.HeadSHA == nil || *job.HeadSHA != head {
					t.Fatal("retry did not retain old publication")
				}
				if scenario == "republished" || scenario == "instruction-after-republish" {
					head = strings.Repeat("b", 40)
					report.Epoch = job.LeaseEpoch
					if _, err = s.Report(ctx, 1, "gt12", report); err != nil {
						t.Fatal(err)
					}
					if scenario == "instruction-after-republish" {
						if _, err = s.Control(ctx, 1, ControlInput{ID: "33333333-3333-4333-8333-333333333333", Action: "instruction", By: "issue-screen", Text: "さらに修正"}, ControlOptions{}); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if scenario == "lease-expiry" {
				if _, err = db.ExecContext(ctx, `UPDATE jobs SET lease_expires_at=$1 WHERE id=1`, at.Add(-time.Second)); err != nil {
					t.Fatal(err)
				}
				if err = s.Sweep(ctx, a.Exempt); err != nil {
					t.Fatal(err)
				}
				job, _, err := s.Next(ctx, "gt12", 4, a)
				if err != nil || job == nil || job.ID != 1 {
					t.Fatalf("expired publication resume=%v err=%v", job, err)
				}
			}
			if _, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,filed_at,scope,scope_source,view) VALUES(2,$1,2,'queued',$2,ARRAY['src/'],'declared','{}')`, repo, at.Add(-time.Hour)); err != nil {
				t.Fatal(err)
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
			r, err := newReservation(ctx, tx, queued, running, a, "nab-9s", at)
			if err != nil {
				t.Fatal(err)
			}
			if (r.holders[repo] != nil) != protected || (len(r.reservations) > 0) == protected {
				t.Fatalf("protected=%v holders=%v reservations=%v", protected, r.holders, r.reservations)
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			job, _, err := s.Next(ctx, "nab-9s", 4, a)
			if err != nil || protected && job != nil || !protected && (job == nil || job.ID != 2) {
				t.Fatalf("protected=%v picked=%v err=%v", protected, job, err)
			}
			var overlaps, reserved, leases int
			if err = db.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE kind='dispatch_skip' AND data->>'reason'='overlap'),count(*) FILTER(WHERE kind='dispatch_skip' AND data->>'reason'='scope_reserved'),count(*) FILTER(WHERE kind='leased') FROM job_events WHERE job_id=2`).Scan(&overlaps, &reserved, &leases); err != nil {
				t.Fatal(err)
			}
			if (overlaps == 1) != protected || reserved != 0 || (leases == 1) == protected {
				t.Fatalf("overlap=%d scope_reserved=%d leased=%d", overlaps, reserved, leases)
			}
		})
	}
}

func TestPostgresPublishedRunningStillConsumesSlots(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	a := balanceAssignments(t)
	const repo = "mofua-suki/issue-desk-intake"
	if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,lease_host,lease_epoch,lease_expires_at,branch,pr,head_sha,view) VALUES
(1,$1,1,'running',ARRAY['src/'],'gt12',1,$2,'work',12,$3,'{"phase":"publishing"}'),
(2,$1,2,'queued',ARRAY['src/a.go'],NULL,0,NULL,NULL,NULL,NULL,'{}')`, repo, at.Add(time.Hour), strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := s.Next(ctx, "gt12", 1, a)
	if err != nil || job != nil {
		t.Fatalf("published running lost its slot: %v %v", job, err)
	}
	var skips, leases int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE kind='dispatch_skip' AND data->>'reason'='no_slot' AND data->>'hostRunning'='1' AND data->>'slots'='0'),count(*) FILTER(WHERE kind='leased') FROM job_events WHERE job_id=2`).Scan(&skips, &leases); err != nil {
		t.Fatal(err)
	}
	if skips != 1 || leases != 0 {
		t.Fatalf("no_slot=%d leased=%d", skips, leases)
	}
}

func TestPostgresDispatchRereadsInstructionAfterCycleLock(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	a := balanceAssignments(t)
	const repo = "mofua-suki/issue-desk-intake"
	if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4),('nab-9s',true,$1,4)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,lease_host,lease_epoch,lease_expires_at,branch,pr,head_sha,view) VALUES
(1,$1,1,'running',ARRAY['src/'],'gt12',1,$2,'work',12,$3,'{"phase":"publishing"}'),
(2,$1,2,'queued',ARRAY['src/a.go'],NULL,0,NULL,NULL,NULL,NULL,'{}')`, repo, at.Add(time.Hour), strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	// Hold an uncommitted instruction in the same storage representation that
	// Control writes (the lifecycle test above exercises the public API).
	cycleGuard, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cycleGuard.Rollback()
	job, err := readJob(ctx, cycleGuard, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	appendControlInstruction(job, ControlInput{ID: "11111111-1111-4111-8111-111111111111", Text: "公開後の追加修正", By: "issue-screen"}, at)
	body, err := jsonValue(job.View)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cycleGuard.ExecContext(ctx, "UPDATE jobs SET view=$2::jsonb WHERE id=$1", job.ID, body); err != nil {
		t.Fatal(err)
	}
	if err = addEvent(ctx, cycleGuard, job.ID, "issue-screen", "control", map[string]any{"action": "instruction", "generation": job.Generation, "instruction": true}, at); err != nil {
		t.Fatal(err)
	}
	dispatcher, err := New(db, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		job *Job
		err error
	}
	dispatched := make(chan result, 1)
	go func() { job, _, err := dispatcher.Next(ctx, "nab-9s", 4, a); dispatched <- result{job, err} }()
	// Observe the database lock rather than assuming a scheduler delay means wait.
	for {
		select {
		case got := <-dispatched:
			t.Fatalf("dispatch bypassed locked running state: %+v", got)
		default:
		}
		var waiting bool
		if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%LOCK TABLE jobs,job_deps IN SHARE ROW EXCLUSIVE MODE%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if err = cycleGuard.Commit(); err != nil {
		t.Fatal(err)
	}
	got := <-dispatched
	if got.err != nil || got.job != nil {
		t.Fatalf("instruction was lost after lock wait: job=%v err=%v", got.job, got.err)
	}
	var overlaps, leases int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE kind='dispatch_skip' AND data->>'reason'='overlap'),count(*) FILTER(WHERE kind='leased') FROM job_events WHERE job_id=2`).Scan(&overlaps, &leases); err != nil {
		t.Fatal(err)
	}
	if overlaps != 1 || leases != 0 {
		t.Fatalf("overlap=%d leased=%d", overlaps, leases)
	}
}
