package issueapi

import (
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"testing"
)

func TestSearchUpdatedRange(t *testing.T) {
	row := func(updated string) Object {
		return Object{"project": "a/b", "state": "open", "status": "queued", "createdAt": "2026-11-01T00:00:00Z", "closedAt": "", "updatedAt": updated}
	}
	match := func(r Object, query Object) bool {
		t.Helper()
		periods, err := searchPeriods(query)
		if err != nil {
			t.Fatal(err)
		}
		return searchRowMatches(r, query, fleet.Text(query["state"]), map[string]bool{}, periods)
	}
	inside := row("2026-11-02T01:00:00Z")
	span := Object{"updatedFrom": "2026-11-02T10:00", "updatedTo": "2026-11-02T11:00"}
	if !match(row("2026-11-02T01:00:00Z"), span) {
		t.Fatal("from is inclusive and a value without an offset is Japan time")
	}
	if match(row("2026-11-02T02:00:00Z"), span) {
		t.Fatal("to is exclusive")
	}
	for _, value := range []string{"", "not-a-time"} {
		if match(row(value), span) {
			t.Fatalf("range kept a row with updatedAt %q", value)
		}
		if !match(row(value), Object{}) {
			t.Fatalf("no range dropped a row with updatedAt %q", value)
		}
	}
	for _, other := range []Object{
		{"project": "x/y"}, {"state": "closed"}, {"host": "nas"}, {"q": "nothing"},
		{"createdFrom": "2026-11-03T00:00"}, {"closedFrom": "2026-11-03T00:00"},
	} {
		query := Object{"updatedFrom": span["updatedFrom"], "updatedTo": span["updatedTo"]}
		for k, v := range other {
			query[k] = v
		}
		if match(inside, query) {
			t.Fatalf("not combined with AND: %#v", other)
		}
	}
	if !match(inside, Object{"updatedFrom": span["updatedFrom"], "createdFrom": "2026-10-01T00:00", "project": "a/b", "state": "open"}) {
		t.Fatal("matching conditions should keep the row")
	}
	for _, query := range []Object{
		{"updatedFrom": "2026-02-30T00:00"}, {"updatedTo": "yesterday"},
		{"updatedFrom": "2026-11-02T11:00", "updatedTo": "2026-11-02T11:00"},
		{"updatedFrom": "2026-11-02T12:00", "updatedTo": "2026-11-02T11:00"},
	} {
		if _, err := searchPeriods(query); err == nil {
			t.Fatalf("accepted %#v", query)
		}
	}
}

func TestSearchPageMatchesListAndCountPostgres(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pg.Exec(`INSERT INTO job_events(job_id,kind,host,at,data) VALUES ($1,'dispatch_skip','nas','2026-11-01T00:00:00Z','{"reason":"scope_reserved"}')`, f.job); err != nil {
		t.Fatal(err)
	}
	f.api.issues = []Object{clone(f.api.issue)}
	for i := 2; i <= 4; i++ {
		issue := clone(f.api.issue)
		issue["number"] = i
		issue["title"] = fmt.Sprintf("issue %d", i)
		issue["updated_at"] = fmt.Sprintf("2026-11-%02dT00:00:00Z", i)
		f.api.issues = append(f.api.issues, issue)
	}
	queries := []Object{
		{}, {"q": "issue"}, {"project": "owner/repo", "state": "open", "status": "waiting"},
		{"host": "nonexistent"}, {"priority": "high"}, {"prioritySource": "ai"},
		{"createdFrom": "2020-01-01T00:00Z", "updatedTo": "2026-11-04T00:00Z"},
	}
	for _, query := range queries {
		rows, _, err := f.s.listFiltered(f.ctx, fleet.Text(query["state"]), fleet.Text(query["project"]))
		if err != nil {
			t.Fatal(err)
		}
		statuses := map[string]bool{}
		if query["status"] == "waiting" {
			for _, v := range []string{"pending", "queued", "untracked"} {
				statuses[v] = true
			}
		}
		periods, err := searchPeriods(query)
		if err != nil {
			t.Fatal(err)
		}
		var matched []Object
		for _, row := range rows {
			if searchRowMatches(row, query, fleet.Text(query["state"]), statuses, periods) {
				matched = append(matched, row)
			}
		}
		for _, page := range []int64{1, 2, 4, 10} {
			expected := searchPage(matched, 1, page)
			q := clone(query)
			q["limit"] = "1"
			q["page"] = fmt.Sprint(page)
			got, err := f.s.search(f.ctx, q, auth.Principal{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("query=%v page=%d got=%v want=%v", query, page, got, expected)
			}
			q["countOnly"] = "1"
			count, err := f.s.search(f.ctx, q, auth.Principal{})
			if err != nil || count["total"] != expected["total"] {
				t.Fatalf("query=%v count=%v err=%v", query, count, err)
			}
		}
	}
}

func TestSearchKeepsUnselectedProjectionFailurePostgres(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pg.Exec(`UPDATE jobs SET status='held',hold_reason='scope_wait' WHERE id=$1`, f.job); err != nil {
		t.Fatal(err)
	}
	// The jobs store remains available, while the scope-wait read fails.
	f.s.o.Postgres = nil
	for _, query := range []Object{{"countOnly": "1"}, {"q": "does-not-match"}, {"page": "2", "limit": "1"}} {
		if _, err := f.s.search(f.ctx, query, auth.Principal{}); err == nil {
			t.Fatalf("query %v hid scope-wait failure", query)
		}
	}
}

func TestSearchCombinedFiltersAndCountPostgres(t *testing.T) {
	f := newFixture(t)
	f.api.issues = []Object{clone(f.api.issue)}
	second := clone(f.api.issue)
	second["number"] = 2
	second["title"] = "別の依頼"
	second["updated_at"] = "2026-11-02T00:00:00Z"
	f.api.issues = append(f.api.issues, second)
	rows, err := f.s.list(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	filters := []struct{ key, value string }{
		{"project", "owner/repo"}, {"state", "open"}, {"status", "waiting"}, {"host", "nas"}, {"q", "実装"},
		{"priority", "normal"}, {"prioritySource", "ai"}, {"createdFrom", "2026-10-01T00:00Z"},
		{"createdTo", "2026-12-01T00:00Z"}, {"updatedFrom", "2026-10-01T00:00Z"},
		{"updatedTo", "2026-11-03T00:00Z"}, {"closedFrom", "2026-10-01T00:00Z"}, {"closedTo", "2026-11-03T00:00Z"},
	}
	// Exercise every filter alone and every pair, plus representative wider
	// combinations, without issuing a search for every subset.
	masks := []int{0, (1 << len(filters)) - 1, (1 << 11) - 1}
	for i := range filters {
		masks = append(masks, 1<<i)
		for j := i + 1; j < len(filters); j++ {
			masks = append(masks, 1<<i|1<<j)
		}
	}
	for _, mask := range masks {
		query := Object{}
		for i, filter := range filters {
			if mask&(1<<i) != 0 {
				query[filter.key] = filter.value
			}
		}
		periods, err := searchPeriods(query)
		if err != nil {
			t.Fatal(err)
		}
		statuses := map[string]bool{}
		if query["status"] == "waiting" {
			for _, status := range []string{"pending", "queued", "untracked"} {
				statuses[status] = true
			}
		}
		matched := []Object{}
		for _, row := range rows {
			if searchRowMatches(row, query, fleet.Text(query["state"]), statuses, periods) {
				matched = append(matched, row)
			}
		}
		expected := searchPage(matched, 100, 1)
		normal, err := f.s.search(f.ctx, clone(query), auth.Principal{})
		if err != nil {
			t.Fatal(err)
		}
		countQuery := clone(query)
		countQuery["countOnly"] = "1"
		count, err := f.s.search(f.ctx, countQuery, auth.Principal{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(normal, expected) || count["total"] != expected["total"] {
			t.Fatalf("query=%v normal=%v count=%v expected=%v", query, normal, count, expected)
		}
	}
}

func TestSearchCombinedTextAndUpdatedBoundaryPostgres(t *testing.T) {
	f := newFixture(t)
	second := clone(f.api.issue)
	second["number"] = 2
	second["title"] = "別の依頼"
	second["updated_at"] = "2026-11-02T00:00:00Z"
	f.api.issues = []Object{clone(f.api.issue), second}
	for _, tc := range []struct {
		to    string
		total int
	}{
		{"2026-11-03T00:00Z", 1},
		{"2026-11-02T00:00Z", 0},
	} {
		query := Object{"project": "owner/repo", "state": "open", "q": "別の依頼", "updatedFrom": "2026-11-01T00:00Z", "updatedTo": tc.to}
		result, err := f.s.search(f.ctx, clone(query), auth.Principal{})
		if err != nil {
			t.Fatal(err)
		}
		issues := result["issues"].([]Object)
		if result["total"] != tc.total || len(issues) != tc.total {
			t.Fatalf("query=%v result=%v", query, result)
		}
		if tc.total == 1 {
			if n, ok := number(issues[0]["number"]); !ok || n != 2 {
				t.Fatalf("query=%v result=%v", query, result)
			}
		}
		query["countOnly"] = "1"
		count, err := f.s.search(f.ctx, query, auth.Principal{})
		if err != nil || count["total"] != tc.total {
			t.Fatalf("query=%v count=%v err=%v", query, count, err)
		}
	}
}

func TestSearchMineUsesAuthenticatedHostPostgres(t *testing.T) {
	f := newFixture(t)
	p := auth.Principal{Kind: auth.PC, Name: "nas"}
	for _, query := range []Object{{}, {"state": "open", "status": "waiting"}, {"countOnly": "1"}} {
		mine := clone(query)
		mine["mine"] = "1"
		host := clone(query)
		host["host"] = "nas"
		got, err := f.s.search(f.ctx, mine, p)
		if err != nil {
			t.Fatal(err)
		}
		want, err := f.s.search(f.ctx, host, p)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("mine=%v host=%v err=%v", got, want, err)
		}
	}
	if _, err := f.s.search(f.ctx, Object{"mine": "1", "host": "nas"}, p); err == nil {
		t.Fatal("accepted mine with host")
	}
	if _, err := f.s.search(f.ctx, Object{"mine": "1"}, auth.Principal{Kind: auth.Screen}); err == nil {
		t.Fatal("accepted mine for screen")
	}
}
