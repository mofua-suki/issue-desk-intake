package screenapi

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"testing"
)

// A candidate that could not be read must not turn its displayed rows into
// removals; readable candidates keep updating, and real deletions surface
// once the candidate is readable again.
func TestPartialListPreservesPreviousRows(t *testing.T) {
	s := &Service{o: Options{Config: func() (Object, error) {
		return Object{"repositories": []any{Object{"name": "owner/ok"}, Object{"name": "owner/gone"}}}, nil
	}, Lists: map[string]ListReader{}, Aux: map[string]func(context.Context) (any, error){}}}
	failing := false
	okTitle := "読める候補"
	goneRows := []Object{{"project": "owner/gone", "number": 1, "updatedAt": "1"}, {"project": "owner/gone", "number": 2, "updatedAt": "2"}}
	for _, kind := range rowTypes {
		kind := kind
		s.o.Lists[kind] = func(ctx context.Context, _ string) ([]Object, error) {
			if kind != "issue" {
				return []Object{}, nil
			}
			rows := []Object{{"project": "owner/ok", "number": 7, "updatedAt": "3", "title": okTitle}}
			if failing {
				ReportUnreadProjects(ctx, []UnreadProject{{Project: "owner/gone", Reason: "GiteaがHTTP 404を返しました。"}})
				return rows, nil
			}
			return append(rows, goneRows...), nil
		}
	}
	d := NewDelta(DeltaOptions{})
	load := func(ctx context.Context) (Snapshot, error) { return s.snapshot(ctx, "", newListTiming()) }
	ctx := context.Background()
	full, e := d.Respond(ctx, "scope", nil, load)
	if e != nil || len(full["rows"].([]Row)) != 3 || full["unreadProjects"] != nil {
		t.Fatal(full, e)
	}
	cursor := fleet.Text(full["cursor"])

	failing, okTitle = true, "更新後"
	change, e := d.Respond(ctx, "scope", &cursor, load)
	if e != nil || change["full"] != false {
		t.Fatal(change, e)
	}
	if removed := change["removed"].([]string); len(removed) != 0 {
		t.Fatalf("unread candidate rows were removed: %v", removed)
	}
	if updated := change["updated"].([]Row); len(updated) != 1 || updated[0].Key != "issue:owner/ok#7" {
		t.Fatalf("readable candidate update missing: %v", updated)
	}
	unread, _ := change["unreadProjects"].([]UnreadProject)
	if len(unread) != 1 || unread[0].Project != "owner/gone" || unread[0].Reason == "" {
		t.Fatalf("unread candidate not reported: %v", change["unreadProjects"])
	}
	cursor = fleet.Text(change["cursor"])

	// Still unreadable: still not removed.
	change, e = d.Respond(ctx, "scope", &cursor, load)
	if e != nil || len(change["removed"].([]string)) != 0 || change["unreadProjects"] == nil {
		t.Fatal(change, e)
	}
	cursor = fleet.Text(change["cursor"])

	// A full response reports the unread candidate so the screen keeps its rows.
	fresh, e := d.Respond(ctx, "scope", nil, load)
	if e != nil || fresh["unreadProjects"] == nil || len(fresh["rows"].([]Row)) != 1 {
		t.Fatal(fresh, e)
	}

	// Readable again with #2 deleted meanwhile: only the real deletion is removed,
	// the unchanged carried row is not resent.
	failing, goneRows = false, goneRows[:1]
	change, e = d.Respond(ctx, "scope", &cursor, load)
	if e != nil || change["unreadProjects"] != nil {
		t.Fatal(change, e)
	}
	if removed := change["removed"].([]string); len(removed) != 1 || removed[0] != "issue:owner/gone#2" {
		t.Fatalf("deletion after recovery: %v", removed)
	}
	if len(change["added"].([]Row)) != 0 || len(change["updated"].([]Row)) != 0 {
		t.Fatalf("carried row changed: %v", change)
	}
}

// The tab counts omit an unread candidate's Issues, so they are marked partial
// and name the candidate.
func TestPartialListCountsArePartial(t *testing.T) {
	f := newFixture(t)
	f.s.o.Lists["issue"] = func(ctx context.Context, _ string) ([]Object, error) {
		ReportUnreadProjects(ctx, []UnreadProject{{Project: "owner/gone", Reason: "GiteaがHTTP 404を返しました。"}})
		return []Object{{"project": "owner/repo", "number": 1, "status": "running"}}, nil
	}
	code, value := f.call("GET", "/api/list?view=counts", "screen", nil)
	body := fleet.ObjectOrEmpty(value)
	unread := fleet.Items(body["unreadProjects"])
	if code != 200 || body["partial"] != true || len(unread) != 1 || fleet.ObjectOrEmpty(unread[0])["project"] != "owner/gone" {
		t.Fatal(code, body)
	}
}

// The counts' fill-in of saved filing results skips unread candidates: their
// Issues are unknown, not missing, so they must not become pending rows.
func TestPartialListCountsDoNotFillUnreadIssues(t *testing.T) {
	snapshot := Snapshot{Rows: []Row{
		{Key: "draft:a", Type: "draft", Item: Object{"id": "a", "project": "owner/ok", "status": "submitted", "issues": []any{Object{"project": "owner/gone", "number": 5}, Object{"number": 7}}}},
		{Key: "draft:b", Type: "draft", Item: Object{"id": "b", "project": "owner/ok", "status": "investigation_waiting", "investigations": []any{Object{"project": "owner/gone", "number": 9}}}},
	}}
	pending := func(s Snapshot) []string {
		out := []string{}
		for _, r := range classifiedListRows(s) {
			if r["kind"] == "issue" && r["status"] == "pending" {
				out = append(out, fmt.Sprint(r["project"], "#", r["number"]))
			}
		}
		return out
	}
	if got := pending(snapshot); len(got) != 3 {
		t.Fatalf("readable candidates must still be filled in: %v", got)
	}
	snapshot.UnreadProjects = []UnreadProject{{Project: "owner/gone", Reason: "読めません"}}
	if got := pending(snapshot); len(got) != 1 || got[0] != "owner/ok#7" {
		t.Fatalf("pending=%v", got)
	}
}
