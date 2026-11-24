package statistics

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
)

func selectionReasonCounts(reasons []string) []any {
	counts := map[string]int{}
	for _, s := range reasons {
		counts[s]++
	}
	keys := []string{}
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	out := []any{}
	for _, key := range keys {
		out = append(out, map[string]any{"reason": key, "count": counts[key]})
	}
	return out
}
func average(values []float64) any {
	if len(values) == 0 {
		return nil
	}
	sum := float64(0)
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}
func describeMS(values []float64) map[string]any {
	return map[string]any{"count": len(values), "averageMs": average(values), "medianMs": percentile(values, .5)}
}
func (s *Service) TestSelection(ctx context.Context, q Query) (any, error) {
	lines, alive, end, e := s.logs(ctx)
	if e != nil {
		return nil, e
	}
	steps := logSteps(lines, end, alive)
	parsed := selectionParse(lines, steps)
	return map[string]any{"project": q.Project, "reason": nil, "before": s.testSelectionPeriod(lines, steps, parsed, q.Before, q.Project), "after": s.testSelectionPeriod(lines, steps, parsed, q.After, q.Project)}, nil
}

// selectionWindow is the period and project the sections of one period count.
type selectionWindow struct {
	p       Period
	project string
	// since is the first logged day when the log starts within the period.
	since any
}

func (w selectionWindow) inside(at time.Time, repo string) bool {
	return !at.Before(w.p.From) && at.Before(w.p.To) && (w.project == "" || w.project == repo)
}

func selectionNone(reason string) map[string]any { return map[string]any{"reason": reason} }

// testSelectionPeriod builds each section of the period: fixed tests, test
// selection, full runs and leaks, and reuse before merge.
func (s *Service) testSelectionPeriod(lines []logLine, steps []*loggedStep, parsed selectionInput, p Period, project string) any {
	out := p.Dates()
	if len(lines) == 0 || !lines[0].At.Before(p.To) {
		for _, name := range []string{"fixed", "selection", "full", "leaks", "followReuse"} {
			out[name] = selectionNone("この期間の監視の記録がありません")
		}
		return out
	}
	w := selectionWindow{p: p, project: project}
	if lines[0].At.After(p.From) {
		w.since = lines[0].At.In(JST).Format("2006-01-02")
	}
	out["fixed"] = fixedTestSection(steps, parsed, w)
	out["selection"] = selectionSection(parsed, w)
	out["full"], out["leaks"] = s.fullTestSections(parsed, w)
	out["followReuse"] = followReuseSection(parsed, w)
	return out
}

// fixedTestSection measures the fixed test steps by the kind of selection that chose them.
func fixedTestSection(steps []*loggedStep, parsed selectionInput, w selectionWindow) map[string]any {
	fixed := []*loggedStep{}
	measured, chosen, whole := []float64{}, []float64{}, []float64{}
	missing, running, unlabeled := 0, 0, 0
	for _, span := range steps {
		if span.Label != "固定テスト" || !w.inside(stepPlaced(span), span.Repo) {
			continue
		}
		fixed = append(fixed, span)
		if stepMeasured(span) {
			ms := float64(span.End.Sub(span.Start)) / float64(time.Millisecond)
			measured = append(measured, ms)
			switch parsed.Kinds[span] {
			case "chosen":
				chosen = append(chosen, ms)
			case "whole":
				whole = append(whole, ms)
			default:
				unlabeled++
			}
		} else if span.Outcome == "running" {
			running++
		} else {
			missing++
		}
	}
	if len(fixed) == 0 {
		return selectionNone("期間内に固定テストの工程の記録がありません")
	}
	row := describeMS(measured)
	row["reason"], row["since"], row["missing"], row["running"], row["chosen"], row["whole"], row["unlabeled"] = nil, w.since, missing, running, describeMS(chosen), describeMS(whole), unlabeled
	return row
}

// selectionSection counts the test choices and the share of files they selected.
func selectionSection(parsed selectionInput, w selectionWindow) map[string]any {
	choices, chosenCount, wholeCount, notRun := 0, 0, 0, 0
	reasons, shares, runShares := []string{}, []float64{}, []float64{}
	for _, c := range parsed.Choices {
		if !w.inside(c.At, c.Repo) {
			continue
		}
		choices++
		if c.NotRun {
			notRun++
			continue
		}
		if c.Kind == "chosen" {
			chosenCount++
			if c.Total > 0 {
				share := float64(c.Files) / float64(c.Total)
				shares = append(shares, share)
				runShares = append(runShares, share)
			}
		} else {
			wholeCount++
			reasons = append(reasons, reasonKind(c.Reason, false))
			runShares = append(runShares, 1)
		}
	}
	if choices == 0 {
		return selectionNone("期間内にテスト選択の行がありません")
	}
	return map[string]any{"reason": nil, "since": w.since, "chosen": chosenCount, "whole": wholeCount, "notRun": notRun, "reasons": selectionReasonCounts(reasons), "selectedShare": average(shares), "runShare": average(runShares)}
}

// fullTestSections counts the full test runs and lists the failures that a selection had omitted.
func (s *Service) fullTestSections(parsed selectionInput, w selectionWindow) (full, leaks map[string]any) {
	fulls := []*fullRun{}
	statuses := map[string]int{}
	failedFiles, unknownFiles, cutFiles, cutRuns, merged, unknownSelections, noPrevious := 0, 0, 0, 0, 0, 0, 0
	items := []any{}
	for _, r := range parsed.Fulls {
		if !w.inside(r.At, r.Repo) {
			continue
		}
		fulls = append(fulls, r)
		statuses[r.Status]++
		if r.Status != "failed" {
			continue
		}
		failedFiles += len(r.Failed)
		if r.UnknownFiles {
			unknownFiles++
		}
		cutFiles += r.Cut
		if r.Cut > 0 {
			cutRuns++
		}
		merged += r.Merged
		unknownSelections += r.UnknownSelections
		noPrevious += r.NoPrevious
		files := []string{}
		for file := range r.Leaks {
			files = append(files, file)
		}
		sort.Strings(files)
		for _, file := range files {
			issues := []any{}
			for _, key := range r.Leaks[file] {
				repo, number, _ := strings.Cut(key, "#")
				n, _ := strconv.ParseInt(number, 10, 64)
				var desk any
				if s.Desk != nil {
					desk = s.Desk(repo, n)
				}
				issues = append(issues, map[string]any{"project": repo, "number": n, "desk": desk})
			}
			items = append(items, map[string]any{"version": r.Version, "at": iso(r.At), "file": file, "issues": issues})
		}
	}
	if len(fulls) == 0 {
		return selectionNone("期間内に全部のテストの結果の行がありません"), selectionNone("期間内に全部のテストの結果の行がありません")
	}
	full = map[string]any{"reason": nil, "since": w.since, "runs": statuses["passed"] + statuses["failed"], "passed": statuses["passed"], "failed": statuses["failed"], "failedFiles": failedFiles, "unknownFiles": unknownFiles, "cutFiles": cutFiles, "cutRuns": cutRuns, "interrupted": statuses["interrupted"], "unrunnable": statuses["unrunnable"]}
	count := len(items)
	omitted := 0
	if count > 50 {
		omitted = count - 50
		items = items[:50]
	}
	leaks = map[string]any{"reason": nil, "since": w.since, "count": count, "failedRuns": statuses["failed"], "merged": merged, "unknownFiles": unknownFiles, "unknownSelections": unknownSelections, "cutRuns": cutRuns, "noPrevious": noPrevious, "items": items, "omitted": omitted}
	return full, leaks
}

// followReuseSection counts the passes reused, rechecked or refused before merge.
func followReuseSection(parsed selectionInput, w selectionWindow) map[string]any {
	follows := map[string]int{}
	saved, count := 0, 0
	refusals := []string{}
	for _, f := range parsed.Follows {
		if !w.inside(f.At, f.Repo) {
			continue
		}
		count++
		follows[f.Kind]++
		saved += f.SavedMS
		if f.Kind == "notReused" {
			refusals = append(refusals, reasonKind(f.Reason, true))
		}
	}
	if count == 0 {
		return selectionNone("期間内に取り込み前の合格の行がありません")
	}
	return map[string]any{"reason": nil, "since": w.since, "reused": follows["reused"], "rechecked": follows["rechecked"], "notReused": follows["notReused"], "reasons": selectionReasonCounts(refusals), "savedMs": saved}
}
