package operations

import (
	"context"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/pgtest"
	"testing"
)

// abandonedCause reads the latest abandoned cause for one issue from the saved
// recovery ledger, which Reserve and Candidates consult.
func abandonedCause(ctx context.Context, r *JobRecovery, repo string, issue int64) (RecoveryCause, bool, error) {
	rows, e := r.records(ctx)
	if e != nil {
		return RecoveryCause{}, false, e
	}
	var latest RecoveryCause
	found := false
	for _, row := range rows {
		if row.Repo != repo || row.Issue != issue {
			continue
		}
		for hash, c := range row.Causes {
			if c.State == "abandoned" && (!found || c.At.After(latest.At)) {
				c.CauseHash = hash
				latest, found = c, true
			}
		}
	}
	return latest, found, nil
}

func TestRecoveryLedgerPostgres(t *testing.T) {
	s := &Service{DB: pgtest.New(t), PostgreSQL: postgres(t)}
	r := &JobRecovery{Service: s}
	ctx := context.Background()
	c := RecoveryCandidate{Job: jobs.Job{Repo: "owner/repo", Issue: 1, Generation: 0}, Cause: "A"}
	got, id, err := r.Reserve(ctx, c)
	if err != nil || got != "proceed" || id == "" {
		t.Fatal(got, id, err)
	}
	restarted := &JobRecovery{Service: s}
	got, _, err = restarted.Reserve(ctx, c)
	if err != nil || got != "already_reserved" {
		t.Fatal(got, err)
	}
	if err = r.Finish(ctx, c, id, "sent"); err != nil {
		t.Fatal(err)
	}
	c.Job.Generation++
	got, _, err = r.Reserve(ctx, c)
	if err != nil || got != "abandon" {
		t.Fatal(got, err)
	}
	a, ok, err := abandonedCause(ctx, r, c.Job.Repo, c.Job.Issue)
	if err != nil || !ok || a.CauseHash != "A" || a.At.IsZero() {
		t.Fatal(a, ok, err)
	}
	c.Cause = "B"
	got, id, err = r.Reserve(ctx, c)
	if err != nil || got != "proceed" {
		t.Fatal(got, err)
	}
	records, err := r.records(ctx)
	if err != nil || len(records) != 1 || records[0].Causes["B"].PreviousCause != "A" || records[0].Causes["B"].State != "reserved" {
		t.Fatal(records, err)
	}
	if err = r.Finish(ctx, c, id, "skipped"); err != nil {
		t.Fatal(err)
	}
	c.Cause = "A"
	c.Job.Generation++
	got, _, err = r.Reserve(ctx, c)
	if err != nil || got != "already_reserved" {
		t.Fatal(got, err)
	}
}

func TestRecoveryCandidatesPostgres(t *testing.T) {
	s := &Service{DB: pgtest.New(t), PostgreSQL: postgres(t), Repositories: func() ([]string, error) { return []string{"owner/repo"}, nil }}
	r := &JobRecovery{Service: s, UsageLimit: 100}
	for _, row := range []struct {
		issue                int
		status, hold, result string
	}{{1, "failed", "", `{"error":"failed"}`}, {2, "held", "question", `{"questions":["why?"]}`}, {3, "held", "review_held", `{"summary":"review","review":{"findings":[{"message":"fix"}]}}`}, {4, "held", "question", `{"questions":[]}`}} {
		var hold any
		if row.hold != "" {
			hold = row.hold
		}
		_, err := s.PostgreSQL.Exec(`INSERT INTO jobs(repo,issue,status,hold_reason,result,view,updated_at) VALUES('owner/repo',$1,$2,$3,$4::jsonb,'{"reviewHolds":0}'::jsonb,now()-interval '1 hour')`, row.issue, row.status, hold, row.result)
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.Candidates(context.Background())
	if err != nil || len(got) != 3 {
		t.Fatalf("%v %v", got, err)
	}
	for i, c := range got {
		if c.Job.Issue != int64(i+1) {
			t.Fatal(got)
		}
	}
	ok, err := r.Recheck(context.Background(), got[0])
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	_, err = s.PostgreSQL.Exec(`UPDATE jobs SET view='{"stopRequested":true}' WHERE issue=1`)
	if err != nil {
		t.Fatal(err)
	}
	ok, err = r.Recheck(context.Background(), got[0])
	if err != nil || ok {
		t.Fatal(ok, err)
	}
}
