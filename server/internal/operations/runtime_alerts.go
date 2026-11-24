package operations

import (
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"math"
	"os"
	"path/filepath"
	"time"
)

func (s *Service) runtimeAlerts() []any {
	out := []any{}
	if alert := intakeHandoffAlert(s.readRuntimeState("intake-handoff.json", 4096), s.now()); alert != nil {
		out = append(out, alert)
	}
	if alert := intakeNASUpdateAlert(s.readRuntimeState("intake-nas-update.json", 256)); alert != nil {
		out = append(out, alert)
	}
	return out
}

// readRuntimeState は受付の状態ファイルを上限の大きさまで読む。読めなければ nil を返す。
func (s *Service) readRuntimeState(name string, max int64) map[string]any {
	file := filepath.Join(s.RuntimeDir, "state", name)
	stat, e := os.Stat(file)
	if e != nil || stat.Size() > max {
		return nil
	}
	raw, e := os.ReadFile(file)
	if e != nil {
		return nil
	}
	var r map[string]any
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	return r
}

// intakeHandoffAlert は直近7日の受付の入れ替えで入口の断が30秒以上続いた記録を要対応にする。該当しなければ nil を返す。
func intakeHandoffAlert(r map[string]any, now time.Time) map[string]any {
	complete := fleet.Number(r["completedAt"])
	total, failures, ms := fleet.Number(r["total"]), fleet.Number(r["failures"]), fleet.Number(r["longestFailureMs"])
	valid := len(r) == 5 && revision.MatchString(fleet.Text(r["revision"])) && total > 0 && math.Trunc(total) == total && failures >= 0 && failures <= total && math.Trunc(failures) == failures && ms >= 0 && math.Trunc(ms) == ms && (failures > 0 || ms == 0) && complete > 0 && complete <= 8640000000000000-9*3600000 && math.Trunc(complete) == complete
	if valid && complete <= float64(now.UnixMilli()) && float64(now.UnixMilli())-complete <= 7*86400000 && ms >= 30000 {
		at := time.UnixMilli(int64(complete))
		return map[string]any{"id": "intake-handoff", "kind": "intake_handoff", "status": "stall_alert", "project": fleet.SystemProject, "detectedAt": iso(at), "title": "受付の入れ替え中に入口の断が30秒以上続きました", "detail": fmt.Sprintf("更新先の版: %v\n失敗数 / 確認の総数: %.0f / %.0f\n最長の連続した断: %.0fミリ秒\n完了時刻: %s（日本時間）\n閾値は hostd の1要求の期限（30秒）です。次の入れ替えの記録が閾値未満なら、この要対応は消えます。", r["revision"], failures, total, ms, at.In(time.FixedZone("JST", 9*3600)).Format("2006-01-02 15:04:05"))}
	}
	return nil
}

// intakeNASUpdateAlert はNAS更新が同じ更新先で2回以上続けて失敗した記録を要対応にする。該当しなければ nil を返す。
func intakeNASUpdateAlert(r map[string]any) map[string]any {
	validStage := false
	for _, stage := range []string{"entry", "start", "healthz", "drain", "entry-prepare", "handoff", "takeover", "entry-check", "stop-old"} {
		if r["stage"] == stage {
			validStage = true
		}
	}
	failures := fleet.Number(r["failures"])
	if len(r) == 3 && revision.MatchString(fleet.Text(r["revision"])) && failures >= 2 && failures <= 999999999 && math.Trunc(failures) == failures && validStage {
		return map[string]any{"id": "intake-nas-update", "kind": "intake_nas_update", "status": "stall_alert", "project": fleet.SystemProject, "title": "NAS更新が同じ更新先で連続して失敗し、望む版へ進んでいません", "detail": fmt.Sprintf("更新先の版: %v\n連続失敗回数: %.0f回\n最後の失敗段階: %v\n更新成功、または次の更新開始時に更新先の版が変わると、この要対応は消えます。", r["revision"], failures, r["stage"])}
	}
	return nil
}
