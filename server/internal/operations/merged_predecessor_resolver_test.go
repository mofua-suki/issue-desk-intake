package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
)

const mergeTestSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type mergeTestGitea struct {
	failed        map[string]bool
	unmerged      bool
	missingCommit bool
	calls         int
	pull          map[string]any
}

func (f *mergeTestGitea) Do(_ context.Context, _, path string, _ any, out any) error {
	f.calls++
	if f.failed[path] {
		return errors.New("unavailable")
	}
	sha := mergeTestSHA
	if f.missingCommit {
		sha = ""
	}
	pull := map[string]any{"merged": !f.unmerged, "merge_commit_sha": sha}
	for key, value := range f.pull {
		pull[key] = value
	}
	data, _ := json.Marshal(pull)
	return json.Unmarshal(data, out)
}

func TestMergedPredecessorResolver(t *testing.T) {
	for _, tc := range []struct {
		name, reason, route, status string
		own, unmerged, want         bool
	}{
		{"direct_pr", "other_route", "owner/repo", "merged", false, false, true}, {"own_pr", "voluntary", "", "", true, false, true},
		{"voluntary", "voluntary", "", "", false, false, false}, {"mismatch/repository", "other_route", "other/repo", "merged", false, false, false},
		{"mismatch/refused", "other_route", "owner/repo", "refused", false, false, false}, {"mismatch/failed", "other_route", "owner/repo", "failed", false, false, false},
		{"mismatch/missing", "other_route", "owner/repo", "", false, false, false}, {"mismatch/unmerged", "other_route", "owner/repo", "merged", false, true, false},
		{"mismatch/missing_commit", "other_route", "owner/repo", "merged", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t)
			s.PostgreSQL = postgres(t)
			s.Gitea = &mergeTestGitea{unmerged: tc.unmerged, missingCommit: tc.name == "mismatch/missing_commit"}
			ctx := context.Background()
			if _, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS close_reasons(id TEXT PRIMARY KEY,json TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			reason, _ := json.Marshal(map[string]any{"reason": tc.reason, "route": map[string]any{"repo": tc.route, "pr": 12}})
			if _, err := s.DB.Exec(`INSERT INTO close_reasons(id,json) VALUES($1,$2)`, "owner/repo#1", string(reason)); err != nil {
				t.Fatal(err)
			}
			var own any
			if tc.own {
				own = 12
			}
			if _, err := s.PostgreSQL.Exec(`INSERT INTO jobs(id,repo,issue,status,pr) VALUES(1,'owner/repo',1,'closed',$1),(2,'owner/repo',2,'queued',NULL)`, own); err != nil {
				t.Fatal(err)
			}
			if _, err := s.PostgreSQL.Exec(`INSERT INTO job_deps(job_id,depends_on) VALUES(2,1); UPDATE jobs SET view='{"dependsOnIssues":[{"repo":"owner/repo","issue":1}]}' WHERE id=2`); err != nil {
				t.Fatal(err)
			}
			if tc.status != "" {
				if _, err := s.PostgreSQL.Exec(`INSERT INTO requests(id,kind,repo,payload,sender,status,result) VALUES('request','merge_manual','owner/repo','{"number":12}','pc','done',$1::jsonb)`, fmt.Sprintf(`{"status":%q}`, tc.status)); err != nil {
					t.Fatal(err)
				}
			}
			if err := (&MergedPredecessorResolver{Service: s}).Resolve(ctx); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := s.PostgreSQL.QueryRow(`SELECT view FROM jobs WHERE id=2`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var view map[string]any
			if err := json.Unmarshal(raw, &view); err != nil {
				t.Fatal(err)
			}
			ready, err := jobs.DependenciesReady(ctx, s.PostgreSQL, 2, view)
			if err != nil || ready != tc.want {
				t.Fatalf("ready=%v err=%v view=%v", ready, err, view)
			}
			record := fleet.Object(fleet.Object(view["dependencyMerges"])["1"])
			if !tc.want {
				if len(record) != 0 {
					t.Fatalf("unexpected record %v", record)
				}
				return
			}
			source := "merge_manual"
			if tc.own {
				source = "own_pr"
			}
			if fleet.Number(record["pr"]) != 12 || record["mergeCommit"] != mergeTestSHA || record["source"] != source || record["repo"] != "owner/repo" || record["checkedAt"] == nil {
				t.Fatalf("record %v", record)
			}
			store, err := jobs.New(s.PostgreSQL, jobs.Options{Now: s.Now})
			if err != nil {
				t.Fatal(err)
			}
			merges, err := store.DependencyMerges(ctx, 2)
			if err != nil || len(merges) != 1 || fleet.Object(merges[0])["mergeCommitSha"] != mergeTestSHA {
				t.Fatalf("merges %v err %v", merges, err)
			}
			if _, err = s.PostgreSQL.Exec(`INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,1)`, s.now()); err != nil {
				t.Fatal(err)
			}
			var a map[string]any
			_ = json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":1,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &a)
			assignments, err := fleet.NormalizeAssignments(a)
			if err != nil {
				t.Fatal(err)
			}
			job, _, err := store.Next(ctx, "nas", 1, assignments)
			if err != nil || job == nil || job.ID != 2 {
				t.Fatalf("dispatch %v err %v", job, err)
			}
		})
	}
	t.Run("read_failure/request", func(t *testing.T) {
		s := fixture(t)
		s.PostgreSQL = postgres(t)
		s.Gitea = &mergeTestGitea{}
		if _, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS close_reasons(id TEXT PRIMARY KEY,json TEXT NOT NULL); INSERT INTO close_reasons(id,json) VALUES('owner/repo#1','{"reason":"other_route","route":{"repo":"owner/repo","pr":12}}')`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PostgreSQL.Exec(`INSERT INTO jobs(id,repo,issue,status,pr) VALUES(1,'owner/repo',1,'closed',NULL),(2,'owner/repo',2,'closed',13),(3,'owner/repo',3,'queued',NULL); INSERT INTO job_deps(job_id,depends_on) VALUES(3,1),(3,2); INSERT INTO requests(id,kind,repo,payload,sender,status,result) VALUES('req','merge_manual','owner/repo','{"number":12}','pc','done','{"status":"merged"}'); ALTER TABLE requests RENAME TO requests_temporarily_unavailable`); err != nil {
			t.Fatal(err)
		}
		r := &MergedPredecessorResolver{Service: s}
		if err := r.Resolve(t.Context()); err == nil {
			t.Fatal("expected request read error")
		}
		var raw string
		if err := s.PostgreSQL.QueryRow(`SELECT view::text FROM jobs WHERE id=3`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(raw, `"1"`) || !strings.Contains(raw, `"2"`) {
			t.Fatal(raw)
		}
		if _, err := s.PostgreSQL.Exec(`ALTER TABLE requests_temporarily_unavailable RENAME TO requests`); err != nil {
			t.Fatal(err)
		}
		if err := r.Resolve(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := s.PostgreSQL.QueryRow(`SELECT view::text FROM jobs WHERE id=3`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(raw, `"1"`) || !strings.Contains(raw, `"2"`) {
			t.Fatal(raw)
		}
	})
	t.Run("read_failure", func(t *testing.T) {
		s := fixture(t)
		s.PostgreSQL = postgres(t)
		f := &mergeTestGitea{failed: map[string]bool{"/repos/owner/repo/pulls/12": true}}
		s.Gitea = f
		if _, err := s.PostgreSQL.Exec(`INSERT INTO jobs(id,repo,issue,status,pr) VALUES(1,'owner/repo',1,'closed',12),(2,'owner/repo',2,'closed',13),(3,'owner/repo',3,'queued',NULL); INSERT INTO job_deps(job_id,depends_on) VALUES(3,1),(3,2)`); err != nil {
			t.Fatal(err)
		}
		r := &MergedPredecessorResolver{Service: s}
		if err := r.Resolve(t.Context()); err == nil {
			t.Fatal("expected read error")
		}
		var raw string
		if err := s.PostgreSQL.QueryRow(`SELECT view::text FROM jobs WHERE id=3`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(raw, `"1"`) || !strings.Contains(raw, `"2"`) {
			t.Fatal(raw)
		}
		delete(f.failed, "/repos/owner/repo/pulls/12")
		if err := r.Resolve(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := s.PostgreSQL.QueryRow(`SELECT view::text FROM jobs WHERE id=3`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(raw, `"1"`) || !strings.Contains(raw, `"2"`) {
			t.Fatal(raw)
		}
	})
}

func TestVerifiedMergeReleasesDependencyAfterReportRejection(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	source := map[string]any{"sha": strings.Repeat("b", 40), "ref": "work", "repo": map[string]any{"full_name": "owner/repo"}}
	target := map[string]any{"ref": "other", "repo": map[string]any{"full_name": "owner/repo", "default_branch": "trunk"}}
	gitea := &mergeTestGitea{unmerged: true, pull: map[string]any{"number": 12, "head": source, "base": target}}
	s.Gitea = gitea
	if _, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS close_reasons(id TEXT PRIMARY KEY,json TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	setup := `INSERT INTO jobs(id,repo,issue,status,pr,generation,view,result) VALUES
		(1,'owner/repo',1,'failed',12,2,
		 jsonb_build_object('workerExecution',jsonb_build_object('stage','merge','generation',2),
		   'continuation',jsonb_build_object('artifact',jsonb_build_object('generation',2,'headSha',$1::text,'branch','work'))),
		 '{"leaseDiagnostics":{"lastReportRejection":{"reportKind":"merged","generation":2}}}'),
		(2,'owner/repo',2,'queued',NULL,0,'{"dependsOnIssues":[{"repo":"owner/repo","issue":1}]}','{}')`
	published := strings.Repeat("c", 40)
	if _, err := s.PostgreSQL.Exec(setup, published); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostgreSQL.Exec(`INSERT INTO job_deps(job_id,depends_on) VALUES(2,1)`); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	resolver := &MergedPredecessorResolver{Service: s}
	store, err := jobs.New(s.PostgreSQL, jobs.Options{Now: s.Now})
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(want bool) {
		t.Helper()
		if err := resolver.Resolve(ctx); err != nil {
			t.Fatal(err)
		}
		merges, err := store.DependencyMerges(ctx, 2)
		if err != nil || (len(merges) == 1) != want {
			t.Fatalf("merges=%v err=%v want=%v", merges, err, want)
		}
		if want {
			record := fleet.Object(merges[0])
			if record["status"] != "merged" || record["mergeCommitSha"] != mergeTestSHA {
				t.Fatalf("merge=%v", record)
			}
		}
	}
	resolve(false)
	gitea.unmerged = false
	target["ref"] = "trunk"
	resolve(false)
	source["sha"] = published
	target["ref"] = "other"
	resolve(false)
	target["ref"] = "trunk"
	resolve(true)
	family, err := store.FamilyDependencies(ctx, []*jobs.Job{{ID: 2}})
	if err != nil || len(family[2]) != 0 {
		t.Fatalf("family=%v err=%v", family, err)
	}
	var waiting bool
	if err := s.PostgreSQL.QueryRow(`SELECT dependency_waiting(2,1)`).Scan(&waiting); err != nil || waiting {
		t.Fatalf("dependency waiting=%v err=%v", waiting, err)
	}
	if _, err := s.PostgreSQL.Exec(`UPDATE jobs SET generation=3 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	resolve(false)
	if _, err := s.PostgreSQL.Exec(`UPDATE jobs SET status='closed',view=view-'continuation' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	resolve(false)
}

func TestMergedPredecessorSkipsTerminalSuccessors(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	f := &mergeTestGitea{unmerged: true}
	s.Gitea = f
	if _, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS close_reasons(id TEXT PRIMARY KEY,json TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostgreSQL.Exec(`INSERT INTO jobs(id,repo,issue,status,pr) VALUES(1,'owner/repo',1,'closed',12),(2,'owner/repo',2,'merged',NULL),(3,'owner/repo',3,'closed',NULL),(4,'owner/repo',4,'failed',NULL),(5,'owner/repo',5,'stopped',NULL); INSERT INTO job_deps(job_id,depends_on) VALUES(2,1),(3,1),(4,1),(5,1)`); err != nil {
		t.Fatal(err)
	}
	r := &MergedPredecessorResolver{Service: s}
	for i := 0; i < 2; i++ {
		if err := r.Resolve(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if f.calls != 0 {
		t.Fatalf("terminal successors caused %d calls", f.calls)
	}
	if _, err := s.PostgreSQL.Exec(`UPDATE jobs SET status='queued' WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	if err := r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatalf("reopened successor: calls=%d", f.calls)
	}
	f.unmerged = false
	if err := r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	var ready bool
	if err := s.PostgreSQL.QueryRow(`SELECT view->'dependencyMerges'->'1'->>'mergeCommit'=$1 FROM jobs WHERE id=2`, mergeTestSHA).Scan(&ready); err != nil || !ready {
		t.Fatalf("record ready=%v err=%v", ready, err)
	}
}
