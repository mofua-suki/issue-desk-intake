package startup

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/consultations"
)

// A consultation that filed Issues into a candidate that cannot be read must not
// stay in the active tabs: its Issues are unknown, not missing, and the screen
// would show them as fictitious pending rows. Checked on the active diff, a
// fresh active read (reopened screen), the full read, and view=counts/work.
func TestScopeUnreadCandidateConsultationsAreNotPending(t *testing.T) {
	const (
		submitted = "00000000-0000-4000-8000-000000000011"
		closed    = "00000000-0000-4000-8000-000000000012"
	)
	c := scopeCase{
		Name:   "unread-candidate",
		Issues: []map[string]any{{"number": float64(1), "state": "open"}},
		Drafts: []consultations.Record{
			{"id": submitted, "project": "owner/app", "status": "submitted", "issue": map[string]any{"project": "owner/gone", "number": float64(5)}},
			{"id": closed, "project": "owner/app", "status": "closed", "issues": []any{map[string]any{"project": "owner/gone", "number": float64(6)}}},
		},
		Other: []string{"owner/gone"},
	}
	_, api, _, _, h := setupScopeCase(t, c)
	gone := func(n float64, state string) map[string]any {
		return map[string]any{"number": n, "state": state, "title": "gone", "user": map[string]any{"login": "intake"}, "created_at": "2026-11-01T00:00:00Z", "updated_at": "2026-11-01T00:00:00Z", "closed_at": "2026-11-02T00:00:00Z", "body": ""}
	}
	api.other["owner/gone"] = []map[string]any{gone(5, "open"), gone(6, "closed")}

	// Readable: #5 is listed, #6 is omitted as closed, so neither consultation is active.
	first, _, _ := scopeRequest(t, h, "/api/list?tabs=active")
	rows := scopeRows(first)
	if rows["issue:owner/gone#5"] == nil || rows["draft:"+submitted] != nil || rows["draft:"+closed] != nil || first["unreadProjects"] != nil {
		t.Fatalf("readable active rows=%v", keysOf(rows))
	}

	api.unread["owner/gone"] = true
	noDraft := func(name string, keys []string) {
		t.Helper()
		for _, k := range keys {
			if k == "draft:"+submitted || k == "draft:"+closed {
				t.Fatalf("%s: unread candidate's consultation became active: %v", name, keys)
			}
		}
	}
	unreadReported := func(name string, body map[string]any) {
		t.Helper()
		list, _ := body["unreadProjects"].([]any)
		if len(list) != 1 || list[0].(map[string]any)["project"] != "owner/gone" {
			t.Fatalf("%s: unreadProjects=%v", name, body["unreadProjects"])
		}
	}

	// tabs=active diff: the shown Issue row is kept and no consultation is added.
	diff, _, _ := scopeRequest(t, h, "/api/list?tabs=active&since="+url.QueryEscape(first["cursor"].(string)))
	unreadReported("diff", diff)
	for _, k := range diff["removed"].([]any) {
		if k == "issue:owner/gone#5" {
			t.Fatalf("diff removed an unread candidate's row: %v", diff["removed"])
		}
	}
	for _, field := range []string{"added", "updated"} {
		keys := []string{}
		for _, r := range diff[field].([]any) {
			keys = append(keys, r.(map[string]any)["key"].(string))
		}
		noDraft("diff "+field, keys)
	}

	// Reopened screen (fresh active read without a cursor).
	fresh, _, _ := scopeRequest(t, h, "/api/list?tabs=active")
	unreadReported("fresh", fresh)
	freshRows := scopeRows(fresh)
	noDraft("fresh", keysOf(freshRows))
	if freshRows["issue:owner/app#1"] == nil {
		t.Fatalf("readable candidate missing: %v", keysOf(freshRows))
	}

	// The full read keeps every consultation and names the unread candidate so
	// the screen skips filling in its Issues (requestRows' unreadProjects).
	full, _, _ := scopeRequest(t, h, "/api/list")
	unreadReported("full", full)
	if fullRows := scopeRows(full); fullRows["draft:"+submitted] == nil || fullRows["draft:"+closed] == nil {
		t.Fatalf("full rows=%v", keysOf(fullRows))
	}

	// view=counts/work: partial, and no pending row for the unread candidate.
	for _, view := range []string{"counts", "work"} {
		body, _, _ := scopeRequest(t, h, "/api/list?view="+view)
		unreadReported(view, body)
		if body["partial"] != true {
			t.Fatalf("%s not partial: %v", view, body)
		}
		raw, _ := json.Marshal(body["groups"])
		if strings.Contains(string(raw), "owner/gone") {
			t.Fatalf("%s groups contain the unread candidate: %s", view, raw)
		}
	}
}

func keysOf(rows map[string]map[string]any) []string {
	out := []string{}
	for k := range rows {
		out = append(out, k)
	}
	return out
}
