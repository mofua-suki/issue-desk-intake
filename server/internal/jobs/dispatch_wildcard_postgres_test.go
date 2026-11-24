package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/store"
)

func wildcardFixture(t *testing.T, age time.Duration) (*sql.DB, context.Context, *Store, *fleet.Assignments, time.Time) {
	t.Helper()
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"a":{"maxImplementations":4,"repositories":{"owner/repo":{"rank":1},"owner/other":{"rank":1}}},"b":{"maxImplementations":4,"repositories":{"owner/repo":{"rank":2}}}},"repositories":{}}`), &raw); err != nil {
		t.Fatal(err)
	}
	a, err := fleet.NormalizeAssignments(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('a',true,$1,4),('b',true,$1,4)`, at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,priority,filed_at,scope,scope_source,lease_host,lease_epoch,lease_expires_at,view) VALUES
 (1,'owner/repo',1,'running',0,$1,ARRAY['busy/'],'declared','outside',1,$2,'{}'),
 (2,'owner/repo',2,'queued',1,$3,ARRAY['*'],'estimated',NULL,0,NULL,'{}')`, at, at.Add(time.Hour), at.Add(-age))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	return db, ctx, s, a, at
}
func wildcardNext(t *testing.T, ctx context.Context, s *Store, a *fleet.Assignments, host string, want int64) {
	t.Helper()
	job, _, err := s.Next(ctx, host, 4, a)
	if err != nil || (want == 0 && job != nil) || (want != 0 && (job == nil || job.ID != want)) {
		t.Fatalf("host=%s job=%v want=%d err=%v", host, job, want, err)
	}
}
func wildcardCandidate(t *testing.T, db *sql.DB, ctx context.Context, id int, at time.Time) {
	t.Helper()
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,priority,filed_at,scope,scope_source) VALUES($1::bigint,'owner/repo',$1::bigint::integer,'queued',0,$2,ARRAY['free/'],'declared')`, id, at)
	if err != nil {
		t.Fatal(err)
	}
}
func TestPostgresHighEstimatedWildcardPreventsOvertaking(t *testing.T) {
	db, ctx, s, a, at := wildcardFixture(t, 0)
	for i := 3; i < 7; i++ {
		wildcardCandidate(t, db, ctx, i, at)
		wildcardNext(t, ctx, s, a, "a", 0)
		wildcardNext(t, ctx, s, a, "b", 0)
	}
	var leases, skips int
	var status string
	if err := db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE kind='leased' AND job_id>=3),count(*) FILTER(WHERE kind='dispatch_skip' AND data->>'reason'='scope_reserved' AND data->>'by'='2') FROM job_events`).Scan(&leases, &skips); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id=1`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if leases != 0 || skips == 0 || status != "running" {
		t.Fatalf("leases=%d skips=%d status=%s", leases, skips, status)
	}
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET status='merged' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	wildcardNext(t, ctx, s, a, "a", 2)
	t.Run("deployed-assignments", func(t *testing.T) {
		db, ctx, s, _, at := wildcardFixture(t, 0)
		if _, err := db.ExecContext(ctx, `UPDATE jobs SET repo='mofua-suki/issue-desk-intake'; UPDATE hosts SET host=CASE host WHEN 'a' THEN 'gt12' ELSE 'nab-9s' END`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,priority,filed_at,scope,scope_source) VALUES(3,'mofua-suki/issue-desk-intake',3,'queued',0,$1,ARRAY['free/'],'declared')`, at); err != nil {
			t.Fatal(err)
		}
		wildcardNext(t, ctx, s, balanceAssignments(t), "gt12", 0)
	})

}
func TestPostgresHighEstimatedWildcardAgeAndReload(t *testing.T) {
	for _, age := range []time.Duration{0, 29 * time.Minute, 30 * time.Minute, 31 * time.Minute, 2*time.Hour - time.Second, 2 * time.Hour, 2*time.Hour + time.Second, 3 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			db, ctx, _, a, at := wildcardFixture(t, age)
			wildcardCandidate(t, db, ctx, 3, at)
			var schema string
			if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
			if err != nil {
				t.Fatal(err)
			}
			params := u.Query()
			params.Set("search_path", schema)
			u.RawQuery = params.Encode()
			db, err = store.OpenPostgres(ctx, u.String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			s, err := New(db, Options{Now: func() time.Time { return at }})
			if err != nil {
				t.Fatal(err)
			}
			wildcardNext(t, ctx, s, a, "a", 0)
			if _, err = db.ExecContext(ctx, `UPDATE jobs SET status='merged' WHERE id=1`); err != nil {
				t.Fatal(err)
			}
			wildcardNext(t, ctx, s, a, "a", 2)
		})
	}
}
func TestPostgresHighEstimatedWildcardEligibility(t *testing.T) {
	for _, scenario := range []string{"dependency", "capacity", "artifact", "continuation", "preference", "preference-ready", "adopt-failed", "other-host", "resume-other-host"} {
		t.Run(scenario, func(t *testing.T) {
			db, ctx, s, a, at := wildcardFixture(t, time.Minute)
			wildcardCandidate(t, db, ctx, 3, at)
			switch scenario {
			case "dependency":
				if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES(2,1)`); err != nil {
					t.Fatal(err)
				}
			case "capacity":
				if _, err := db.ExecContext(ctx, `UPDATE jobs SET lease_host='b' WHERE id=1`); err != nil {
					t.Fatal(err)
				}
				a.Hosts["b"].Max = 1
				if _, err := db.ExecContext(ctx, `UPDATE jobs SET view='{"continuation":{"artifact":{"generation":0,"branch":"work","headSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","owner":"b"}}}',branch='work',head_sha='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' WHERE id=2`); err != nil {
					t.Fatal(err)
				}
			case "artifact":
				if _, err := db.ExecContext(ctx, `UPDATE jobs SET view='{"continuation":{"artifact":{"generation":0,"branch":"work","headSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","owner":"missing"}}}',branch='work',head_sha='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' WHERE id=2`); err != nil {
					t.Fatal(err)
				}
			case "continuation":
				if _, err := db.ExecContext(ctx, `UPDATE hosts SET capacity=0 WHERE host='b'`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=$1::jsonb WHERE id=2`, fmt.Sprintf(`{"continuation":{"pending":true,"generation":0,"host":"b","at":%q}}`, at.Format(time.RFC3339Nano))); err != nil {
					t.Fatal(err)
				}
			case "adopt-failed":
				if _, err := db.ExecContext(ctx, `UPDATE jobs SET view='{"continuation":{"artifact":{"generation":0,"branch":"work","headSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","owner":"b"}}}',branch='work',head_sha='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' WHERE id=2`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES(1,'b','handed_back',$1,'{"reason":"adopt_failed"}')`, at); err != nil {
					t.Fatal(err)
				}
			case "preference", "preference-ready":
				dispatch := fleet.Object(a.Output["dispatch"])
				dispatch["preferenceSeconds"] = 60
				if scenario == "preference-ready" {
					dispatch["preferenceSeconds"] = 0
				}
				dispatch["leaseMinutes"] = 15
				dispatch["loadLimit"] = map[string]any{"minutes": 5, "cpuPercent": 90}
				if _, err := db.ExecContext(ctx, `UPDATE hosts SET capacity=2 WHERE host='a'`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO host_load_samples(host,received_at,load) VALUES('b',$1,'{"cpuPercent":100}'),('b',$2,'{"cpuPercent":100}')`, at.Add(-time.Minute), at); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=$1::jsonb WHERE id=3`, fmt.Sprintf(`{"continuation":{"pending":true,"generation":0,"host":"a","at":%q}}`, at.Format(time.RFC3339Nano))); err != nil {
					t.Fatal(err)
				}
			case "resume-other-host":
				if _, err := db.ExecContext(ctx, `UPDATE jobs SET view='{"publishPending":true,"continuation":{"artifact":{"generation":0,"branch":"work","headSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","owner":"b"}}}',branch='work',head_sha='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' WHERE id=2`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES(2,'a','leased',$1,'{}')`, at.Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
			case "other-host":
				if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=$1::jsonb WHERE id=2`, fmt.Sprintf(`{"continuation":{"pending":true,"generation":0,"host":"b","at":%q}}`, at.Format(time.RFC3339Nano))); err != nil {
					t.Fatal(err)
				}
			}
			want := int64(3)
			if scenario == "other-host" || scenario == "resume-other-host" || scenario == "preference-ready" {
				want = 0
			}

			wildcardNext(t, ctx, s, a, "a", want)
			var offers, leases int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE kind='dispatch_offer' AND job_id=2),count(*) FILTER(WHERE kind='leased' AND job_id=2 AND at >= $1) FROM job_events`, at).Scan(&offers, &leases); err != nil {
				t.Fatal(err)
			}
			var skips int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=3 AND kind='dispatch_skip' AND data->>'reason'='scope_reserved' AND data->>'by'='2'`).Scan(&skips); err != nil {
				t.Fatal(err)
			}
			if ((scenario == "other-host" || scenario == "resume-other-host" || scenario == "preference-ready") && skips == 0) || (scenario != "other-host" && scenario != "resume-other-host" && scenario != "preference-ready" && skips != 0) {
				t.Fatalf("scenario=%s skips=%d", scenario, skips)
			}
			if offers != 0 || leases != 0 {
				t.Fatalf("offers=%d leases=%d", offers, leases)
			}
		})
	}
}
func TestPostgresHighEstimatedWildcardReleaseAndFallback(t *testing.T) {
	for _, change := range []string{"status='stopped'", "priority=0", "scope=ARRAY['other/']"} {
		t.Run(change, func(t *testing.T) {
			db, ctx, s, a, at := wildcardFixture(t, time.Hour)
			wildcardCandidate(t, db, ctx, 3, at)
			wildcardNext(t, ctx, s, a, "a", 0)
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET `+change+` WHERE id=2`); err != nil {
				t.Fatal(err)
			}
			if change == "scope=ARRAY['other/']" {
				wildcardNext(t, ctx, s, a, "a", 2)
				if _, err := db.ExecContext(ctx, `UPDATE jobs SET status='stopped' WHERE id=2`); err != nil {
					t.Fatal(err)
				}
			}
			wildcardNext(t, ctx, s, a, "a", 3)
		})
	}
	t.Run("other-repository", func(t *testing.T) {
		db, ctx, s, a, at := wildcardFixture(t, time.Hour)
		_, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,priority,filed_at,scope,scope_source) VALUES(3,'owner/other',3,'queued',0,$1,ARRAY['free/'],'declared')`, at)
		if err != nil {
			t.Fatal(err)
		}
		wildcardNext(t, ctx, s, a, "a", 3)
	})
	t.Run("ordinary-reservation", func(t *testing.T) {
		db, ctx, s, a, at := wildcardFixture(t, time.Hour)
		wildcardCandidate(t, db, ctx, 3, at)
		_, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,priority,filed_at,scope,scope_source) VALUES(4,'owner/repo',4,'queued',0,$1,ARRAY['free/'],'declared'),(5,'owner/repo',5,'queued',1,$2,ARRAY['later/'],'declared')`, at.Add(-time.Hour), at)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES(5,3)`); err != nil {
			t.Fatal(err)
		}
		wildcardNext(t, ctx, s, a, "a", 0)
		var leases, ordinary, interrupt, releases int
		err = db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE kind='leased'),count(*) FILTER (WHERE kind='dispatch_skip' AND data->>'reason'='scope_reserved' AND data->>'by'='4'),count(*) FILTER (WHERE kind='dispatch_skip' AND data->>'reason'='scope_reserved' AND data->>'by'='2'),count(*) FILTER (WHERE kind='dispatch_skip' AND data->>'release'='true') FROM job_events WHERE job_id=3`).Scan(&leases, &ordinary, &interrupt, &releases)
		if err != nil || leases != 0 || ordinary == 0 || interrupt == 0 || releases != 0 {
			t.Fatalf("leases=%d ordinary=%d interrupt=%d releases=%d err=%v", leases, ordinary, interrupt, releases, err)
		}
	})

}

func TestPostgresHighEstimatedWildcardAllowsPublicationResume(t *testing.T) {
	for _, host := range []string{"a", "b"} {
		t.Run(host, func(t *testing.T) {
			db, ctx, s, a, at := wildcardFixture(t, time.Hour)
			wildcardCandidate(t, db, ctx, 3, at)
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET scope=ARRAY['busy/'],view='{"publishPending":true}' WHERE id=3`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES(3,'a','leased',$1,'{}')`, at.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			want := int64(0)
			if host == "a" {
				want = 3
			}
			wildcardNext(t, ctx, s, a, host, want)
			var skips int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=3 AND kind='dispatch_skip' AND data->>'reason'='scope_reserved' AND data->>'by'='2'`).Scan(&skips); err != nil {
				t.Fatal(err)
			}
			if skips != 0 {
				t.Fatalf("publication resume recorded interrupt reservation: %d", skips)
			}
		})
	}
}
