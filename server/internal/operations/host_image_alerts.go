package operations

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"strings"
	"time"
)

func imageFailureLines(f map[string]any) []string {
	text := func(v any) string {
		if s := fleet.Text(v); s != "" {
			return s
		}
		return "未受信"
	}
	checks := fleet.Strings(f["selfCheckFailed"])
	check := "未受信"
	if len(checks) > 0 {
		check = strings.Join(checks, ", ")
	}
	return []string{"失敗の段: " + text(f["stage"]), "理由: " + text(f["reason"]), "詳細: " + text(f["detail"]), "失敗した自己検査: " + check}
}
func (s *Service) hostImageAlerts(view map[string]any) []any {
	out := []any{}
	for _, raw := range fleet.List(view["hosts"]) {
		r := fleet.Object(raw)
		if r["alive"] != true {
			continue
		}
		host := fleet.Text(r["name"])
		image := fleet.Object(r["image"])
		desired := fleet.Object(image["desired"])
		results := fleet.List(image["results"])
		failed := fleet.Strings(image["failedTags"])
		failure := fleet.Object(image["lastFailure"])
		if alert := s.failedDesiredTagAlert(host, r, desired, results, failed, failure); alert != nil {
			out = append(out, alert)
		}
		if alert := s.repeatedUpdateFailureAlert(host, results, failure); alert != nil {
			out = append(out, alert)
		}
	}
	return out
}

// hostDriftAlert はPCのイメージの版ずれの要対応1件を作る。
func hostDriftAlert(id, host, title string, at time.Time, lines []string) map[string]any {
	return map[string]any{"id": id + host, "kind": "host_drift", "status": "stall_alert", "project": fleet.SystemProject, "host": host, "detectedAt": iso(at), "title": title, "detail": usage.Redact(strings.Join(lines, "\n"))}
}

// failedDesiredTagAlert は望む版のタグがそのPCで失敗していて入れ替わらないときの要対応を作る。該当しなければ nil を返す。
func (s *Service) failedDesiredTagAlert(host string, r, desired map[string]any, results []any, failed []string, failure map[string]any) map[string]any {
	for _, tag := range failed {
		if tag != desired["tag"] || desired["revision"] == r["revision"] || !revision.MatchString(fleet.Text(desired["revision"])) || !revision.MatchString(fleet.Text(r["revision"])) {
			continue
		}
		at := s.now()
		for _, v := range results {
			x := fleet.Object(v)
			if x["status"] == "failed" && x["tag"] == tag {
				if stamp, e := time.Parse(time.RFC3339Nano, fleet.Text(x["at"])); e == nil {
					at = stamp
				}
				break
			}
		}
		lines := []string{"PC: " + host, "監視の版: " + fleet.Text(r["revision"]), fmt.Sprintf("望む版: %s（source: %v / role: %v）", tag, desired["source"], desired["role"]), "失敗タグ: " + strings.Join(failed, ", "), "最後の失敗の時刻: " + at.In(time.FixedZone("JST", 9*3600)).Format("2006-01-02 15:04") + "（日本時間）"}
		lines = append(lines, imageFailureLines(failure)...)
		lines = append(lines, "同じタグは再試行されないので、新しいコミットの発行か、望む版を未失敗のタグへ戻すまで入れ替わりません。")
		return hostDriftAlert("host-drift-failed-tag-", host, "望む版のイメージが失敗していて入れ替わりません（"+host+"）", at, lines)
	}
	return nil
}

// repeatedUpdateFailureAlert はほかのPCが成功した版の更新が、このPCで2回続けて失敗したときの要対応を作る。該当しなければ nil を返す。
func (s *Service) repeatedUpdateFailureAlert(host string, results []any, failure map[string]any) map[string]any {
	if len(results) < 2 || failure == nil {
		return nil
	}
	a, b := fleet.Object(results[0]), fleet.Object(results[1])
	if a["status"] != "failed" || b["status"] != "failed" || len(fleet.Strings(a["succeededOn"])) == 0 || len(fleet.Strings(b["succeededOn"])) == 0 {
		return nil
	}
	at := s.now()
	for _, x := range []map[string]any{a, b} {
		if stamp, e := time.Parse(time.RFC3339Nano, fleet.Text(x["at"])); e == nil && stamp.Before(at) {
			at = stamp
		}
	}
	lines := []string{"PC: " + host}
	for _, x := range []map[string]any{a, b} {
		lines = append(lines, fmt.Sprintf("失敗したタグ: %v（ほかのPCの成功: %s）", x["tag"], strings.Join(fleet.Strings(x["succeededOn"]), ", ")))
	}
	lines = append(lines, imageFailureLines(failure)...)
	lines = append(lines, "周期の時刻: "+fleet.Text(failure["cycleAt"]), "失敗が続く間は drain が短く打ち切られます。", "直近の結果が成功になる、または失敗が続かなくなると消えます。")
	return hostDriftAlert("host-drift-update-failing-", host, "ほかのPCが成功した版の更新が"+host+"で2回続けて失敗しています", at, lines)
}
