package statistics

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"sort"
	"strings"
	"time"
)

func fitReference(lines []string, limit int) string {
	out := []string{}
	used := 0
	for i, line := range lines {
		note := fmt.Sprintf("（上限のため以下 %d行を省略）", len(lines)-i)
		if used+len(line)+1+len(note)+1 > limit {
			if used+len(note)+1 <= limit {
				out = append(out, note)
			}
			break
		}
		out = append(out, line)
		used += len(line) + 1
	}
	return strings.Join(out, "\n")
}
func stepReferenceSection(steps []*loggedStep, p Period, project, title string) []string {
	out := []string{"## " + title}
	groups := map[string][]*loggedStep{}
	keys := []string{}
	for _, s := range steps {
		at := stepPlaced(s)
		if at.Before(p.From) || !at.Before(p.To) || project != "" && s.Repo != project {
			continue
		}
		if _, ok := groups[s.Label]; !ok {
			keys = append(keys, s.Label)
		}
		groups[s.Label] = append(groups[s.Label], s)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return append(out, "この期間に工程の記録がありません。")
	}
	for _, key := range keys {
		values := []any{}
		interrupted, missing, running := 0, 0, 0
		wait := []any{}
		waitMissing := 0
		for _, s := range groups[key] {
			if s.Outcome == "running" {
				running++
				continue
			}
			if !stepMeasured(s) {
				missing++
				continue
			}
			values = append(values, float64(s.End.Sub(s.Start))/float64(time.Minute))
			if s.Outcome == "中断" {
				interrupted++
			}
			if minutes, hasChildren, measured := stepWait(steps, s); hasChildren && !measured {
				waitMissing++
			} else if hasChildren {
				wait = append(wait, minutes)
			}
		}
		d, w := AccountDistribution(values), AccountDistribution(wait)
		maxValue := any(nil)
		if len(values) > 0 {
			maxValue = values[0]
			for _, v := range values {
				if fleet.Number(v) > fleet.Number(maxValue) {
					maxValue = v
				}
			}
		}
		out = append(out, fmt.Sprintf("- %s: %v件（中断%d件）、平均%s分・中央値%s分・最大%s分、待ち平均%s分（%v件、欠測%d件）、工程欠測%d件・実行中%d件", key, d["known"], interrupted, referenceRatio(d["mean"]), referenceRatio(d["median"]), referenceRatio(maxValue), referenceRatio(w["mean"]), w["known"], waitMissing, missing, running))
	}
	return out
}

// stepWait is the time in minutes of the step not covered by its inner steps.
// hasChildren is false without inner steps; measured is false when one is unmeasured.
func stepWait(steps []*loggedStep, s *loggedStep) (minutes float64, hasChildren, measured bool) {
	children := []*loggedStep{}
	bad := false
	for _, child := range steps {
		if child.Parent == s {
			children = append(children, child)
			if !stepMeasured(child) {
				bad = true
			}
		}
	}
	if len(children) == 0 {
		return 0, false, false
	}
	if bad {
		return 0, true, false
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Start.Before(children[j].Start) })
	covered := time.Duration(0)
	end := s.Start
	for _, child := range children {
		start, finish := child.Start, child.End
		if start.Before(s.Start) {
			start = s.Start
		}
		if finish.After(s.End) {
			finish = s.End
		}
		if start.Before(end) {
			start = end
		}
		if finish.After(start) {
			covered += finish.Sub(start)
		}
		if finish.After(end) {
			end = finish
		}
	}
	return float64(s.End.Sub(s.Start)-covered) / float64(time.Minute), true, true
}
func measurementTotals(total map[string]any) string {
	parts := []string{fmt.Sprintf("%v件", total["count"])}
	for _, key := range []string{"inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens", "costUsd", "weighted"} {
		v := "不明"
		if total[key] != nil {
			v = fmt.Sprint(total[key])
		}
		parts = append(parts, fmt.Sprintf("%s=%s（不明%v件）", key, v, fleet.Object(total["unknown"])[key]))
	}
	return strings.Join(parts, " ")
}
func (s *Service) MeasurementReference(ctx context.Context, project string) (string, error) {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	p := ReferencePeriod(now, 30)
	stepsText := s.stepsReference(ctx, now, p, project)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	scope := "使用量は受付DBに集まった全PC分（未送信分は含まない）。工程別計測はNASの記録の範囲で、使用量とは観測範囲が異なる。"
	missing := func() (string, error) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return stepsText + "\n\n" + scope + "\nDB使用量: 欠測（DB要約の取得処理に失敗）。ローカル使用量へは代替しません。", nil
	}
	if s.DB == nil {
		return missing()
	}
	store := usage.Store{DB: s.DB}
	rows, skipped, e := store.Read(ctx, project, p.From, p.To)
	if e != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return missing()
	}
	lookback, _, e := store.Read(ctx, project, p.From.Add(-30*24*time.Hour), p.From)
	if e != nil {
		return missing()
	}
	var statuses []string
	rows, statuses = usage.CorrectCosts(rows, lookback)
	summary, detail := s.usageReferenceLines(rows, statuses, p)
	hosts := []string{}
	if s.Assignments != nil {
		for host := range s.Assignments.Hosts {
			hosts = append(hosts, host)
		}
	}
	sort.Strings(hosts)
	senders := []string{}
	if s.TokenNames != nil {
		var err error
		senders, err = s.TokenNames(ctx)
		if err != nil {
			return missing()
		}
	}
	coverage, e := store.Coverage(ctx, p.From, p.To, now, hosts, senders)
	if e != nil {
		return missing()
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return stepsText + "\n\n" + scope + fmt.Sprintf("\nDB使用量: 取得成功（正常0件も含む）。読めなかった行: %d。\n\n[要約種別: usage]\n", skipped) + usage.Redact(fitReference(summary, 3000)) + "\n\n[要約種別: detail]\n" + usage.Redact(fitReference(detail, 2000)) + "\n\n[要約種別: coverage]\n" + usage.CoverageText(coverage), nil
}

// stepsReference summarizes the logged steps of the project and of all repositories.
func (s *Service) stepsReference(ctx context.Context, now time.Time, p Period, project string) string {
	stepsText := "計測資料なし（理由: 工程別の共有DBに接続していません）"
	if s.DB != nil {
		lines, alive, _, e := s.logs(ctx)
		if e != nil {
			stepsText = "計測資料なし（理由: 工程別の要約を作れませんでした）"
		} else if len(lines) == 0 {
			stepsText = "計測資料なし（理由: NASに工程の記録がありません）"
		} else {
			steps := logSteps(lines, now, alive)
			head := []string{fmt.Sprintf("工程別の計測（NASのrunner.log記録。%s〜%s、日本時間）", p.From.In(JST).Format("2006-01-02"), p.To.Add(-time.Millisecond).In(JST).Format("2006-01-02")), "待ち時間は外側の工程のうち内側の工程が覆わない時間。欠測はゼロで埋めず件数で示す。時間は分。", "起票→着手は共有DBの起票日時を参照し、確定できない値は欠測に数える。工程の記録と共有DBの使用量は観測範囲が異なる。"}
			if project != "" {
				head = append(head, stepReferenceSection(steps, p, project, "選択済みプロジェクト "+project)...)
				if lead, e := s.measurementLead(ctx, steps, p, project); e == nil {
					head = append(head, lead...)
				} else {
					head = append(head, "起票→着手: 欠測（起票日時を読み取れませんでした）")
				}
			}
			head = append(head, stepReferenceSection(steps, p, "", "全体（NASのすべてのリポジトリ）")...)
			if lead, e := s.measurementLead(ctx, steps, p, ""); e == nil {
				head = append(head, lead...)
			} else {
				head = append(head, "起票→着手: 欠測（起票日時を読み取れませんでした）")
			}
			stepsText = fitReference(head, 8000)
		}
	}
	return stepsText
}

// usageReferenceLines summarizes the usage rows with their cost corrections, and by step, provider, model and repository.
func (s *Service) usageReferenceLines(rows []map[string]any, statuses []string, p Period) ([]string, []string) {
	correction := map[string]int{"corrected": 0, "filled": 0, "uncorrectable": 0}
	for _, status := range statuses {
		if status != "" {
			correction[status]++
		}
	}
	summary := []string{fmt.Sprintf("使用量 %s〜%s（受付DB）", p.From.In(JST).Format("2006-01-02"), p.To.Add(-time.Millisecond).In(JST).Format("2006-01-02")), "入力・出力・キャッシュは記録された値の合計。費用は同一セッションの累積値を補正。不明は0へ補完しません。", measurementTotals(usage.Totals(rows, s.Weights))}
	summary = append(summary, fmt.Sprintf("費用補正%d件・補完%d件・補正不能%d件", correction["corrected"], correction["filled"], correction["uncorrectable"]))
	detail := []string{}
	for _, axis := range []string{"step", "provider", "model", "repo"} {
		for _, g := range usage.Groups(rows, axis, s.Weights) {
			detail = append(detail, fmt.Sprintf("%s=%v %s", axis, g[axis], measurementTotals(g)))
		}
	}
	return summary, detail
}
