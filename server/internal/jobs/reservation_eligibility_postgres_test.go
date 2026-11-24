package jobs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPostgresReservationOwnerEligibilityByHost(t *testing.T) {
	const repo = "mofua-suki/issue-desk-intake"
	for _, scenario := range []string{"eligible", "continuation", "artifact", "preference", "running-overlap", "edge-dependency", "explicit-dependency"} {
		for _, host := range []string{"gt12", "nab-9s"} {
			if scenario == "continuation" && host == "nab-9s" {
				continue
			}
			t.Run(scenario+"/"+host, func(t *testing.T) {
				db, ctx := testPostgres(t)
				at := time.Now().UTC().Truncate(time.Millisecond)
				a := balanceAssignments(t)
				if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4),('nab-9s',true,$1,4)`, at); err != nil {
					t.Fatal(err)
				}
				ownerView := map[string]any{}
				continuation := func(owner string) map[string]any {
					return map[string]any{"pending": true, "generation": 0, "host": owner, "at": at.Format(time.RFC3339Nano)}
				}
				candidateView := map[string]any{"continuation": continuation(host)}
				switch scenario {
				case "eligible", "continuation":
					ownerView["continuation"] = continuation("nab-9s")
				case "artifact":
					ownerView["continuation"] = map[string]any{"artifact": map[string]any{"generation": 0, "branch": "work", "headSha": strings.Repeat("a", 40), "owner": "nab-9s"}}
				case "explicit-dependency":
					ownerView["dependsOnIssues"] = []any{map[string]any{"repo": repo, "issue": 99}}
				}
				if scenario == "eligible" {
					ownerView["continuation"] = continuation(host)
				}
				ownerJSON, _ := json.Marshal(ownerView)
				candidateJSON, _ := json.Marshal(candidateView)
				if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,priority,filed_at,scope,scope_source,view) VALUES
(1,$1,1,'queued',0,$2,ARRAY['src/'],'declared',$4::jsonb),
(2,$1,2,'queued',0,$3,ARRAY['src/free.go'],'declared',$5::jsonb),
(3,$1,3,'queued',1,$3,ARRAY['later/'],'declared','{}');
`, repo, at.Add(-time.Hour), at.Add(-time.Minute), string(ownerJSON), string(candidateJSON)); err != nil {
					t.Fatal(err)
				}
				// Inherited priority makes the young candidate run through reservation
				// deferral before its older owner, preserving the existing priority rules.
				if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES(3,2)`); err != nil {
					t.Fatal(err)
				}
				if scenario == "artifact" {
					if _, err := db.ExecContext(ctx, `UPDATE jobs SET branch='work',head_sha=$1 WHERE id=1`, strings.Repeat("a", 40)); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "running-overlap" || scenario == "edge-dependency" {
					if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,lease_host,lease_epoch,lease_expires_at) VALUES(4,$1,4,'running',ARRAY['src/busy.go'],'elsewhere',1,$2)`, repo, at.Add(time.Hour)); err != nil {
						t.Fatal(err)
					}
					if scenario == "edge-dependency" {
						if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES(1,4)`); err != nil {
							t.Fatal(err)
						}
					}
				}
				// Confirm holder vs ordinary reservation using the actual persisted jobs.
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				queued, err := dispatchJobs(ctx, tx, "WHERE status='queued' AND id IN(1,2)")
				if err != nil {
					t.Fatal(err)
				}
				running, err := dispatchJobs(ctx, tx, "WHERE status='running'")
				if err != nil {
					t.Fatal(err)
				}
				r, err := newReservation(ctx, tx, queued, running, a, host, at)
				if err != nil {
					t.Fatal(err)
				}
				holder := scenario == "running-overlap" || scenario == "edge-dependency"
				if (r.holders[repo] != nil) != holder {
					t.Fatalf("holder=%v want=%v", r.holders, holder)
				}
				if !holder && (len(r.reservations) != 1 || r.reservations[0].ID != 1) {
					t.Fatalf("reservations=%v", r.reservations)
				}
				ownerEligible := scenario == "eligible" || (scenario == "continuation" || scenario == "artifact") && host == "nab-9s" || scenario == "preference" && host == "gt12"
				var candidate *Job
				for _, job := range queued {
					if job.ID == 2 {
						candidate = job
					}
				}
				deferred, err := r.deferJob(candidate, "")
				if err != nil || deferred != ownerEligible {
					t.Fatalf("standalone defer=%v want=%v err=%v", deferred, ownerEligible, err)
				}
				if err = tx.Rollback(); err != nil {
					t.Fatal(err)
				}
				s, err := New(db, Options{Now: func() time.Time { return at }})
				if err != nil {
					t.Fatal(err)
				}

				want := int64(2)
				if ownerEligible {
					want = 1
				}
				job, _, err := s.Next(ctx, host, 4, a)
				if err != nil || job == nil || job.ID != want {
					t.Fatalf("job=%v want=%d err=%v", job, want, err)
				}
				var reservations, releases, leases, offers int
				if err = db.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE job_id=2 AND kind='dispatch_skip' AND data->>'reason'='scope_reserved' AND data->>'by'='1' AND data->>'host'=$1),count(*) FILTER(WHERE kind='dispatch_skip' AND data->>'release'='true'),count(*) FILTER(WHERE kind='leased'),count(*) FILTER(WHERE job_id=1 AND kind='dispatch_offer') FROM job_events`, host).Scan(&reservations, &releases, &leases, &offers); err != nil {
					t.Fatal(err)
				}
				if (reservations == 1) != ownerEligible || releases != 0 || leases != 1 {
					t.Fatalf("scope_reserved=%d release=%d leased=%d", reservations, releases, leases)
				}
				if !ownerEligible && offers != 0 {
					t.Fatalf("eligibility probe wrote %d offers for owner", offers)
				}
				var ownerStatus string
				if err = db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id=1`).Scan(&ownerStatus); err != nil {
					t.Fatal(err)
				}
				if !ownerEligible && ownerStatus != "queued" {
					t.Fatalf("probe dispatched owner: %s", ownerStatus)
				}
			})
		}
	}
}
