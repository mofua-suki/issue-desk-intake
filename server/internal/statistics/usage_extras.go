package statistics

import (
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

func wholeValue(v any) (float64, bool) {
	n, ok := v.(float64)
	return n, ok && n >= 0 && n == math.Trunc(n) && n <= 9007199254740991
}
func measuredSum() map[string]any { return map[string]any{"knownTotal": float64(0), "missing": 0} }
func measuredAdd(summary map[string]any, value any) {
	if n, ok := wholeValue(value); ok {
		summary["knownTotal"] = summary["knownTotal"].(float64) + n
	} else {
		summary["missing"] = summary["missing"].(int) + 1
	}
}
func weightedAdd(summary map[string]any, row, weights map[string]any) {
	if n, ok := usage.Weighted(row, weights)["weighted"].(float64); ok {
		summary["knownTotal"] = summary["knownTotal"].(float64) + n
	} else {
		summary["missing"] = summary["missing"].(int) + 1
	}
}
func draftUsageRow(row map[string]any) bool {
	return row["source"] == "intake" && fleet.Text(row["draft"]) != "" && usage.IsIntakeStep(fleet.Text(row["step"]))
}
func callTrigger(row map[string]any) any {
	if draftUsageRow(row) && oneOf(row["callTrigger"], strings.Fields("initial user_reply context_read automatic_continuation failure_retry recovery_continuation ai_message_continuation format_repair rate_limit_retry filing_recompare")) {
		return row["callTrigger"]
	}
	return nil
}

var repairIndex = regexp.MustCompile(`\[\d+\]`)
var repairField = regexp.MustCompile(`^[A-Za-z0-9]+(?:\.[A-Za-z0-9]+)*$`)

func (s *Service) usageExtras(rows []map[string]any) map[string]any {
	t := newUsageExtrasTally()
	ordered := append([]map[string]any{}, rows...)
	sort.SliceStable(ordered, func(i, j int) bool { return fleet.Text(ordered[i]["at"]) < fleet.Text(ordered[j]["at"]) })
	for _, r := range ordered {
		if !draftUsageRow(r) {
			continue
		}
		step := usage.CanonicalStep(fleet.Text(r["step"]))
		provider := r["provider"]
		if fleet.Text(provider) == "" {
			provider = nil
		}
		s.addDraftUsage(t, r, step, provider)
		t.addContextReads(r)
		if step == usage.ConsultationStep {
			s.addConsultationTurn(t, r, provider)
			if r["callTrigger"] == "format_repair" {
				s.addRepair(t, r, step)
			}
		}
	}
	return t.output()
}

// usageExtrasTally groups the consultation usage rows by draft, read path, session turn and repair.
type usageExtrasTally struct {
	drafts, sessions, repairs map[string]map[string]any
	positions                 map[string]int
	paths                     map[string]map[string]any
	pathDrafts                map[string]map[string]bool
}

func newUsageExtrasTally() *usageExtrasTally {
	return &usageExtrasTally{drafts: map[string]map[string]any{}, sessions: map[string]map[string]any{}, repairs: map[string]map[string]any{}, positions: map[string]int{}, paths: map[string]map[string]any{}, pathDrafts: map[string]map[string]bool{}}
}

// addDraftUsage adds the row to its draft, step, trigger and provider group.
func (s *Service) addDraftUsage(t *usageExtrasTally, r map[string]any, step string, provider any) {
	key := identity(r["repo"], r["draft"], step, callTrigger(r), provider)
	g := t.drafts[key]
	if g == nil {
		g = map[string]any{"repo": r["repo"], "draft": r["draft"], "step": step, "callTrigger": callTrigger(r), "provider": provider, "rows": 0, "weighted": measuredSum(), "firstAt": r["at"], "lastAt": r["at"], "outcome": "unknown", "desks": []any{}}
		for _, metric := range strings.Fields("calls inputTokens outputTokens cacheReadTokens cacheWriteTokens contextReadRequested contextReadRepeated") {
			g[metric] = measuredSum()
		}
		if outcome := s.draftOutcomes[fleet.Text(r["draft"])]; outcome != nil {
			g["outcome"], g["desks"] = outcome["outcome"], outcome["desks"]
		}
		t.drafts[key] = g
	}
	g["rows"] = g["rows"].(int) + 1
	weightedAdd(fleet.Object(g["weighted"]), r, s.Weights)
	if fleet.Text(r["at"]) < fleet.Text(g["firstAt"]) {
		g["firstAt"] = r["at"]
	}
	if fleet.Text(r["at"]) > fleet.Text(g["lastAt"]) {
		g["lastAt"] = r["at"]
	}
	for _, metric := range strings.Fields("calls inputTokens outputTokens cacheReadTokens cacheWriteTokens contextReadRequested contextReadRepeated") {
		v := r[metric]
		if metric == "contextReadRequested" {
			v = fleet.Object(r["contextReads"])["requested"]
		}
		if metric == "contextReadRepeated" {
			v = fleet.Object(r["contextReads"])["repeated"]
		}
		measuredAdd(fleet.Object(g[metric]), v)
	}
}

// addContextReads adds the row's context reads to each path.
func (t *usageExtrasTally) addContextReads(r map[string]any) {
	seen := map[string]bool{}
	for _, raw := range fleet.List(fleet.Object(r["contextReads"])["files"]) {
		f := fleet.Object(raw)
		key := identity(f["repo"], f["path"])
		p := t.paths[key]
		if p == nil {
			p = map[string]any{"repo": f["repo"], "path": f["path"], "requested": float64(0), "repeated": float64(0), "rows": 0}
			t.paths[key] = p
			t.pathDrafts[key] = map[string]bool{}
		}
		p["requested"] = fleet.Number(p["requested"]) + fleet.Number(f["requested"])
		p["repeated"] = fleet.Number(p["repeated"]) + fleet.Number(f["repeated"])
		if !seen[key] {
			p["rows"] = p["rows"].(int) + 1
		}
		seen[key] = true
		t.pathDrafts[key][identity(r["repo"], r["draft"])] = true
	}
}

// addConsultationTurn adds the consultation row to its provider, session mode and turn order.
func (s *Service) addConsultationTurn(t *usageExtrasTally, r map[string]any, provider any) {
	id := identity(r["repo"], r["draft"])
	t.positions[id]++
	order := t.positions[id]
	if order > 10 {
		order = 10
	}
	var mode any
	if oneOf(r["sessionMode"], []string{"new", "resumed", "once"}) {
		mode = r["sessionMode"]
	}
	key := identity(provider, mode, order)
	group := t.sessions[key]
	if group == nil {
		group = map[string]any{"provider": provider, "sessionMode": mode, "order": order, "rows": 0, "weighted": measuredSum(), "contextMaxTokens": nil}
		for _, metric := range strings.Fields("inputTokens cacheReadTokens outputTokens cacheWriteTokens modelCalls") {
			group[metric] = measuredSum()
		}
		t.sessions[key] = group
	}
	group["rows"] = group["rows"].(int) + 1
	for _, metric := range strings.Fields("inputTokens cacheReadTokens outputTokens cacheWriteTokens modelCalls") {
		measuredAdd(fleet.Object(group[metric]), r[metric])
	}
	weightedAdd(fleet.Object(group["weighted"]), r, s.Weights)
	if n, ok := wholeValue(r["contextMaxTokens"]); ok && (group["contextMaxTokens"] == nil || n > fleet.Number(group["contextMaxTokens"])) {
		group["contextMaxTokens"] = n
	}
}

// addRepair adds a format repair row to its kind and field.
func (s *Service) addRepair(t *usageExtrasTally, r map[string]any, step string) {
	var kind, field any
	if usage.RecordedRepairKind(r["repairKind"]) {
		kind = r["repairKind"]
	}
	f := repairIndex.ReplaceAllString(fleet.Text(r["repairField"]), "")
	if kind == "draft_format" && len(f) <= 80 && repairField.MatchString(f) {
		field = f
	}
	key := identity(step, kind, field)
	repair := t.repairs[key]
	if repair == nil {
		repair = map[string]any{"step": step, "repairKind": kind, "repairField": field, "rows": 0, "weighted": measuredSum()}
		t.repairs[key] = repair
	}
	repair["rows"] = repair["rows"].(int) + 1
	weightedAdd(fleet.Object(repair["weighted"]), r, s.Weights)
}

// output lists the groups in a stable order, the read paths by volume.
func (t *usageExtrasTally) output() map[string]any {
	drafts, sessions, repairs, paths, pathDrafts := t.drafts, t.sessions, t.repairs, t.paths, t.pathDrafts
	list := func(groups map[string]map[string]any) []any {
		keys := []string{}
		for key := range groups {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := []any{}
		for _, key := range keys {
			out = append(out, groups[key])
		}
		return out
	}
	pathRows := []any{}
	for key, p := range paths {
		p["drafts"] = len(pathDrafts[key])
		pathRows = append(pathRows, p)
	}
	sort.Slice(pathRows, func(i, j int) bool {
		a, b := fleet.Object(pathRows[i]), fleet.Object(pathRows[j])
		av, bv := fleet.Number(a["requested"])+fleet.Number(a["repeated"]), fleet.Number(b["requested"])+fleet.Number(b["repeated"])
		if av != bv {
			return av > bv
		}
		if a["repo"] != b["repo"] {
			return fleet.Text(a["repo"]) < fleet.Text(b["repo"])
		}
		return fleet.Text(a["path"]) < fleet.Text(b["path"])
	})
	omitted := 0
	if len(pathRows) > 200 {
		omitted = len(pathRows) - 200
		pathRows = pathRows[:200]
	}
	sessionRows := list(sessions)
	sort.Slice(sessionRows, func(i, j int) bool {
		a, b := fleet.Object(sessionRows[i]), fleet.Object(sessionRows[j])
		if a["provider"] != b["provider"] {
			return fleet.Text(a["provider"]) < fleet.Text(b["provider"])
		}
		modes := map[string]int{"new": 0, "resumed": 1, "once": 2, "": 3}
		if a["sessionMode"] != b["sessionMode"] {
			return modes[fleet.Text(a["sessionMode"])] < modes[fleet.Text(b["sessionMode"])]
		}
		return a["order"].(int) < b["order"].(int)
	})
	return map[string]any{"drafts": list(drafts), "sessions": sessionRows, "repairs": list(repairs), "contextReadPaths": pathRows, "contextReadPathsOmitted": omitted}
}

func detailTotals(rows []map[string]any) map[string]any {
	out := map[string]any{"calls": nil, "contextMaxTokens": nil, "contextTotalTokens": nil, "promptTotalBytes": nil, "promptAverageBytes": nil}
	unknown := map[string]any{"calls": 0, "contextMaxTokens": 0, "contextTotalTokens": 0, "promptTotalBytes": 0}
	prompts := 0
	for _, r := range rows {
		for _, k := range []string{"calls", "contextMaxTokens", "contextTotalTokens", "promptTotalBytes"} {
			n, ok := wholeValue(r[k])
			if !ok {
				unknown[k] = unknown[k].(int) + 1
				continue
			}
			if k == "contextMaxTokens" {
				if out[k] == nil || n > fleet.Number(out[k]) {
					out[k] = n
				}
			} else {
				out[k] = fleet.Number(out[k]) + n
			}
			if k == "promptTotalBytes" {
				prompts++
			}
		}
	}
	if prompts > 0 {
		out["promptAverageBytes"] = fleet.Number(out["promptTotalBytes"]) / float64(prompts)
	}
	out["detailUnknown"] = unknown
	return out
}
func attachDetail(target map[string]any, rows []map[string]any) {
	for k, v := range detailTotals(rows) {
		target[k] = v
	}
}
func periodRows(rows []map[string]any, p Period) []map[string]any {
	out := []map[string]any{}
	for _, r := range rows {
		t, e := time.Parse(time.RFC3339Nano, fleet.Text(r["at"]))
		if e == nil && !t.Before(p.From) && t.Before(p.To) {
			out = append(out, r)
		}
	}
	return out
}
