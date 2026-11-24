package jobs

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hosts"
)

func balanceAssignments(t *testing.T, configure ...func(map[string]any)) *fleet.Assignments {
	t.Helper()
	data, err := os.ReadFile("../../../assignments.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err = json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, edit := range configure {
		edit(raw)
	}
	a, err := fleet.NormalizeAssignments(raw)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPostgresDispatchBalancesRealAssignments(t *testing.T) {
	for _, repo := range []string{"mofua-suki/issue-desk-intake", "mofua-suki/issue-desk-worker"} {
		t.Run(repo, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now().UTC().Truncate(time.Millisecond)
			a := balanceAssignments(t)
			if !reflect.DeepEqual(a.Implementers[repo], []string{"gt12", "nab-9s"}) || a.Hosts["gt12"].Max != 4 || a.Hosts["nab-9s"].Max != 4 || fleet.Number(fleet.Object(a.Output["dispatch"])["preferenceSeconds"]) != 30 {
				t.Fatal("unexpected deployed assignments")
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4),('nab-9s',true,$1,4)`, at); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source) VALUES(1,$1,1,'queued',ARRAY['one.go'],'declared'),(2,$1,2,'queued',ARRAY['two.go'],'declared')`, repo); err != nil {
				t.Fatal(err)
			}
			s, err := New(db, Options{Now: func() time.Time { return at }})
			if err != nil {
				t.Fatal(err)
			}
			check := func(host string, want int64) {
				t.Helper()
				job, _, err := s.Next(ctx, host, 4, a)
				if err != nil || (want == 0 && job != nil) || (want != 0 && (job == nil || job.ID != want)) {
					t.Fatalf("host=%s job=%v want=%d err=%v", host, job, want, err)
				}
			}
			check("nab-9s", 0)
			check("gt12", 1)
			check("gt12", 0)
			check("nab-9s", 2)
		})
	}
}

func TestPostgresDispatchPreferenceWaitAndOccupancy(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	a := balanceAssignments(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4),('nab-9s',true,$1,4)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source) VALUES(1,'mofua-suki/issue-desk-intake',1,'queued',ARRAY['queued.go'],'declared')`); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	check := func(host string, want bool) {
		t.Helper()
		job, _, err := s.Next(ctx, host, 4, a)
		if err != nil || (job != nil) != want {
			t.Fatalf("host=%s job=%v want=%v err=%v", host, job, want, err)
		}
	}
	check("nab-9s", false)
	at = at.Add(29 * time.Second)
	check("nab-9s", false)
	// Three unrelated jobs leave one slot on gt12, without blocking the scope.
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,lease_host,lease_epoch,lease_expires_at) SELECT n,'other/repo',n,'running',ARRAY['busy.go'],'gt12',1,$1 FROM generate_series(2,4) n`, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	check("gt12", false)
	// Restore equal occupancy before the deadline: the original offer time survives.
	if _, err := db.ExecContext(ctx, `DELETE FROM jobs WHERE id BETWEEN 2 AND 4`); err != nil {
		t.Fatal(err)
	}
	at = at.Add(time.Second)
	check("nab-9s", true)
}

func TestPostgresDispatchEmptyHostDoesNotWait(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	a := balanceAssignments(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4),('nab-9s',true,$1,4)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,lease_host,lease_epoch,lease_expires_at) SELECT n,'other/repo',n,'running',ARRAY['busy.go'],'gt12',1,$1 FROM generate_series(2,4) n`, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source) VALUES(1,'mofua-suki/issue-desk-intake',1,'queued',ARRAY['queued.go'],'declared')`); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := s.Next(ctx, "gt12", 4, a)
	if err != nil || job != nil {
		t.Fatalf("busy host: %v %v", job, err)
	}
	job, _, err = s.Next(ctx, "nab-9s", 4, a)
	if err != nil || job == nil || job.ID != 1 {
		t.Fatalf("empty host: %v %v", job, err)
	}
}

func TestAvailableSlotsLimitsAndEligibility(t *testing.T) {
	at := time.Now()
	a := balanceAssignments(t)
	repo := "mofua-suki/issue-desk-intake"
	host := "gt12"
	states := map[string]hostState{host: {Host: host, Ready: true, Heartbeat: at, Capacity: 3}}
	running := []*Job{{LeaseHost: &host, Repo: repo}, {LeaseHost: &host, Repo: "other/repo"}}
	if got := availableSlots(host, repo, states, running, a, at); got != 1 {
		t.Fatalf("reported capacity: %d", got)
	}
	entry := a.Hosts[host].Repositories[repo]
	entry.Max = 1
	a.Hosts[host].Repositories[repo] = entry
	if got := availableSlots(host, repo, states, running, a, at); got > 0 {
		t.Fatalf("repository limit: %d", got)
	}
	entry.Max = 4
	a.Hosts[host].Repositories[repo] = entry
	row := states[host]
	row.Capabilities = map[string]any{"repositories": []any{"other/repo"}}
	states[host] = row
	if got := availableSlots(host, repo, states, running, a, at); got != 0 {
		t.Fatalf("unreported repository: %d", got)
	}
	row.Capabilities = nil
	row.Heartbeat = at.Add(-heartbeatFreshness)
	states[host] = row
	if got := availableSlots(host, repo, states, running, a, at); got != 0 {
		t.Fatalf("stale heartbeat: %d", got)
	}
}

func TestPostgresDispatchSkipReasons(t *testing.T) {
	const repo = "mofua-suki/issue-desk-intake"
	for _, tc := range []struct{ name, host, reason string }{
		{"overlap", "gt12", "overlap"}, {"priority preference", "nab-9s", "preference"}, {"continuation preference", "nab-9s", "preference"},
		{"edge dependency", "gt12", "dependency"}, {"edge dependency full", "gt12", "dependency"}, {"explicit dependency", "gt12", "dependency"}, {"missing explicit dependency", "gt12", "dependency"}, {"unresolved dependency", "gt12", "dependency"}, {"host slots", "gt12", "no_slot"}, {"repo slots", "gt12", "no_slot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now().UTC().Truncate(time.Millisecond)
			a := balanceAssignments(t)
			if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4),('nab-9s',true,$1,4)`, at); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,updated_at) VALUES(1,$1,1,'queued',ARRAY['src/a.go'],'declared',$2)`, repo, at.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := db.ExecContext(ctx, query, args...); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.name {
			case "overlap":
				exec(`INSERT INTO jobs(id,repo,issue,status,scope,lease_host,lease_epoch,lease_expires_at) VALUES(2,$1,2,'running',ARRAY['src/'],'other',1,$2)`, repo, at.Add(time.Hour))
			case "continuation preference":
				exec(`UPDATE jobs SET view=jsonb_build_object('continuation',jsonb_build_object('pending',true,'generation',generation,'host','gt12','at',$1::text)) WHERE id=1`, at.Format(time.RFC3339Nano))
			case "edge dependency", "edge dependency full":
				exec(`INSERT INTO jobs(id,repo,issue,status) VALUES(2,'other/repo',2,'queued')`)
				exec(`INSERT INTO job_deps(job_id,depends_on) VALUES(1,2)`)
				if tc.name == "edge dependency full" {
					exec(`INSERT INTO jobs(id,repo,issue,status,lease_host,lease_epoch,lease_expires_at) SELECT n,'other/repo',n,'running','gt12',1,$1 FROM generate_series(3,6) n`, at.Add(time.Hour))
				}
			case "explicit dependency":
				exec(`UPDATE jobs SET view='{"dependsOnIssues":[{"repo":"invalid","issue":0}]}' WHERE id=1`)
			case "missing explicit dependency":
				exec(`UPDATE jobs SET view='{"dependsOnIssues":[{"repo":"owner/repo","issue":123}]}' WHERE id=1`)
			case "unresolved dependency":
				exec(`UPDATE jobs SET view='{"unresolvedDependencies":[{"repo":"owner/repo","issue":123}]}' WHERE id=1`)
			case "host slots":
				exec(`INSERT INTO jobs(id,repo,issue,status,scope,lease_host,lease_epoch,lease_expires_at) SELECT n,'other/repo',n,'running',ARRAY['busy/'],'gt12',1,$1 FROM generate_series(2,5) n`, at.Add(time.Hour))
			case "repo slots":
				entry := a.Hosts["gt12"].Repositories[repo]
				entry.Max = 1
				a.Hosts["gt12"].Repositories[repo] = entry
				exec(`INSERT INTO jobs(id,repo,issue,status,scope,lease_host,lease_epoch,lease_expires_at) VALUES(2,$1,2,'running',ARRAY['other/'],'gt12',1,$2)`, repo, at.Add(time.Hour))
			}
			s, err := New(db, Options{Now: func() time.Time { return at }})
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				job, _, err := s.Next(ctx, tc.host, 4, a)
				if err != nil || job != nil {
					t.Fatalf("job=%v err=%v", job, err)
				}
			}
			var raw []byte
			var n int
			if err = db.QueryRowContext(ctx, `SELECT count(*),min(data::text)::jsonb FROM job_events WHERE job_id=1 AND kind='dispatch_skip'`).Scan(&n, &raw); err != nil {
				t.Fatal(err)
			}
			var data map[string]any
			if err = json.Unmarshal(raw, &data); err != nil {
				t.Fatal(err)
			}
			if n != 1 || data["reason"] != tc.reason || data["host"] != tc.host {
				t.Fatalf("count=%d data=%v", n, data)
			}
			if tc.name == "overlap" && data["by"] != float64(2) {
				t.Fatal(data)
			}
			if tc.reason == "preference" && data["preferredHost"] != "gt12" {
				t.Fatal(data)
			}
			if tc.reason == "no_slot" && (data["slots"] != float64(0) || data["hostRunning"] == nil || data["hostLimit"] == nil || data["repoRunning"] == nil || data["repoLimit"] == nil) {
				t.Fatal(data)
			}
			if tc.name == "host slots" && (data["hostRunning"] != float64(4) || data["hostLimit"] != float64(4) || data["repoRunning"] != float64(0) || data["repoLimit"] != float64(a.RepoLimit(tc.host, repo))) {
				t.Fatalf("host capacity: %v", data)
			}
			if tc.name == "repo slots" && (data["hostRunning"] != float64(1) || data["hostLimit"] != float64(4) || data["repoRunning"] != float64(1) || data["repoLimit"] != float64(1)) {
				t.Fatalf("repo capacity: %v", data)
			}
			var updated time.Time
			if err = db.QueryRowContext(ctx, `SELECT updated_at FROM jobs WHERE id=1`).Scan(&updated); err != nil || !updated.Equal(at.Add(-time.Hour)) {
				t.Fatalf("updated_at %v %v", updated, err)
			}
			if tc.name == "edge dependency" {
				exec(`UPDATE jobs SET status='merged' WHERE id=2`)
				job, _, err := s.Next(ctx, "gt12", 4, a)
				if err != nil || job == nil || job.ID != 1 {
					t.Fatalf("dependency release: %v %v", job, err)
				}
			}
		})
	}
}

func TestPostgresDispatchDoesNotMisreportNonSlotRejections(t *testing.T) {
	for _, kind := range []string{"not ready", "stale", "hold", "drain", "unreported", "restrict", "investigation", "transfer wait", "load", "artifact"} {
		t.Run(kind, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now().UTC().Truncate(time.Millisecond)
			a := balanceAssignments(t)
			repo := "mofua-suki/issue-desk-intake"
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := db.ExecContext(ctx, q, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4)`, at)
			exec(`INSERT INTO jobs(id,repo,issue,status,scope) VALUES(1,$1,1,'queued',ARRAY['src/a.go'])`, repo)
			exec(`INSERT INTO jobs(id,repo,issue,status,lease_host,lease_epoch,lease_expires_at) SELECT n,'other/repo',n,'running','gt12',1,$1 FROM generate_series(2,5) n`, at.Add(time.Hour))
			switch kind {
			case "not ready":
				exec(`UPDATE hosts SET ready=false WHERE host='gt12'`)
			case "stale":
				exec(`UPDATE hosts SET heartbeat_at=$1 WHERE host='gt12'`, at.Add(-heartbeatFreshness))
			case "hold":
				exec(`UPDATE hosts SET intake_hold='{"reasons":["held"]}' WHERE host='gt12'`)
			case "drain":
				exec(`UPDATE hosts SET drain=jsonb_build_object('active',true,'at',$1::text) WHERE host='gt12'`, at.Format(time.RFC3339Nano))
			case "unreported":
				exec(`UPDATE hosts SET capabilities='{"repositories":["other/repo"]}' WHERE host='gt12'`)
			case "investigation":
				exec(`UPDATE jobs SET view='{"workType":"investigation"}' WHERE id=1`)
			case "transfer wait":
				source, destination, consumer := transferFixture()
				exec(`INSERT INTO jobs(id,repo,issue,status,view,result) VALUES(10,$1,$2,$3,$4::jsonb,$5::jsonb),(11,$6,$7,$8,$9::jsonb,$10::jsonb)`, source.Repo, source.Issue, source.Status, transferJSON(source.View), transferJSON(source.Result), destination.Repo, destination.Issue, destination.Status, transferJSON(destination.View), transferJSON(destination.Result))
				exec(`UPDATE jobs SET view=$1::jsonb WHERE id=1`, transferJSON(consumer.View))
				exec(`INSERT INTO job_deps(job_id,depends_on) VALUES(1,10)`)
			case "artifact":
				j := artifactJob("nab-9s", false)
				exec(`UPDATE jobs SET branch=$1, head_sha=$2, view=$3::jsonb WHERE id=1`, *j.Branch, *j.HeadSHA, transferJSON(j.View))
			case "load":
				exec(`INSERT INTO host_load_samples(host,received_at,load) VALUES('gt12',$1,'{"cpuPercent":100}'),('gt12',$2,'{"cpuPercent":100}')`, at.Add(-time.Minute), at)
			}
			s, _ := New(db, Options{Now: func() time.Time { return at }})
			var restrict [][]string
			if kind == "restrict" {
				restrict = append(restrict, []string{"other/repo"})
			}
			job, _, err := s.Next(ctx, "gt12", 4, a, restrict...)
			if err != nil || job != nil {
				t.Fatalf("job=%v err=%v", job, err)
			}
			var n int
			if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=1 AND kind='dispatch_skip' AND data->>'reason'='no_slot'`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("no_slot=%d err=%v", n, err)
			}
		})
	}
}

func TestPostgresDispatchExcludedHostsDoNotReceiveIssueDesk(t *testing.T) {
	for _, host := range []string{"a9max", "x1-pro1", "x1-pro2"} {
		for _, repo := range []string{"mofua-suki/issue-desk-intake", "mofua-suki/issue-desk-worker"} {
			t.Run(host+"/"+repo, func(t *testing.T) {
				db, ctx := testPostgres(t)
				at := time.Now().UTC().Truncate(time.Millisecond)
				a := balanceAssignments(t)
				if _, err := db.ExecContext(ctx, "INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES($1,true,$2,3)", host, at); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, "INSERT INTO jobs(id,repo,issue,status,scope,scope_source) VALUES(1,$1,1,'queued',ARRAY['file.go'],'declared')", repo); err != nil {
					t.Fatal(err)
				}
				s, err := New(db, Options{Now: func() time.Time { return at }})
				if err != nil {
					t.Fatal(err)
				}
				if job, _, err := s.Next(ctx, host, 3, a); err != nil || job != nil {
					t.Fatalf("excluded host received job: %v %v", job, err)
				}
				if _, err := db.ExecContext(ctx, "INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4)", at); err != nil {
					t.Fatal(err)
				}
				if job, _, err := s.Next(ctx, "gt12", 4, a); err != nil || job == nil || job.ID != 1 {
					t.Fatalf("remaining host could not claim: %v %v", job, err)
				}
			})
		}
	}
}

func TestPostgresDispatchPreferenceSurvivesCandidateChanges(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	start := at
	// Candidate preference with a fallback needs three eligible hosts.
	// The shipped two-host policy is verified by the separate dispatch tests.
	a := balanceAssignments(t, func(raw map[string]any) {
		fleet.Object(raw["hosts"])["third"] = map[string]any{"maxImplementations": float64(4), "repositories": map[string]any{"mofua-suki/issue-desk-intake": map[string]any{"rank": float64(4)}}}
	})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4),('nab-9s',true,$1,4),('third',true,$1,4)`, at)
	exec(`INSERT INTO jobs(id,repo,issue,status,scope,scope_source,updated_at) VALUES(1,'mofua-suki/issue-desk-intake',1,'queued',ARRAY['queued.go'],'declared',$1)`, at.Add(-time.Hour))
	// An offer written by the previous implementation starts one new interval.
	exec(`INSERT INTO job_events(job_id,kind,data,at) VALUES(1,'dispatch_offer','{"signature":"legacy"}',$1)`, at.Add(-time.Hour))
	newStore := func() *Store {
		t.Helper()
		s, err := New(db, Options{Now: func() time.Time { return at }})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := newStore()
	check := func(host string, want bool) {
		t.Helper()
		job, _, err := s.Next(ctx, host, 4, a)
		if err != nil || (job != nil) != want {
			t.Fatalf("at=%v host=%s job=%v want=%v err=%v", at, host, job, want, err)
		}
	}
	offers := func(want int, when time.Time) {
		t.Helper()
		var count int
		var last time.Time
		if err := db.QueryRowContext(ctx, `SELECT count(*),max(at) FROM job_events WHERE job_id=1 AND kind='dispatch_offer'`).Scan(&count, &last); err != nil || count != want || !last.Equal(when) {
			t.Fatalf("offers=%d last=%v want=%d %v err=%v", count, last, want, when, err)
		}
	}
	check("third", false)
	offers(2, start)
	// Repeatedly remove and restore the preferred host by exhausting its slots.
	for _, sec := range []int{5, 15} {
		at = start.Add(time.Duration(sec) * time.Second)
		exec(`INSERT INTO jobs(id,repo,issue,status,lease_host,lease_epoch,lease_expires_at) SELECT n,'other/repo',n,'running','gt12',1,$1 FROM generate_series(2,5) n`, start.Add(time.Hour))
		// The full host cannot receive a job; the remaining candidate keeps waiting.
		check("gt12", false)
		check("third", false)
		exec(`DELETE FROM jobs WHERE id BETWEEN 2 AND 5`)
		check("third", false)
		offers(2, start)
	}
	// A store reload reads the persisted offer. A missing querying host is rejected.
	s = newStore()
	at = start.Add(59 * time.Second)
	check("third", false)
	exec(`UPDATE hosts SET ready=false WHERE host='third'`)
	at = start.Add(60 * time.Second)
	check("third", false)
	exec(`UPDATE hosts SET ready=true,heartbeat_at=$1 WHERE host='third'`, at)
	check("third", true)
	offers(2, start)
}

func TestPostgresDispatchPreferenceStartsNewInterval(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	a := balanceAssignments(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4),('nab-9s',true,$1,4)`, at)
	exec(`INSERT INTO jobs(id,repo,issue,status,scope,scope_source,updated_at) VALUES(1,'mofua-suki/issue-desk-intake',1,'queued',ARRAY['queued.go'],'declared',$1)`, at)
	s, err := New(db, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		job, _, err := s.Next(ctx, "nab-9s", 4, a)
		if err != nil || (job != nil) != want {
			t.Fatalf("at=%v job=%v want=%v err=%v", at, job, want, err)
		}
	}
	check(false)
	for _, update := range []string{`UPDATE jobs SET generation=generation+1 WHERE id=1 AND $1::timestamptz IS NOT NULL`, `UPDATE jobs SET updated_at=$1 WHERE id=1`} {
		at = at.Add(31 * time.Second)
		exec(`UPDATE hosts SET heartbeat_at=$1`, at)
		exec(update, at)
		check(false)
		at = at.Add(29 * time.Second)
		check(false)
	}
	at = at.Add(time.Second)
	check(true)
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=1 AND kind='dispatch_offer'`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("offers=%d err=%v", n, err)
	}
}

func TestPostgresIndefiniteDrainBlocksDispatchUntilResumed(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	a := balanceAssignments(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source) VALUES(1,'mofua-suki/issue-desk-intake',1,'queued',ARRAY['queued.go'],'declared')`); err != nil {
		t.Fatal(err)
	}
	manual := &hosts.Service{DB: db, Now: func() time.Time { return at.Add(-151 * time.Minute) }}
	if _, err := manual.Drain(ctx, "gt12", "screen", map[string]any{"indefinite": true, "reason": "maintenance"}, true); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := s.Next(ctx, "gt12", 4, a)
	if err != nil || job != nil {
		t.Fatalf("drained: job=%v err=%v", job, err)
	}
	if _, err = manual.Drain(ctx, "gt12", "screen", nil, false); err != nil {
		t.Fatal(err)
	}
	job, _, err = s.Next(ctx, "gt12", 4, a)
	if err != nil || job == nil || job.ID != 1 {
		t.Fatalf("resumed: job=%v err=%v", job, err)
	}
}
