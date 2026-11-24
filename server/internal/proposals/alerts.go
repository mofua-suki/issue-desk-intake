package proposals

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"sort"
	"time"
)

func dismissed(records map[string]Object, r Object) bool {
	fingerprint := str(r["fingerprint"])
	proposalID := str(r["id"])
	if proposalID == "" {
		key := fmt.Sprintf("dismissal-comment:%s#%d:%d", str(r["repo"]), num(r["number"]), num(r["commentId"]))
		mark := records[key]
		return fingerprint != "" && mark["type"] == "dismissal" && mark["status"] == "dismissed" && mark["fingerprint"] == fingerprint
	}
	if fingerprint == "" {
		return false
	}
	key := fmt.Sprintf("dismissal:%s#%d:%s", str(r["repo"]), num(r["number"]), proposalID)
	mark := records[key]
	return mark["type"] == "dismissal" && mark["status"] == "dismissed" && mark["fingerprint"] == fingerprint
}
func checked(r Object) time.Time {
	value := str(r["checkedAt"])
	if value == "" {
		value = str(r["at"])
	}
	at, _ := time.Parse(time.RFC3339Nano, value)
	return at
}

type alertFamily struct {
	repo, job, generation, batch string
	number                       int64
}

func proposalAlertFamily(r Object) (alertFamily, bool) {
	m := workerID.FindStringSubmatch(str(r["id"]))
	if m == nil || !validNumber(r["number"]) || !validNumber(r["commentId"]) || str(r["repo"]) == "" {
		return alertFamily{}, false
	}
	return alertFamily{str(r["repo"]), m[1], m[2], m[4], num(r["number"])}, true
}

func (s *Service) UnfiledProposalAlerts(ctx context.Context) ([]any, error) {
	saved, e := s.records.List(ctx, "proposals")
	if e != nil {
		return nil, e
	}
	marks, completed, familyCompleted := proposalAlertMarks(saved)
	order, groups := s.openProposalGroups(saved, completed)
	out := []any{}
	now := s.now()
	for _, k := range order {
		records := groups[k]
		latest, shown := latestProposalRecord(records, familyCompleted)
		if !shown {
			continue
		}
		if latest["type"] == "rejection" && dismissed(marks, latest) {
			continue
		}
		oldest := oldestRecentPost(records, now)
		if oldest.IsZero() || now.Sub(oldest) <= time.Hour {
			continue
		}
		out = append(out, s.unfiledProposalAlert(k, latest, oldest))
	}
	return out, nil
}

// proposalAlertKey identifies the proposal of a record; a rejection without a proposal ID
// is identified by its comment.
func proposalAlertKey(r Object) string {
	if str(r["id"]) == "" && r["type"] == "rejection" {
		return fmt.Sprintf("%s#%d:comment:%d", str(r["repo"]), num(r["number"]), num(r["commentId"]))
	}
	return fmt.Sprintf("%s#%d:%s", str(r["repo"]), num(r["number"]), str(r["id"]))
}

// proposalAlertMarks indexes the saved marks and dismissals, the completed proposals, and the
// latest completed comment of each proposal family.
func proposalAlertMarks(saved []consultations.Record) (map[string]Object, map[string]bool, map[alertFamily]map[string]int64) {
	marks := map[string]Object{}
	completed := map[string]bool{}
	familyCompleted := map[alertFamily]map[string]int64{}
	for _, r := range saved {
		marks[str(r["key"])] = Object(r)
		if r["type"] == "dismissal" {
			marks["dismissal:"+proposalAlertKey(Object(r))] = Object(r)
		}
		if r["type"] == "proposal" && (r["status"] == "notified" || r["status"] == "discarded") {
			completed[proposalAlertKey(Object(r))] = true
			if family, ok := proposalAlertFamily(Object(r)); ok {
				if familyCompleted[family] == nil {
					familyCompleted[family] = map[string]int64{}
				}
				id := str(r["id"])
				if n := num(r["commentId"]); n > familyCompleted[family][id] {
					familyCompleted[family][id] = n
				}
			}
		}
	}
	return marks, completed, familyCompleted
}

// openProposalGroups groups the unfinished proposals and rejections of managed projects by
// proposal, in first-seen order.
func (s *Service) openProposalGroups(saved []consultations.Record, completed map[string]bool) ([]string, map[string][]Object) {
	groups := map[string][]Object{}
	order := []string{}
	for _, raw := range saved {
		r := Object(raw)
		proposal := r["type"] == "proposal" && r["status"] != "notified" && r["status"] != "discarded"
		rejection := r["type"] == "rejection" && (r["status"] == "scan_excluded" || r["status"] == "rejected")
		k := proposalAlertKey(r)
		if !proposal && !rejection || s.o.Project(str(r["repo"])) != nil || completed[k] {
			continue
		}
		if _, has := groups[k]; !has {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	return order, groups
}

// latestProposalRecord picks the record that represents the group. shown is false for a
// rejection-only group whose every comment has a later completed sibling.
func latestProposalRecord(records []Object, familyCompleted map[alertFamily]map[string]int64) (Object, bool) {
	for _, r := range records {
		if r["type"] == "proposal" {
			return r, true
		}
	}
	// Hide a rejection-only group only when every recorded comment has a later completed sibling.
	superseded := true
	for _, r := range records {
		family, ok := proposalAlertFamily(r)
		later := false
		if ok {
			for id, comment := range familyCompleted[family] {
				if id != str(r["id"]) && comment > num(r["commentId"]) {
					later = true
					break
				}
			}
		}
		if !later {
			superseded = false
			break
		}
	}
	if superseded {
		return nil, false
	}
	sort.SliceStable(records, func(i, j int) bool { return checked(records[i]).After(checked(records[j])) })
	return records[0], true
}

// oldestRecentPost returns the oldest posting time within the last 7 days, or zero when none.
func oldestRecentPost(records []Object, now time.Time) time.Time {
	oldest := time.Time{}
	for _, r := range records {
		at, e := time.Parse(time.RFC3339Nano, str(r["postedAt"]))
		if e != nil || at.Before(now.Add(-7*24*time.Hour)) || at.After(now) {
			continue
		}
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
	}
	return oldest
}

// unfiledProposalAlert formats the attention row of an unfiled proposal.
func (s *Service) unfiledProposalAlert(k string, latest Object, oldest time.Time) Object {
	state := "1時間超・採用拒否"
	if latest["type"] == "proposal" {
		state = "1時間超・採用後 " + str(latest["status"])
	} else if latest["status"] == "scan_excluded" {
		state = "1時間超・走査除外"
	}
	var desk any
	var link any
	if s.o.Desk != nil {
		desk = s.o.Desk(str(latest["repo"]), num(latest["number"]))
	}
	if s.o.Link != nil {
		kind := "issues"
		if latest["sourceKind"] == "pulls" {
			kind = "pulls"
		}
		link = s.o.Link(str(latest["repo"]), kind, num(latest["number"]))
	}
	reason := latest["reason"]
	if reason == nil || reason == "" {
		reason = latest["error"]
	}
	if latest["type"] == "rejection" && str(latest["id"]) == "" && (reason == nil || reason == "" || reason == "案の形式が不正です。") {
		reason = "案の形式が不正です（当時の詳細未保存）。"
	}
	at := latest["checkedAt"]
	if at == nil {
		at = latest["at"]
	}
	updated := at
	if updated == nil {
		updated = contract.ISO(oldest)
	}
	return Object{"key": "unfiled-proposal:" + k, "kind": "unfiled_proposal", "kindName": "未起票の後続案", "state": "waiting", "stateLabel": state, "project": latest["repo"], "number": latest["number"], "sourceKind": latest["sourceKind"], "desk": desk, "url": link, "title": "後続案が1時間を超えて未起票", "proposalId": latest["id"], "proposalStatus": latest["status"], "commentId": latest["commentId"], "postedAt": contract.ISO(oldest), "checkedAt": at, "updatedAt": updated, "reason": reason}
}
