package startup

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/listscope"
	"issue-desk-intake/server/internal/workplan"
)

// issuesRequest reads GET /api/issues as the signed-in screen. Rows are keyed
// by repository and Issue number; a refusal returns its status and message.
func issuesRequest(t *testing.T, h http.Handler, path string) (int, map[string]map[string]any, string) {
	t.Helper()
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787"+path, nil)
	r.AddCookie(&http.Cookie{Name: "desk_session", Value: "screen"})
	r.Header.Set("Origin", "http://127.0.0.1:8787")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		return w.Code, nil, w.Body.String()
	}
	var list []map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &list); e != nil || list == nil {
		t.Fatalf("%s: not a list: %v %s", path, e, w.Body.String())
	}
	rows := map[string]map[string]any{}
	for _, row := range list {
		rows[listscope.IssueKey(fmt.Sprint(row["project"]), row["number"])] = row
	}
	if len(rows) != len(list) {
		t.Fatalf("%s: duplicate rows: %s", path, w.Body.String())
	}
	return w.Code, rows, ""
}

// GET /api/issues?scope=active returns the Issue rows of the screen's active
// tabs, unchanged from the full list, for every shared classification case.
// requestRows fills in a pending row for a consultation's Issue that is missing
// from the list, so the two ranges must not drift apart.
func TestIssuesActiveScopeMatchesActiveTabs(t *testing.T) {
	for _, c := range scopeCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			app, _, _, _, h := setupScopeCase(t, c)
			// The first read records the close baseline. Read the scoped list cold,
			// then compare two reads taken after it.
			if _, cold, refusal := issuesRequest(t, app.Mux, "/api/issues?scope=active"); cold == nil {
				t.Fatalf("cold active list refused: %s", refusal)
			}
			_, full, refusal := issuesRequest(t, app.Mux, "/api/issues")
			if full == nil {
				t.Fatalf("full list refused: %s", refusal)
			}
			_, active, refusal := issuesRequest(t, app.Mux, "/api/issues?scope=active")
			if active == nil {
				t.Fatalf("active list refused: %s", refusal)
			}
			want := []string{}
			for _, n := range c.ActiveIssues {
				want = append(want, fmt.Sprintf("owner/app#%d", n))
			}
			got := []string{}
			for key, row := range active {
				got = append(got, key)
				if !reflect.DeepEqual(row, full[key]) {
					t.Fatalf("retained row changed %s: active=%v full=%v", key, row, full[key])
				}
			}
			sort.Strings(got)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("active issues=%v want=%v", got, want)
			}
			screen := []string{}
			for key := range scopeRows(firstScopeBody(t, h, "/api/list?tabs=active")) {
				if issue, found := strings.CutPrefix(key, "issue:"); found {
					screen = append(screen, issue)
				}
			}
			sort.Strings(screen)
			if !reflect.DeepEqual(got, screen) {
				t.Fatalf("active issues=%v screen active tabs=%v", got, screen)
			}
		})
	}
}

func firstScopeBody(t *testing.T, h http.Handler, path string) map[string]any {
	t.Helper()
	body, _, _ := scopeRequest(t, h, path)
	return body
}

// A finished Issue nobody refers to leaves the active scope, while closed
// Issues that a consultation or an unfinished acceptance plan still needs stay.
func TestIssuesActiveScopeKeepsClosedRowsStillNeeded(t *testing.T) {
	const (
		investigating = "00000000-0000-4000-8000-000000000021"
		unsaved       = "00000000-0000-4000-8000-000000000004"
	)
	plan := &workplan.Plan{Version: 1, Requirements: []workplan.Requirement{{ID: "report", Criterion: "訂正", Owner: "reporter", Phase: "report", Method: "投稿", Expected: "訂正済み", EvidenceKind: "report_comment"}}}
	c := scopeCase{
		Name: "issues-active-scope",
		Issues: []map[string]any{
			{"number": float64(1), "state": "closed"},
			{"number": float64(2), "state": "closed"},
			{"number": float64(3), "state": "closed", "jobStatus": "held", "holdReason": "review_held", "jobView": map[string]any{"executionPlan": plan.Record(), "codeMerged": true, "executionWait": "reporter"}},
			// Posted by a consultation whose acknowledgement was never saved.
			{"number": float64(4), "state": "closed", "draft": float64(4)},
			{"number": float64(5), "state": "open"},
		},
		Drafts: []consultations.Record{
			{"id": investigating, "project": "owner/app", "status": "investigation_waiting", "investigations": []any{map[string]any{"project": "owner/app", "number": float64(2)}}},
			{"id": unsaved, "project": "owner/app", "status": "thinking"},
		},
		Other: []string{"owner/gone"},
	}
	app, api, _, _, _ := setupScopeCase(t, c)

	_, full, refusal := issuesRequest(t, app.Mux, "/api/issues")
	if len(full) != 5 {
		t.Fatalf("full list=%v %s", full, refusal)
	}
	_, active, refusal := issuesRequest(t, app.Mux, "/api/issues?scope=active")
	got := []string{}
	for key := range active {
		got = append(got, key)
	}
	sort.Strings(got)
	if want := []string{"owner/app#2", "owner/app#3", "owner/app#4", "owner/app#5"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active issues=%v want=%v %s", got, want, refusal)
	}

	for _, path := range []string{"/api/issues?scope=full", "/api/issues?scope=", "/api/issues?scope=active&scope=all"} {
		if code, _, message := issuesRequest(t, app.Mux, path); code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d %s", path, code, message)
		}
	}

	// A bare array cannot name an unread candidate, so the scoped list fails too.
	api.unread["owner/gone"] = true
	if code, _, message := issuesRequest(t, app.Mux, "/api/issues?scope=active"); code != http.StatusServiceUnavailable || !strings.Contains(message, "owner/gone") {
		t.Fatalf("unread candidate: status=%d %s", code, message)
	}
}
