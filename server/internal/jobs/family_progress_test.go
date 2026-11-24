package jobs

import (
	"reflect"
	"strings"
	"testing"
)

func TestPostgresFamilyDependenciesFollowDispatch(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 1)
	pred := seedJob(t, ctx, db, "owner/repo", "nas", 2, 1)
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET view='{"dependsOnIssues":[{"repo":"owner/repo","issue":2}]}' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		all, e := s.AllForIssues(ctx)
		if e != nil {
			t.Fatal(e)
		}
		var target *Job
		for _, job := range all {
			if job.ID == id {
				target = job
				break
			}
		}
		if target == nil {
			t.Fatal("target job missing")
		}
		ready, e := DependenciesReady(ctx, db, id, target.View)
		if e != nil || ready == want {
			t.Fatalf("DependenciesReady=%v err=%v want waiting=%v", ready, e, want)
		}
		waiting, e := s.FamilyDependencies(ctx, all)
		if e != nil || (len(waiting[id]) > 0) != want {
			t.Fatalf("waiting=%v err=%v", waiting, e)
		}
	}
	check(true)
	if _, err = db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES($1,$2)`, id, pred); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status string
		wait   bool
	}{{"running", true}, {"failed", true}, {"held", true}, {"stopped", true}, {"merged", false}, {"closed", true}} {
		if _, err = db.ExecContext(ctx, `UPDATE jobs SET status=$1, hold_reason=CASE WHEN $1='held' THEN 'question' ELSE NULL END WHERE id=$2`, tc.status, pred); err != nil {
			t.Fatal(err)
		}
		check(tc.wait)
	}
}

func TestPostgresFamilyIssueReadsMatchFull(t *testing.T) {
	db, ctx := testPostgres(t)
	a := seedJob(t, ctx, db, "owner/repo", "nas", 1, 1)
	b := seedJob(t, ctx, db, "OWNER/REPO", "nas", 1, 1)
	missing := seedJob(t, ctx, db, "owner/repo", "nas", 2, 1)
	invalid := seedJob(t, ctx, db, "owner/repo", "nas", 3, 1)
	pred := seedJob(t, ctx, db, "owner/repo", "nas", 4, 1)
	outside := seedJob(t, ctx, db, "outside/repo", "nas", 9, 1)
	for _, x := range []struct {
		id   int64
		view string
	}{{a, `{"dependsOnIssues":[{"repo":"owner/repo","issue":4}]}`}, {missing, `{"dependsOnIssues":[{"repo":"owner/repo","issue":8}]}`}, {invalid, `{"dependsOnIssues":"bad"}`}, {outside, `{"dependsOnIssues":[{"repo":"owner/repo","issue":4}]}`}} {
		if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=$1::jsonb WHERE id=$2`, x.view, x.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES($1,$2),($3,$2)`, a, pred, outside); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	all, err := s.AllForIssues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := s.ForIssues(ctx, []Issue{{"Owner/Repo", 1}, {"owner/repo", 2}, {"owner/repo", 3}, {"missing/repo", 99}, {"OWNER/REPO", 1}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]*Job{}
	for _, j := range all {
		if j.ID == a || j.ID == b || j.ID == missing || j.ID == invalid {
			want[j.ID] = j
		}
	}
	got := map[int64]*Job{}
	for _, j := range selected {
		got[j.ID] = j
	}
	if !reflect.DeepEqual(got, want) || len(got) != 4 {
		t.Fatalf("selected=%v want=%v", got, want)
	}
	fullDeps, err := s.FamilyDependencies(ctx, all)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := s.FamilyDependencies(ctx, selected)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[int64][]string{}
	for id, reasons := range fullDeps {
		if _, ok := want[id]; ok {
			expected[id] = reasons
		}
	}
	if !reflect.DeepEqual(filtered, expected) {
		t.Fatalf("dependencies=%v want=%v", filtered, expected)
	}
	if len(filtered[a]) != 1 || len(filtered[missing]) != 1 || len(filtered[invalid]) != 1 || !strings.Contains(filtered[missing][0], "依存の記録なし") || filtered[invalid][0] != "依存の記録が不正です" {
		t.Fatalf("unexpected reasons: %v", filtered)
	}
	// A nil database makes any accidental query on empty input fail.
	empty := &Store{}
	if rows, err := empty.ForIssues(ctx, nil); err != nil || len(rows) != 0 {
		t.Fatalf("empty jobs: %v %v", rows, err)
	}
	if deps, err := empty.FamilyDependencies(ctx, nil); err != nil || len(deps) != 0 {
		t.Fatalf("empty deps: %v %v", deps, err)
	}
}
