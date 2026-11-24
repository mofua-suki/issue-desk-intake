package operations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
	"time"
)

func TestPostgresDependencyAlertPredecessorStates(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	s.StartedAt = s.now().Add(-time.Hour)
	s.Assignments = &fleet.Assignments{Hosts: map[string]*fleet.Host{"worker": {Max: 1}}, Implementers: map[string][]string{"owner/repo": {"worker"}}, Output: map[string]any{}}
	_, err := s.PostgreSQL.Exec(`INSERT INTO jobs(id,repo,issue,status) VALUES(1,'owner/prior',1,'merged'),(2,'owner/repo',2,'queued'),(3,'owner/repo',3,'queued'); INSERT INTO job_deps(job_id,depends_on) VALUES(2,1)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, status      string
		waiting, verified bool
	}{
		{"merged", "merged", false, false},
		{"closed without merge", "closed", true, false},
		{"closed with merge", "closed", false, true},
		{"queued", "queued", true, false},
		{"running", "running", true, false},
		{"held", "held", true, false},
		{"failed", "failed", true, false},
		{"stopped", "stopped", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.PostgreSQL.Exec(`UPDATE jobs SET status=$1,hold_reason=CASE WHEN $1='held' THEN 'question' ELSE NULL END WHERE id=1`, tc.status)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.PostgreSQL.Exec(`UPDATE jobs SET view=CASE WHEN $1
				THEN jsonb_build_object('dependencyMerges',jsonb_build_object('1',jsonb_build_object('pr',12,'mergeCommit',$2::text)))
				ELSE view-'dependencyMerges' END WHERE id=2`, tc.verified, mergeTestSHA)
			if err != nil {
				t.Fatal(err)
			}
			alerts, err := s.dispatchAlerts(context.Background(), map[string]any{})
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.waiting {
				want = 1
			}
			found := false
			for _, v := range alerts {
				row := fleet.Object(v)
				if row["kind"] == "dispatch_idle" {
					found = true
					if !strings.Contains(fleet.Text(row["detail"]), fmt.Sprintf("依存待ち %d件", want)) {
						t.Fatalf("alert=%v", row)
					}
				}
			}
			if !found {
				t.Fatalf("missing alert: %v", alerts)
			}
		})
	}
}
