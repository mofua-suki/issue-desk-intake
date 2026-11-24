package operations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"time"
)

// Delay alerts use complete observed one-minute windows. A process pause that
// skips a whole window is a coverage gap, never several manufactured samples.
type runtimeDelayWindow struct {
	Start, First, Last time.Time
	Max                float64
	Samples            int
}

func (m *RuntimeMeasurements) observeWindow(at time.Time, delay float64) {
	start := at.Truncate(time.Minute)
	if m.window.Start.IsZero() {
		m.window = runtimeDelayWindow{Start: start}
	}
	if !start.Equal(m.window.Start) {
		complete := !m.window.First.IsZero() && m.window.First.Sub(m.window.Start) <= 5*time.Second && at.Sub(m.window.Last) <= 5*time.Second && start.Sub(m.window.Start) == time.Minute
		if complete && m.window.Max > 2000 {
			m.delayClearStreak = 0
			m.delayStreak++
			m.delayLastOver = m.window.Last
			if m.delayStreak >= 3 && m.delayAlertAt.IsZero() {
				m.delayAlertAt = m.window.Start
			}
			m.delayAlertMax = m.window.Max
		} else {
			m.delayStreak = 0
			if complete && m.window.Max <= 2000 {
				m.delayClearStreak++
			} else {
				m.delayClearStreak = 0
			}
		}
		if !m.delayAlertAt.IsZero() && m.delayClearStreak >= 10 {
			m.delayAlertAt = time.Time{}
		}

		m.window = runtimeDelayWindow{Start: start}
	}
	if m.window.First.IsZero() {
		m.window.First = at
	}
	m.window.Last = at
	m.window.Samples++
	m.window.Max = max(m.window.Max, delay)
}
func (m *RuntimeMeasurements) Alerts(ctx context.Context) ([]any, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []any{}
	if !m.delayAlertAt.IsZero() {
		out = append(out, map[string]any{"id": "intake-go-timer-delay", "kind": "intake_runtime_delay", "status": "stall_alert", "project": fleet.SystemProject, "detectedAt": iso(m.delayAlertAt), "title": "受付のGoの周期起動が遅れ続けています", "detail": fmt.Sprintf("観測元: go-runtime\n観測期間: %s〜%s\n直近の超過窓の最大遅延: %vms\n閾値: 2000msを1分ごとの観測で3回続けて超過。10分間超過がない状態で外れます。\nNode.jsのイベントループとは別の指標です。NASのCPU負荷と共有DB往復時間を確認してください。", iso(m.started), iso(m.ended), m.delayAlertMax)})
	}
	return out, nil
}
