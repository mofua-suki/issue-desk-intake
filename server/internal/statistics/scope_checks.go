package statistics

import (
	"regexp"
)

var scopeCheckPattern = regexp.MustCompile(`^範囲の照合 ([^\s#]+)#\d+(（後から付けた宣言）)?: (?:宣言外の変更 (\d+)件（.*）|(宣言外の変更なし)|(実変更を読めません)|(宣言が(?:ありません|不明です)（記録のみ）))(?:。公開の前に .+ のマージを待ちます)?$`)

func scopeChecks(lines []logLine, p Period, project string, readErr error) map[string]any {
	out := map[string]any{}
	for _, kind := range []string{"normal", "backfilled", "undeclared"} {
		reason := map[string]string{"normal": "期間内に通常の宣言での照合がありません", "backfilled": "期間内に後から付けた宣言での照合がありません", "undeclared": "期間内に宣言なし・不明のままの照合がありません"}[kind]
		if readErr != nil {
			reason = "監視の記録を読めませんでした"
		} else if len(lines) == 0 {
			reason = "監視の記録がありません"
		} else if !lines[0].At.Before(p.To) {
			reason = "記録の始まりより前の期間です"
		}
		count, withOutside, outside, unreadable := 0, 0, 0, 0
		for _, line := range lines {
			if line.At.Before(p.From) || !line.At.Before(p.To) {
				continue
			}
			m := scopeCheckPattern.FindStringSubmatch(line.Body)
			if m == nil || project != "" && m[1] != project {
				continue
			}
			k := "normal"
			if m[6] != "" {
				k = "undeclared"
			} else if m[2] != "" {
				k = "backfilled"
			}
			if k != kind {
				continue
			}
			count++
			n := intText(m[3])
			outside += n
			if n > 0 {
				withOutside++
			}
			if m[5] != "" {
				unreadable++
			}
		}
		if count == 0 || readErr != nil {
			out[kind] = map[string]any{"reason": reason}
		} else {
			var since any
			if len(lines) > 0 && lines[0].At.After(p.From) {
				since = lines[0].At.In(JST).Format("2006-01-02")
			}
			out[kind] = map[string]any{"reason": nil, "since": since, "checks": count, "withOutside": withOutside, "outside": outside, "unreadable": unreadable}
		}
	}
	return out
}
