package statistics

import (
	"bufio"
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var filingByteNames = []string{"requestsBody", "requestsOther", "jobs", "consultationsBody", "consultationsMessages", "consultationsOther", "target", "filePresence", "evidence", "instructions", "promptSchema", "residual", "promptTotal", "separateSchema"}
var filingID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var filingPrefix = regexp.MustCompile(`^(?:\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z )?filing-check-observation (.*)$`)
var filingKinds = strings.Fields("attempt attempt_finished compare_requested compare_input compare_completed hold_suppressed hold_released comparison_fetch_failed no_comparison new_issue reconciled_issue submit_unknown")

func oneOf(v any, list []string) bool {
	for _, x := range list {
		if v == x {
			return true
		}
	}
	return false
}
func filingTime(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	t, e := time.Parse(time.RFC3339Nano, s)
	if e != nil {
		return t, false
	}
	return t, s == contract.ISO(t) || s == t.UTC().Format("2006-01-02T15:04:05Z")
}
func filingNumber(v any) bool {
	x, ok := v.(float64)
	return ok && x >= 0 && x <= 9007199254740991 && x == float64(int64(x))
}
func parseFiling(lines []string) ([]map[string]any, map[string]int) {
	rows := []map[string]any{}
	seen := map[string]bool{}
	diagnostics := map[string]int{"invalidLines": 0, "unknownVersions": 0, "missingIds": 0, "duplicates": 0}
	for _, line := range lines {
		m := filingPrefix.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(m[1]), &event) != nil || event == nil {
			diagnostics["invalidLines"]++
			continue
		}
		if event["v"] != float64(1) {
			diagnostics["unknownVersions"]++
			continue
		}
		if !filingIDValid(event, "eventId") || !filingIDValid(event, "draft") || !filingIDValid(event, "attemptId") || !fleet.ValidRepo(fleet.Text(event["repo"])) || (oneOf(event["kind"], []string{"compare_requested", "compare_input", "compare_completed"}) && !filingIDValid(event, "comparisonId")) {
			diagnostics["missingIds"]++
			continue
		}
		if !validFilingFields(event) {
			diagnostics["invalidLines"]++
			continue
		}
		id := fleet.Text(event["eventId"])
		if seen[id] {
			diagnostics["duplicates"]++
			continue
		}
		seen[id] = true
		rows = append(rows, event)
	}
	return rows, diagnostics
}

// validFilingFields checks the time, kind, trigger and the optional fields of an observation.
func validFilingFields(event map[string]any) bool {
	_, valid := filingTime(event["at"])
	if !oneOf(event["kind"], filingKinds) || !oneOf(event["trigger"], []string{"automatic_recovery", "explicit_resend", "unknown"}) {
		valid = false
	}
	for _, key := range []string{"parent", "child"} {
		value, exists := event[key]
		if !exists || value != nil && !filingIDValid(event, key) {
			valid = false
		}
	}
	outcome, exists := event["outcome"]
	if !exists || outcome != nil && !oneOf(outcome, strings.Fields("allowed evidence_requested refused invalid_answer ai_failed fetch_failed unchanged legacy_or_changed revision_changed comparison_changed no_hold explicit_retry direct_recheck")) {
		valid = false
	}
	if value, exists := event["issue"]; !exists || value != nil && (!filingNumber(value) || fleet.Number(value) < 1) {
		valid = false
	}
	if value, exists := event["issueAt"]; !exists {
		valid = false
	} else if value != nil {
		if _, ok := filingTime(value); !ok {
			valid = false
		}
	}
	if value, exists := event["bytes"]; !exists {
		valid = false
	} else if value != nil {
		b := fleet.Object(value)
		if b == nil {
			valid = false
		} else {
			for _, name := range filingByteNames {
				v, exists := b[name]
				if !exists && name == "evidence" {
					continue
				}
				if !exists || v != nil && !filingNumber(v) {
					valid = false
				}
			}
		}
	}
	return valid
}
func filingIDValid(event map[string]any, key string) bool {
	return filingID.MatchString(fleet.Text(event[key]))
}
func filingKey(e map[string]any) string { return identity(e["repo"], e["draft"]) }
func filingComparison(e map[string]any) string {
	return identity(e["repo"], e["draft"], e["comparisonId"])
}
func filingDetails(events []map[string]any, p Period, parsed map[string]int) map[string]any {
	selected, requests, index := selectFilingEvents(events, p)
	counts, triggers, outcomes := map[string]int{}, map[string]int{}, map[string]int{}
	diagnostics := map[string]int{}
	for k, v := range parsed {
		diagnostics[k] = v
	}
	for _, key := range []string{"startsOnly", "endsOnly", "missingInputs", "inputsOnly"} {
		diagnostics[key] = 0
	}
	byDraft := map[string]int{}
	issues := map[string]bool{}
	requestsFiled := 0
	latest := map[string]time.Time{}
	for _, e := range selected {
		kind := fleet.Text(e["kind"])
		counts[kind]++
		triggers[fleet.Text(e["trigger"])]++
		if e["outcome"] != nil {
			outcomes[fleet.Text(e["outcome"])]++
		}
		key := filingComparison(e)
		switch kind {
		case "compare_requested":
			if !index.ends[key] {
				diagnostics["startsOnly"]++
			}
			if !index.inputKeys[key] {
				diagnostics["missingInputs"]++
			}
			byDraft[filingKey(e)]++
			if index.filed[filingKey(e)] {
				requestsFiled++
			}
			at, _ := filingTime(e["at"])
			if at.After(latest[filingKey(e)]) {
				latest[filingKey(e)] = at
			}
		case "compare_completed":
			if !index.starts[key] {
				diagnostics["endsOnly"]++
			}
		case "compare_input":
			if !index.starts[key] {
				diagnostics["inputsOnly"]++
			}
		case "new_issue":
			if at, ok := filingTime(e["issueAt"]); ok && e["issue"] != nil && !at.Before(p.From) && at.Before(p.To) {
				issues[identity(e["repo"], e["issue"])] = true
			}
		}
	}
	var perIssue any
	if len(issues) > 0 {
		perIssue = float64(len(requests)) / float64(len(issues))
	}
	return map[string]any{"counts": counts, "triggers": triggers, "outcomes": outcomes, "diagnostics": diagnostics, "repetition": filingRepetition(byDraft), "bytes": filingBytes(selected, requests, index.measuredKeys), "filing": map[string]any{"newIssuesInPeriod": len(issues), "comparisonRequests": len(requests), "requestsPerNewIssue": perIssue, "requestsForFiledDrafts": requestsFiled}, "recurrence": filingRecurrence(selected, latest)}
}

// filingIndex records which comparisons have each kind of observation, over all
// observations including those outside the period.
type filingIndex struct {
	starts, ends, inputKeys, filed map[string]bool
	measuredKeys                   map[string]map[string]bool
}

// selectFilingEvents selects the observations within the period and indexes all of them.
func selectFilingEvents(events []map[string]any, p Period) ([]map[string]any, []map[string]any, filingIndex) {
	selected, requests := []map[string]any{}, []map[string]any{}
	starts, ends, inputKeys, filed := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	measuredKeys := map[string]map[string]bool{}
	for _, name := range filingByteNames {
		measuredKeys[name] = map[string]bool{}
	}
	for _, e := range events {
		at, _ := filingTime(e["at"])
		if !at.Before(p.From) && at.Before(p.To) {
			selected = append(selected, e)
			if e["kind"] == "compare_requested" {
				requests = append(requests, e)
			}
		}
		switch e["kind"] {
		case "compare_requested":
			starts[filingComparison(e)] = true
		case "compare_completed":
			ends[filingComparison(e)] = true
		case "compare_input":
			inputKeys[filingComparison(e)] = true
			for _, name := range filingByteNames {
				if filingNumber(fleet.Object(e["bytes"])[name]) {
					measuredKeys[name][filingComparison(e)] = true
				}
			}
		case "new_issue", "reconciled_issue":
			if e["issue"] != nil {
				filed[filingKey(e)] = true
			}
		}
	}
	return selected, requests, filingIndex{starts: starts, ends: ends, inputKeys: inputKeys, filed: filed, measuredKeys: measuredKeys}
}

// filingRepetition summarizes how many comparisons each draft requested.
func filingRepetition(byDraft map[string]int) map[string]any {
	repeat := []int{}
	once, multiple := 0, 0
	for _, count := range byDraft {
		repeat = append(repeat, count)
		if count == 1 {
			once++
		} else {
			multiple++
		}
	}
	sort.Ints(repeat)
	var maximum, median any
	if len(repeat) > 0 {
		maximum = repeat[len(repeat)-1]
		median = float64(repeat[(len(repeat)-1)/2]+repeat[len(repeat)/2]) / 2
	}
	return map[string]any{"once": once, "multiple": multiple, "maximum": maximum, "median": median}
}

// filingBytes totals the measured input sizes and counts the requests without a measurement.
func filingBytes(selected, requests []map[string]any, measuredKeys map[string]map[string]bool) map[string]any {
	bytes := map[string]any{}
	for _, name := range filingByteNames {
		total, measured, known := float64(0), 0, 0
		for _, e := range selected {
			v := fleet.Object(e["bytes"])[name]
			if e["kind"] == "compare_input" && filingNumber(v) {
				total += fleet.Number(v)
				measured++
			}
		}
		for _, e := range requests {
			if measuredKeys[name][filingComparison(e)] {
				known++
			}
		}
		var sum any
		if measured > 0 {
			sum = total
		}
		bytes[name] = map[string]any{"total": sum, "measured": measured, "unknown": len(requests) - known}
	}
	return bytes
}

// filingRecurrence measures how often a draft requested again after each kind of failure.
func filingRecurrence(selected []map[string]any, latest map[string]time.Time) map[string]any {
	recurrence := map[string]any{}
	for _, category := range strings.Fields("submit_unknown comparison_fetch_failed refused invalid_answer ai_failed hold_suppressed") {
		earliest := map[string]time.Time{}
		for _, e := range selected {
			if e["kind"] != category && e["outcome"] != category {
				continue
			}
			key := filingKey(e)
			at, _ := filingTime(e["at"])
			if earliest[key].IsZero() || at.Before(earliest[key]) {
				earliest[key] = at
			}
		}
		count := 0
		for key, at := range earliest {
			if latest[key].After(at) {
				count++
			}
		}
		var rate any
		if len(earliest) > 0 {
			rate = float64(count) / float64(len(earliest))
		}
		recurrence[category] = map[string]any{"denominatorDrafts": len(earliest), "repeatedDrafts": count, "rate": rate}
	}
	return recurrence
}
func (s *Service) FilingCheck(_ context.Context, q Query) (any, error) {
	reason := "保持記録がありません"
	out := map[string]any{"project": q.Project, "unavailable": []any{}, "before": nil, "after": nil}
	file, e := os.Open(filepath.Join(s.RuntimeDir, "filing-check-events.jsonl"))
	if e != nil {
		if !os.IsNotExist(e) {
			reason = "保持記録を読めませんでした"
		}
		out["unavailable"] = []any{reason}
		out["reasons"] = map[string]any{"before": reason, "after": reason}
		return out, nil
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	lines := []string{}
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	var meta map[string]any
	if scanner.Err() != nil || len(lines) == 0 || json.Unmarshal([]byte(lines[0]), &meta) != nil {
		out["unavailable"] = []any{"保持記録を読めませんでした"}
		out["reasons"] = map[string]any{"before": "保持記録を読めませんでした", "after": "保持記録を読めませんでした"}
		return out, nil
	}
	started, valid := filingTime(meta["startedAt"])
	complete, validComplete := filingTime(meta["completeSince"])
	if meta["v"] != float64(1) || !valid || !validComplete {
		out["unavailable"] = []any{"保持記録を読めませんでした"}
		out["reasons"] = map[string]any{"before": "保持記録を読めませんでした", "after": "保持記録を読めませんでした"}
		return out, nil
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	events, diagnostics := parseFiling(lines[1:])
	kept := []map[string]any{}
	for _, e := range events {
		at, _ := filingTime(e["at"])
		if !at.Before(now.Add(-30*24*time.Hour)) && (q.Project == "" || strings.EqualFold(q.Project, fleet.Text(e["repo"]))) {
			kept = append(kept, e)
		}
	}
	reasons := map[string]any{"before": nil, "after": nil}
	for name, p := range map[string]Period{"before": q.Before, "after": q.After} {
		reason = ""
		if p.From.Before(started) {
			reason = "収集開始前の期間を含みます"
		} else if p.From.Before(complete) {
			reason = "保存失敗を含む期間です"
		} else if p.From.Before(now.Add(-30 * 24 * time.Hour)) {
			reason = "保持範囲外の期間を含みます"
		}
		if reason != "" {
			reasons[name] = reason
		} else {
			out[name] = filingDetails(kept, p, diagnostics)
		}
	}
	out["reasons"] = reasons
	return out, nil
}
