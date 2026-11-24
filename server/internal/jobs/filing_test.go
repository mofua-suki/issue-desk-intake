package jobs

import (
	"strconv"
	"sync"
	"testing"
)

func TestPostgresFilingCreatesOnceAndKeepsOriginalDecision(t *testing.T) {
	db, ctx := testPostgres(t)
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err := s.Create(ctx, NewJob{Repo: "owner/app", Issue: 1, Title: "先行", Scope: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	value := NewJob{Repo: "owner/app", Issue: 2, Title: "依頼", Scope: []string{"b.go"}, DependsOn: []int64{predecessor.Job.ID, predecessor.Job.ID}, View: map[string]any{"futureField": true}}
	var wg sync.WaitGroup
	results := make(chan CreatedJob, 2)
	failures := make(chan error, 2)
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); result, err := s.Create(ctx, value); results <- result; failures <- err }()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	created := 0
	var id int64
	for result := range results {
		if result.Created {
			created++
		}
		if id != 0 && id != result.Job.ID {
			t.Fatal("duplicate job")
		}
		id = result.Job.ID
	}
	if created != 1 {
		t.Fatal("not exactly one create", created)
	}
	duplicate, err := s.Create(ctx, NewJob{Repo: "owner/app", Issue: 2, Title: "別の題名", Scope: []string{"changed.go"}})
	if err != nil || duplicate.Created || duplicate.Job.Title != "依頼" || len(duplicate.Job.Scope) != 1 || duplicate.Job.Scope[0] != "b.go" {
		t.Fatal("existing decision changed", err)
	}
	detail, err := s.Detail(ctx, id)
	if err != nil || len(detail["dependsOn"].([]int64)) != 1 {
		t.Fatal("dependency duplication", detail, err)
	}
}
func TestPostgresFilingRollsBackMissingDependency(t *testing.T) {
	db, ctx := testPostgres(t)
	s, _ := New(db, Options{})
	if _, err := s.Create(ctx, NewJob{Repo: "owner/app", Issue: 1, Scope: []string{}, DependsOn: []int64{9999}}); err == nil {
		t.Fatal("missing dependency accepted")
	}
	job, err := s.ByIssue(ctx, "owner/app", 1)
	if err != nil || job != nil {
		t.Fatal("partially filed job remains")
	}
	result, err := s.Create(ctx, NewJob{Repo: "owner/app", Issue: 1, Scope: []string{}})
	if err != nil || result.Job.Scope == nil || len(result.Job.Scope) != 0 {
		t.Fatal("explicit empty scope changed", result, err)
	}
	if _, err = s.Create(ctx, NewJob{Repo: "owner/app", Issue: 2, Kind: "investigation"}); err == nil {
		t.Fatal("retired kind accepted")
	}
}

func TestPostgresFilingReferencesResolveOnlyAfterPredecessorExists(t *testing.T) {
	db, ctx := testPostgres(t)
	s, _ := New(db, Options{})
	dependent, err := s.Create(ctx, NewJob{Repo: "owner/app", Issue: 2, Scope: []string{"b.go"}, View: map[string]any{"dependsOnIssues": []any{map[string]any{"repo": "owner/app", "issue": 1}}, "unresolvedDependencies": []any{map[string]any{"repo": "owner/app", "issue": 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ResolveFilingReferences(ctx); err != nil {
		t.Fatal(err)
	}
	job, _ := s.Get(ctx, dependent.Job.ID)
	if len(job.View["unresolvedDependencies"].([]any)) != 1 {
		t.Fatal("missing predecessor erased")
	}
	predecessor, err := s.Create(ctx, NewJob{Repo: "owner/app", Issue: 1, Scope: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ResolveFilingReferences(ctx); err != nil {
		t.Fatal(err)
	}
	detail, err := s.Detail(ctx, dependent.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	deps := detail["dependsOn"].([]int64)
	if len(deps) != 1 || deps[0] != predecessor.Job.ID {
		t.Fatal("missing durable edge", deps)
	}
	job, _ = s.Get(ctx, dependent.Job.ID)
	if job.View["unresolvedDependencies"] != nil {
		t.Fatal("resolved reference remains pending")
	}
}

func TestPostgresExplicitDependencyRejectsCycleAndTakesOwnership(t *testing.T) {
	db, ctx := testPostgres(t)
	s, _ := New(db, Options{})
	first, err := s.Create(ctx, NewJob{Repo: "owner/app", Issue: 1, Scope: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Create(ctx, NewJob{Repo: "owner/app", Issue: 2, Scope: []string{"b.go"}, DependsOn: []int64{first.Job.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddDependencies(ctx, first.Job.ID, []int64{second.Job.ID}); err == nil {
		t.Fatal("indirect cycle accepted")
	}
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_build_object('acceptanceTransferWait',jsonb_build_object('ownedTargets',jsonb_build_object($2::text,true))) WHERE id=$1`, second.Job.ID, strconv.FormatInt(first.Job.ID, 10)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddDependencies(ctx, second.Job.ID, []int64{first.Job.ID}); err != nil {
		t.Fatal(err)
	}
	job, err := s.Get(ctx, second.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	wait := job.View["acceptanceTransferWait"].(map[string]any)
	owned := wait["ownedTargets"].(map[string]any)
	if len(owned) != 0 {
		t.Fatal("ordinary dependency remains owned by automatic transfer", owned)
	}
}
