package issueapi

import (
	"errors"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"strings"
	"testing"
)

// partialFixture serves owner/repo (already numbered by newFixture) and
// owner/gone, whose Issue read fails with a Gitea 404.
func partialFixture(t *testing.T) (*fixture, *multiRepoFixture) {
	t.Helper()
	f := newFixture(t)
	known := clone(f.api.issue)
	known["number"] = 3
	closed := clone(f.api.issue)
	closed["number"] = 4
	closed["state"] = "closed"
	closed["closed_at"] = "2026-10-01T00:00:00Z"
	gone := clone(f.api.issue)
	gone["number"] = 7
	api := &multiRepoFixture{API: f.api, items: map[string][]Object{"owner/repo": {clone(f.api.issue), known, closed}, "owner/gone": {gone}}, errors: map[string]error{"owner/gone": &gitea.Error{Status: 404}}}
	f.s.o.Gitea = api
	f.s.o.Repositories = func() []string { return []string{"owner/repo", "owner/gone"} }
	f.s.o.Project = func(string) error { return nil }
	return f, api
}

func unreadOf(t *testing.T, value any) []UnreadProject {
	t.Helper()
	unread, ok := value.([]UnreadProject)
	if !ok {
		t.Fatalf("unreadProjects: %#v", value)
	}
	return unread
}

func TestSearchReturnsReadableCandidatesWhenOneRepositoryFailsPostgres(t *testing.T) {
	f, _ := partialFixture(t)
	result, err := f.s.search(f.ctx, Object{"state": "open"}, auth.Principal{})
	if err != nil {
		t.Fatal(err)
	}
	issues := fleet.Items(result["issues"])
	if result["total"] != 2 || len(issues) != 2 || result["incomplete"] != true {
		t.Fatalf("partial search: %v", result)
	}
	for _, issue := range issues {
		if fleet.ObjectOrEmpty(issue)["project"] != "owner/repo" {
			t.Fatalf("row of an unread repository: %v", issue)
		}
	}
	unread := unreadOf(t, result["unreadProjects"])
	if len(unread) != 1 || unread[0].Project != "owner/gone" || !strings.Contains(unread[0].Reason, "404") {
		t.Fatalf("unread: %#v", unread)
	}
	count, err := f.s.search(f.ctx, Object{"state": "open", "countOnly": "1"}, auth.Principal{})
	if err != nil || count["total"] != 2 || count["incomplete"] != true || len(unreadOf(t, count["unreadProjects"])) != 1 {
		t.Fatalf("partial count: %v %v", count, err)
	}
	// Filtered to a readable project: its rows are complete, but the unread
	// candidate is still reported.
	filtered, err := f.s.search(f.ctx, Object{"state": "open", "project": "owner/repo"}, auth.Principal{})
	if err != nil || filtered["total"] != 2 || filtered["incomplete"] != nil || len(unreadOf(t, filtered["unreadProjects"])) != 1 {
		t.Fatalf("filtered: %v %v", filtered, err)
	}
	// A known project is still numbered from a partial list.
	if _, ok, err := f.s.o.Numbers.Number(f.ctx, "owner/repo", 3); err != nil || !ok {
		t.Fatalf("known project number: %v %v", ok, err)
	}
	if _, ok, err := f.s.o.Numbers.Number(f.ctx, "owner/gone", 7); err != nil || ok {
		t.Fatalf("unread project numbered: %v %v", ok, err)
	}
	rows, unreadRows, err := f.s.ListPartial(f.ctx)
	if err != nil || len(rows) != 3 || len(unreadRows) != 1 || unreadRows[0].Project != "owner/gone" {
		t.Fatalf("ListPartial: %d %v %v", len(rows), unreadRows, err)
	}
}

func TestRequestedRepositoryFailureIsAnErrorPostgres(t *testing.T) {
	f, api := partialFixture(t)
	_, err := f.s.search(f.ctx, Object{"state": "open", "project": "owner/gone"}, auth.Principal{})
	var giteaErr *gitea.Error
	if !errors.As(err, &giteaErr) || giteaErr.Status != 404 {
		t.Fatalf("requested project: %v", err)
	}
	// Nothing readable is an error, not an empty partial result.
	api.errors["owner/repo"] = &gitea.Error{Status: 404}
	if result, err := f.s.search(f.ctx, Object{"state": "open"}, auth.Principal{}); err == nil {
		t.Fatalf("all unread: %v", result)
	}
}

func TestIssueArrayRefusesPartialListPostgres(t *testing.T) {
	f, api := partialFixture(t)
	code, value := f.call("GET", "/api/issues", "screen", nil)
	if code != 503 || !strings.Contains(fleet.Text(fleet.ObjectOrEmpty(value)["error"]), "owner/gone") {
		t.Fatalf("partial array: %d %v", code, value)
	}
	delete(api.errors, "owner/gone")
	code, value = f.call("GET", "/api/issues", "screen", nil)
	if items, ok := value.([]any); code != 200 || !ok || len(items) != 4 {
		t.Fatalf("recovered array: %d %v", code, value)
	}
}

func TestPartialListDoesNotRecordCloseBaselinePostgres(t *testing.T) {
	f, api := partialFixture(t)
	if _, err := f.s.search(f.ctx, Object{"state": "open"}, auth.Principal{}); err != nil {
		t.Fatal(err)
	}
	if baseline, err := f.s.closeBaseline(f.ctx); err != nil || baseline != nil {
		t.Fatalf("baseline recorded from a partial list: %v %v", baseline, err)
	}
	delete(api.errors, "owner/gone")
	if _, err := f.s.search(f.ctx, Object{"state": "open"}, auth.Principal{}); err != nil {
		t.Fatal(err)
	}
	baseline, err := f.s.closeBaseline(f.ctx)
	if err != nil || fleet.ObjectOrEmpty(baseline["issues"])["owner/repo#4"] != "2026-10-01T00:00:00Z" {
		t.Fatalf("baseline after recovery: %v %v", baseline, err)
	}
	if _, ok, err := f.s.o.Numbers.Number(f.ctx, "owner/gone", 7); err != nil || !ok {
		t.Fatalf("recovered project number: %v %v", ok, err)
	}
}
