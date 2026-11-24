package statistics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"reflect"
	"sort"
	"time"
)

func outcome(status string, instructions int) any {
	switch status {
	case "closed":
		return "closed"
	case "failed":
		return "failed"
	case "review_held":
		return "review_held"
	case "merged", "completed":
		if instructions > 0 {
			return "user_revised"
		}
		return "passed"
	case "":
		return nil
	default:
		return "in_progress"
	}
}
func (s *Service) instructions(ctx context.Context) (map[string]int, error) {
	actions, err := s.localRows(ctx, "actions")
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, a := range actions {
		p := fleet.Object(a["payload"])
		if a["comment"] == nil && a["postedComment"] == nil {
			continue
		}
		action := fleet.Text(p["action"])
		if action == "reply" || action == "retry" || (action == "resume" || action == "run") && fleet.Text(p["text"]) != "" {
			counts[fmt.Sprint(p["repo"], "#", p["number"])]++
		}
	}
	return counts, nil
}
func (s *Service) issueStates(ctx context.Context) (map[string]map[string]any, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT repo,issue,status,hold_reason,title,view FROM jobs")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]any{}
	for rows.Next() {
		var repo, status, title string
		var number int64
		var hold *string
		var raw []byte
		if err = rows.Scan(&repo, &number, &status, &hold, &title, &raw); err != nil {
			return nil, err
		}
		if status == "held" && hold != nil && *hold == "review_held" {
			status = "review_held"
		}
		var view map[string]any
		if err = json.Unmarshal(raw, &view); err != nil {
			return nil, err
		}
		out[fmt.Sprintf("%s#%d", repo, number)] = map[string]any{"status": status, "title": title, "view": view}
	}
	return out, rows.Err()
}
func (s *Service) Tiers(ctx context.Context, q Query) (any, error) {
	if s.Drafts == nil {
		return nil, errors.New("draft statistics backend missing")
	}
	drafts, err := s.Drafts(ctx)
	if err != nil {
		return nil, err
	}
	states, err := s.issueStates(ctx)
	if err != nil {
		return nil, err
	}
	instructions, err := s.instructions(ctx)
	if err != nil {
		return nil, err
	}
	all, _, err := (&usage.Store{DB: s.DB}).Read(ctx, "", time.Unix(0, 0), time.Now().Add(24*time.Hour))
	if err != nil {
		return nil, err
	}
	return s.calculateTiers(ctx, q, drafts, states, instructions, all)
}

func (s *Service) calculateTiers(ctx context.Context, q Query, drafts []map[string]any, states map[string]map[string]any, instructions map[string]int, all []map[string]any) (any, error) {
	all = correctedUsageCosts(all)
	intakeByDraft, workerByIssue := indexTierUsage(ctx, all)
	decisions := tierDecisions(drafts)
	sort.SliceStable(drafts, func(i, j int) bool { return fleet.Text(drafts[i]["createdAt"]) > fleet.Text(drafts[j]["createdAt"]) })
	rows := s.tierRows(ctx, q, drafts, states, instructions, intakeByDraft, workerByIssue, decisions)
	total := len(rows)
	shown := rows
	if len(shown) > 200 {
		shown = shown[:200]
	}
	byModel := tierLevelModels(rows)
	return map[string]any{"rows": shown, "total": total, "omitted": total - len(shown), "unavailable": []any{}, "byLevelModel": byModel, "rates": map[string]any{"before": tierRatePeriod(rows, q.Before), "after": tierRatePeriod(rows, q.After)}}, nil
}

type issueUsageKey struct {
	repo   string
	number float64
}

// indexTierUsage groups the intake rows by draft and the worker rows by issue.
func indexTierUsage(ctx context.Context, all []map[string]any) (map[any][]map[string]any, map[issueUsageKey][]map[string]any) {
	intakeByDraft := map[any][]map[string]any{}
	workerByIssue := map[issueUsageKey][]map[string]any{}
	for _, r := range all {
		countTierWork(ctx, 1)
		switch r["source"] {
		case "intake":
			if id := r["draft"]; id == nil || reflect.TypeOf(id).Comparable() {
				intakeByDraft[id] = append(intakeByDraft[id], r)
			}
		case "worker":
			if repo, ok := r["repo"].(string); ok {
				key := issueUsageKey{repo, fleet.Number(r["issue"])}
				workerByIssue[key] = append(workerByIssue[key], r)
			}
		}
	}
	return intakeByDraft, workerByIssue
}

// tierDecisions maps each filed issue to the implementation tier decided in its consultation.
func tierDecisions(drafts []map[string]any) map[string]any {
	decisions := map[string]any{}
	for _, d := range drafts {
		if decision := tierDecision(d["implementationTier"]); decision != nil {
			i := fleet.Object(d["issue"])
			repo := fleet.Text(i["project"])
			if repo == "" {
				repo = fleet.Text(d["project"])
			}
			if fleet.Number(i["number"]) > 0 {
				decisions[fmt.Sprintf("%s#%d", repo, int64(fleet.Number(i["number"])))] = decision
			}
		}
	}
	return decisions
}

// tierRows builds a row for each issue filed from a consultation with a tier.
func (s *Service) tierRows(ctx context.Context, q Query, drafts []map[string]any, states map[string]map[string]any, instructions map[string]int, intakeByDraft map[any][]map[string]any, workerByIssue map[issueUsageKey][]map[string]any, decisions map[string]any) []any {
	rows := []any{}
	for _, draft := range drafts {
		tier := fleet.Object(draft["tier"])
		if tier == nil || draft["parentSplit"] != nil || draft["proposal"] != nil {
			continue
		}
		issues := fleet.List(draft["issues"])
		if len(issues) == 0 {
			issues = []any{draft["issue"]}
		}
		issues = append(issues, fleet.List(draft["investigations"])...)
		seen := map[string]bool{}
		countTierWork(ctx, 1)
		var mine []map[string]any
		if id := draft["id"]; id == nil || reflect.TypeOf(id).Comparable() {
			mine = intakeByDraft[id]
		}
		countTierWork(ctx, len(mine))
		turns := 0
		for _, raw := range fleet.List(draft["messages"]) {
			if fleet.Object(raw)["role"] == "assistant" {
				turns++
			}
		}
		for _, raw := range issues {
			issue := fleet.Object(raw)
			number := int64(fleet.Number(issue["number"]))
			repo := fleet.Text(issue["project"])
			if repo == "" {
				repo = fleet.Text(draft["project"])
			}
			if number < 1 || q.Project != "" && repo != q.Project {
				continue
			}
			key := fmt.Sprintf("%s#%d", repo, number)
			if seen[key] {
				continue
			}
			seen[key] = true
			state := states[key]
			count := instructions[key]
			var desk any
			if s.Desk != nil {
				desk = s.Desk(repo, number)
			}
			history := fleet.List(tier["history"])
			start := map[string]any{"level": tier["level"], "source": tier["source"], "reason": tier["reason"], "decidedAt": tier["decidedAt"], "requestedTier": draft["requestedTier"]}
			if tier["pending"] == true {
				start["pending"] = true
			}
			if len(history) > 0 {
				start["level"] = fleet.Object(history[0])["from"]
			}
			row := map[string]any{"draft": draft["id"], "createdAt": draft["createdAt"], "project": draft["project"], "level": tier["level"], "model": tier["model"], "effort": tier["effort"], "source": tier["source"], "reason": usage.Redact(fleet.Text(tier["reason"])), "start": start, "history": history, "issue": map[string]any{"project": repo, "number": number, "desk": desk, "title": state["title"], "url": issue["url"], "plan": nil}, "status": state["status"], "instructions": count, "outcome": outcome(fleet.Text(state["status"]), count), "turns": turns, "usage": usage.Totals(mine, s.Weights)}

			var spent any
			if len(mine) > 0 {
				spent = outcomeTotals(mine)
			}
			row["usage"] = spent
			if _, exists := draft["messages"].([]any); !exists {
				row["turns"] = nil
			}
			countTierWork(ctx, 1)
			issueRows := workerByIssue[issueUsageKey{repo, float64(number)}]
			countTierWork(ctx, len(issueRows))
			uniqueIssues := map[string]bool{}
			for _, raw := range issues {
				i := fleet.Object(raw)
				r := fleet.Text(i["project"])
				if r == "" {
					r = fleet.Text(draft["project"])
				}
				if fleet.Number(i["number"]) > 0 {
					uniqueIssues[identity(r, i["number"])] = true
				}
			}
			row["issueUsage"], row["combined"] = outcomeIssue(issueRows), combinedOutcome(mine, issueRows, len(uniqueIssues))
			row["implementation"], row["implementationDecision"] = tierImplementation(fleet.Object(state["view"])["executionSettings"]), decisions[key]
			if state == nil {
				row["statusReason"] = "依頼一覧に無いIssue"
			}
			split := fleet.List(draft["issues"])
			for index, raw := range split {
				i := fleet.Object(raw)
				if fleet.Number(i["number"]) == float64(number) && (i["project"] == nil || i["project"] == repo) {
					fleet.Object(row["issue"])["plan"] = fmt.Sprintf("%d/%d", index+1, len(split))
				}
			}
			for _, raw := range fleet.List(draft["investigations"]) {
				i := fleet.Object(raw)
				if fleet.Number(i["number"]) == float64(number) && (i["project"] == nil || i["project"] == repo) {
					fleet.Object(row["issue"])["investigation"] = true
				}
			}
			if overrides := fleet.List(draft["modelOverrides"]); len(overrides) > 0 {
				row["overrides"] = overrides
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// tierLevelModels totals the rows by level, model and override, ordered by cost.
func tierLevelModels(rows []any) []any {
	groups := map[string]map[string]any{}
	seen := map[string]map[string]bool{}
	for _, raw := range rows {
		row := fleet.Object(raw)
		overridden := len(fleet.List(row["overrides"])) > 0
		key := identity(row["level"], row["model"], overridden)
		g := groups[key]
		if g == nil {
			outcomes := map[string]any{}
			for _, name := range []string{"passed", "review_held", "user_revised", "failed", "in_progress", "closed", "unknown"} {
				outcomes[name] = 0
			}
			g = map[string]any{"level": row["level"], "model": row["model"], "overridden": overridden, "consultations": 0, "issues": 0, "outcomes": outcomes, "raised": 0, "costUsd": nil, "costUnknown": 0, "costPerIssueUsd": nil, "knownIssues": 0}
			groups[key] = g
			seen[key] = map[string]bool{}
		}
		g["issues"] = g["issues"].(int) + 1
		o := fleet.Object(g["outcomes"])
		name := fleet.Text(row["outcome"])
		if name == "" {
			name = "unknown"
		}
		o[name] = o[name].(int) + 1
		if fleet.Object(row["usage"])["costUsd"] != nil {
			g["knownIssues"] = g["knownIssues"].(int) + 1
		}
		id := fleet.Text(row["draft"])
		if seen[key][id] {
			continue
		}
		seen[key][id] = true
		g["consultations"] = g["consultations"].(int) + 1
		if len(fleet.List(row["history"])) > 0 {
			g["raised"] = g["raised"].(int) + 1
		}
		cost := fleet.Object(row["usage"])["costUsd"]
		if cost == nil {
			g["costUnknown"] = g["costUnknown"].(int) + 1
		} else {
			g["costUsd"] = fleet.Number(g["costUsd"]) + fleet.Number(cost)
		}
	}
	byModel := []any{}
	for _, g := range groups {
		known := g["knownIssues"].(int)
		delete(g, "knownIssues")
		if g["costUsd"] != nil && known > 0 {
			g["costPerIssueUsd"] = fleet.Number(g["costUsd"]) / float64(known)
		}
		byModel = append(byModel, g)
	}
	sort.Slice(byModel, func(i, j int) bool {
		a, b := fleet.Object(byModel[i]), fleet.Object(byModel[j])
		ac, bc := float64(-1), float64(-1)
		if a["costUsd"] != nil {
			ac = fleet.Number(a["costUsd"])
		}
		if b["costUsd"] != nil {
			bc = fleet.Number(b["costUsd"])
		}
		if ac != bc {
			return ac > bc
		}
		if a["level"] != b["level"] {
			return fleet.Text(a["level"]) < fleet.Text(b["level"])
		}
		if a["model"] != b["model"] {
			return fleet.Text(a["model"]) < fleet.Text(b["model"])
		}
		return a["overridden"] == false && b["overridden"] == true
	})
	return byModel
}
func (s *Service) Followups(ctx context.Context, q Query) (any, error) {
	records, err := s.localRows(ctx, "proposals")
	if err != nil {
		return nil, err
	}
	states, err := s.issueStates(ctx)
	if err != nil {
		return nil, err
	}
	instructions, err := s.instructions(ctx)
	if err != nil {
		return nil, err
	}
	rows := s.followupRows(records, states, instructions, q)
	before, after := followupPeriod(rows, "before", q.Before), followupPeriod(rows, "after", q.After)
	total := len(rows)
	if len(rows) > 200 {
		rows = rows[:200]
	}
	return map[string]any{"project": q.Project, "before": before, "after": after, "rows": rows, "total": total, "omitted": total - len(rows), "unavailable": []any{}}, nil
}

// followupRows lists the accepted follow-up and improvement proposals of either period, newest first.
func (s *Service) followupRows(records []map[string]any, states map[string]map[string]any, instructions map[string]int, q Query) []any {
	rows := []any{}
	for _, r := range records {
		issue := fleet.Object(r["issue"])
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["acceptedAt"]))
		if r["type"] != "proposal" || (r["kind"] != "followup" && r["kind"] != "improvements") || e != nil || fleet.Number(issue["number"]) < 1 {
			continue
		}
		repo := fleet.Text(issue["project"])
		if repo == "" {
			repo = fleet.Text(r["repo"])
		}
		if q.Project != "" && repo != q.Project {
			continue
		}
		periods := []any{}
		if !at.Before(q.Before.From) && at.Before(q.Before.To) {
			periods = append(periods, "before")
		}
		if !at.Before(q.After.From) && at.Before(q.After.To) {
			periods = append(periods, "after")
		}
		if len(periods) == 0 {
			continue
		}
		key := fmt.Sprint(repo, "#", issue["number"])
		state := states[key]
		var desk any
		if s.Desk != nil {
			desk = s.Desk(repo, int64(fleet.Number(issue["number"])))
		}
		rows = append(rows, map[string]any{"acceptedAt": ISO(at), "project": repo, "number": issue["number"], "desk": desk, "title": state["title"], "url": issue["url"], "kind": r["kind"], "source": fmt.Sprint(r["repo"], "#", r["number"]), "status": state["status"], "instructions": instructions[key], "outcome": outcome(fleet.Text(state["status"]), instructions[key]), "periods": periods})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return fleet.Text(fleet.Object(rows[i])["acceptedAt"]) > fleet.Text(fleet.Object(rows[j])["acceptedAt"])
	})
	return rows
}

// followupPeriod counts the period's proposals by kind and outcome.
func followupPeriod(rows []any, name string, p Period) any {
	counts := map[string]any{"followup": 0, "improvements": 0, "total": 0}
	outcomes := map[string]any{}
	for _, key := range []string{"passed", "user_revised", "review_held", "failed", "closed", "in_progress", "unknown"} {
		outcomes[key] = 0
	}
	for _, raw := range rows {
		row := fleet.Object(raw)
		included := false
		for _, value := range fleet.List(row["periods"]) {
			if value == name {
				included = true
			}
		}
		if !included {
			continue
		}
		kind := fleet.Text(row["kind"])
		counts[kind] = counts[kind].(int) + 1
		counts["total"] = counts["total"].(int) + 1
		o := fleet.Text(row["outcome"])
		if o == "" {
			o = "unknown"
		}
		outcomes[o] = outcomes[o].(int) + 1
	}
	out := p.Dates()
	out["counts"], out["outcomes"], out["pending"] = counts, outcomes, nil
	return out
}
