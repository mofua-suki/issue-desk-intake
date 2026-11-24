package operations

import (
	"fmt"
	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/fleet"
	"sort"
	"strings"
	"time"
)

// Count physical calls across all steps and providers in the trailing hour.
// The default threshold is three; an alert requires more than three events.
// The window includes its lower and upper endpoints.
const idleTimeoutWindow = time.Hour
const idleTimeoutThreshold = 3

type idleTimeoutRecord struct {
	At           float64 `json:"at"`
	Model        string  `json:"model"`
	Effort       string  `json:"effort"`
	DiagnosticID string  `json:"diagnosticId"`
}

func validIdleTimeout(e idleTimeoutRecord) bool {
	return validFailureTime(e.At) && len(e.Model) <= 128 && len(e.Effort) <= 32 && len(e.DiagnosticID) <= 64
}
func idleValue(value string, max int) string {
	if value == "" || len(value) > max || failureID(value) != value {
		return "不明"
	}
	return value
}
func (s *AIFailures) addIdleTimeout(o ai.Observation, at float64) {
	cutoff := at - float64(idleTimeoutWindow.Milliseconds())
	events := s.record.IdleTimeouts[:0]
	for _, e := range s.record.IdleTimeouts {
		if e.At >= cutoff {
			events = append(events, e)
		}
	}
	s.record.IdleTimeouts = append(events, idleTimeoutRecord{At: at, Model: idleValue(o.Model, 128), Effort: idleValue(o.Effort, 32), DiagnosticID: idleValue(o.DiagnosticID, 64)})
}
func (s *AIFailures) idleTimeoutAlert(now time.Time) map[string]any {
	end := float64(now.UnixMilli())
	start := end - float64(idleTimeoutWindow.Milliseconds())
	counts := map[string]int{}
	var latest idleTimeoutRecord
	count := 0
	for _, e := range s.record.IdleTimeouts {
		if e.At < start || e.At > end {
			continue
		}
		count++
		counts[fmt.Sprintf("モデル: %s / 推論の強さ: %s", idleValue(e.Model, 128), idleValue(e.Effort, 32))]++
		if count == 1 || e.At >= latest.At {
			latest = e
		}
	}
	if count <= idleTimeoutThreshold {
		return nil
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	detail := []string{fmt.Sprintf("無出力時間切れ %d件（直近%s、境界を含む。閾値%d件超）", count, idleTimeoutWindow, idleTimeoutThreshold)}
	for _, k := range keys {
		detail = append(detail, fmt.Sprintf("%s: %d件", k, counts[k]))
	}
	detail = append(detail, "直近の診断ID: "+idleValue(latest.DiagnosticID, 64))
	return map[string]any{"id": "intake-ai-idle-timeouts", "kind": "intake_ai_idle_timeout", "status": "stall_alert", "project": fleet.SystemProject, "detectedAt": iso(time.UnixMilli(int64(latest.At))), "title": "受付AIの無出力時間切れが頻発", "detail": strings.Join(detail, "\n")}
}
