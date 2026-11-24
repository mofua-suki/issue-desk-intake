package startup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"issue-desk-intake/server/internal/fleet"
)

func groupTestSource(t *testing.T, groups any, omit bool) *fleet.Service {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	var raw map[string]any
	if err := fleet.ReadJSON(filepath.Join(root, "assignments.json"), &raw); err != nil {
		t.Fatal(err)
	}
	if omit {
		delete(raw, "repositoryGroups")
	} else {
		raw["repositoryGroups"] = groups
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "assignments.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return &fleet.Service{FleetPath: filepath.Join(root, "fleet.json"), AssignmentsPath: path}
}

func shippedAssignmentsSource(t *testing.T) *fleet.Service {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	return &fleet.Service{FleetPath: filepath.Join(root, "fleet.json"), AssignmentsPath: filepath.Join(root, "assignments.json")}
}

func TestRepositoryGroupsAllowedCandidates(t *testing.T) {
	// Load the shipped assignment file, whose two members are both assigned hosts.
	source := shippedAssignmentsSource(t)
	_, assignments, err := source.Load()
	if err != nil {
		t.Fatal(err)
	}
	pair := []any{"mofua-suki/sample-expense-server", "mofua-suki/sample-expense-client"}
	for _, name := range pair {
		if len(assignments.Implementers[name.(string)]) == 0 {
			t.Fatalf("missing assignment for %s", name)
		}
	}
	hasPair := func(shared map[string]any) bool {
		for _, group := range fleet.List(fleet.Object(shared["assignments"])["repositoryGroups"]) {
			if reflect.DeepEqual(group, pair) {
				return true
			}
		}
		return false
	}
	// A local configuration older than the distributed assignments must not stop the intake.
	for _, candidates := range [][]string{nil, {"mofua-suki/issue-desk-intake"}, {"mofua-suki/sample-expense-server"}, {"mofua-suki/sample-expense-client"}} {
		c := &Config{Values: map[string]any{}, Repositories: candidates}
		c.groupWarnings = &repositoryGroupWarnings{emit: func(string) {}}
		shared, err := c.Shared(context.Background(), source)
		if err != nil {
			t.Fatalf("%v: %v", candidates, err)
		}
		if hasPair(shared) {
			t.Fatalf("%v: group with missing candidates kept", candidates)
		}
	}
	c := &Config{Values: map[string]any{}, Repositories: []string{"mofua-suki/sample-expense-server", "mofua-suki/sample-expense-client"}}
	shared, err := c.Shared(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if !hasPair(shared) {
		t.Fatal("group with every candidate dropped")
	}
}

func TestRepositoryGroupsStartupToleratesMissingCandidates(t *testing.T) {
	// The lifecycle fixture only allows owner/repo; switch it to the shipped
	// assignments, whose groups name repositories this intake does not handle.
	active := openLifecycleApp(t, lifecycleGitea, nil)
	active.StopBackground()
	active.Config.AssignmentsPath = shippedAssignmentsPath(t)
	var warnings []string
	active.Config.groupWarnings = &repositoryGroupWarnings{emit: func(message string) { warnings = append(warnings, message) }}
	standby, err := OpenBusinessApp(context.Background(), active.Config, active.DB, active.Revision, func(string) {})
	if err != nil {
		t.Fatalf("startup stopped by repository group mismatch: %v", err)
	}
	defer standby.Close()
	if standby.Engine == nil || standby.Gitea == nil {
		t.Fatal("business services not initialized")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "mofua-suki/sample-expense-server") {
		t.Fatalf("warnings: %#v", warnings)
	}
}

func TestRepositoryGroupsWarningDeduplication(t *testing.T) {
	c := &Config{Values: map[string]any{}, Repositories: []string{"owner/one", "owner/two"}}
	var mu sync.Mutex
	var warnings []string
	c.groupWarnings = &repositoryGroupWarnings{emit: func(message string) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings, message)
	}}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(warnings)
	}
	missing := groupTestSource(t, []any{[]any{"owner/one", "owner/three"}}, false)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Shared(context.Background(), missing); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if count() != 1 || !strings.Contains(warnings[0], "owner/one, owner/three") || !strings.Contains(warnings[0], "候補に無い: owner/three") {
		t.Fatalf("first mismatch: %#v", warnings)
	}
	if _, err := c.Shared(context.Background(), missing); err != nil || count() != 1 {
		t.Fatalf("repeated mismatch logged again: %v %#v", err, warnings)
	}
	changed := groupTestSource(t, []any{[]any{"owner/two", "owner/four"}}, false)
	if _, err := c.Shared(context.Background(), changed); err != nil || count() != 2 || !strings.Contains(warnings[1], "owner/four") {
		t.Fatalf("changed mismatch: %v %#v", err, warnings)
	}
	// Resolving the mismatch and seeing it again records it again.
	if _, err := c.Shared(context.Background(), groupTestSource(t, nil, true)); err != nil || count() != 2 {
		t.Fatalf("resolved: %v %#v", err, warnings)
	}
	if _, err := c.Shared(context.Background(), changed); err != nil || count() != 3 {
		t.Fatalf("recurred: %v %#v", err, warnings)
	}
}

func TestRepositoryGroupsSharedConfiguration(t *testing.T) {
	c := &Config{Values: map[string]any{"model": "local"}, Repositories: []string{"owner/one", "owner/two", "owner/seven"}}
	absent, err := c.Shared(context.Background(), groupTestSource(t, nil, true))
	if err != nil {
		t.Fatal(err)
	}
	c.groupWarnings = &repositoryGroupWarnings{emit: func(string) {}}
	source := groupTestSource(t, []any{[]any{"owner/seven", "owner/three"}, []any{"Owner/one", "owner/two"}, []any{"owner/four", "owner/five"}}, false)
	before, _, err := source.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, loaded, err := source.Load()
	if err != nil {
		t.Fatal(err)
	}
	present, err := c.Shared(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	after, reloaded, err := source.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(loaded.RepositoryGroups, reloaded.RepositoryGroups) || len(reloaded.RepositoryGroups) != 3 {
		t.Fatal("filtering changed the loaded assignments")
	}
	a, b := fleet.Object(absent["assignments"]), fleet.Object(present["assignments"])
	if got := b["repositoryGroups"]; !reflect.DeepEqual(got, []any{[]any{"Owner/one", "owner/two"}}) {
		t.Fatalf("groups: %#v", got)
	}
	if got := a["repositoryGroups"]; !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("omitted: %#v", got)
	}
	delete(a, "repositoryGroups")
	delete(b, "repositoryGroups")
	if !reflect.DeepEqual(absent, present) {
		t.Fatal("group registration changed other shared settings or created approval data")
	}
}

func TestRepositoryGroupsFilterKeepsInput(t *testing.T) {
	groups := [][]string{{"owner/one", "owner/three"}, {"Owner/one", "owner/two"}}
	original := [][]string{{"owner/one", "owner/three"}, {"Owner/one", "owner/two"}}
	valid, disabled := filterRepositoryGroups(groups, []string{"owner/one", "owner/two"})
	if !reflect.DeepEqual(groups, original) {
		t.Fatalf("input changed: %#v", groups)
	}
	if !reflect.DeepEqual(valid, [][]string{{"Owner/one", "owner/two"}}) || len(disabled) != 1 || !reflect.DeepEqual(disabled[0].Missing, []string{"owner/three"}) {
		t.Fatalf("valid=%#v disabled=%#v", valid, disabled)
	}
	valid[0][0] = "changed"
	disabled[0].Group[0] = "changed"
	if !reflect.DeepEqual(groups, original) {
		t.Fatalf("result shares input: %#v", groups)
	}
}
