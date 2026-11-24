package issueapi

import (
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"time"
)

type searchPeriod struct{ from, to *time.Time }

// searchPeriods parses the optional created, closed and updated ranges before any row is read.
func searchPeriods(query Object) (map[string]searchPeriod, error) {
	periods := map[string]searchPeriod{}
	for _, key := range []string{"created", "closed", "updated"} {
		var span searchPeriod
		for _, side := range []string{"from", "to"} {
			v := fleet.Text(query[key+strings.ToUpper(side[:1])+side[1:]])
			if v == "" {
				continue
			}
			at, e := issueTime(v)
			if e != nil {
				return nil, fail(400, "日付の範囲が不正です。")
			}
			if side == "from" {
				span.from = &at
			} else {
				span.to = &at
			}
		}
		if span.from != nil && span.to != nil && !span.from.Before(*span.to) {
			return nil, fail(400, "日付の範囲が逆転しています。")
		}
		periods[key] = span
	}
	return periods, nil
}

// searchRowMatches applies every search condition with AND. A range excludes rows whose time is
// empty or unparsable.
func searchRowMatches(row Object, query Object, state string, statuses map[string]bool, periods map[string]searchPeriod) bool {
	if project := fleet.Text(query["project"]); project != "" && row["project"] != project {
		return false
	}
	if host := fleet.Text(query["host"]); host != "" && row["host"] != host {
		return false
	}
	if state != "" && row["state"] != state {
		return false
	}
	if len(statuses) > 0 && !statuses[fleet.Text(row["status"])] {
		return false
	}
	if q := strings.ToLower(fleet.Text(query["q"])); q != "" && !strings.Contains(strings.ToLower(fleet.Text(row["title"])), q) && !strings.Contains(strings.ToLower(fleet.Text(row["shownTitle"])), q) {
		return false
	}
	priority := fleet.ObjectOrEmpty(row["priority"])
	if value := fleet.Text(query["priority"]); value != "" {
		if len(priority) == 0 || value == "auto" && priority["source"] != "ai" || value != "auto" && priority["level"] != value {
			return false
		}
	}
	if value := fleet.Text(query["prioritySource"]); value != "" && priority["source"] != value {
		return false
	}
	for key, span := range periods {
		if span.from == nil && span.to == nil {
			continue
		}
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(row[key+"At"]))
		if e != nil || span.from != nil && at.Before(*span.from) || span.to != nil && !at.Before(*span.to) {
			return false
		}
	}
	return true
}
