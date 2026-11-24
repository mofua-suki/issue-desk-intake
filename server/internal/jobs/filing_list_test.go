package jobs

import (
	"context"
	"reflect"
	"testing"
)

func TestPostgresFilingSnapshotIncludesAllDependencies(t *testing.T) {
	db, ctx := testPostgres(t)
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Create(ctx, NewJob{Repo: "owner/app", Issue: 1, Scope: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Create(ctx, NewJob{Repo: "owner/app", Issue: 2, Scope: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	dependent, err := s.Create(ctx, NewJob{Repo: "owner/app", Issue: 3, Scope: []string{}, DependsOn: []int64{second.Job.ID, first.Job.ID}})
	if err != nil {
		t.Fatal(err)
	}
	for issue := int64(4); issue <= 205; issue++ {
		if _, err = s.Create(ctx, NewJob{Repo: "owner/app", Issue: issue, Scope: []string{}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.Create(ctx, NewJob{Repo: "owner/other", Issue: 1, Scope: []string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "UPDATE jobs SET status='closed' WHERE id=$1", first.Job.ID); err != nil {
		t.Fatal(err)
	}
	rows, deps, err := s.ForFiling(ctx, "OWNER/APP")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 204 || len(deps) != 204 {
		t.Fatalf("comparison was paged or mixed: rows=%d deps=%d", len(rows), len(deps))
	}
	if _, ok := deps[first.Job.ID]; ok {
		t.Fatal("closed job remained a comparison candidate")
	}
	if !reflect.DeepEqual(deps[dependent.Job.ID], []int64{first.Job.ID, second.Job.ID}) {
		t.Fatalf("dependencies lost or unsorted: %v", deps[dependent.Job.ID])
	}
	if empty, ok := deps[second.Job.ID]; !ok || empty == nil || len(empty) != 0 {
		t.Fatalf("empty dependency set missing: %v", empty)
	}
	empty, edges, err := s.ForFiling(ctx, "owner/missing")
	if err != nil || len(empty) != 0 || len(edges) != 0 {
		t.Fatalf("empty repository: %v %v %v", empty, edges, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err = s.ForFiling(canceled, "owner/app"); err == nil {
		t.Fatal("failed read became an empty success")
	}
}
