package jobs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func artifactJob(owner string, published bool) *Job {
	head, branch := strings.Repeat("a", 40), "codex/issue-1-job-1"
	continuation := map[string]any{"artifact": map[string]any{"generation": float64(0), "branch": branch, "headSha": head, "owner": owner}}
	if published {
		continuation["publication"] = map[string]any{"generation": float64(0), "branch": branch, "headSha": head, "verified": true}
	}
	return &Job{Generation: 0, Branch: &branch, HeadSHA: &head, View: map[string]any{"continuation": continuation}}
}

// The dispatch guard must agree with the Report guard: a host that Report would
// refuse for every head change must not receive the lease.
func TestArtifactDispatchableMatchesReportGuard(t *testing.T) {
	other := strings.Repeat("b", 40)
	for _, c := range []struct {
		name      string
		job       *Job
		host      string
		want      bool
		reportsOK bool
	}{
		{"owner", artifactJob("nab-9s", false), "nab-9s", true, true},
		{"non-owner unpublished", artifactJob("nab-9s", false), "gt12", false, false},
		{"non-owner published", artifactJob("nab-9s", true), "gt12", true, true},
		{"no head", &Job{View: map[string]any{}}, "gt12", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := artifactDispatchable(c.job, c.host); got != c.want {
				t.Fatalf("dispatchable=%v want %v", got, c.want)
			}
			if c.job.HeadSHA == nil {
				return
			}
			_, err := updateReportArtifact(c.job, c.host, Report{HeadSHA: Optional[string]{Present: true, Value: &other}}, false)
			if (err == nil) != c.reportsOK {
				t.Fatalf("report err=%v want ok=%v", err, c.reportsOK)
			}
		})
	}
	reset := artifactJob("nab-9s", false)
	reset.View["retryFromBase"] = map[string]any{"generation": float64(0), "by": "issue-screen"}
	if !artifactDispatchable(reset, "gt12") {
		t.Fatal("a retry from the base must stay dispatchable")
	}
}

func TestPostgresDispatchKeepsUnpublishedArtifactOnOwner(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	a := balanceAssignments(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('gt12',true,$1,4),('nab-9s',true,$1,4)`, at); err != nil {
		t.Fatal(err)
	}
	job := artifactJob("nab-9s", false)
	view, err := json.Marshal(job.View)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,branch,head_sha,view) VALUES(1,'mofua-suki/issue-desk-intake',1,'queued',ARRAY['one.go'],'declared',$1,$2,$3::jsonb)`, *job.Branch, *job.HeadSHA, string(view)); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	// gt12 is the preferred implementer, but Report would refuse its head change.
	if got, _, err := s.Next(ctx, "gt12", 4, a); err != nil || got != nil {
		t.Fatalf("non-owner got=%v err=%v", got, err)
	}
	// The owner must not wait for the refused preferred host.
	got, _, err := s.Next(ctx, "nab-9s", 4, a)
	if err != nil || got == nil || got.ID != 1 {
		t.Fatalf("owner got=%v err=%v", got, err)
	}
}

// A legacy row whose leases moved between hosts has no owner Report accepts, so
// dispatch restarts it from the default branch; a single-host history keeps it.
func TestPostgresDispatchResetsLegacyArtifactWithoutOwner(t *testing.T) {
	for _, c := range []struct {
		name, host string
		reset      bool
	}{{"single-host", "one", false}, {"mixed-hosts", "one", true}, {"different-current-host", "two", true}} {
		t.Run(c.name, func(t *testing.T) {
			db, ctx, s, a, id := legacyArtifactFixture(t, 2, "queued")
			if c.name == "mixed-hosts" {
				if _, err := db.ExecContext(ctx, `UPDATE job_events SET host='two' WHERE job_id=$1 AND kind='leased' AND (data->>'epoch')::int=1`, id); err != nil {
					t.Fatal(err)
				}
			}
			if c.host == "two" {
				if _, err := db.ExecContext(ctx, `UPDATE hosts SET ready=false WHERE host='one'`); err != nil {
					t.Fatal(err)
				}
			}
			job := takeLegacyArtifact(t, ctx, s, a, id, c.host)
			if got := artifactFromBase(job); got != c.reset {
				t.Fatalf("reset=%v want %v: %#v", got, c.reset, job.View)
			}
			r := legacyArtifactReport(job.LeaseEpoch, "running")
			if _, err := s.Report(ctx, id, c.host, r); err != nil {
				t.Fatal(err)
			}
			saved, err := s.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if identity, ok := artifactOf(saved); !ok || identity.Owner != c.host || identity.Head != *r.HeadSHA.Value {
				t.Fatalf("saved artifact: %+v %v", identity, ok)
			}
		})
	}
}
