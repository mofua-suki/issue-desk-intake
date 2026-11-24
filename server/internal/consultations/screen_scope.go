package consultations

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/listscope"
)

func scopeReferences(d Record) []Record {
	refs := []Record{}
	for _, v := range fleet.Items(d["issues"]) {
		if r := record(v); len(r) > 0 {
			refs = append(refs, r)
		}
	}
	if len(refs) == 0 && len(record(d["issue"])) > 0 {
		refs = append(refs, record(d["issue"]))
	}
	return refs
}
func scopeProject(d, issue Record) string {
	if p := text(issue["project"]); p != "" {
		return p
	}
	return text(d["project"])
}
func scopeImages(d Record) bool {
	for _, v := range fleet.Items(d["messages"]) {
		if len(fleet.Items(record(v)["images"])) > 0 {
			return true
		}
	}
	return false
}
func scopeHandoff(d Record) bool { return d["status"] == "project_handoff" || d["handoffRow"] == true }

// ScopeDraftEntries omits only definite inactive rows. Family links in either
// direction form connected components, including nested and incomplete splits.
// Before Issue reads finish, unresolved posted references are kept conservatively.
// unread names candidate projects whose Issues could not be read; their
// references are unknown rather than missing, so they never keep a terminal
// consultation active (the client would show them as pending Issue rows).
func ScopeDraftEntries(ctx context.Context, entries []DraftFields, issues []map[string]any, omitted map[string]bool, unread map[string]bool, resolved bool) []DraftFields {
	if !listscope.Active(ctx) {
		return entries
	}
	byID := map[string]int{}
	for i, e := range entries {
		id := text(e.Record["id"])
		// Preserve malformed snapshots for DraftScreenViews' family projection fallback.
		if id == "" || id != e.Key {
			return entries
		}
		if _, exists := byID[id]; exists {
			return entries
		}
		byID[id] = i
	}
	links := make([][]int, len(entries))
	connect := func(i int, id string) {
		if j, ok := byID[id]; ok {
			links[i] = append(links[i], j)
			links[j] = append(links[j], i)
		}
	}
	for i, e := range entries {
		connect(i, text(e.Record["parentSplit"]))
		for _, id := range fleet.Items(e.Record["splitChildren"]) {
			connect(i, text(id))
		}
	}
	present := map[string]bool{}
	for _, i := range issues {
		present[listscope.IssueKey(text(i["project"]), i["number"])] = true
	}
	keep := make([]bool, len(entries))
	queue := []int{}
	for i, e := range entries {
		if scopeEntryActive(e.Record, present, omitted, unread, resolved) {
			keep[i] = true
			queue = append(queue, i)
		}
	}
	for n := 0; n < len(queue); n++ {
		for _, j := range links[queue[n]] {
			if !keep[j] {
				keep[j] = true
				queue = append(queue, j)
			}
		}
	}
	out := make([]DraftFields, 0, len(queue))
	for i, e := range entries {
		if keep[i] {
			out = append(out, e)
		}
	}
	return out
}

// scopeEntryActive reports whether the consultation itself must stay in the scoped list.
func scopeEntryActive(d Record, present, omitted, unread map[string]bool, resolved bool) bool {
	active := !scopeHandoff(d)
	refs := scopeReferences(d)
	switch {
	case !active:
	case d["status"] == "closed" && d["cleanup"] != nil:
		active = false
	case d["status"] == "submitted" || d["status"] == "closed":
		active = d["status"] == "submitted" && (numberPositive(record(d["familyProgress"])["remaining"]) || numberPositive(record(d["familyProgress"])["unknown"]))
		// Non-submitted image/split publication can hide an Issue in requestRows.
		active = active || d["status"] == "closed" && (scopeImages(d) || len(fleet.Items(d["splitIssues"])) > 0)
		for _, ref := range refs {
			project := scopeProject(d, ref)
			key := listscope.IssueKey(project, ref["number"])
			if !resolved || !present[key] && !omitted[key] && !unread[project] {
				active = true
			}
		}
		// Keep split families until FamilyProgress has been attached.
		if !resolved && d["splitChildren"] != nil && d["familyProgress"] == nil {
			active = true
		}
	}
	// A completed consultation can still decorate investigation Issue rows.
	if !scopeHandoff(d) && text(d["parentSplit"]) == "" && len(fleet.Items(d["investigations"])) > 0 {
		if _, valid := integer(d["priorityReviewRound"]); valid {
			active = true
		}
	}
	return active
}
func numberPositive(v any) bool { n, ok := integer(v); return ok && n > 0 }

// DraftScopeProtection keeps Issue rows that a continuing consultation could
// hide, decorate or replace. Ordinary posted references need no row assembly.
func DraftScopeProtection(entries []DraftFields) listscope.Protection {
	p := listscope.Protection{Drafts: map[string]bool{}, Issues: map[string]bool{}}
	for _, e := range entries {
		d := e.Record
		if scopeHandoff(d) || text(d["parentSplit"]) != "" {
			continue
		}
		if id := text(d["id"]); id == "" || id != e.Key || p.Drafts[id] {
			p.All = true
		}
		p.Drafts[text(d["id"])] = true
		for _, key := range []string{"issue", "issues", "investigations"} {
			values := fleet.Items(d[key])
			if key == "issue" {
				values = []any{d[key]}
			}
			for _, v := range values {
				r := record(v)
				if r["number"] != nil {
					p.Issues[listscope.IssueKey(scopeProject(d, r), r["number"])] = true
				}
			}
		}
	}
	return p
}
