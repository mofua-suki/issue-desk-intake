package admincli

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"math"
	"strconv"
	"strings"
)

var issueMetrics = []string{"inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens", "uncachedInputTokens", "normalizedCacheWriteTokens", "weighted", "elapsedMs"}

func reportCell(v any) string {
	if v == nil {
		return "不明"
	}
	if n, ok := v.(float64); ok {
		return strconv.FormatFloat(math.Round(n*100)/100, 'f', -1, 64)
	}
	return strings.NewReplacer("|", " ", "\r", " ", "\n", " ").Replace(fmt.Sprint(v))
}
func reportTable(headers []string, rows [][]any) []string {
	out := []string{strings.Join(headers, " | "), strings.TrimSuffix(strings.Repeat("--- | ", len(headers)), " | ")}
	for _, row := range rows {
		cells := []string{}
		for _, v := range row {
			cells = append(cells, reportCell(v))
		}
		out = append(out, strings.Join(cells, " | "))
	}
	return out
}
func usageReportTable(r map[string]any) string {
	p, total := fleet.Object(r["period"]), fleet.Object(r["total"])
	lines := []string{fmt.Sprintf("マージされた依頼: %v〜%v（日本時間） %v件", p["fromDate"], p["toDate"], total["count"]), "時間はミリ秒。工程の部分合計は不明件数を併記。再試行の使用量は工程と重なる内訳。", fmt.Sprintf("下書きは最初の起票先へ集約（分割相談 %v行）。読めない行 %v。", fleet.Object(r["attribution"])["splitRows"], r["skipped"]), ""}
	rows := [][]any{}
	for _, raw := range fleet.List(r["issues"]) {
		i := fleet.Object(raw)
		size, d := fleet.Object(i["size"]), fleet.Object(i["durations"])
		rows = append(rows, []any{i["key"], i["tier"], size["lines"], size["files"], d["filedToStartMs"], d["startToPrMs"], d["prToMergeMs"], d["filedToMergeMs"], i["retries"], i["reviewReturns"], i["unknownGenerations"]})
	}
	lines = append(lines, reportTable([]string{"依頼", "段", "変更行", "ファイル数", "起票→着手", "着手→PR", "PR→マージ", "起票→マージ", "再試行", "自動差し戻し", "不明世代"}, rows)...)
	for _, raw := range fleet.List(r["issues"]) {
		i := fleet.Object(raw)
		total := fleet.Object(i["total"])
		lines = append(lines, "", fmt.Sprintf("%v 工程別（使用量記録 %v行・途中の記録 %v行）", i["key"], total["count"], total["partialRows"]))
		rows = [][]any{}
		steps := append([]any{}, fleet.List(i["steps"])...)
		for index, key := range []string{"total", "retryUsage"} {
			value := map[string]any{}
			for k, v := range fleet.Object(i[key]) {
				value[k] = v
			}
			value["step"] = []string{"全工程", "再試行（内訳）"}[index]
			steps = append(steps, value)
		}
		for _, raw := range steps {
			s := fleet.Object(raw)
			row := []any{s["step"], s["count"]}
			for _, key := range issueMetrics {
				v := s[key]
				if n := fleet.Number(fleet.Object(s["unknown"])[key]); n > 0 {
					v = fmt.Sprintf("%s（不明%v行）", reportCell(v), n)
				}
				row = append(row, v)
			}
			rows = append(rows, row)
		}
		lines = append(lines, reportTable(append([]string{"工程", "記録数"}, issueMetrics...), rows)...)
	}
	for index, axis := range []string{"total", "repositories", "tiers", "sizes", "combinations"} {
		name := []string{"全体", "リポジトリ別", "段別", "大きさ別", "組み合わせ"}[index]
		groups := fleet.List(r[axis])
		if axis == "total" {
			v := map[string]any{"key": "全体"}
			for k, x := range total {
				v[k] = x
			}
			groups = []any{v}
		}
		rows = [][]any{}
		for _, raw := range groups {
			g := fleet.Object(raw)
			for _, section := range []string{"metrics", "durations", "rework"} {
				keys := issueMetrics
				if section == "durations" {
					keys = []string{"filedToStartMs", "startToPrMs", "prToMergeMs", "filedToMergeMs"}
				} else if section == "rework" {
					keys = []string{"retries", "reviewReturns", "reviewHolds", "implementationRetries", "nonImplementationRetries", "unknownRetries"}
				}
				for _, metric := range keys {
					v := fleet.Object(fleet.Object(g[section])[metric])
					rows = append(rows, []any{g["key"], g["count"], metric, v["known"], v["unknown"], v["median"], v["mean"], v["sum"]})
				}
			}
		}
		lines = append(lines, "", name)
		lines = append(lines, reportTable([]string{"区分", "依頼数", "指標", "既知", "不明", "中央値/件", "平均/件", "合計"}, rows)...)
		rows = [][]any{}
		for _, raw := range groups {
			g := fleet.Object(raw)
			v := fleet.Object(g["reworkRate"])
			rows = append(rows, []any{g["key"], v["count"], v["denominator"], v["unknown"], v["rate"]})
		}
		lines = append(lines, "", name+" 手戻り率（再試行か自動差し戻しが1回以上。不明な依頼は分母から除く）")
		lines = append(lines, reportTable([]string{"区分", "手戻りあり件数", "判定できた件数", "不明", "割合"}, rows)...)
	}
	return strings.Join(lines, "\n")
}
