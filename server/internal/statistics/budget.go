package statistics

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"math"
	"regexp"
	"time"
)

var promptParts = usage.PromptPartNames()

func numeric(v any) any {
	n, ok := v.(float64)
	if !ok || n < 0 || n != math.Trunc(n) || n > 9007199254740991 {
		return nil
	}
	return n
}
func budgetSize(raw any) map[string]any {
	v := fleet.Object(raw)
	return map[string]any{"chars": numeric(v["chars"]), "bytes": numeric(v["bytes"])}
}
func choice(v any, allowed ...string) string {
	for _, name := range allowed {
		if v == name {
			return name
		}
	}
	return "unknown"
}
func flag(v any) any {
	if b, ok := v.(bool); ok {
		return b
	}
	return nil
}
func measurement(raw any) map[string]any {
	v := fleet.Object(raw)
	parts := map[string]any{}
	for _, part := range promptParts {
		parts[part] = budgetSize(fleet.Object(v["parts"])[part])
	}
	reason := "unmeasured"
	if v != nil {
		reason = "measured"
		if value, exists := v["reason"]; exists {
			reason = choice(value, "measured", "invalid_input", "unrecognized_layout")
		}
	}
	return map[string]any{"total": budgetSize(v["total"]), "parts": parts, "exceeded": map[string]any{"chars": flag(fleet.Object(v["exceeded"])["chars"]), "bytes": flag(fleet.Object(v["exceeded"])["bytes"])}, "reason": reason}
}
func excess(v map[string]any) any {
	e := fleet.Object(v["exceeded"])
	if e["chars"] == true || e["bytes"] == true {
		return true
	}
	if e["chars"] == false && e["bytes"] == false {
		return false
	}
	return nil
}
func identifier(v any) any {
	if s, ok := v.(string); ok && regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString(s) {
		return s
	}
	return nil
}
func stamp(v any) any {
	at, e := time.Parse(time.RFC3339Nano, fleet.Text(v))
	if e != nil {
		return nil
	}
	return ISO(at)
}
func metric() map[string]any {
	return map[string]any{"known": 0, "missing": 0, "knownTotal": 0.0, "mean": nil, "max": nil}
}
func addMetric(m map[string]any, v any) {
	if v == nil {
		m["missing"] = m["missing"].(int) + 1
		return
	}
	n := fleet.Number(v)
	m["known"] = m["known"].(int) + 1
	m["knownTotal"] = fleet.Number(m["knownTotal"]) + n
	m["mean"] = fleet.Number(m["knownTotal"]) / float64(m["known"].(int))
	if m["max"] == nil || n > fleet.Number(m["max"]) {
		m["max"] = n
	}
}
func (s *Service) Budget(ctx context.Context, name string, q Query) (any, error) {
	if s.Drafts == nil {
		return nil, errors.New("draft statistics backend missing")
	}
	drafts, err := s.Drafts(ctx)
	if err != nil {
		return nil, err
	}
	if name == "consultationBudget" {
		return map[string]any{"reason": nil, "before": consultationBudgetPeriod(drafts, q.Project, q.Before), "after": consultationBudgetPeriod(drafts, q.Project, q.After)}, nil
	}
	records, coverage := inputBudgetRecords(drafts, q.Project)
	return map[string]any{"project": q.Project, "unavailable": []any{}, "coverage": coverage, "before": inputBudgetPeriod(records, q.Before), "after": inputBudgetPeriod(records, q.After)}, nil
}

// consultationBudgetPeriod counts the consultation budget stops and continuations in the period.
func consultationBudgetPeriod(drafts []map[string]any, project string, p Period) any {
	stopped, stops, continuations := 0, 0, 0
	for _, d := range drafts {
		if project != "" && d["project"] != project {
			continue
		}
		b := fleet.Object(d["consultationBudget"])
		count := 0
		for _, raw := range fleet.List(b["stops"]) {
			at, e := time.Parse(time.RFC3339Nano, fleet.Text(fleet.Object(raw)["at"]))
			if e == nil && !at.Before(p.From) && at.Before(p.To) {
				count++
			}
		}
		if count > 0 {
			stopped++
		}
		stops += count
		for _, raw := range fleet.List(b["continuations"]) {
			at, e := time.Parse(time.RFC3339Nano, fleet.Text(fleet.Object(raw)["at"]))
			if e == nil && !at.Before(p.From) && at.Before(p.To) {
				continuations++
			}
		}
	}
	return map[string]any{"stoppedDrafts": stopped, "stops": stops, "continuations": continuations}
}

// inputBudgetRecords normalizes the input budget records kept in the drafts
// and reports how complete the retained records are.
func inputBudgetRecords(drafts []map[string]any, project string) ([]map[string]any, map[string]any) {
	coverage := map[string]any{"complete": false, "reason": "retained_observations_only", "recordingStart": nil, "unrecordedDrafts": 0, "invalidRecords": 0, "duplicateRecords": 0, "dropped": 0.0, "droppedKnownDrafts": 0, "droppedMissingDrafts": 0, "retention": []any{}}
	records := []map[string]any{}
	for _, draft := range drafts {
		if project != "" && draft["project"] != project {
			continue
		}
		b := fleet.Object(draft["inputBudget"])
		if b["v"] != 1.0 || fleet.List(b["records"]) == nil {
			coverage["unrecordedDrafts"] = coverage["unrecordedDrafts"].(int) + 1
			continue
		}
		dropped := numeric(b["dropped"])
		reason := "retention_unknown"
		if dropped == nil {
			coverage["droppedMissingDrafts"] = coverage["droppedMissingDrafts"].(int) + 1
		} else {
			coverage["droppedKnownDrafts"] = coverage["droppedKnownDrafts"].(int) + 1
			coverage["dropped"] = fleet.Number(coverage["dropped"]) + fleet.Number(dropped)
			reason = "retained_since_first_record"
			if fleet.Number(dropped) > 0 {
				reason = "outside_retention"
			}
		}
		coverage["retention"] = append(fleet.List(coverage["retention"]), map[string]any{"draft": identifier(draft["id"]), "total": numeric(b["total"]), "dropped": dropped, "firstSequence": numeric(b["firstSequence"]), "lastSequence": numeric(b["lastSequence"]), "firstAt": stamp(b["firstAt"]), "lastAt": stamp(b["lastAt"]), "reason": reason})
		seen := map[string]bool{}
		for _, raw := range fleet.List(b["records"]) {
			r := fleet.Object(raw)
			if r["v"] != 1.0 || stamp(r["at"]) == nil {
				coverage["invalidRecords"] = coverage["invalidRecords"].(int) + 1
				continue
			}
			full := measurement(r["full"])
			var delta any
			if r["delta"] != nil {
				delta = measurement(r["delta"])
			}
			var rejected any
			if excess(full) == true || delta != nil && excess(fleet.Object(delta)) == true {
				rejected = true
			} else if excess(full) == false && (delta == nil || excess(fleet.Object(delta)) == false) {
				rejected = false
			}
			row := map[string]any{"draft": identifier(draft["id"]), "checkId": identifier(r["checkId"]), "revision": numeric(r["revision"]), "turn": identifier(r["turn"]), "sequence": numeric(r["sequence"]), "at": stamp(r["at"]), "stage": choice(r["stage"], "consultation", "filing"), "callTrigger": choice(r["callTrigger"], "initial", "user_reply", "context_read", "automatic_continuation", "failure_retry", "recovery_continuation", "ai_message_continuation", "format_repair", "rate_limit_retry", "filing_recompare"), "repairKind": choice(r["repairKind"], usage.RecordedRepairKinds()...), "phase": choice(r["phase"], "before_reduction", "after_reduction", "pre_send"), "transport": choice(r["transport"], "guard", "codex", "claude"), "mode": choice(r["mode"], "full", "resumed", "fallback_full"), "limits": budgetSize(r["limits"]), "full": full, "delta": delta, "sent": nil, "finalStop": nil}
			if r["phase"] == "pre_send" {
				row["sent"] = budgetSize(r["sent"])
			}
			if r["phase"] == "after_reduction" && r["transport"] == "guard" {
				row["finalStop"] = rejected
			}
			key := identity(row)
			if row["sequence"] != nil {
				key = fmt.Sprint(row["sequence"])
			}
			if seen[key] {
				coverage["duplicateRecords"] = coverage["duplicateRecords"].(int) + 1
				continue
			}
			seen[key] = true
			records = append(records, row)
		}
	}
	return records, coverage
}

// inputBudgetPeriod groups the period's input budget records by stage, trigger, phase, input and part.
func inputBudgetPeriod(records []map[string]any, p Period) any {
	rows := []any{}
	checks, stops := map[string]bool{}, map[string]bool{}
	missing, transmissions, missingStop := 0, 0, 0
	groups := map[string]map[string]any{}
	ordered := []string{}
	for _, r := range records {
		at, _ := time.Parse(time.RFC3339Nano, fleet.Text(r["at"]))
		if at.Before(p.From) || !at.Before(p.To) {
			continue
		}
		rows = append(rows, r)
		valid := r["checkId"] != nil && r["draft"] != nil
		if valid {
			checks[identity(r["draft"], r["checkId"])] = true
		} else {
			missing++
		}
		if r["finalStop"] == true && valid {
			stops[identity(r["draft"], r["checkId"])] = true
		}
		if r["phase"] == "pre_send" && (r["transport"] == "codex" || r["transport"] == "claude") {
			transmissions++
		}
		if r["phase"] == "after_reduction" && r["transport"] == "guard" && (r["finalStop"] == nil || !valid) {
			missingStop++
		}
		for _, mode := range []string{"full", "delta", "sent"} {
			value := fleet.Object(r[mode])
			if value == nil {
				continue
			}
			parts := append([]string{"total"}, promptParts...)
			if mode == "sent" {
				parts = []string{"total"}
			}
			for _, part := range parts {
				dimensions := map[string]any{"stage": r["stage"], "callTrigger": r["callTrigger"], "phase": r["phase"], "transport": r["transport"], "mode": r["mode"], "input": mode, "part": part}
				key := identity(dimensions)
				g := groups[key]
				if g == nil {
					g = dimensions
					g["count"], g["chars"], g["bytes"], g["exceeded"], g["excessMissing"] = 0, metric(), metric(), 0, 0
					groups[key] = g
					ordered = append(ordered, key)
				}
				g["count"] = g["count"].(int) + 1
				measured := value
				if mode != "sent" {
					if part == "total" {
						measured = fleet.Object(value["total"])
					} else {
						measured = fleet.Object(fleet.Object(value["parts"])[part])
					}
				}
				addMetric(fleet.Object(g["chars"]), measured["chars"])
				addMetric(fleet.Object(g["bytes"]), measured["bytes"])
				if mode != "sent" && part == "total" {
					switch excess(value) {
					case true:
						g["exceeded"] = g["exceeded"].(int) + 1
					case nil:
						g["excessMissing"] = g["excessMissing"].(int) + 1
					}
				}
			}
		}
	}
	out := p.Dates()
	out["complete"], out["reason"], out["count"], out["knownChecks"], out["missingCheckIds"] = false, "retained_observations_only", len(rows), len(checks), missing
	out["transmissions"], out["observedFinalStops"], out["missingFinalStop"], out["historicalStopTotal"], out["reductionEffect"] = transmissions, len(stops), missingStop, nil, nil
	list := []any{}
	for _, key := range ordered {
		list = append(list, groups[key])
	}
	out["groups"], out["records"] = list, rows
	return out
}
