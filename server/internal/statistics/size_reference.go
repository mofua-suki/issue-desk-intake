package statistics

import (
	"context"
	"database/sql"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"math"
	"sort"
	"strings"
	"time"
)

// ReferencePeriod includes today's Japanese calendar date, independent of UTC boundaries.
func ReferencePeriod(now time.Time, days int) Period {
	d := now.In(JST)
	end := time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, JST)
	return Period{end.Add(-time.Duration(days) * 24 * time.Hour), end}
}
func referenceNumber(v any) string {
	if n, ok := accountKnown(v); ok {
		return fmt.Sprint(math.Round(n))
	}
	return "不明"
}
func referenceRatio(v any) string {
	if n, ok := accountKnown(v); ok {
		return fmt.Sprint(math.Round(n*100) / 100)
	}
	return "不明"
}
func referenceWeighted(i AccountRecord) any {
	total := fleet.Object(i["total"])
	if fleet.Number(fleet.Object(total["unknown"])["weighted"]) > 0 || fleet.Number(total["partialRows"]) > 0 {
		return nil
	}
	return total["weighted"]
}
func sizeReferenceIssues(data ReworkRows, weights AccountRecord) []AccountRecord {
	_, events := accountGroups(data.Events, func(r AccountRecord) string { return fmt.Sprint(r["job_id"]) })
	_, rows := accountGroups(data.Usage, accountKey)
	out := []AccountRecord{}
	for _, j := range data.Jobs {
		out = append(out, AccountIssue(j, events[fmt.Sprint(j["id"])], rows[accountKey(j)], weights))
	}
	return out
}
func referenceMean(issues []AccountRecord) string {
	values := []any{}
	for _, i := range issues {
		values = append(values, referenceWeighted(i))
	}
	d := AccountDistribution(values)
	if d["mean"] == nil {
		return "該当なし"
	}
	return fmt.Sprintf("%s（%v件）", referenceNumber(d["mean"]), d["known"])
}
func referenceBandLines(issues []AccountRecord) []string {
	if len(issues) == 0 {
		return []string{"- 対象期間にマージされた依頼なし"}
	}
	keys, groups := accountGroups(issues, func(i AccountRecord) string {
		s := fleet.Object(i["size"])
		return "行:" + fleet.Text(s["lineBand"]) + "/ファイル:" + fleet.Text(s["fileBand"])
	})
	sort.Strings(keys)
	out := []string{}
	for _, key := range keys {
		g := groups[key]
		known, reworked := 0, 0
		values := []any{}
		none, retried, returned := []AccountRecord{}, []AccountRecord{}, []AccountRecord{}
		for _, i := range g {
			values = append(values, referenceWeighted(i))
			if fleet.Number(i["unknownGenerations"]) > 0 {
				continue
			}
			known++
			r, b := fleet.Number(i["retries"]), fleet.Number(i["reviewReturns"])
			if r > 0 || b > 0 {
				reworked++
			}
			if r == 0 && b == 0 {
				none = append(none, i)
			}
			if r > 0 {
				retried = append(retried, i)
			}
			if b > 0 {
				returned = append(returned, i)
			}
		}
		rate := "不明"
		if known > 0 {
			rate = fmt.Sprint(math.Round(float64(reworked)/float64(known)*1000)/10) + "%"
		}
		d := AccountDistribution(values)
		few := ""
		if len(g) < 5 || known < 5 {
			few = fmt.Sprintf(" 【少数（%d件）のため傾向として断定しない（手戻りを判定できた件数は%d件）】", len(g), known)
		}
		out = append(out, fmt.Sprintf("- %s: 依頼%d件、手戻り率 %s（%d/%d）、不明%d件、weighted 中央値 %s・平均 %s（%v件）、平均: 手戻りなし %s／再試行あり %s／差し戻しあり %s%s", key, len(g), rate, reworked, known, len(g)-known, referenceNumber(d["median"]), referenceNumber(d["mean"]), d["known"], referenceMean(none), referenceMean(retried), referenceMean(returned), few))
	}
	return out
}
func referenceEstimateLines(issues []AccountRecord, risk AccountRecord) []string {
	paired, missingEstimate, missingActual := 0, 0, 0
	for _, i := range issues {
		e, s := fleet.Object(i["estimate"]), fleet.Object(i["size"])
		_, lines := accountKnown(s["lines"])
		_, files := accountKnown(s["files"])
		if e == nil {
			missingEstimate++
		}
		if !lines || !files {
			missingActual++
		}
		if e != nil && lines && files {
			paired++
		}
	}
	out := []string{fmt.Sprintf("- 見込みと実際: 両方あり%d件、見込みなし%d件、実際なし%d件（項目別欠測は各分母から除外、見込み0は倍率から除外）。", paired, missingEstimate, missingActual)}
	for _, key := range []string{"lines", "files"} {
		threshold := float64(500)
		label, ending := "行数", "超え"
		if key == "files" {
			threshold = 16
			label, ending = "ファイル数", "以上"
		}
		if n, ok := accountKnown(risk["risk"+strings.ToUpper(key[:1])+key[1:]]); ok {
			threshold = n
		}
		ratios := []any{}
		double, under := 0, 0
		for _, i := range issues {
			e := fleet.Object(i["estimate"])
			actual, ok := accountKnown(fleet.Object(i["size"])[key])
			if e == nil || !ok {
				continue
			}
			estimate := fleet.Number(e[key])
			if estimate > 0 {
				ratios = append(ratios, actual/estimate)
				if actual >= estimate*2 {
					double++
				}
			}
			if key == "files" && estimate < threshold && actual >= threshold || key == "lines" && estimate <= threshold && actual > threshold {
				under++
			}
		}
		d := AccountDistribution(ratios)
		out = append(out, fmt.Sprintf("- %s: 実際÷見込み 中央値%s・平均%s（%v件）、2倍以上%d件、閾値%v%sの過小見込み%d件", label, referenceRatio(d["median"]), referenceRatio(d["mean"]), d["known"], double, threshold, ending, under))
	}
	return out
}
func SizeReferenceText(project string, period Period, issues []AccountRecord, risk AccountRecord) string {
	out := []string{"# 変更の大きさ別の手戻り率と1件あたりの量（直近30日・マージ済みの依頼）", fmt.Sprintf("期間: %s〜%s（日本時間）。区分は独立レビューの最後の差分の行数帯/ファイル数帯（取れないものは「不明」）。", period.From.In(JST).Format("2006-01-02"), period.To.Add(-time.Millisecond).In(JST).Format("2006-01-02")), "手戻りあり = 再試行か自動の差し戻しが1回以上。手戻り率 = 手戻りあり件数/判定できた件数（世代を判定できない依頼は不明に数え分母から除く）。weighted は使用量の重み付き合計で、記録が欠けた依頼は平均・中央値から除く。", "少数の区分は傾向として断定せず、直近30日・マージ済みの依頼だけの実績として読む。"}
	scoped := []AccountRecord{}
	for _, i := range issues {
		if i["repo"] == project {
			scoped = append(scoped, i)
		}
	}
	if project != "" {
		out = append(out, "## 見込みの補正（選択済みプロジェクト）")
		out = append(out, referenceEstimateLines(scoped, risk)...)
	}
	out = append(out, "## 見込みの補正（全体）")
	out = append(out, referenceEstimateLines(issues, risk)...)
	sections := []struct {
		title string
		rows  []AccountRecord
	}{}
	if project != "" {
		sections = append(sections, struct {
			title string
			rows  []AccountRecord
		}{"選択済みプロジェクト " + project, scoped})
	}
	sections = append(sections, struct {
		title string
		rows  []AccountRecord
	}{"全体", issues})
	used, omitted := len(strings.Join(out, "\n")), 0
	for _, section := range sections {
		title := "## " + section.title
		out = append(out, "", title)
		used += len(title) + 2
		for _, line := range referenceBandLines(section.rows) {
			if used+len(line)+1 > 5840 {
				omitted++
				continue
			}
			out = append(out, line)
			used += len(line) + 1
		}
	}
	if omitted > 0 {
		out = append(out, fmt.Sprintf("- 省略: %d行（本文の上限 6000 bytes を超えた分）", omitted))
	}
	return strings.Join(out, "\n") + "\n"
}
func (s *Service) SizeReference(ctx context.Context, project string) (string, error) {
	missing := func(reason string) (string, error) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "規模別の手戻り資料なし（理由: " + reason + "）", nil
	}
	if s.DB == nil {
		return missing("受付の共有DBに接続していません")
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	period := ReferencePeriod(now, 30)
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if e != nil {
		return missing("集計を読み取れませんでした")
	}
	defer tx.Rollback()
	data, e := ReadReworkRows(ctx, tx, "", period)
	if e != nil {
		return missing("集計を読み取れませんでした")
	}
	if e = s.issueConsultationRows(ctx, tx, &data, period.To); e != nil {
		return missing("起票前の使用量を読み取れませんでした")
	}
	if e = tx.Commit(); e != nil {
		return missing("集計を読み取れませんでした")
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	data.Usage, _ = usage.CorrectCosts(data.Usage, nil)
	return SizeReferenceText(project, period, sizeReferenceIssues(data, breakdownWeights(s.Weights)), s.Risk), nil
}
